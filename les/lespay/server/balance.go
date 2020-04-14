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
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

var errBalanceOverflow = errors.New("balance overflow")

const (
	maxBalance               = math.MaxInt64   // maximum allowed balance value
	posThreshold             = 1000000         // minimum positive balance that is persisted in the database
	negThreshold             = 1000000         // minimum negative balance that is persisted in the database
	persistExpirationRefresh = time.Minute * 5 // refresh period of the token expiration persistence
)

const (
	balanceCallbackUpdate = iota // called when priority drops below the last minimum estimate
	balanceCallbackZero          // called when priority drops to zero (positive balance exhausted)
	balanceCallbackCount         // total number of balance callbacks
)

// BalanceTrackerSetup contains node state flags and fields used by BalanceTracker
type BalanceTrackerSetup struct {
	PriorityFlag, UpdateFlag, activeFlag            nodestate.Flags
	BalanceField, negBalanceKeyField, capacityField nodestate.Field
}

// NewBalanceTrackerSetup creates a new BalanceTrackerSetup and initializes the fields
// and flags controlled by BalanceTracker
func NewBalanceTrackerSetup(setup *nodestate.Setup) BalanceTrackerSetup {
	return BalanceTrackerSetup{
		// PriorityFlag is set if the node has a positive balance
		PriorityFlag: setup.NewFlag("priorityNode"),
		// UpdateFlag set and then immediately reset if the balance has been updated and
		// therefore priority is suddenly changed
		UpdateFlag: setup.NewFlag("balanceUpdate"),
		// BalanceField contains the NodeBalance struct which implements nodePriority,
		// allowing on-demand priority calculation and future priority estimation
		BalanceField: setup.NewField("balance", reflect.TypeOf(&NodeBalance{})),
	}
}

// Init sets the fields used by BalanceTracker as an input
func (bts *BalanceTrackerSetup) Init(negBalanceKeyField, capacityField nodestate.Field) {
	bts.negBalanceKeyField = negBalanceKeyField
	bts.capacityField = capacityField
}

// BalanceTracker tracks positive and negative balances for connected nodes.
// After negBalanceKeyField is set externally, a NodeBalance is created and previous
// balance values are loaded from the database. Both balances are exponentially expired
// values. Costs are deducted from the positive balance if present, otherwise added to
// the negative balance. If the capacity is non-zero then a time cost is applied
// continuously while individual request costs are applied immediately.
// The two balances are translated into a single priority value that also depends
// on the actual capacity.
type BalanceTracker struct {
	BalanceTrackerSetup
	clock                      mclock.Clock
	lock                       sync.Mutex
	ns                         *nodestate.NodeStateMachine
	ndb                        *nodeDB
	posExp, negExp             utils.ValueExpirer
	posExpTC, negExpTC         uint64
	posThreshold, negThreshold uint64
	totalAmount                utils.ExpiredValue
	lastActiveBalanceUpdate    mclock.AbsTime
	quit                       chan struct{}
}

// NewBalanceTracker creates a new BalanceTracker
func NewBalanceTracker(ns *nodestate.NodeStateMachine, setup BalanceTrackerSetup, db ethdb.KeyValueStore, clock mclock.Clock, posExp, negExp utils.ValueExpirer) *BalanceTracker {
	ndb := newNodeDB(db, clock)
	bt := &BalanceTracker{
		ns:                  ns,
		BalanceTrackerSetup: setup,
		ndb:                 ndb,
		clock:               clock,
		posExp:              posExp,
		negExp:              negExp,
		quit:                make(chan struct{}),
	}
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
			bt.totalAmount.AddExp(bt.ndb.getOrNewBalance(ids[i].Bytes(), false))
		}
		if stop {
			break
		}
	}

	ns.SubscribeField(bt.capacityField, func(node *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		n, _ := ns.GetField(node, bt.BalanceField).(*NodeBalance)
		if n == nil {
			log.Error("Capacity field changed while node field is missing")
			return
		}
		newCap, _ := newValue.(uint64)
		if newCap != 0 {
			n.setCapacity(newCap)
		} else {
			n.deactivate(false)
		}
	})

	ns.SubscribeField(bt.negBalanceKeyField, func(node *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		if newValue != nil {
			ns.SetField(node, bt.BalanceField, bt.newNodeBalance(node, newValue.(string)))
		} else {
			if n, _ := ns.GetField(node, bt.BalanceField).(*NodeBalance); n != nil {
				n.deactivate(true)
			}
			ns.SetState(node, nodestate.Flags{}, bt.PriorityFlag, 0)
			ns.SetField(node, bt.BalanceField, nil)
		}
	})

	// The positive and negative balances of clients are stored in database
	// and both of these decay exponentially over time. Delete them if the
	// value is small enough.
	bt.ndb.evictCallBack = bt.canDropBalance

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

// Stop saves expiration offset and unsaved node balances and shuts BalanceTracker down
func (bt *BalanceTracker) Stop() {
	now := bt.clock.Now()
	bt.ndb.setExpiration(bt.posExp.LogOffset(now), bt.negExp.LogOffset(now))
	close(bt.quit)
	bt.ns.ForEach(nodestate.Flags{}, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		if n, ok := bt.ns.GetField(node, bt.BalanceField).(*NodeBalance); ok {
			n.lock.Lock()
			n.storeBalance(true, true)
			n.lock.Unlock()
			bt.ns.SetField(node, bt.BalanceField, nil)
		}
	})
	bt.ndb.close()
}

// TotalTokenAmount returns the current total amount of service tokens in existence
func (bt *BalanceTracker) TotalTokenAmount() uint64 {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	now := bt.clock.Now()
	if now > bt.lastActiveBalanceUpdate+mclock.AbsTime(time.Second*10) {
		bt.ns.ForEach(nodestate.Flags{}, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
			if n, ok := bt.ns.GetField(node, bt.BalanceField).(*NodeBalance); ok {
				n.lock.Lock()
				bt.updateTotalAmount(n)
				n.lock.Unlock()
			}
		})
		bt.lastActiveBalanceUpdate = now
	}
	return bt.totalAmount.Value(bt.posExp.LogOffset(now))
}

// GetPosBalanceIDs lists node IDs with an associated positive balance
func (bt *BalanceTracker) GetPosBalanceIDs(start, stop enode.ID, maxCount int) (result []enode.ID) {
	return bt.ndb.getPosBalanceIDs(start, stop, maxCount)
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

// newNodeBalance loads balances from the database and creates a NodeBalance instance
// for the given node. It also sets the PriorityFlag and adds balanceCallbackZero if
// the node has a positive balance.
func (bt *BalanceTracker) newNodeBalance(node *enode.Node, negBalanceKey string) *NodeBalance {
	pb := bt.ndb.getOrNewBalance(node.ID().Bytes(), false)
	nb := bt.ndb.getOrNewBalance([]byte(negBalanceKey), true)
	n := &NodeBalance{
		bt:            bt,
		node:          node,
		negBalanceKey: negBalanceKey,
		balance:       balance{pos: pb, neg: nb},
		initTime:      bt.clock.Now(),
		lastUpdate:    bt.clock.Now(),
	}
	n.storedBalance = n.balance
	n.summedBalance = n.balance.pos
	for i := range n.callbackIndex {
		n.callbackIndex[i] = -1
	}
	if n.checkPriorityStatus() {
		n.bt.ns.SetState(n.node, n.bt.PriorityFlag, nodestate.Flags{}, 0)
	}
	return n
}

// storeBalance stores either a positive or a negative balance in the database if necessary
func (bt *BalanceTracker) storeBalance(id []byte, neg bool, value utils.ExpiredValue, storedValue *utils.ExpiredValue) {
	if bt.canDropBalance(bt.clock.Now(), neg, value) {
		if (*storedValue) != (utils.ExpiredValue{}) {
			bt.ndb.delBalance(id, neg) // balance is small enough, drop it directly.
			(*storedValue) = utils.ExpiredValue{}
		}
	} else {
		if value != (*storedValue) {
			bt.ndb.setBalance(id, neg, value)
			(*storedValue) = value
		}
	}
}

// canDropBalance tells whether a positive or negative balance is below the threshold
// and therefore can be dropped from the database
func (bt *BalanceTracker) canDropBalance(now mclock.AbsTime, neg bool, b utils.ExpiredValue) bool {
	if neg {
		return b.Value(bt.negExp.LogOffset(now)) <= negThreshold
	} else {
		return b.Value(bt.posExp.LogOffset(now)) <= posThreshold
	}
}

// updateTotalAmount updates total token amount with the balance change of the given
// node since last update.
// Note: this function needs both bt.lock and n.lock to be held.
func (bt *BalanceTracker) updateTotalAmount(n *NodeBalance) {
	if n.balance.pos != n.summedBalance {
		bt.totalAmount.AddExp(n.balance.pos)
		bt.totalAmount.SubExp(n.summedBalance)
		n.summedBalance = n.balance.pos
	}
}

// PriceFactors determine the pricing policy (may apply either to positive or
// negative balances which may have different factors).
// - TimeFactor is cost unit per nanosecond of connection time
// - CapacityFactor is cost unit per nanosecond of connection time per 1000000 capacity
// - RequestFactor is cost unit per request "realCost" unit
type PriceFactors struct {
	TimeFactor, CapacityFactor, RequestFactor float64
}

// timePrice returns the price of connection per nanosecond at the given capacity
func (p PriceFactors) timePrice(cap uint64) float64 {
	if cap == 0 {
		return 0 // no pricing applied for inactive nodes
	}
	return p.TimeFactor + float64(cap)*p.CapacityFactor/1000000
}

// NodeBalance keeps track of the positive and negative balances of a connected
// client and calculates actual and projected future priority values.
// Implements nodePriority interface.
type NodeBalance struct {
	bt                               *BalanceTracker
	lock                             sync.RWMutex
	node                             *enode.Node
	negBalanceKey                    string
	priorityFlag, closed             bool
	capacity                         uint64
	balance, storedBalance           balance
	summedBalance                    utils.ExpiredValue
	posFactor, negFactor             PriceFactors
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

// GetBalance returns the current positive and negative balance
func (n *NodeBalance) GetBalance() (uint64, uint64) {
	n.lock.Lock()
	defer n.lock.Unlock()

	now := n.bt.clock.Now()
	n.updateBalance(now)
	return n.balance.pos.Value(n.bt.posExp.LogOffset(now)), n.balance.neg.Value(n.bt.negExp.LogOffset(now))
}

// AddPosBalance adds the given amount to the positive balance and returns the balance
// before and after the operation. Exceeding maxBalance results in an error (balance is
// unchanged) while adding a negative amount higher than the current balance results in
// zero balance.
func (n *NodeBalance) AddPosBalance(amount int64) (uint64, uint64, error) {
	n.bt.lock.Lock()
	n.lock.Lock()
	var setPriority bool
	defer func() {
		n.lock.Unlock()
		n.bt.lock.Unlock()
		if setPriority {
			n.bt.ns.SetState(n.node, n.bt.PriorityFlag, nodestate.Flags{}, 0)
		}
		n.signalPriorityUpdate()
	}()

	now := n.bt.clock.Now()
	n.updateBalance(now)
	logOffset := n.bt.posExp.LogOffset(now)
	oldValue := n.balance.pos.Value(logOffset)
	if amount > 0 && (amount > maxBalance || oldValue > maxBalance-uint64(amount)) {
		return oldValue, oldValue, errBalanceOverflow
	}
	n.balance.pos.Add(amount, logOffset)
	setPriority = n.checkPriorityStatus()
	n.checkCallbacks(now)
	newValue := n.balance.pos.Value(logOffset)
	n.storeBalance(true, false)
	n.bt.updateTotalAmount(n)
	return oldValue, newValue, nil
}

// SetBalance sets the positive and negative balance to the given values
func (n *NodeBalance) SetBalance(pos, neg uint64) error {
	n.bt.lock.Lock()
	n.lock.Lock()
	var setPriority bool
	defer func() {
		n.lock.Unlock()
		n.bt.lock.Unlock()
		if setPriority {
			n.bt.ns.SetState(n.node, n.bt.PriorityFlag, nodestate.Flags{}, 0)
		}
		n.signalPriorityUpdate()
	}()

	if pos > maxBalance || neg > maxBalance {
		return errBalanceOverflow
	}
	now := n.bt.clock.Now()
	n.updateBalance(now)
	var pb, nb utils.ExpiredValue
	pb.Add(int64(pos), n.bt.posExp.LogOffset(now))
	nb.Add(int64(neg), n.bt.negExp.LogOffset(now))
	n.balance.pos = pb
	n.balance.neg = nb
	setPriority = n.checkPriorityStatus()
	n.checkCallbacks(now)
	n.storeBalance(true, true)
	n.bt.updateTotalAmount(n)
	return nil
}

// RequestServed should be called after serving a request for the given peer
func (n *NodeBalance) RequestServed(cost uint64) uint64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.closed {
		return 0
	}
	now := n.bt.clock.Now()
	n.updateBalance(now)
	fcost := float64(cost)

	posExp := n.bt.posExp.LogOffset(now)
	if !n.balance.pos.IsZero() {
		if n.posFactor.RequestFactor != 0 {
			c := -int64(fcost * n.posFactor.RequestFactor)
			cc := n.balance.pos.Add(c, posExp)
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
		if n.negFactor.RequestFactor != 0 {
			n.balance.neg.Add(int64(fcost*n.negFactor.RequestFactor), n.bt.negExp.LogOffset(now))
			n.checkCallbacks(now)
		}
	}
	n.sumReqCost += cost
	return n.balance.pos.Value(posExp)
}

// Priority returns the actual priority based on the current balance
func (n *NodeBalance) Priority(now mclock.AbsTime, capacity uint64) int64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.updateBalance(now)
	return n.balanceToPriority(n.balance, capacity)
}

// EstMinPriority gives a lower estimate for the priority at a given time in the future.
// An average request cost per time is assumed that is twice the average cost per time
// in the current session.
// If update is true then a priority callback is added that turns UpdateFlag on and off
// in case the priority goes below the estimated minimum.
func (n *NodeBalance) EstMinPriority(at mclock.AbsTime, capacity uint64, update bool) int64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	var avgReqCost float64
	dt := time.Duration(n.lastUpdate - n.initTime)
	if dt > time.Second {
		avgReqCost = float64(n.sumReqCost) * 2 / float64(dt)
	}
	pri := n.balanceToPriority(n.reducedBalance(at, capacity, avgReqCost), capacity)
	if update {
		n.addCallback(balanceCallbackUpdate, pri, n.signalPriorityUpdate)
	}
	return pri
}

// PosBalanceMissing calculates the missing amount of positive balance in order to
// connect at targetCapacity, stay connected for the given amount of time and then
// still have a priority of targetPriority
func (n *NodeBalance) PosBalanceMissing(targetPriority int64, targetCapacity uint64, after time.Duration) uint64 {
	n.lock.Lock()
	defer n.lock.Unlock()

	now := n.bt.clock.Now()
	if targetPriority < 0 {
		timePrice := n.negFactor.timePrice(targetCapacity)
		timeCost := uint64(float64(after) * timePrice)
		negBalance := n.balance.neg.Value(n.bt.negExp.LogOffset(now))
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
	posBalance := n.balance.pos.Value(n.bt.posExp.LogOffset(now))
	if posRequired > posBalance {
		return posRequired - posBalance
	}
	return 0
}

// SetPriceFactors sets the price factors. TimeFactor is the price of a nanosecond of
// connection while RequestFactor is the price of a request cost unit.
func (n *NodeBalance) SetPriceFactors(posFactor, negFactor PriceFactors) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.closed {
		return
	}
	now := n.bt.clock.Now()
	n.updateBalance(now)
	n.posFactor, n.negFactor = posFactor, negFactor
	n.checkCallbacks(now)
}

// GetPriceFactors returns the price factors
func (n *NodeBalance) GetPriceFactors() (posFactor, negFactor PriceFactors) {
	n.lock.Lock()
	defer n.lock.Unlock()

	return n.posFactor, n.negFactor
}

// deactivate stops time/capacity cost deduction and saves the balances in the database
func (n *NodeBalance) deactivate(close bool) {
	n.lock.Lock()
	defer n.lock.Unlock()
	if n.closed {
		return
	}
	n.updateBalance(n.bt.clock.Now())
	n.capacity = 0
	if n.updateEvent != nil {
		n.updateEvent.Stop()
		n.updateEvent = nil
	}
	n.storeBalance(true, true)
	n.closed = close
}

// updateBalance updates balance based on the time factor
func (n *NodeBalance) updateBalance(now mclock.AbsTime) {
	if now > n.lastUpdate {
		n.balance = n.reducedBalance(now, n.capacity, 0)
		n.lastUpdate = now
	}
}

// storeBalance stores the positive and/or negative balance of the node in the database
func (n *NodeBalance) storeBalance(pos, neg bool) {
	if pos {
		n.bt.storeBalance(n.node.ID().Bytes(), false, n.balance.pos, &n.storedBalance.pos)
	}
	if neg {
		n.bt.storeBalance([]byte(n.negBalanceKey), true, n.balance.neg, &n.storedBalance.neg)
	}
}

// addCallback sets up a one-time callback to be called when priority reaches
// the threshold. If it has already reached the threshold the callback is called
// immediately.
// Note: should be called while n.lock is held
func (n *NodeBalance) addCallback(id int, threshold int64, callback func()) {
	n.removeCallback(id)
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
	now := n.bt.clock.Now()
	n.updateBalance(now)
	n.checkCallbacks(now)
}

// removeCallback removes the given callback and returns true if it was active
// Note: should be called while n.lock is held
func (n *NodeBalance) removeCallback(id int) bool {
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

// checkCallbacks checks whether the threshold of any of the active callbacks
// have been reached and calls them if necessary. It also sets up or updates
// a scheduled event to ensure that is will be called again just after the next
// threshold has been reached.
// Note: checkCallbacks assumes that the balance has been recently updated.
func (n *NodeBalance) checkCallbacks(now mclock.AbsTime) {
	if n.callbackCount == 0 || n.capacity == 0 {
		return
	}
	pri := n.balanceToPriority(n.balance, n.capacity)
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
			n.updateEvent = n.bt.clock.AfterFunc(dt, func() {
				n.lock.Lock()
				defer n.lock.Unlock()

				if n.callbackCount != 0 {
					now := n.bt.clock.Now()
					n.updateBalance(now)
					n.checkCallbacks(now)
				}
			})
		}
	}
}

// balanceExhausted should be called when the positive balance is exhausted (priority goes to zero/negative)
func (n *NodeBalance) balanceExhausted() {
	n.lock.Lock()
	n.storeBalance(true, false)
	n.priorityFlag = false
	n.lock.Unlock()
	n.bt.ns.SetState(n.node, nodestate.Flags{}, n.bt.PriorityFlag, 0)
}

// checkPriorityStatus checks whether the node has gained priority status and sets the priority
// callback if necessary. It assumes that the balance has been recently updated.
// Note that the priority flag has to be set by the caller after the mutex has been released.
func (n *NodeBalance) checkPriorityStatus() bool {
	if !n.priorityFlag && !n.balance.pos.IsZero() {
		n.priorityFlag = true
		n.addCallback(balanceCallbackZero, 0, func() { n.balanceExhausted() })
		return true
	}
	return false
}

// signalPriorityUpdate signals that the priority fell below the previous minimum estimate
func (n *NodeBalance) signalPriorityUpdate() {
	n.bt.ns.SetState(n.node, n.bt.UpdateFlag, nodestate.Flags{}, 0)
	n.bt.ns.SetState(n.node, nodestate.Flags{}, n.bt.UpdateFlag, 0)
}

// setCapacity updates the capacity value used for priority calculation
// Note: capacity should never be zero
func (n *NodeBalance) setCapacity(capacity uint64) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.closed {
		return
	}
	now := n.bt.clock.Now()
	n.updateBalance(now)
	n.capacity = capacity
	n.checkCallbacks(now)
}

// balanceToPriority converts a balance to a priority value. Lower priority means
// first to disconnect. Positive balance translates to positive priority. If positive
// balance is zero then negative balance translates to a negative priority.
func (n *NodeBalance) balanceToPriority(b balance, capacity uint64) int64 {
	if !b.pos.IsZero() {
		return int64(b.pos.Value(n.bt.posExp.LogOffset(n.bt.clock.Now())) / capacity)
	}
	return -int64(b.neg.Value(n.bt.negExp.LogOffset(n.bt.clock.Now())))
}

// reducedBalance estimates the reduced balance at a given time in the fututre based
// on the current balance, the time factor and an estimated average request cost per time ratio
func (n *NodeBalance) reducedBalance(at mclock.AbsTime, capacity uint64, avgReqCost float64) balance {
	dt := float64(at - n.lastUpdate)
	b := n.balance
	if !b.pos.IsZero() {
		factor := n.posFactor.timePrice(capacity) + n.posFactor.RequestFactor*avgReqCost
		diff := -int64(dt * factor)
		dd := b.pos.Add(diff, n.bt.posExp.LogOffset(at))
		if dd == diff {
			dt = 0
		} else {
			dt += float64(dd) / factor
		}
	}
	if dt > 0 {
		factor := n.negFactor.timePrice(capacity) + n.negFactor.RequestFactor*avgReqCost
		b.neg.Add(int64(dt*factor), n.bt.negExp.LogOffset(at))
	}
	return b
}

// timeUntil calculates the remaining time needed to reach a given priority level
// assuming that no requests are processed until then. If the given level is never
// reached then (0, false) is returned.
// Note: the function assumes that the balance has been recently updated and
// calculates the time starting from the last update.
func (n *NodeBalance) timeUntil(priority int64) (time.Duration, bool) {
	now := n.bt.clock.Now()
	var dt float64
	if !n.balance.pos.IsZero() {
		posBalance := n.balance.pos.Value(n.bt.posExp.LogOffset(now))
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
	negBalance := n.balance.neg.Value(n.bt.negExp.LogOffset(now))
	timePrice := n.negFactor.timePrice(n.capacity)
	if uint64(-priority) > negBalance {
		if timePrice < 1e-100 {
			return 0, false
		}
		dt += float64(uint64(-priority)-negBalance) / timePrice
	}
	return time.Duration(dt), true
}
