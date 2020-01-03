// Copyright 2020 The go-ethereum Authors
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
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

var (
	ActiveFlag      = utils.NewNodeStateFlag("active", false, false)
	InactiveFlag    = utils.NewNodeStateFlag("inactive", false, false)
	ppNodeInfoField = utils.NewNodeStateField("ppNodeInfo", reflect.TypeOf(ppNodeInfo{}), nil)
	CapacityField   = utils.NewNodeStateField("capacity", reflect.TypeOf(uint64(0)), []*utils.NodeStateFlag{ActiveFlag})
)

const lazyQueueRefresh = time.Second * 10 // refresh period of the active queue

// PriorityPool handles a set of nodes where each node has a capacity (a scalar value)
// and a priority (which can change over time and can also depend on the capacity).
// A node is active if it has at least the necessary minimal amount of capacity while
// inactive nodes have 0 capacity (values between 0 and the minimum are not allowed).
// The pool ensures that the number and total capacity of all active nodes are limited
// and the highest priority nodes are active at all times (limits can be changed
// during operation with immediate effect).
//
// When activating clients a priority bias is applied in favor of the already active
// nodes in order to avoid nodes quickly alternating between active and inactive states
// when their priorities are close to each other. The bias is specified in terms of
// duration (time) because priorities are expected to usually get lower over time and
// therefore a future minimum prediction (see EstMinPriority) should monotonously
// decrease with the specified time parameter.
// This time bias can be interpreted as minimum expected active time at the given
// capacity (if the threshold priority stays the same).
//
// Nodes are added to the pool by externally putting them into "requested" state. The
// highest priority nodes in "requested" state are moved to "active" state as soon as
// the minimum capacity can be granted for them. The capacity of lower priority active
// nodes is reduced or they are demoted to "requested" state if their priority is
// insufficient even at minimal capacity. The pool only moves nodes between "active"
// and "requested" states. Both state flags can be revoked externally but only
// "requested" should be granted by the caller.
type PriorityPool struct {
	ns                     *utils.NodeStateMachine
	activeQueue            *prque.LazyQueue
	inactiveQueue          *prque.Prque
	lock                   sync.Mutex
	changed                []*ppNodeInfo
	activeCount, activeCap uint64
	maxCount, maxCap       uint64
	minCap                 uint64
}

// ppNode interface provides the priority of each node
type ppNode interface {
	// Priority should return the current priority of the node (higher is better)
	Priority(now mclock.AbsTime, cap uint64) int64
	// EstMinPriority should return a lower estimate for the minimum of the node priority
	// value starting from the current moment until the given time. If the priority goes
	// under the returned estimate before the specified moment then it is the caller's
	// responsibility to call UpdatePriority.
	EstMinPriority(until mclock.AbsTime, cap uint64) int64
}

type ppNodeInfo struct {
	node                            ppNode
	id                              enode.ID
	connected                       bool
	capacity, origCap               uint64
	bias                            time.Duration
	forced, changed                 bool
	activeIndex, inactiveIndex      int
	stopCh                          chan struct{}
	ppNodeFieldIndex, capFieldIndex int
}

func NewPriorityPool(ns *utils.NodeStateMachine, minCap uint64, minBias time.Duration, nodeField *utils.NodeStateField) *PriorityPool {
	pp := &PriorityPool{
		ns:               ns,
		activeQueue:      prque.NewLazyQueue(activeSetIndex, activePriority, activeMaxPriority, clock, lazyQueueRefresh),
		inactiveQueue:    prque.New(inactiveSetIndex),
		minCap:           minCap,
		minBias:          minBias,
		stActive:         ns.MustRegisterState(ActiveFlag),
		stInactive:       ns.MustRegisterState(InactiveFlag),
		nodeFieldIndex:   ns.FieldIndex(nodeField),
		ppNodeFieldIndex: ns.MustRegisterField(ppNodeInfoField),
		capFieldIndex:    ns.MustRegisterField(CapacityField),
		stopCh:           make(chan struct{}),
	}

	ns.AddFieldSub(pp.nodeFieldIndex, func(id enode.ID, state utils.NodeStateBitMask, oldValue, newValue interface{}) {
		if newValue != nil {
			c := &ppNodeInfo{
				id:            id,
				node:          newValue.(ppNode),
				activeIndex:   -1,
				inactiveIndex: -1,
			}
			ns.SetField(id, ppNodeFieldIndex, c)
			if state&(stActive|stInactive) != 0 {
				pp.connectedNode(c)
			}
		} else {
			if c := pp.nodeInfo(id); c != nil {
				pp.disconnectedNode(c)
			}
			ns.UpdateState(id, 0, stActive|stInactive, 0)
			ns.SetField(id, ppNodeFieldIndex, nil)
		}
	})
	ns.AddStateSub(stActive|stInactive, func(id enode.ID, oldState, newState utils.NodeStateBitMask) {
		if c := pp.nodeInfo(id); c != nil {
			if oldState == 0 {
				pp.connectedNode(c)
			}
			if newState == 0 {
				pp.disconnectedNode(c)
			}
		}
	})
	go func() {
		for {
			select {
			case <-clock.After(lazyQueueRefresh):
				pp.lock.Lock()
				pp.activeQueue.Refresh()
				pp.lock.Unlock()
			case <-pp.stopCh:
				return
			}
		}
	}()
	return pp
}

func (pp *PriorityPool) Stop() {
	close(pp.stopCh)
}

// inactiveSetIndex callback updates clientInfo item index in inactiveQueue
func inactiveSetIndex(a interface{}, index int) {
	a.(*ppNodeInfo).inactiveIndex = index
}

// activeSetIndex callback updates clientInfo item index in activeQueue
func activeSetIndex(a interface{}, index int) {
	a.(*ppNodeInfo).activeIndex = index
}

func invertPriority(p int64) int64 {
	if p == math.MinInt64 {
		return math.MaxInt64
	}
	return -p
}

// activePriority callback returns actual priority of clientInfo item in activeQueue
func activePriority(a interface{}, now mclock.AbsTime) int64 {
	c := a.(*ppNodeInfo)
	if c.forced {
		return math.MinInt64
	}
	if c.bias == 0 {
		return invertPriority(c.client.Priority(now, c.capacity))
	} else {
		return invertPriority(c.client.EstMinPriority(now+mclock.AbsTime(c.bias), c.capacity))
	}
}

// activeMaxPriority callback returns estimated maximum priority of clientInfo item in activeQueue
func activeMaxPriority(a interface{}, until mclock.AbsTime) int64 {
	c := a.(*ppNodeInfo)
	if c.forced {
		return math.MinInt64
	}
	if c.bias == 0 {
		return invertPriority(c.client.EstMinPriority(until, c.capacity))
	} else {
		return invertPriority(c.client.EstMinPriority(until+mclock.AbsTime(c.bias), c.capacity))
	}
}

func (pp *PriorityPool) inactivePriority(p *ppNodeInfo) int64 {
	p.node.EstMinPriority(pp.clock.Now()+mclock.AbsTime(pp.minBias), pp.minCap)
}

func (pp *PriorityPool) nodeInfo(id enode.ID) *ppNodeInfo {
	if c := pp.ns.GetField(id, ppNodeFieldIndex); c != nil {
		return c.(*ppNodeInfo)
	}
	return nil
}

// Note: this function is called by the state sub and initiated from outside
func (pp *PriorityPool) connectedNode(c *ppNodeInfo) {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	if c.connected {
		return
	}
	c.connected = true
	pp.inactiveQueue.Push(c, pp.inactivePriority(c))
	pp.tryActivate()
}

// Note: this function is called by the state sub and initiated from outside
func (pp *PriorityPool) disconnectedNode(c *ppNodeInfo) {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	if !c.connected {
		return
	}
	c.connected = false
	pp.activeQueue.Remove(c.queueIndex)
	pp.inactiveQueue.Remove(c.queueIndex)
	if c.capacity != 0 {
		pp.setCapacity(c, 0)
		pp.tryActivate()
	}
}

func (pp *PriorityPool) markForChange(c *ppNodeInfo) {
	if c.changed {
		return
	}
	c.changed = true
	c.origCap = c.capacity
	pp.changed = append(pp.changed, c)
}

func (pp *PriorityPool) finalizeChanges(commit bool) {
	for _, c := range pp.changed {
		// always remove and push back in order to update biased/forced priority
		pp.activeQueue.Remove(c.activeIndex)
		pp.inactiveQueue.Remove(c.inactiveIndex)
		c.bias = 0
		c.forced = false
		c.changed = false
		capChanged := c.capacity != c.origCap
		if !commit {
			pp.setCapacity(c, c.origCap)
		}
		c.origCap = 0
		if c.connected {
			if c.capacity != 0 {
				pp.activeQueue.Push(c)
			} else {
				pp.inactiveQueue.Push(c, pp.inactivePriority(c))
			}
			if capChanged && commit {
				pp.ns.SetField(c.id, pp.capFieldIndex, c.capacity)
				if c.origCap == 0 {
					pp.ns.UpdateState(c.id, pp.stActive, pp.stInactive, 0)
				}
				if c.capacity == 0 {
					pp.ns.UpdateState(c.id, pp.stInactive, pp.stActive, 0)
				}
			}
		}
	}
	pp.changed = nil
}

func (pp *PriorityPool) SetLimits(maxCount, maxCap uint64) {
	inc := (maxCount > pp.maxCount) || (maxCap > pp.maxCap)
	dec := (maxCount < pp.maxCount) || (maxCap < pp.maxCap)
	pp.maxCount, pp.maxCap = maxCount, maxCap
	if dec {
		pp.enforceLimits()
		pp.finalizeChanges(true)
	}
	if inc {
		pp.tryActivate()
	}
}

func (pp *PriorityPool) UpdatePriority(id enode.ID) {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	c := pp.nodeInfo(id)
	pp.activeQueue.Remove(c.activeIndex)
	pp.inactiveQueue.Remove(c.inactiveIndex)
	if c.capacity != 0 {
		pp.activeQueue.Push(c)
	} else {
		pp.inactiveQueue.Push(c, pp.inactivePriority(c))
	}
	pp.tryActivate()
}

// returns lowest priority to compete with (higher is better)
func (pp *PriorityPool) RequestCapacity(id enode.ID, targetCap uint64, bias time.Duration, setCap bool) (int64, bool) {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	c := pp.nodeInfo(id)
	if c == nil {
		log.Error("RequestCapacity called for unknown node", "id", id)
		return math.MaxInt64, false
	}
	var priority int64
	if cap > c.capacity {
		priority = invertPriority(c.client.EstMinPriority(pp.clock.Now()+mclock.AbsTime(bias), cap))
	} else {
		priority = invertPriority(c.client.Priority(pp.clock.Now(), cap))
	}
	pp.markForChange(c)
	pp.setCapacity(c, targetCap)
	c.forced = true
	pp.activeQueue.Remove(c.activeIndex)
	pp.activeQueue.Push(c)
	minPriority := pp.enforceLimits()
	// if capacity update is possible now then minPriority == math.MinInt64
	// if it is not possible at all then minPriority == math.MaxInt64
	allowed := priority > minPriority
	pp.finalizeChanges(setCap && allowed)
	return minPriority, allowed
}

// returns lowest connection priority (higher is better)
func (pp *PriorityPool) enforceLimits() int64 {
	if pp.activeCap <= pp.maxCap && pp.activeCount <= pp.maxCount {
		return math.MinInt64
	}
	var maxActivePriority int64
	pp.activeQueue.MultiPop(func(data interface{}, priority int64) bool {
		c := data.(*ppNodeInfo)
		pp.markForChange(c)
		maxActivePriority = priority
		if c.capacity == pp.minCap {
			pp.setCapacity(c, 0)
		} else {
			sub := c.capacity / 4
			if c.capacity-sub < pp.minCap {
				sub = c.capacity - pp.minCap
			}
			pp.setCapacity(c, c.capacity-sub)
			pp.activeQueue.Push(c)
		}
		return pp.activeCap > pp.maxCap || pp.activeCount > pp.maxCount
	})
	return invertPriority(maxPriority)
}

func (pp *PriorityPool) tryActivate() {
	var commit bool
	for pp.inactiveQueue.Len() > 0 {
		c := pp.inactiveQueue.PopItem().(*ppNodeInfo)
		pp.markForChange(c)
		pp.setCapacity(c, pp.minCap)
		c.bias = pp.minBias
		pp.activeQueue.Push(c)
		pp.enforceLimits()
		if c.capacity > 0 {
			commit = true
		} else {
			break
		}
	}
	pp.finalizeChanges(commit)
}

func (pp *PriorityPool) setCapacity(n *ppNodeInfo, cap uint64) {
	pp.activeCap += cap - n.capacity
	if n.capacity == 0 {
		pp.activeCount++
	}
	if cap == 0 {
		pp.activeCount--
	}
	n.capacity = cap
}

func (pp *PriorityPool) ActiveCapacity() uint64 {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	return pp.activeCap
}
