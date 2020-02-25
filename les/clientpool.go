// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package les

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

const (
	defaultPosExpTC          = 36000            // default time constant (in seconds) for exponentially reducing positive balance
	defaultNegExpTC          = 3600             // default time constant (in seconds) for exponentially reducing negative balance
	lazyQueueRefresh         = time.Second * 10 // refresh period of the connected queue
	tryActivatePeriod        = time.Second * 5  // periodically check whether inactive clients can be activated
	dropInactiveCycles       = 2                // number of activation check periods after non-priority inactive peers are dropped
	persistExpirationRefresh = time.Minute * 5  // refresh period of the token expiration persistence
	freeRatioTC              = time.Hour        // time constant of token supply control based on free service availability

	// activeBias is applied to already connected clients So that
	// already connected client won't be kicked out very soon and we
	// can ensure all connected clients can have enough time to request
	// or sync some data.
	//
	// todo(rjl493456442) make it configurable. It can be the option of
	// free trial time!
	activeBias = time.Minute * 3

	// connectedClientSize is the size of connected client cache.
	connectedClientSize = 512
)

// expirer is a tiny tool to calculate cumulative expiration.
type expirer struct {
	lock    sync.RWMutex
	clock   mclock.Clock
	close   chan struct{}
	last    mclock.AbsTime
	rate    uint64
	rateI   float64
	expired fixed64
	coeff   func() float64
}

func newExpirer(clock mclock.Clock, rate uint64, expired fixed64, coeff func() float64) *expirer {
	exp := &expirer{
		clock:   clock,
		last:    clock.Now(),
		rate:    rate,
		expired: expired,
		coeff:   coeff,
		close:   make(chan struct{}),
	}
	if rate != 0 {
		exp.rateI = fixedFactor / float64(rate*uint64(time.Second))
	}
	go exp.loop()
	return exp
}

func (e *expirer) stop() {
	close(e.close)
}

func (e *expirer) loop() {
	for {
		select {
		case <-e.close:
			return
		case <-e.clock.After(time.Second * 5):
			e.lock.Lock()
			now := e.clock.Now()
			diff := now - e.last
			if diff < 0 {
				diff = 0
			}
			if e.rate != 0 {
				e.expired += fixed64(float64(diff) * e.rateI * e.coeff())
			}
			e.last = now
			e.lock.Unlock()
		}
	}
}

// setRate sets the expiration rate specified in second,
// 0 means infinite (no expiration).
func (e *expirer) setRate(rate uint64) {
	e.lock.Lock()
	defer e.lock.Unlock()

	if rate != 0 {
		e.rateI = fixedFactor / float64(rate*uint64(time.Second))
	} else {
		e.rateI = 0
	}
	e.rate = rate
}

// getRate returns the current expiration rate in second.
func (e *expirer) getRate() uint64 {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return e.rate
}

// expiration implements expirationController which returns
// the cumulative expiration.
func (e *expirer) expiration(now mclock.AbsTime) fixed64 {
	e.lock.RLock()
	defer e.lock.RUnlock()

	if e.rate == 0 {
		return e.expired
	}
	diff := now - e.last
	if diff < 0 {
		diff = 0
	}
	return e.expired + fixed64(float64(diff)*e.rateI*e.coeff())
}

// clientPool implements a client database that assigns a priority to each client
// based on a positive and negative balance. Positive balance is externally assigned
// to prioritized clients and is decreased with connection time and processed
// requests (unless the price factors are zero). If the positive balance is zero
// then negative balance is accumulated.
//
// Balance tracking and priority calculation for connected clients is done by
// balanceTracker. activeQueue ensures that clients with the lowest positive or
// highest negative balance get evicted when the total capacity allowance is full
// and new clients with a better balance want to connect.
//
// Already connected nodes receive a small bias in their favor in order to avoid
// accepting and instantly kicking out clients. In theory, we try to ensure that
// each client can have several minutes of connection time.
//
// Balances of disconnected clients are stored in nodeDB including positive balance
// and negative banalce. Boeth positive balance and negative balance will decrease
// exponentially. If the balance is low enough, then the record will be dropped.
type clientPool struct {
	ndb        *nodeDB
	lock       sync.Mutex
	clock      mclock.Clock
	stopCh     chan struct{}
	closed     bool
	removePeer func(enode.ID)

	clientCache   map[enode.ID]*clientInfo // Cache contains both active peers and inactive peers
	activeQueue   *prque.LazyQueue         // Queue of all connected active peers
	inactiveQueue *prque.Prque             // Queue of all connected but inactive peers(schedule to drop, waiting to be activiated)

	// Inactive **free** clients will be scheduled for dropping.
	waitingDrop map[uint64][]*clientInfo
	dropCycle   uint64

	posExpirer *expirer
	negExpirer *expirer
	freeRatio  metrics.Meter

	activeBalances   expiredValue
	inactiveBalances expiredValue
	lastUpdate       mclock.AbsTime

	posFactors priceFactors
	negFactors priceFactors

	connLimit      int    // The maximum number of connections that clientpool can support
	capLimit       uint64 // The maximum cumulative capacity that clientpool can support
	activeCap      uint64 // The sum of the capacity of the current clientpool connected
	activePriority uint64 // The sum of the capacity of currently connected priority clients
	minCap         uint64 // The minimal capacity value allowed for any client
	freeClientCap  uint64 // The capacity value of each free client
	disableBias    bool   // Disable connection bias(used in testing)
}

// clientPoolPeer represents a client peer in the pool.
// Positive balances are assigned to node key while negative balances are assigned
// to freeClientId. Currently network IP address without port is used because
// clients have a limited access to IP addresses while new node keys can be easily
// generated so it would be useless to assign a negative value to them.
type clientPoolPeer interface {
	ID() enode.ID
	freeClientId() string
	updateCapacity(uint64)
	freeze()
}

// clientInfo defines all information required by clientpool.
type clientInfo struct {
	id             enode.ID
	address        string
	active         bool
	capacity       uint64
	priority       bool
	pool           *clientPool
	peer           clientPoolPeer
	queueIndex     int
	balanceTracker balanceTracker

	// Various statistics
	connectAt   mclock.AbsTime
	activateAt  mclock.AbsTime
	activateT   time.Duration
	deactivateN int
}

// connSetIndex callback updates clientInfo item index in activeQueue
func connSetIndex(a interface{}, index int) {
	a.(*clientInfo).queueIndex = index
}

// connPriority callback returns actual priority of clientInfo item in activeQueue
func connPriority(a interface{}, now mclock.AbsTime) int64 {
	c := a.(*clientInfo)
	return c.balanceTracker.getPriority(now)
}

// connMaxPriority callback returns estimated maximum priority of clientInfo item in activeQueue
func connMaxPriority(a interface{}, until mclock.AbsTime) int64 {
	c := a.(*clientInfo)
	pri := c.balanceTracker.estimatedPriority(until, true)
	c.balanceTracker.addCallback(balanceCallbackQueue, pri+1, func() {
		c.pool.lock.Lock()
		if c.active && c.queueIndex != -1 {
			c.pool.activeQueue.Update(c.queueIndex)
		}
		c.pool.lock.Unlock()
	})
	return pri
}

// newClientPool creates a new client pool
func newClientPool(db ethdb.Database, minCap, freeClientCap uint64, clock mclock.Clock, removePeer func(enode.ID)) *clientPool {
	ndb := newNodeDB(db, clock)
	posExp, negExp := ndb.getExpiration()
	pool := &clientPool{
		ndb:           ndb,
		clock:         clock,
		clientCache:   make(map[enode.ID]*clientInfo),
		activeQueue:   prque.NewLazyQueue(connSetIndex, connPriority, connMaxPriority, clock, lazyQueueRefresh),
		inactiveQueue: prque.New(connSetIndex),
		waitingDrop:   make(map[uint64][]*clientInfo),
		freeRatio:     metrics.NewMeterForced(),
		minCap:        minCap,
		freeClientCap: freeClientCap,
		removePeer:    removePeer,
		stopCh:        make(chan struct{}),
	}
	// set default expiration constants used by tests and
	// server can overwrites this if token sale is active
	pool.posExpirer = newExpirer(clock, 0, posExp, func() float64 { return pool.freeRatio.Rate1() / 100 })
	pool.negExpirer = newExpirer(clock, defaultNegExpTC, negExp, func() float64 { return pool.freeRatio.Rate1() / 100 })

	// calculate total token balance amount
	var start enode.ID
	for {
		ids := pool.ndb.getPosBalanceIDs(start, enode.ID{}, 1000)
		var stop bool
		l := len(ids)
		if l == 1000 {
			l--
			start = ids[l]
		} else {
			stop = true
		}
		for i := 0; i < l; i++ {
			pb := pool.ndb.getOrNewBalance(ids[i].Bytes(), false)
			if pb.value.value(pool.posExpirer.expiration(clock.Now())) <= uint64(time.Second) {
				pool.ndb.delBalance(ids[i].Bytes(), false)
			}
			pool.inactiveBalances.addExp(pb.value)
		}
		if stop {
			break
		}
	}
	// The positive and negative balances of clients are stored in database
	// and both of these decay exponentially over time. Delete them if the
	// value is small enough.
	ndb.evictCallBack = func(now mclock.AbsTime, neg bool, b tokenBalance) bool {
		var expiration fixed64
		if neg {
			expiration = pool.negExpirer.expiration(now)
		} else {
			expiration = pool.posExpirer.expiration(now)
		}
		return b.value.value(expiration) <= uint64(time.Second)
	}
	go func() {
		for {
			select {
			case <-clock.After(lazyQueueRefresh):
				pool.lock.Lock()
				pool.activeQueue.Refresh()
				pool.lock.Unlock()
			case <-pool.stopCh:
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-clock.After(persistExpirationRefresh):
				now := pool.clock.Now()
				posExp := pool.posExpirer.expiration(now)
				negExp := pool.negExpirer.expiration(now)
				pool.ndb.setExpiration(posExp, negExp)
			case <-pool.stopCh:
				return
			}
		}
	}()
	go pool.dropPeers()
	go pool.freeRatioLoop()
	return pool
}

func (pool *clientPool) dropPeers() {
	for {
		select {
		case <-pool.clock.After(tryActivatePeriod):
			pool.lock.Lock()
			pool.tryActivateClients()
			for _, c := range pool.waitingDrop[pool.dropCycle] {
				if _, ok := pool.clientCache[c.id]; ok && !c.active && !c.priority {
					pool.drop(c.peer, true)
				}
			}
			delete(pool.waitingDrop, pool.dropCycle)
			pool.dropCycle++
			pool.lock.Unlock()
		case <-pool.stopCh:
			return
		}
	}
}

func (pool *clientPool) freeRatioLoop() {
	for {
		select {
		case <-pool.clock.After(time.Second * 5):
			pool.lock.Lock()
			active, limit, freecap := pool.activePriority, pool.capLimit, pool.freeClientCap
			pool.lock.Unlock()

			ratio := float64(0)
			if active < limit {
				free := limit - active
				if free > freecap {
					threshold := limit / 4
					if free > limit {
						ratio = 1
					} else {
						ratio = float64(free-freecap) / float64(threshold-freecap)
					}
				}
			}
			pool.freeRatio.Mark(int64(ratio * 100))
		case <-pool.stopCh:
			return
		}
	}
}

// stop shuts the client pool down
func (pool *clientPool) stop() {
	close(pool.stopCh)
	pool.lock.Lock()
	pool.closed = true
	pool.lock.Unlock()
	now := pool.clock.Now()
	pool.ndb.setExpiration(pool.posExpirer.expiration(now), pool.negExpirer.expiration(now))
	pool.ndb.close()
	pool.posExpirer.stop()
	pool.negExpirer.stop()
}

// setExpirationTCs sets positive and negative token expiration time constants.
// Specified in seconds, 0 means infinite (no expiration).
func (pool *clientPool) setExpirationTCs(pos, neg uint64) {
	pool.posExpirer.setRate(pos)
	pool.negExpirer.setRate(neg)
}

// getExpirationTCs returns the current positive and negative token
// expiration time constants
func (pool *clientPool) getExpirationTCs() (uint64, uint64) {
	return pool.posExpirer.getRate(), pool.negExpirer.getRate()
}

// totalTokenLimit returns the current token supply limit. Token prices are based
// on the ratio of total token amount and supply limit while the limit depends on
// average freeRatio, ensuring the availability of free service most of the time.
func (pool *clientPool) totalTokenLimit() uint64 {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	d := pool.freeRatio.Rate1() / 100
	if d > 0.5 {
		d = math.Log(d/0.5) * float64(freeRatioTC)
	} else {
		d = 0
	}
	return uint64(d * float64(pool.capLimit) * pool.posFactors.capacityFactor)
}

// totalTokenAmount returns the total amount of currently existing service tokens
func (pool *clientPool) totalTokenAmount() uint64 {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	now := pool.clock.Now()
	if now > pool.lastUpdate+mclock.AbsTime(time.Second) {
		pool.activeBalances = expiredValue{}
		for _, c := range pool.clientCache {
			if c.active {
				pos, _ := c.balanceTracker.getBalance(now)
				pool.activeBalances.addExp(pos)
			}
		}
		pool.lastUpdate = now
	}
	sum := pool.activeBalances
	sum.addExp(pool.inactiveBalances)
	return sum.value(pool.posExpirer.expiration(now))
}

func (pool *clientPool) newClient(id enode.ID, address string, cap uint64, peer clientPoolPeer) *clientInfo {
	// Load positive balance, erase it if it's already too small
	now := pool.clock.Now()
	pb := pool.ndb.getOrNewBalance(id.Bytes(), false)
	if pb != (tokenBalance{}) && pb.value.value(pool.posExpirer.expiration(now)) <= uint64(time.Second) {
		pool.ndb.delBalance(id.Bytes(), false)
		pb = tokenBalance{}
	}
	// Load negative balance, erase it if it's already too small
	nb := pool.ndb.getOrNewBalance([]byte(address), true)
	if nb != (tokenBalance{}) && nb.value.value(pool.negExpirer.expiration(now)) <= uint64(time.Second) {
		pool.ndb.delBalance([]byte(address), true)
		nb = tokenBalance{}
	}
	client := &clientInfo{
		id:         id,
		address:    address,
		capacity:   cap,
		priority:   pb.value.base != 0,
		pool:       pool,
		peer:       peer,
		queueIndex: -1,
		connectAt:  pool.clock.Now(),
	}
	client.balanceTracker = newBalanceTracker(pool.posExpirer, pool.negExpirer, pool.clock, cap, pb.value, nb.value, pool.posFactors, pool.negFactors)
	return client
}

// connect should be called after a successful handshake. If the connection was
// rejected, there is no need to call disconnect.
func (pool *clientPool) connect(peer clientPoolPeer, reqCapacity uint64) (uint64, error) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	// Short circuit if clientPool is already closed.
	if pool.closed {
		return 0, errors.New("client pool is already closed")
	}
	// De-duplicate connected peers.
	id, freeID := peer.ID(), peer.freeClientId()
	if _, ok := pool.clientCache[id]; ok {
		clientRejectedMeter.Mark(1)
		return 0, fmt.Errorf("Client already connected address=%s id=%s", freeID, peerIdToString(id))
	}
	// Check whether there is enough capacity space for accpeting new client.
	if reqCapacity == 0 {
		reqCapacity = pool.freeClientCap
	}
	if reqCapacity < pool.minCap {
		reqCapacity = pool.minCap
	}
	client := pool.newClient(id, freeID, reqCapacity, peer)
	if missing, _ := pool.capAvailable(id, freeID, reqCapacity, 0, client.balanceTracker, true); missing != 0 {
		// Capacity is not available, add client to inactive queue.
		if len(pool.clientCache) >= connectedClientSize {
			return 0, errors.New("client pool is full")
		}
		pool.clientCache[id] = client
		pool.inactiveQueue.Push(client, -connPriority(client, pool.clock.Now()))
		return 0, nil
	}
	// capacity is available, add client
	pool.clientCache[id] = client
	pool.activateClient(client, true)
	clientConnectedMeter.Mark(1)
	log.Debug("Client accepted", "address", freeID)
	return client.capacity, nil
}

// disconnect should be called when a connection is terminated. If the disconnection
// was initiated by the pool itself using disconnectFn then calling disconnect is
// not necessary but permitted.
func (pool *clientPool) disconnect(p clientPoolPeer) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	pool.drop(p, false)
}

func (pool *clientPool) drop(p clientPoolPeer, kicked bool) {
	// Short circuit if client pool is already closed.
	if pool.closed {
		return
	}
	client, ok := pool.clientCache[p.ID()]
	if !ok {
		log.Debug("Client not connected", "id", p.ID(), "address", p.freeClientId())
		return
	}
	tryActivate := client.active
	if client.active {
		pool.deactivateClient(client, false)
	}
	pool.finalizeBalance(client, pool.clock.Now())
	pool.inactiveQueue.Remove(client.queueIndex)
	delete(pool.clientCache, client.id)
	if kicked {
		if pool.removePeer != nil {
			pool.removePeer(p.ID())
		}
		clientKickedMeter.Mark(1)
		log.Debug("Client kicked out", "id", client.id, "address", client.address)
	} else {
		clientDisconnectedMeter.Mark(1)
		log.Debug("Client disconnected", "id", client.id, "address", client.address)
	}
	if tryActivate {
		pool.tryActivateClients()
	}
}

// capAvailable checks whether the current priority level of the given client
// is enough to connect or change capacity to the requested level and then stay
// connected for at least the specified duration. If not then the additional
// required amount of positive balance is returned.
//
// This function assumes the lock is held.
func (pool *clientPool) capAvailable(id enode.ID, freeID string, capacity uint64, minConnTime time.Duration, tracker balanceTracker, kick bool) (uint64, uint64) {
	newCapacity, newCount := pool.activeCap+capacity, pool.activeQueue.Size()+1
	client := pool.clientCache[id]
	if client != nil && client.active {
		// It's a capacity raise operation, apply diff only.
		newCapacity, newCount = newCapacity-client.capacity, newCount-1
	}
	// Calculate the minimal connection time.
	connTime := activeBias
	if pool.disableBias {
		connTime = 0
	}
	if connTime < minConnTime {
		connTime = minConnTime
	}
	// Calculate the avaiable capacity
	var remain uint64
	if pool.capLimit < pool.activeCap {
		remain = 0
	} else {
		remain = pool.capLimit - pool.activeCap
	}
	// The clientpool has enough capacity space to apply the action.
	if newCapacity <= pool.capLimit && newCount <= pool.connLimit {
		// It's a free client and we have the space, no service token needed.
		if capacity == pool.freeClientCap {
			return 0, remain
		}
		// A higher capacity is required, the client has to stay as paid client.
		missing := tracker.posBalanceMissing(0, capacity, connTime)
		return missing, remain
	}
	// If the clientpool is full already, pick some clients to de-activiate
	// or reject new client(or capacity raise request)
	var (
		popList        []*clientInfo
		targetPriority int64
	)
	pool.activeQueue.MultiPop(func(data interface{}, priority int64) bool {
		c := data.(*clientInfo)
		popList = append(popList, c)
		if c != client {
			targetPriority = priority
			newCapacity -= c.capacity
			newCount--
		}
		return newCapacity > pool.capLimit || newCount > pool.connLimit
	})
	if newCapacity > pool.capLimit || newCount > pool.connLimit {
		for _, c := range popList {
			pool.activeQueue.Push(c)
		}
		return math.MaxUint64, remain // It's impossible to raise capacity or accept client.
	}
	// Calculate how many tokens needed to allocate capacity space.
	if capacity != pool.freeClientCap && targetPriority > 0 {
		targetPriority = 0
	}
	missing := tracker.posBalanceMissing(targetPriority, capacity, connTime)
	if missing != 0 {
		kick = false
	}
	for _, c := range popList {
		if kick && c != client {
			pool.deactivateClient(c, true)
		} else {
			pool.activeQueue.Push(c)
		}
	}
	return missing, remain
}

// forClients iterates through a list of clients, calling the callback for each one.
// If a client is not connected then clientInfo is nil. If the specified list is empty
// then the callback is called for all connected clients.
func (pool *clientPool) forClients(ids []enode.ID, callback func(*clientInfo, enode.ID) error) error {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	if len(ids) > 0 {
		for _, id := range ids {
			if err := callback(pool.clientCache[id], id); err != nil {
				return err
			}
		}
		return nil
	}
	for _, c := range pool.clientCache {
		if err := callback(c, c.id); err != nil {
			return err
		}
	}
	return nil
}

// setDefaultFactors sets the default price factors applied to subsequently connected clients
func (pool *clientPool) setDefaultFactors(posFactors, negFactors priceFactors) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	pool.posFactors = posFactors
	pool.negFactors = negFactors
}

// getDefaultFactors gets the default price factors.
func (pool *clientPool) getDefaultFactors() (priceFactors, priceFactors) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	return pool.posFactors, pool.negFactors
}

// activateClient sets the client in active state.
// This function assumes the lock is held.
func (pool *clientPool) activateClient(client *clientInfo, init bool) {
	if _, ok := pool.clientCache[client.id]; !ok || client.active {
		return
	}
	// Set the status as active
	client.active = true
	client.balanceTracker.activate()
	client.activateAt = pool.clock.Now()

	// Register new client to connection queue.
	pool.activeQueue.Push(client)
	pool.activeCap += client.capacity

	// If the current client is a paid client, monitor the status of client,
	// downgrade it to normal client if positive balance is used up.
	//
	// Special case: client in inactive queue for very long time and all
	// positive balance is expired. But anyway the callback will reset
	// the priority flag back.
	if client.priority {
		pb, _ := client.balanceTracker.getBalance(pool.clock.Now())
		pool.inactiveBalances.subExp(pb)
		pool.activeBalances.addExp(pb)
		pool.activePriority += client.capacity
		client.balanceTracker.addCallback(balanceCallbackZero, 0, func() { pool.balanceExhausted(client.id) })
	}
	// Notify remote peer to update capacity if it's reactivated
	if !init {
		client.peer.updateCapacity(client.capacity)
	}
	totalConnectedGauge.Update(int64(pool.activeCap))
	log.Debug("Client activated", "id", client.id, "address", client.address)
}

// deactivateClient sets the client in inactive state
// This function assumes the lock is held.
func (pool *clientPool) deactivateClient(client *clientInfo, scheduleDrop bool) {
	if _, ok := pool.clientCache[client.id]; !ok || !client.active {
		return
	}
	// Set the status as inactive
	client.active = false
	client.balanceTracker.deactivate()
	client.activateT += time.Duration(pool.clock.Now() - client.activateAt)
	client.deactivateN += 1

	// Remove the client from connection queue.
	pool.activeQueue.Remove(client.queueIndex)
	pool.activeCap -= client.capacity

	if client.priority {
		pb, _ := client.balanceTracker.getBalance(pool.clock.Now())
		pool.inactiveBalances.addExp(pb)
		pool.activeBalances.subExp(pb)
		pool.activePriority -= client.capacity
		client.balanceTracker.removeCallback(balanceCallbackZero)
	}
	client.peer.updateCapacity(0)
	totalConnectedGauge.Update(int64(pool.activeCap))

	// Push into inactive queue, schedule drop if specified.
	pool.inactiveQueue.Push(client, -connPriority(client, pool.clock.Now()))
	if scheduleDrop {
		pool.waitingDrop[pool.dropCycle+dropInactiveCycles] = append(pool.waitingDrop[pool.dropCycle+dropInactiveCycles], client)
	}
	log.Debug("Client deactivated", "id", client.id, "address", client.address)
}

// tryActivateClients checks whether some inactive clients have enough priority now
// and activates them if possible.
// This function assumes the lock is held.
func (pool *clientPool) tryActivateClients() {
	now := pool.clock.Now()
	for pool.inactiveQueue.Size() != 0 {
		client := pool.inactiveQueue.PopItem().(*clientInfo)
		if missing, _ := pool.capAvailable(client.id, client.address, client.capacity, 0, client.balanceTracker, true); missing != 0 {
			pool.inactiveQueue.Push(client, -connPriority(client, now))
			return
		}
		pool.activateClient(client, false)
		clientActivatedMeter.Mark(1)
	}
}

// capacityInfo returns the total capacity allowance, the total capacity of connected
// clients and the total capacity of connected and prioritized clients
func (pool *clientPool) capacityInfo() (uint64, uint64, uint64) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	return pool.capLimit, pool.activeCap, pool.activePriority
}

// finalizeBalance stops the balance tracker, retrieves the final balances and
// stores them in posBalanceQueue and negBalanceQueue
func (pool *clientPool) finalizeBalance(c *clientInfo, now mclock.AbsTime) {
	c.balanceTracker.stop(now)
	pos, neg := c.balanceTracker.getBalance(now)
	for index, value := range []expiredValue{pos, neg} {
		var (
			id         []byte
			expiration fixed64
		)
		neg := index == 1
		if !neg {
			id = c.id.Bytes()
			expiration = pool.posExpirer.expiration(pool.clock.Now())
		} else {
			id = []byte(c.address)
			expiration = pool.negExpirer.expiration(pool.clock.Now())
		}
		if value.value(expiration) > uint64(time.Second) {
			pool.ndb.setBalance(id, neg, tokenBalance{value: value})
		} else {
			pool.ndb.delBalance(id, neg) // balance is small enough, drop it directly.
		}
	}
}

// balanceExhausted callback is called by balanceTracker when positive balance
// is exhausted. It revokes priority status and also reduces the client capacity
// if necessary.
func (pool *clientPool) balanceExhausted(id enode.ID) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	c := pool.clientCache[id]
	if c == nil || !c.priority || !c.active {
		return
	}
	pool.activePriority -= c.capacity
	c.priority = false
	if c.capacity != pool.freeClientCap {
		pool.activeCap += pool.freeClientCap - c.capacity
		c.capacity = pool.freeClientCap
		c.balanceTracker.setCapacity(c.capacity)
		c.peer.updateCapacity(c.capacity)
		totalConnectedGauge.Update(int64(pool.activeCap))
	}
	pool.ndb.delBalance(id.Bytes(), false)
}

// setactiveLimit sets the maximum number and total capacity of connected clients,
// dropping some of them if necessary.
func (pool *clientPool) setLimits(totalConn int, totalCap uint64) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	pool.connLimit = totalConn
	pool.capLimit = totalCap
	if pool.activeCap > pool.capLimit || pool.activeQueue.Size() > pool.connLimit {
		pool.activeQueue.MultiPop(func(data interface{}, priority int64) bool {
			pool.deactivateClient(data.(*clientInfo), true)
			return pool.activeCap > pool.capLimit || pool.activeQueue.Size() > pool.connLimit
		})
	}
	pool.tryActivateClients()
}

// setCapacity sets the assigned capacity of a client.
// The status of client can be:
// - not connected
// - inactive
// - active
// * If the client is not connected or inactive, this function
// can only act as the query function to return all tokens
// needed to stay at least specified connection time with given
// capacity(may not changed).
// * If the client is active already and `setCap` is true, the
// action can be applied.
func (pool *clientPool) setCapacity(id enode.ID, freeID string, capacity uint64, minConnTime time.Duration, setCap bool) (uint64, uint64, error) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	// Short circuit if new capacity is invalid
	if capacity < pool.minCap {
		return 0, 0, errInvalidCap
	}
	var tracker balanceTracker
	client := pool.clientCache[id]
	if client == nil {
		// Client is not connected
		pb := pool.ndb.getOrNewBalance(id.Bytes(), false)
		nb := pool.ndb.getOrNewBalance([]byte(freeID), true)
		tracker = newBalanceTracker(pool.posExpirer, pool.negExpirer, pool.clock, capacity, pb.value, nb.value, pool.posFactors, pool.negFactors)
	} else {
		tracker = client.balanceTracker
	}
	canApply := setCap && client != nil && client.active
	// Check whether it's possible to change capacity and stay
	// at least specified connectin time.
	missing, remain := pool.capAvailable(id, freeID, capacity, minConnTime, tracker, canApply)
	if missing != 0 {
		return missing, remain, nil
	}
	// capacity update is possible, do it.
	if canApply {
		// Short circuit if capacity is not requested to change.
		if client.capacity == capacity {
			return 0, remain, nil
		}
		oldCap := client.capacity
		client.capacity = capacity
		client.balanceTracker.setCapacity(capacity)

		pool.activeCap += capacity - oldCap
		pool.activeQueue.Update(client.queueIndex)

		// If it's allowed to change the capacity,
		// it means the client is a paid client.
		pool.activePriority += capacity - oldCap
		client.peer.updateCapacity(client.capacity)

		totalConnectedGauge.Update(int64(pool.activeCap))
		pool.tryActivateClients()
	}
	return 0, remain, nil
}

// requestCost feeds request cost after serving a request from the given peer and
// returns the remaining token balance
func (pool *clientPool) requestCost(p *clientPeer, cost uint64) (uint64, uint64) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	c := pool.clientCache[p.ID()]
	if c == nil || pool.closed {
		return 0, 0
	}
	return c.balanceTracker.requestCost(cost)
}

// getBalance retrieves a single token balance entry from cache or the database
func (pool *clientPool) getBalance(clientID enode.ID, bid []byte, isneg bool) tokenBalance {
	if c := pool.clientCache[clientID]; c != nil {
		pos, neg := c.balanceTracker.getBalance(pool.clock.Now())
		if isneg {
			return tokenBalance{value: neg}
		} else {
			return tokenBalance{value: pos}
		}
	}
	return pool.ndb.getOrNewBalance(bid, isneg)
}

// getBalance retrieves a single token balance entry from cache or the database
func (pool *clientPool) getBalanceLocked(clientID enode.ID, bid []byte, neg bool) uint64 {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	b := pool.getBalance(clientID, bid, neg)
	if neg {
		return b.value.value(pool.negExpirer.expiration(pool.clock.Now()))
	}
	return b.value.value(pool.posExpirer.expiration(pool.clock.Now()))
}

// setBalance writes the token balance entry to cache and the database
func (pool *clientPool) setBalance(clientID enode.ID, bid []byte, isneg bool, b tokenBalance) {
	if c := pool.clientCache[clientID]; c != nil {
		pos, neg := c.balanceTracker.getBalance(pool.clock.Now())
		if isneg {
			c.balanceTracker.setBalance(pos, b.value)
		} else {
			c.balanceTracker.setBalance(b.value, neg)
		}
	}
	pool.ndb.setBalance(bid, isneg, b)
}

// addBalance updates the balance of a client by given diff(amount).
// Return old and modified one positive balance if succeeds.
func (pool *clientPool) addBalance(id enode.ID, amount int64) (uint64, uint64, error) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	// Ensure the given balance difference is valid to apply.
	now := pool.clock.Now()
	pos := pool.getBalance(id, id.Bytes(), false)
	exp := pool.posExpirer.expiration(now)
	old := pos.value
	oldValue := old.value(exp)
	if amount > 0 && (amount > maxBalance || oldValue > maxBalance-uint64(amount)) {
		return oldValue, oldValue, errBalanceOverflow
	}
	pos.value.add(amount, exp)
	pool.setBalance(id, id.Bytes(), false, pos)

	client := pool.clientCache[id]
	if client == nil || !client.active {
		// It's a non-connected or inactive client.
		pool.inactiveBalances.subExp(old)
		pool.inactiveBalances.addExp(pos.value)
		if client != nil {
			pool.inactiveQueue.Remove(client.queueIndex)
			pool.inactiveQueue.Push(client, -connPriority(client, now))
			client.priority = pos.value.base != 0
		}
	} else {
		// It's an active client.
		pool.activeBalances.subExp(old)
		pool.activeBalances.addExp(pos.value)
		pool.activeQueue.Update(client.queueIndex)
		if !client.priority && pos.value.base > 0 {
			pool.activePriority += client.capacity
			client.priority = true
			client.balanceTracker.addCallback(balanceCallbackZero, 0, func() { pool.balanceExhausted(id) })
		}
		// callback will revoke the priority status if new balance is zero.
	}
	pool.tryActivateClients()
	return oldValue, pos.value.value(exp), nil
}

// balanceSum returns the sum of positive balance. Only used in testing.
func (pool *clientPool) balanceSum() (expiredValue, expiredValue) {
	pool.lock.Lock()
	defer pool.lock.Unlock()

	return pool.activeBalances, pool.inactiveBalances
}
