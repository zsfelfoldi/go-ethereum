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
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/ethdb"
	lps "github.com/ethereum/go-ethereum/les/lespay/server"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

const (
	defaultPosExpTC = 36000 // default time constant (in seconds) for exponentially reducing positive balance
	defaultNegExpTC = 3600  // default time constant (in seconds) for exponentially reducing negative balance

	// activeBias is applied to already connected clients So that
	// already connected client won't be kicked out very soon and we
	// can ensure all connected clients can have enough time to request
	// or sync some data.
	//
	// todo(rjl493456442) make it configurable. It can be the option of
	// free trial time!
	activeBias = time.Minute * 3
)

var nodeField = utils.NewNodeStateField("clientInfo", reflect.TypeOf(clientInfo{}), nil)

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
	lock           sync.Mutex
	clock          mclock.Clock
	closed         bool
	removePeer     func(enode.ID)
	ns             *utils.NodeStateMachine
	pp             *lps.PriorityPool
	bt             *lps.BalanceTracker
	tl             *lps.TokenLimiter
	posExp, negExp *lps.TokenExpirer

	defaultPosFactors, defaultNegFactors priceFactors
	posExpTC, negExpTC                   uint64
	minCap                               uint64 // The minimal capacity value allowed for any client
	capLimit                             uint64
	freeClientCap                        uint64 // The capacity value of each free client
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
	id          enode.ID
	address     string
	peer        clientPoolPeer
	connectedAt mclock.AbsTime
	balance     *lps.NodeBalance
}

func (c *clientInfo) Priority(now mclock.AbsTime, cap uint64) int64 {
	return c.balance.Priority(now, cap)
}

func (c *clientInfo) EstMinPriority(until mclock.AbsTime, cap uint64) int64 {
	return c.balance.EstMinPriority(until, cap, true)
}

func (c *clientInfo) FreeID() string {
	return c.address
}

// newClientPool creates a new client pool
func newClientPool(lespayDb ethdb.Database, minCap, freeClientCap uint64, clock mclock.Clock, removePeer func(enode.ID)) *clientPool {
	ns := utils.NewNodeStateMachine(nil, nil, 0, clock)
	stActive := ns.StateMask(sfActive)
	stInactive := ns.StateMask(sfInactive)

	pool := &clientPool{
		ns:             ns,
		nodeFieldIndex: ns.MustRegisterField(nodeField),
		clock:          clock,
		minCap:         minCap,
		freeClientCap:  freeClientCap,
		removePeer:     removePeer,
		stopCh:         make(chan struct{}),
	}
	pool.tl = lps.NewTokenLimiter(ns, clock, freeClientCap)
	pool.posExp = pool.tl.NewTokenExpirer()
	pool.negExp = pool.tl.NewTokenExpirer()
	pool.pp = lps.NewPriorityPool(ns, minCap, activeBias, nodeField)
	pool.bt = lps.NewBalanceTracker(ns, lespayDb, clock, pool.posExp, pool.negExp, nodeField)

	// set default expiration constants used by tests
	// Note: server overwrites this if token sale is active
	pool.bt.SetExpirationTCs(0, defaultNegExpTC)
	// calculate total token balance amount

	ns.AddStateSub(stInactive|stPriority, func(id enode.ID, oldState, newState utils.NodeStateBitMask) {
		if newState == stInactive {
			ns.AddTimeout(id, stInactive, inactiveTimeout)
		}
		if oldState == stInactive && newState == stInactive|stPriority {
			ns.RemoveTimeout(id, stInactive)
		}
	})

	ns.AddStateSub(stInactive|stActive, func(id enode.ID, oldState, newState utils.NodeStateBitMask) {
		if newState == 0 {
			f.ns.SetField(id, f.nodeFieldIndex, nil)
			pool.removePeer(id)
		}
	})

	ns.AddFieldSub(ns.FieldIndex(server.CapacityField))
	return pool
}

// stop shuts the client pool down
func (f *clientPool) stop() {
	f.lock.Lock()
	f.closed = true
	f.lock.Unlock()
	f.ns.ForEach(f.stConnected, 0, func(id enode.ID) {
		// removing connected flag also enforces saving all balances in BalanceTracker
		f.ns.UpdateState(id, 0, f.stConnected|f.stActive|f.stInactive, 0)
	})
	f.pp.Stop()
	f.bt.Stop()
	f.ns.Stop()
}

func (f *clientPool) clientInfo(id enode.ID) *clientInfo {
	c := f.pp.GetField(id, f.nodeFieldIndex)
	if c == nil {
		return nil
	}
	return c.(*clientInfo)
}

// connect should be called after a successful handshake. If the connection was
// rejected, there is no need to call disconnect.
func (f *clientPool) connect(peer clientPoolPeer, reqCapacity uint64) (uint64, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// Short circuit if clientPool is already closed.
	if f.closed {
		return 0, fmt.Errorf("Client pool is already closed")
	}
	// Dedup connected peers.
	id, freeID := peer.ID(), peer.freeClientId()
	if f.clientInfo(id) != nil {
		clientRejectedMeter.Mark(1)
		log.Debug("Client already connected", "address", freeID, "id", peerIdToString(id))
		return 0, fmt.Errorf("Client already connected address=%s id=%s", freeID, peerIdToString(id))
	}
	c := &clientInfo{
		id:          id,
		address:     freeID,
		peer:        peer,
		connectedAt: f.clock.Now(),
	}
	f.ns.SetField(id, f.nodeFieldIndex, c)
	c.balance = f.bt.GetNode(id)
	if reqCapacity < f.minCap {
		reqCapacity = f.minCap
	}
	f.ns.UpdateState(id, f.stInactive, 0, 0)

	if _, allowed := f.pp.RequestCapacity(id, reqCapacity, f.minBias, true); allowed {
		return reqCapacity, nil
	}
	return 0, nil
}

// disconnect should be called when a connection is terminated. If the disconnection
// was initiated by the pool itself using disconnectFn then calling disconnect is
// not necessary but permitted.
func (f *clientPool) disconnect(p clientPoolPeer) {
	f.ns.UpdateState(id, 0, f.stActive|f.stInactive, 0)
	f.ns.SetField(id, f.nodeFieldIndex, nil)
}

// setDefaultFactors sets the default price factors applied to subsequently connected clients
func (f *clientPool) setDefaultFactors(posFactors, negFactors priceFactors) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.defaultPosFactors = posFactors
	f.defaultNegFactors = negFactors
	f.tl.SetCapacityFactor(posFactors.capacityFactor)
}

// capacityInfo returns the total capacity allowance, the total capacity of connected
// clients and the total capacity of connected and prioritized clients
func (f *clientPool) capacityInfo() (uint64, uint64, uint64) {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.capLimit, f.pp.ActiveCapacity(), f.tl.PriorityCapacity()
}

// setLimits sets the maximum number and total capacity of connected clients,
// dropping some of them if necessary.
func (f *clientPool) setLimits(totalConn int, totalCap uint64) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.capLimit = totalCap
	f.pp.SetLimits(uint64(totalConn), totalCap)
	f.tl.SetCapacityLimit(totalCap)
}

// setCapacity sets the assigned capacity of a connected client
func (f *clientPool) setCapacity(id enode.ID, freeID string, capacity uint64, bias time.Duration, setCap bool) (uint64, uint64, error) {
	c := f.clientInfo(id)
	connected := c != nil
	if !connected {
		if setCap {
			return 0, capacity, fmt.Errorf("client %064x is not connected", c.id[:])
		}
		c = &clientInfo{
			id:      id,
			address: freeID,
		}
		f.ns.SetField(id, f.nodeFieldIndex, c)
		c.balance = f.bt.GetNode(id)
		defer f.ns.SetField(id, f.nodeFieldIndex, nil)
	}
	minPriority, allowed := f.pp.RequestCapacity(id, capacity, bias, setCap)
	if allowed {
		return 0, capacity, nil
	}
	missing := c.balance.PosBalanceMissing(minPriority, capacity, bias)
	if missing < 1 {
		// ensure that we never return 0 missing and insufficient priority error
		missing = 1
	}
	return missing, capacity, errNoPriority
}

// setCapacityLocked is the equivalent of setCapacity used when f.lock is already locked
func (f *clientPool) setCapacityLocked(id enode.ID, freeID string, capacity uint64, minConnTime time.Duration, setCap bool) (uint64, uint64, error) {
	f.lock.Lock()
	defer f.lock.Unlock()

	return f.setCapacity(id, freeID, capacity, minConnTime, setCap)
}
