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

package server

import (
	"errors"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

var (
	PriorityFlag = utils.NewNodeStateFlag("priority", false, false)
	balanceField = utils.NewNodeStateField("balance", reflect.TypeOf(NodeBalance{}), nil)

	errBalanceOverflow = errors.New("balance overflow")
)

const (
	maxBalance               = math.MaxInt64
	persistExpirationRefresh = time.Minute * 5 // refresh period of the token expiration persistence
)

const (
	balanceCallbackQueue = iota
	balanceCallbackZero
	balanceCallbackCount
)

type BalanceTracker struct {
	lock                              sync.Mutex
	clock                             mclock.Clock
	ns                                *utils.NodeStateMachine
	ndb                               *nodeDB
	posExp, negExp                    *TokenExpirer
	posExpTC, negExpTC                uint64
	active                            map[enode.ID]*BalanceTracker
	totalAmount                       utils.ExpiredValue
	balanceFieldIndex, nodeFieldIndex int
	stActive, stPriority              utils.NodeStateBitMask
	lastActiveBalanceUpdate           mclock.AbsTime
	quit                              chan struct{}
}

func NewBalanceTracker(ns *utils.NodeStateMachine, db ethdb.Database, clock mclock.Clock, posExp, negExp *TokenExpirer, nodeField *utils.NodeStateField) *BalanceTracker {
	ndb := newNodeDB(db, clock)
	bt := &BalanceTracker{
		ns:                ns,
		ndb:               ndb,
		clock:             clock,
		posExp:            posExp,
		negExp:            negExp,
		stActive:          ns.StateMask(ActiveFlag),
		stPriority:        ns.MustRegisterState(PriorityFlag),
		balanceFieldIndex: ns.MustRegisterField(balanceField),
		nodeFieldIndex:    ns.FieldIndex(nodeField),
		quit:              make(chan struct{}),
	}
	capFieldIndex := ns.FieldIndex(CapacityField)
	var start enode.ID
	for {
		ids := bt.ndb.getPosBalanceIDs(start, enode.ID{}, 1000)
		var stop bool
		l := len(ids)
		if l == 1000 {
			l--
			start = ids[l]
		} else {
			stop = true
		}
		for i := 0; i < l; i++ {
			bt.totalAmount.AddExp(bt.ndb.getOrNewBalance(ids[i].Bytes(), false).value)
		}
		if stop {
			break
		}
	}

	ns.AddFieldSub(capFieldIndex, func(id enode.ID, state utils.NodeStateBitMask, oldValue, newValue interface{}) {
		n := bt.getNode(id)
		if n == nil {
			log.Error("Capacity field changed while node field is missing")
			return
		}
		oldCap, _ := oldValue.(uint64)
		newCap, _ := newValue.(uint64)
		bt.lock.Lock()
		if newCap != 0 {
			if oldCap == 0 {
				bt.activate(n)
			}
			n.setCapacity(newCap)
		} else {
			bt.deactivate(n)
		}
		bt.lock.Unlock()
	})

	ns.AddFieldSub(bt.nodeFieldIndex, func(id enode.ID, state utils.NodeStateBitMask, oldValue, newValue interface{}) {
		bt.lock.Lock()
		if newValue != nil {
			bt.ns.SetField(id, bt.balanceFieldIndex, bt.newNodeBalance(id, newValue.(btNode).FreeID()))
		} else {
			if bt.deactivate(bt.getNode(id)).priority {
				ns.UpdateState(id, 0, bt.stPriority, 0)
			}
		}
		bt.lock.Unlock()
	})

	// The positive and negative balances of clients are stored in database
	// and both of these decay exponentially over time. Delete them if the
	// value is small enough.
	bt.ndb.evictCallBack = func(now mclock.AbsTime, neg bool, b tokenBalance) bool {
		var expiration utils.Fixed64
		if neg {
			expiration = bt.negExp.LogOffset(now)
		} else {
			expiration = bt.posExp.LogOffset(now)
		}
		return b.value.Value(expiration) <= uint64(time.Second)
	}

	go func() {
		for {
			select {
			case <-clock.After(persistExpirationRefresh):
				now := clock.Now()
				bt.ndb.setExpiration(posExp.LogOffset(now), negExp.LogOffset(now))
			case <-bt.quit:
				return
			}
		}
	}()
	return bt
}

func (bt *BalanceTracker) Stop() {
	close(bt.quit)
	bt.ndb.close()
}

type btNode interface {
	FreeID() string
	Updated()
}

func (bt *BalanceTracker) newNodeBalance(id enode.ID, freeID string) *NodeBalance {
	pb := bt.ndb.getOrNewBalance(id.Bytes(), false)
	nb := bt.ndb.getOrNewBalance([]byte(freeID), true)
	n := &NodeBalance{
		id:         id,
		freeID:     freeID,
		balance:    balance{pos: pb.value, neg: nb.value},
		priority:   !pb.value.IsZero(),
		initTime:   bt.clock.Now(),
		lastUpdate: bt.clock.Now(),
	}
	n.storedBalance = n.balance
	n.summedBalance = n.balance.pos
	for i := range n.callbackIndex {
		n.callbackIndex[i] = -1
	}
	if n.priority {
		n.addCallback(balanceCallbackZero, 0, func() { bt.balanceExhausted(id, n) })
		bt.ns.UpdateState(id, bt.stPriority, 0, 0)
	}
	return n
}

func (bt *BalanceTracker) getNode(id enode.ID) *NodeBalance {
	if n := bt.ns.GetField(id, bt.balanceFieldIndex); n != nil {
		return n.(*NodeBalance)
	}
	return nil
}

func (bt *BalanceTracker) updateTotalAmount(n *NodeBalance) {
	bt.totalAmount.AddExp(n.balance.pos)
	bt.totalAmount.SubExp(n.summedBalance)
	n.summedBalance = n.balance.pos
}

func (bt *BalanceTracker) activate(n *NodeBalance) {
	n.lock.Lock()
	defer n.lock.Unlock()
	if n.active {
		return
	}
	//TODO do we need this?
	n.active = true
}

func (bt *BalanceTracker) deactivate(n *NodeBalance) *NodeBalance {
	n.lock.Lock()
	defer n.lock.Unlock()
	if !n.active {
		return n
	}
	n.stop()
	bt.updateTotalAmount(n)
	bt.storeBalance(n.id.Bytes(), false, n.balance.pos, &n.storedBalance.pos, bt.posExp)
	bt.storeBalance([]byte(n.freeID), true, n.balance.neg, &n.storedBalance.neg, bt.negExp)
	n.active = false
	return n
}

func (bt *BalanceTracker) balanceOperation(id enode.ID, cb func(pb *utils.ExpiredValue) bool) {
	if i := bt.ns.GetField(id, bt.balanceFieldIndex); i != nil {
		n := i.(*NodeBalance)
		n.lock.Lock()
		pb := n.balance.pos
		if cb(&pb) {
			n.setBalance(pb, n.balance.neg)
			bt.storeBalance(id.Bytes(), false, n.balance.pos, &n.storedBalance.pos, bt.posExp)
			if !pb.IsZero() && !n.priority {
				n.priority = true
				n.addCallback(balanceCallbackZero, 0, func() { bt.balanceExhausted(id, n) })
				bt.ns.UpdateState(id, bt.stPriority, 0, 0)
			}
			// if balance is set to zero then reverting to non-priority status
			// is handled by the balanceExhausted callback
			bt.updateTotalAmount(n)
		}
		n.lock.Unlock()
	} else {
		pb := bt.ndb.getOrNewBalance(id.Bytes(), false).value
		stored := pb
		if cb(&pb) {
			bt.totalAmount.AddExp(pb)
			bt.totalAmount.SubExp(stored)
			bt.storeBalance(id.Bytes(), false, pb, &stored, bt.posExp)
		}
	}
}

func (bt *BalanceTracker) GetPosBalance(id enode.ID) uint64 {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	var res uint64
	bt.balanceOperation(id, func(pb *utils.ExpiredValue) bool {
		res = pb.Value(bt.posExp.LogOffset(bt.clock.Now()))
		return false
	})
	return res
}

func (bt *BalanceTracker) AddPosBalance(id enode.ID, amount int64) (oldValue, newValue uint64, err error) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	var res uint64
	bt.balanceOperation(id, func(pb *utils.ExpiredValue) bool {
		logOffset := bt.posExp.LogOffset(bt.clock.Now())
		oldValue = pb.Value(logOffset)
		if amount > 0 && (amount > maxBalance || oldValue > maxBalance-uint64(amount)) {
			newValue = oldValue
			err = errBalanceOverflow
			return false
		}
		pb.Add(amount, logOffset)
		newValue = pb.Value(logOffset)
		return true
	})
	if b := bt.ns.GetField(id, bt.nodeFieldIndex); b != nil {
		b.(btNode).Updated()
	}
	return
}

func (bt *BalanceTracker) balanceExhausted(id enode.ID, n *NodeBalance) {
	n.lock.Lock()
	defer n.lock.Unlock()

	bt.storeBalance(id.Bytes(), false, n.balance.pos, &n.storedBalance.pos, bt.posExp)
	n.priority = false
	bt.ns.UpdateState(id, 0, bt.stPriority, 0)
}

func (bt *BalanceTracker) storeBalance(id []byte, neg bool, value utils.ExpiredValue, storedValue *utils.ExpiredValue, exp *TokenExpirer) {
	if value.Value(exp.LogOffset(bt.clock.Now())) > uint64(time.Second) {
		if value != (*storedValue) {
			bt.ndb.setBalance(id, neg, tokenBalance{value: value})
			(*storedValue) = value
		}
	} else {
		if (*storedValue) != (utils.ExpiredValue{}) {
			bt.ndb.delBalance(id, neg) // balance is small enough, drop it directly.
			(*storedValue) = utils.ExpiredValue{}
		}
	}
}

// TotalTokenAmount returns the total amount of currently existing service tokens
func (bt *BalanceTracker) TotalTokenAmount() uint64 {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	now := bt.clock.Now()
	if now > bt.lastActiveBalanceUpdate+mclock.AbsTime(time.Second) {
		bt.ns.ForEach(bt.stActive, func(id enode.ID, state utils.NodeStateBitMask) {
			if nn := bt.ns.GetField(id, bt.balanceFieldIndex); nn != nil {
				n := nn.(*NodeBalance)
				n.lock.Lock()
				bt.updateTotalAmount(n)
				n.lock.Unlock()
			}
		})
		bt.lastActiveBalanceUpdate = now
	}
	return bt.totalAmount.Value(bt.posExp.LogOffset(now))
}

// SetExpirationTCs sets positive and negative token expiration time constants.
// Specified in seconds, 0 means infinite (no expiration).
func (bt *BalanceTracker) SetExpirationTCs(pos, neg uint64) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.posExpTC, bt.negExpTC = pos, neg
	now := bt.clock.Now()
	if pos > 0 {
		bt.posExp.SetRate(now, 1/float64(pos*uint64(time.Second)))
	} else {
		bt.posExp.SetRate(now, 0)
	}
	if neg > 0 {
		bt.negExp.SetRate(now, 1/float64(neg*uint64(time.Second)))
	} else {
		bt.negExp.SetRate(now, 0)
	}
}

// GetExpirationTCs returns the current positive and negative token expiration
// time constants
func (bt *BalanceTracker) GetExpirationTCs() (pos, neg uint64) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	return bt.posExpTC, bt.negExpTC
}

// priceFactors determine the pricing policy (may apply either to positive or
// negative balances which may have different factors).
// - timeFactor is cost unit per nanosecond of connection time
// - capacityFactor is cost unit per nanosecond of connection time per 1000000 capacity
// - requestFactor is cost unit per request "realCost" unit
type priceFactors struct {
	timeFactor, capacityFactor, requestFactor float64
}

func (p priceFactors) timePrice(cap uint64) float64 {
	return p.timeFactor + float64(cap)*p.capacityFactor/1000000
}

func (p priceFactors) reqPrice() float64 {
	return p.requestFactor
}

// NodeBalance keeps track of the positive and negative balances of a connected
// client and calculates actual and projected future priority values required by
// prque.LazyQueue.
type NodeBalance struct {
	lock                             sync.RWMutex
	id                               enode.ID
	freeID                           string
	active, priority                 bool
	capacity                         uint64
	balance, storedBalance           balance
	summedBalance                    utils.ExpiredValue
	posFactor, negFactor             priceFactors
	sumReqCost                       uint64
	lastUpdate, nextUpdate, initTime mclock.AbsTime
	updateEvent                      mclock.Timer
	// since only a limited and fixed number of callbacks are needed, they are
	// stored in a fixed size array ordered by priority threshold.
	callbacks [balanceCallbackCount]balanceCallback
	// callbackIndex maps balanceCallback constants to callbacks array indexes (-1 if not active)
	callbackIndex [balanceCallbackCount]int
	callbackCount int // number of active callbacks
}

// balance represents a pair of positive and negative balances
type balance struct {
	pos, neg utils.ExpiredValue
}

// balanceCallback represents a single callback that is activated when client priority
// reaches the given threshold
type balanceCallback struct {
	id        int
	threshold int64
	callback  func()
}

// stop shuts down the balance tracker
func (n *NodeBalance) stop() {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.addBalance(now)
	n.posFactor = priceFactors{0, 0, 0}
	n.negFactor = priceFactors{0, 0, 0}
	if n.updateEvent != nil {
		n.updateEvent.Stop()
		n.updateEvent = nil
	}
}

// balanceToPriority converts a balance to a priority value. Lower priority means
// first to disconnect. Positive balance translates to positive priority. If positive
// balance is zero then negative balance translates to a negative priority.
func (n *NodeBalance) balanceToPriority(b balance, capacity uint64) int64 {
	if b.pos.base > 0 {
		return int64(b.pos.value(n.exp.posExpiration(n.clock.Now())) / capacity)
	}
	return -int64(b.neg.value(n.exp.negExpiration(n.clock.Now())))
}

// PosBalanceMissing calculates the missing amount of positive balance in order to
// connect at targetCapacity, stay connected for the given amount of time and then
// still have a priority of targetPriority
func (n *NodeBalance) PosBalanceMissing(targetPriority int64, targetCapacity uint64, after time.Duration) uint64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	now := n.clock.Now()
	if targetPriority < 0 {
		timePrice := n.negFactor.timePrice(targetCapacity)
		timeCost := uint64(float64(after) * timePrice)
		negBalance := n.balance.neg.value(n.exp.negExpiration(now))
		if timeCost+negBalance < uint64(-targetPriority) {
			return 0
		}
		if uint64(-targetPriority) > negBalance && timePrice > 1e-100 {
			if negTime := time.Duration(float64(uint64(-targetPriority)-negBalance) / timePrice); negTime < after {
				after -= negTime
			} else {
				after = 0
			}
		}
		targetPriority = 0
	}
	timePrice := n.posFactor.timePrice(targetCapacity)
	posRequired := uint64(float64(targetPriority)*float64(targetCapacity)+float64(after)*timePrice) + 1
	if posRequired >= maxBalance {
		return math.MaxUint64 // target not reachable
	}
	posBalance := n.balance.pos.value(n.exp.posExpiration(now))
	if posRequired > posBalance {
		return posRequired - posBalance
	}
	return 0
}

// reducedBalance estimates the reduced balance at a given time in the fututre based
// on the current balance, the time factor and an estimated average request cost per time ratio
func (n *NodeBalance) reducedBalance(at mclock.AbsTime, avgReqCost float64) balance {
	dt := float64(at - n.lastUpdate)
	b := n.balance
	if b.pos.base != 0 {
		factor := n.posFactor.timePrice(n.capacity) + n.posFactor.reqPrice()*avgReqCost
		diff := -int64(dt * factor)
		dd := b.pos.add(diff, n.exp.posExpiration(at))
		if dd == diff {
			dt = 0
		} else {
			dt += float64(dd) / factor
		}
	}
	if dt > 0 {
		factor := n.negFactor.timePrice(n.capacity) + n.negFactor.reqPrice()*avgReqCost
		b.neg.add(int64(dt*factor), n.exp.negExpiration(at))
	}
	return b
}

// timeUntil calculates the remaining time needed to reach a given priority level
// assuming that no requests are processed until then. If the given level is never
// reached then (0, false) is returned.
// Note: the function assumes that the balance has been recently updated and
// calculates the time starting from the last update.
func (n *NodeBalance) timeUntil(priority int64) (time.Duration, bool) {
	now := n.clock.Now()
	var dt float64
	if n.balance.pos.base != 0 {
		posBalance := n.balance.pos.value(n.exp.posExpiration(now))
		timePrice := n.posFactor.timePrice(n.capacity)
		if timePrice < 1e-100 {
			return 0, false
		}
		if priority > 0 {
			newBalance := uint64(priority) * n.capacity
			if newBalance > posBalance {
				return 0, false
			}
			dt = float64(posBalance-newBalance) / timePrice
			return time.Duration(dt), true
		} else {
			dt = float64(posBalance) / timePrice
		}
	} else {
		if priority > 0 {
			return 0, false
		}
	}
	// if we have a positive balance then dt equals the time needed to get it to zero
	negBalance := n.balance.neg.value(n.exp.negExpiration(now))
	timePrice := n.negFactor.timePrice(n.capacity)
	if uint64(-priority) > negBalance {
		if timePrice < 1e-100 {
			return 0, false
		}
		dt += float64(uint64(-priority)-negBalance) / timePrice
	}
	return time.Duration(dt), true
}

// setCapacity updates the capacity value used for priority calculation
// Note: capacity should never be zero
func (n *NodeBalance) setCapacity(capacity uint64) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.stopped {
		return
	}
	now := n.clock.Now()
	n.addBalance(now)
	n.capacity = capacity
	n.checkCallbacks(now)
}

// Priority returns the actual priority based on the current balance
func (n *NodeBalance) Priority(now mclock.AbsTime, capacity uint64) int64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.addBalance(now)
	return n.balanceToPriority(n.balance, capacity)
}

// EstMinPriority gives a lower estimate for the priority at a given time in the future.
// If addReqCost is true then an average request cost per time is assumed that is twice the
// average cost per time in the current session. If false, zero request cost is assumed.
func (n *NodeBalance) EstMinPriority(at mclock.AbsTime, capacity uint64, addReqCost bool) int64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	var avgReqCost float64
	if addReqCost {
		dt := time.Duration(n.lastUpdate - n.initTime)
		if dt > time.Second {
			avgReqCost = float64(n.sumReqCost) * 2 / float64(dt)
		}
	}
	return n.balanceToPriority(n.reducedBalance(at, avgReqCost), capacity)
}

// addBalance updates balance based on the time factor
func (n *NodeBalance) addBalance(now mclock.AbsTime) {
	if now > n.lastUpdate {
		n.balance = n.reducedBalance(now, 0)
		n.lastUpdate = now
	}
}

// checkCallbacks checks whether the threshold of any of the active callbacks
// have been reached and calls them if necessary. It also sets up or updates
// a scheduled event to ensure that is will be called again just after the next
// threshold has been reached.
// Note: checkCallbacks assumes that the balance has been recently updated.
func (n *NodeBalance) checkCallbacks(now mclock.AbsTime) {
	if n.callbackCount == 0 {
		return
	}
	pri := n.balanceToPriority(n.balance)
	for n.callbackCount != 0 && n.callbacks[n.callbackCount-1].threshold >= pri {
		n.callbackCount--
		n.callbackIndex[n.callbacks[n.callbackCount].id] = -1
		go n.callbacks[n.callbackCount].callback()
	}
	if n.callbackCount != 0 {
		d, ok := n.timeUntil(n.callbacks[n.callbackCount-1].threshold)
		if !ok {
			n.nextUpdate = 0
			n.updateAfter(0)
			return
		}
		if n.nextUpdate == 0 || n.nextUpdate > now+mclock.AbsTime(d) {
			if d > time.Second {
				// Note: if the scheduled update is not in the very near future then we
				// schedule the update a bit earlier. This way we do need to update a few
				// extra times but don't need to reschedule every time a processed request
				// brings the expected firing time a little bit closer.
				d = ((d - time.Second) * 7 / 8) + time.Second
			}
			n.nextUpdate = now + mclock.AbsTime(d)
			n.updateAfter(d)
		}
	} else {
		n.nextUpdate = 0
		n.updateAfter(0)
	}
}

// updateAfter schedules a balance update and callback check in the future
func (n *NodeBalance) updateAfter(dt time.Duration) {
	if n.updateEvent == nil || n.updateEvent.Stop() {
		if dt == 0 {
			n.updateEvent = nil
		} else {
			n.updateEvent = n.clock.AfterFunc(dt, func() {
				n.lock.Lock()
				defer n.lock.Unlock()

				if n.callbackCount != 0 {
					now := n.clock.Now()
					n.addBalance(now)
					n.checkCallbacks(now)
				}
			})
		}
	}
}

// requestCost should be called after serving a request for the given peer
func (n *NodeBalance) requestCost(cost uint64) uint64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.stopped {
		return 0
	}
	now := n.clock.Now()
	n.addBalance(now)
	fcost := float64(cost)

	posExp := n.exp.posExpiration(now)
	if n.balance.pos.base != 0 {
		if n.posFactor.reqPrice() != 0 {
			c := -int64(fcost * n.posFactor.reqPrice())
			cc := n.balance.pos.add(c, posExp)
			if c == cc {
				fcost = 0
			} else {
				fcost *= 1 - float64(cc)/float64(c)
			}
			n.checkCallbacks(now)
		} else {
			fcost = 0
		}
	}
	if fcost > 0 {
		if n.negFactor.reqPrice() != 0 {
			n.balance.neg.add(int64(fcost*n.negFactor.reqPrice()), n.exp.negExpiration(now))
			n.checkCallbacks(now)
		}
	}
	n.sumReqCost += cost
	return n.balance.pos.value(posExp)
}

// getBalance returns the current positive and negative balance
func (n *NodeBalance) getBalance(now mclock.AbsTime) (utils.ExpiredValue, utils.ExpiredValue) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.addBalance(now)
	return n.balance.pos, n.balance.neg
}

// setBalance sets the positive and negative balance to the given values
func (n *NodeBalance) setBalance(pos, neg utils.ExpiredValue) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	now := n.clock.Now()
	n.addBalance(now)
	n.balance.pos = pos
	n.balance.neg = neg
	n.checkCallbacks(now)
	return nil
}

// SetFactors sets the price factors. timeFactor is the price of a nanosecond of
// connection while requestFactor is the price of a "realCost" unit.
func (n *NodeBalance) SetFactors(posFactor, negFactor priceFactors) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.stopped {
		return
	}
	now := n.clock.Now()
	n.addBalance(now)
	n.posFactor, n.negFactor = posFactor, negFactor
	n.checkCallbacks(now)
}

// setCallback sets up a one-time callback to be called when priority reaches
// the threshold. If it has already reached the threshold the callback is called
// immediately.
func (n *NodeBalance) addCallback(id int, threshold int64, callback func()) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.removeCb(id)
	idx := 0
	for idx < n.callbackCount && threshold > n.callbacks[idx].threshold {
		idx++
	}
	for i := n.callbackCount - 1; i >= idx; i-- {
		n.callbackIndex[n.callbacks[i].id]++
		n.callbacks[i+1] = n.callbacks[i]
	}
	n.callbackCount++
	n.callbackIndex[id] = idx
	n.callbacks[idx] = balanceCallback{id, threshold, callback}
	now := n.clock.Now()
	n.addBalance(now)
	n.checkCallbacks(now)
}

// removeCallback removes the given callback and returns true if it was active
func (n *NodeBalance) removeCallback(id int) bool {
	n.lock.Lock()
	defer n.lock.Unlock()

	return n.removeCb(id)
}

// removeCb removes the given callback and returns true if it was active
// Note: should be called while n.lock is held
func (n *NodeBalance) removeCb(id int) bool {
	idx := n.callbackIndex[id]
	if idx == -1 {
		return false
	}
	n.callbackIndex[id] = -1
	for i := idx; i < n.callbackCount-1; i++ {
		n.callbackIndex[n.callbacks[i+1].id]--
		n.callbacks[i] = n.callbacks[i+1]
	}
	n.callbackCount--
	return true
}
