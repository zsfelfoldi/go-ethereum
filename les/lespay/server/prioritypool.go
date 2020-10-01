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
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

const (
	lazyQueueRefresh = time.Second * 10 // refresh period of the active queue
)

// PriorityPoolSetup contains node state flags and fields used by PriorityPool
// Note: ActiveFlag and InactiveFlag can be controlled both externally and by the pool,
// see PriorityPool description for details.
type PriorityPoolSetup struct {
	// controlled by PriorityPool
	ActiveFlag, InactiveFlag       nodestate.Flags
	CapacityField, ppNodeInfoField nodestate.Field
	// external connections
	updateFlag    nodestate.Flags
	priorityField nodestate.Field
}

// NewPriorityPoolSetup creates a new PriorityPoolSetup and initializes the fields
// and flags controlled by PriorityPool
func NewPriorityPoolSetup(setup *nodestate.Setup) PriorityPoolSetup {
	return PriorityPoolSetup{
		ActiveFlag:      setup.NewFlag("active"),
		InactiveFlag:    setup.NewFlag("inactive"),
		CapacityField:   setup.NewField("capacity", reflect.TypeOf(uint64(0))),
		ppNodeInfoField: setup.NewField("ppNodeInfo", reflect.TypeOf(&ppNodeInfo{})),
	}
}

// Connect sets the fields and flags used by PriorityPool as an input
func (pps *PriorityPoolSetup) Connect(priorityField nodestate.Field, updateFlag nodestate.Flags) {
	pps.priorityField = priorityField // should implement nodePriority
	pps.updateFlag = updateFlag       // triggers an immediate priority update
}

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
// Nodes in the pool always have either InactiveFlag or ActiveFlag set. A new node is
// added to the pool by externally setting InactiveFlag. PriorityPool can switch a node
// between InactiveFlag and ActiveFlag at any time. Nodes can be removed from the pool
// by externally resetting both flags. ActiveFlag should not be set externally.
//
// The highest priority nodes in "inactive" state are moved to "active" state as soon as
// the minimum capacity can be granted for them. The capacity of lower priority active
// nodes is reduced or they are demoted to "inactive" state if their priority is
// insufficient even at minimal capacity.
type PriorityPool struct {
	PriorityPoolSetup
	ns                     *nodestate.NodeStateMachine
	clock                  mclock.Clock
	lock                   sync.Mutex
	activeQueue            *prque.LazyQueue
	inactiveQueue          *prque.Prque
	changed                []*ppNodeInfo
	activeCount, activeCap uint64
	maxCount, maxCap       uint64
	minCap                 uint64
	activeBias             time.Duration
	capacityStepDiv        uint64

	cachedCurve    *CapacityCurve
	ccLock         sync.Mutex
	ccUpdatedAt    mclock.AbsTime
	ccUpdateForced bool
}

// nodePriority interface provides current and estimated future priorities on demand
type nodePriority interface {
	// Priority should return the current priority of the node (higher is better)
	Priority(now mclock.AbsTime) int64
	// EstMinPriority should return a lower estimate for the minimum of the node priority
	// value starting from the current moment until the given time. If the priority goes
	// under the returned estimate before the specified moment then it is the caller's
	// responsibility to signal with updateFlag.
	EstMinPriority(until mclock.AbsTime, cap uint64, update bool) int64
}

// ppNodeInfo is the internal node descriptor of PriorityPool
type ppNodeInfo struct {
	nodePriority               nodePriority
	node                       *enode.Node
	connected                  bool
	capacity, origCap          uint64
	bias                       time.Duration
	forced, changed            bool
	activeIndex, inactiveIndex int
}

// NewPriorityPool creates a new PriorityPool
func NewPriorityPool(ns *nodestate.NodeStateMachine, setup PriorityPoolSetup, clock mclock.Clock, minCap uint64, activeBias time.Duration, capacityStepDiv uint64) *PriorityPool {
	pp := &PriorityPool{
		ns:                ns,
		PriorityPoolSetup: setup,
		clock:             clock,
		activeQueue:       prque.NewLazyQueue(activeSetIndex, activePriority, activeMaxPriority, clock, lazyQueueRefresh),
		inactiveQueue:     prque.New(inactiveSetIndex),
		minCap:            minCap,
		activeBias:        activeBias,
		capacityStepDiv:   capacityStepDiv,
	}

	ns.SubscribeField(pp.priorityField, func(node *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		if newValue != nil {
			c := &ppNodeInfo{
				node:          node,
				nodePriority:  newValue.(nodePriority),
				activeIndex:   -1,
				inactiveIndex: -1,
			}
			ns.SetFieldSub(node, pp.ppNodeInfoField, c)
		} else {
			ns.SetStateSub(node, nodestate.Flags{}, pp.ActiveFlag.Or(pp.InactiveFlag), 0)
			if n, _ := pp.ns.GetField(node, pp.ppNodeInfoField).(*ppNodeInfo); n != nil {
				pp.disconnectedNode(n)
			}
			ns.SetFieldSub(node, pp.CapacityField, nil)
			ns.SetFieldSub(node, pp.ppNodeInfoField, nil)
		}
	})
	ns.SubscribeState(pp.ActiveFlag.Or(pp.InactiveFlag), func(node *enode.Node, oldState, newState nodestate.Flags) {
		if c, _ := pp.ns.GetField(node, pp.ppNodeInfoField).(*ppNodeInfo); c != nil {
			if oldState.IsEmpty() {
				pp.connectedNode(c)
			}
			if newState.IsEmpty() {
				pp.disconnectedNode(c)
			}
		}
	})
	ns.SubscribeState(pp.updateFlag, func(node *enode.Node, oldState, newState nodestate.Flags) {
		if !newState.IsEmpty() {
			pp.updatePriority(node)
		}
	})
	return pp
}

// RequestCapacity checks whether changing the capacity of a node to the given target
// is possible (bias is applied in favor of other active nodes if the target is higher
// than the current capacity).
// If setCap is true then it also performs the change if possible. The function returns
// the minimum priority needed to do the change and whether it is currently allowed.
// If setCap and allowed are both true then the caller can assume that the change was
// successful.
// Note: priorityField should always be set before calling RequestCapacity. If setCap
// is false then both InactiveFlag and ActiveFlag can be unset and they are not changed
// by this function call either.
// Note 2: this function should run inside a NodeStateMachine operation
func (pp *PriorityPool) RequestCapacity(node *enode.Node, targetCap uint64, bias time.Duration, setCap bool) (minPriority int64, allowed bool) {
	pp.lock.Lock()
	pp.activeQueue.Refresh()
	var updates []capUpdate
	defer func() {
		pp.lock.Unlock()
		pp.updateFlags(updates)
	}()

	if targetCap < pp.minCap {
		targetCap = pp.minCap
	}
	c, _ := pp.ns.GetField(node, pp.ppNodeInfoField).(*ppNodeInfo)
	if c == nil {
		log.Error("RequestCapacity called for unknown node", "id", node.ID())
		return math.MaxInt64, false
	}
	var priority int64
	if targetCap > c.capacity {
		priority = c.nodePriority.EstMinPriority(pp.clock.Now()+mclock.AbsTime(bias), targetCap, false) / int64(targetCap)
	} else {
		priority = c.nodePriority.Priority(pp.clock.Now()) / int64(targetCap)
	}
	pp.markForChange(c)
	pp.setCapacity(c, targetCap)
	c.forced = true
	pp.activeQueue.Remove(c.activeIndex)
	pp.inactiveQueue.Remove(c.inactiveIndex)
	pp.activeQueue.Push(c)
	minPriority = pp.enforceLimits()
	// if capacity update is possible now then minPriority == math.MinInt64
	// if it is not possible at all then minPriority == math.MaxInt64
	allowed = priority > minPriority
	updates = pp.finalizeChanges(setCap && allowed)
	return
}

// SetLimits sets the maximum number and total capacity of simultaneously active nodes
func (pp *PriorityPool) SetLimits(maxCount, maxCap uint64) {
	pp.lock.Lock()
	pp.activeQueue.Refresh()
	var updates []capUpdate
	defer func() {
		pp.lock.Unlock()
		pp.ns.Operation(func() { pp.updateFlags(updates) })
	}()

	inc := (maxCount > pp.maxCount) || (maxCap > pp.maxCap)
	dec := (maxCount < pp.maxCount) || (maxCap < pp.maxCap)
	pp.maxCount, pp.maxCap = maxCount, maxCap
	if dec {
		pp.enforceLimits()
		updates = pp.finalizeChanges(true)
	}
	if inc {
		updates = pp.tryActivate()
	}
}

// SetActiveBias sets the bias applied when trying to activate inactive nodes
func (pp *PriorityPool) SetActiveBias(bias time.Duration) {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	pp.activeBias = bias
	pp.tryActivate()
}

// Active returns the number and total capacity of currently active nodes
func (pp *PriorityPool) Active() (uint64, uint64) {
	pp.lock.Lock()
	defer pp.lock.Unlock()

	return pp.activeCount, pp.activeCap
}

// inactiveSetIndex callback updates ppNodeInfo item index in inactiveQueue
func inactiveSetIndex(a interface{}, index int) {
	a.(*ppNodeInfo).inactiveIndex = index
}

// activeSetIndex callback updates ppNodeInfo item index in activeQueue
func activeSetIndex(a interface{}, index int) {
	a.(*ppNodeInfo).activeIndex = index
}

// invertPriority inverts a priority value. The active queue uses inverted priorities
// because the node on the top is the first to be deactivated.
func invertPriority(p int64) int64 {
	if p == math.MinInt64 {
		return math.MaxInt64
	}
	return -p
}

// activePriority callback returns actual priority of ppNodeInfo item in activeQueue
func activePriority(a interface{}, now mclock.AbsTime) int64 {
	c := a.(*ppNodeInfo)
	if c.forced {
		return math.MinInt64
	}
	if c.bias == 0 {
		return invertPriority(c.nodePriority.Priority(now) / int64(c.capacity))
	} else {
		return invertPriority(c.nodePriority.EstMinPriority(now+mclock.AbsTime(c.bias), c.capacity, true) / int64(c.capacity))
	}
}

// activeMaxPriority callback returns estimated maximum priority of ppNodeInfo item in activeQueue
func activeMaxPriority(a interface{}, until mclock.AbsTime) int64 {
	c := a.(*ppNodeInfo)
	if c.forced {
		return math.MinInt64
	}
	return invertPriority(c.nodePriority.EstMinPriority(until+mclock.AbsTime(c.bias), c.capacity, false) / int64(c.capacity))
}

// inactivePriority callback returns actual priority of ppNodeInfo item in inactiveQueue
func (pp *PriorityPool) inactivePriority(p *ppNodeInfo) int64 {
	return p.nodePriority.Priority(pp.clock.Now()) / int64(pp.minCap)
}

// connectedNode is called when a new node has been added to the pool (InactiveFlag set)
// Note: this function should run inside a NodeStateMachine operation
func (pp *PriorityPool) connectedNode(c *ppNodeInfo) {
	pp.lock.Lock()
	pp.activeQueue.Refresh()
	var updates []capUpdate
	defer func() {
		pp.lock.Unlock()
		pp.updateFlags(updates)
	}()

	if c.connected {
		return
	}
	c.connected = true
	pp.inactiveQueue.Push(c, pp.inactivePriority(c))
	updates = pp.tryActivate()
}

// disconnectedNode is called when a node has been removed from the pool (both InactiveFlag
// and ActiveFlag reset)
// Note: this function should run inside a NodeStateMachine operation
func (pp *PriorityPool) disconnectedNode(c *ppNodeInfo) {
	pp.lock.Lock()
	pp.activeQueue.Refresh()
	var updates []capUpdate
	defer func() {
		pp.lock.Unlock()
		pp.updateFlags(updates)
	}()

	if !c.connected {
		return
	}
	c.connected = false
	pp.activeQueue.Remove(c.activeIndex)
	pp.inactiveQueue.Remove(c.inactiveIndex)
	if c.capacity != 0 {
		pp.setCapacity(c, 0)
		updates = pp.tryActivate()
	}
}

// markForChange internally puts a node in a temporary state that can either be reverted
// or confirmed later. This temporary state allows changing the capacity of a node and
// moving it between the active and inactive queue. ActiveFlag/InactiveFlag and
// CapacityField are not changed while the changes are still temporary.
func (pp *PriorityPool) markForChange(c *ppNodeInfo) {
	if c.changed {
		return
	}
	c.changed = true
	c.origCap = c.capacity
	pp.changed = append(pp.changed, c)
}

// setCapacity changes the capacity of a node and adjusts activeCap and activeCount
// accordingly. Note that this change is performed in the temporary state so it should
// be called after markForChange and before finalizeChanges.
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

// enforceLimits enforces active node count and total capacity limits. It returns the
// lowest active node priority. Note that this function is performed on the temporary
// internal state.
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
			sub := c.capacity / pp.capacityStepDiv
			if c.capacity-sub < pp.minCap {
				sub = c.capacity - pp.minCap
			}
			pp.setCapacity(c, c.capacity-sub)
			pp.activeQueue.Push(c)
		}
		return pp.activeCap > pp.maxCap || pp.activeCount > pp.maxCount
	})
	return invertPriority(maxActivePriority)
}

// finalizeChanges either commits or reverts temporary changes. The necessary capacity
// field and according flag updates are not performed here but returned in a list because
// they should be performed while the mutex is not held.
func (pp *PriorityPool) finalizeChanges(commit bool) (updates []capUpdate) {
	for _, c := range pp.changed {
		// always remove and push back in order to update biased/forced priority
		pp.activeQueue.Remove(c.activeIndex)
		pp.inactiveQueue.Remove(c.inactiveIndex)
		c.bias = 0
		c.forced = false
		c.changed = false
		if !commit {
			pp.setCapacity(c, c.origCap)
		}
		if c.connected {
			if c.capacity != 0 {
				pp.activeQueue.Push(c)
			} else {
				pp.inactiveQueue.Push(c, pp.inactivePriority(c))
			}
			if c.capacity != c.origCap && commit {
				updates = append(updates, capUpdate{c.node, c.origCap, c.capacity})
			}
		}
		c.origCap = 0
	}
	pp.changed = nil
	return
}

// capUpdate describes a CapacityField and ActiveFlag/InactiveFlag update
type capUpdate struct {
	node           *enode.Node
	oldCap, newCap uint64
}

// updateFlags performs CapacityField and ActiveFlag/InactiveFlag updates while the
// pool mutex is not held
// Note: this function should run inside a NodeStateMachine operation
func (pp *PriorityPool) updateFlags(updates []capUpdate) {
	for _, f := range updates {
		if f.oldCap == 0 {
			pp.ns.SetStateSub(f.node, pp.ActiveFlag, pp.InactiveFlag, 0)
		}
		if f.newCap == 0 {
			pp.ns.SetStateSub(f.node, pp.InactiveFlag, pp.ActiveFlag, 0)
			pp.ns.SetFieldSub(f.node, pp.CapacityField, nil)
		} else {
			pp.ns.SetFieldSub(f.node, pp.CapacityField, f.newCap)
		}
	}
}

// tryActivate tries to activate inactive nodes if possible
func (pp *PriorityPool) tryActivate() []capUpdate {
	var commit bool
	for pp.inactiveQueue.Size() > 0 {
		c := pp.inactiveQueue.PopItem().(*ppNodeInfo)
		pp.markForChange(c)
		pp.setCapacity(c, pp.minCap)
		c.bias = pp.activeBias
		pp.activeQueue.Push(c)
		pp.enforceLimits()
		if c.capacity > 0 {
			commit = true
		} else {
			break
		}
	}
	return pp.finalizeChanges(commit)
}

// updatePriority gets the current priority value of the given node from the nodePriority
// interface and performs the necessary changes. It is triggered by updateFlag.
// Note: this function should run inside a NodeStateMachine operation
func (pp *PriorityPool) updatePriority(node *enode.Node) {
	pp.lock.Lock()
	pp.activeQueue.Refresh()
	var updates []capUpdate
	defer func() {
		pp.lock.Unlock()
		pp.updateFlags(updates)
	}()

	c, _ := pp.ns.GetField(node, pp.ppNodeInfoField).(*ppNodeInfo)
	if c == nil || !c.connected {
		return
	}
	pp.activeQueue.Remove(c.activeIndex)
	pp.inactiveQueue.Remove(c.inactiveIndex)
	if c.capacity != 0 {
		pp.activeQueue.Push(c)
	} else {
		pp.inactiveQueue.Push(c, pp.inactivePriority(c))
	}
	updates = pp.tryActivate()
}

// CapacityCurve is a snapshot of the priority pool contents in a format that can efficiently
// estimate how much capacity could be granted to a given node at any priority level.
type CapacityCurve struct {
	points           []curveSection      // curve points sorted in descending order of relative priority
	ids              []enode.ID          // node IDs belonging to each curve point (only used during sorting)
	index            map[enode.ID][2]int // curve points belonging to each node (second one is -1 if not used)
	maxCount, maxCap uint64
	exclude          [2]int // curve points of excluded node (-1 if not used)
}

type curvePoint struct {
	relPri int64 // relative priority of the curve point
	// the fields below apply if the relative priority threshold is between the current and the next curve point
	sumRawPri  int64  // total raw priority of nodes with reduced capacity at threshold priority
	capOverPri uint64 // total capacity of untouched nodes with a relative priority over the threshold
	count      uint64 // number of active nodes
}

func (pp *PriorityPool) forceCurveUpdate() {
	pp.ccLock.Lock()
	pp.ccUpdateForced = true
	pp.ccLock.Unlock()
}

func (pp *PriorityPool) GetCapacityCurve() *CapacityCurve {
	pp.ccLock.Lock()
	defer pp.ccLock.Unlock()

	now := pp.clock.Now()
	dt := time.Duration(now - pp.ccUpdatedAt)
	if !pp.ccUpdateForced && pp.cachedCurve != nil && dt < time.Second*10 {
		return pp.cachedCurve
	}

	pp.ccUpdateForced = false
	pp.ccUpdatedAt = now
	curve := &CapacityCurve{
		index:    make(map[enode.ID][2]int),
		maxCount: pp.maxCount,
		maxCap:   pp.maxCap,
		exclude:  [2]int{-1, -1},
	}
	pp.cachedCurve = curve

	pp.ns.Operation(func() {
		pp.ns.ForEach(nodestate.Flags{}, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
			if capacity, _ := f.ns.GetField(node, pp.CapacityField).(uint64); capacity > 0 {
				balance := f.ns.GetField(node, pp.priorityField).(*lps.NodeBalance)
				rawPriority := balance.Priority(now)
				relPriority, minCapPriority := rawPriority/int64(capacity), rawPriority/int64(f.minCap)
				id := node.ID()
				curve.index[id] = [2]int{-1, -1}
				curve.ids = append(curve.ids, id)
				if relPriority == minCapPriority || relPriority <= 0 {
					curve.points = append(curve.points, capacityCurvePoint{
						priority:   relPriority,
						capOverPri: capacity,
						count:      1,
					})
				} else {
					curve.points = append(curve.points, capacityCurvePoint{
						priority:  minCapPriority,
						sumRawPri: rawPriority,
						count:     1,
					})
					curve.points = append(curve.points, capacityCurvePoint{
						priority:   relPriority,
						capOverPri: capacity,
						sumRawPri:  -rawPriority,
					})
					curve.ids = append(curve.ids, id)
				}
			}
		})
	})
	sort.Sort(curve)
	for i := range curve.points {
		index := curve.index[curve.ids[i]]
		if index[0] == -1 {
			index[0] = i
		} else {
			index[1] = i
		}
		curve.index[curve.ids[i]] = index
		if i > 0 {
			curve.points[i].add(curve.points[i-1])
		}
	}
	return curve
}

func (cp *curvePoint) add(cp2 curvePoint) {
	cp.capOverPri += cp2.capOverPri
	cp.sumRawPri += cp2.sumRawPri
	cp.count += cp2.count

}

func (cp *curvePoint) sub(cp2 curvePoint) {
	cp.capOverPri -= cp2.capOverPri
	cp.sumRawPri -= cp2.sumRawPri
	cp.count -= cp2.count
}

func (cp *curvePoint) usedCap(relPri int64) uint64 {
	if relPri < 1 {
		return math.MaxUint64
	}
	return cp.capOverPri + uint64(cp.sumRawPri/relPri)
}

func (cc *CapacityCurve) Len() int {
	return len(cc.points)
}

func (cc *CapacityCurve) Less(i, j int) bool {
	return cc.points[i].relPri > cc.points[j].relPri
}

func (cc *CapacityCurve) Swap(i, j int) {
	cc.points[i], cc.points[j] = cc.points[j], cc.points[i]
	cc.ids[i], cc.ids[j] = cc.ids[j], cc.ids[i]
}

func (cc *CapacityCurve) Exclude(id enode.ID) *CapacityCurve {
	if exclude, ok := cc.index[id]; ok {
		return &CapacityCurve{
			points:  cc.points,
			index:   cc.index,
			exclude: exclude,
		}
	}
	return cc
}

func (cc *CapacityCurve) getPoint(i int) curvePoint {
	c := cc.points[i]
	for _, ei := range cc.exclude {
		if ei == -1 || ei > i {
			break
		}
		ex := cc.points[ei]
		if ei > 0 {
			ex.sub(cc.points[ei-1])
		}
		c.sub(ex)
	}
	return c
}

func (cc *CapacityCurve) MaxCapacity(relPriority func(cap uint64) int64) uint64 {
	//TODO empty curve check
	min, max := -1, len(cc.points)-1

	for min < max {
		mid := (min + max + 1) / 2
		//??freeCap mezo, ami a pontra vonatkozik
		usedCap := cc.points[mid].usedCap(cc.points[mid].relPri)
		var freeCap uint64
		if cc.maxCap > usedCap {
			freeCap = cc.maxCap - usedCap
		}
		if freeCap < f.minCap || cc.points[mid].count >= cc.maxCount || relPriority(freeCap) > cc.points[mid].relPri {
			max = mid - 1
		} else {
			min = mid
		}
	}
	if min == -1 {
		if relPriority(f.capLimit) > cc.points[0].relPri {
			return f.capLimit
		} else {
			return 0
		}
	}
	c := cc.points[min]
	//TODO interval search between minCap and c.freeCap, return result
}
