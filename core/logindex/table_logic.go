// Copyright 2026 The go-ethereum Authors
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

package logindex

import (
	"math"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type mergeControl struct {
	threadCount       int
	params            *Params
	storage           *tableStorage
	getMemFileTotal   func() uint64
	deleteTable       func(id tableID) error
	mergeTable        func(id tableID, stop func() bool) error
	requestBlockTable func(blockNumber uint64) (common.RangeSet[uint64], bool)

	suspended                        bool
	headBlock, tailBlock, finalBlock uint64
	expectedBlockTables              common.RangeSet[uint64]
	lowLevelThreadCount              int
	shutdownCh                       chan struct{}
	threads                          []mergeThread
	stoppedTaskCh                    chan stoppedTask
	triggerCh                        chan chainUpdate
	mergeWg                          sync.WaitGroup
}

type chainUpdate struct {
	headBlock, tailBlock, finalBlock uint64
}

func newMergeControl(
	threadCount int,
	storage *tableStorage,
	getMemFileTotal func() uint64,
	deleteTable func(id tableID) error,
	mergeTable func(id tableID, stop func() bool) error,
	requestBlockTable func(blockNumber uint64) (common.RangeSet[uint64], bool),
) *mergeControl {
	return &mergeControl{
		threadCount:       threadCount,
		params:            storage.params,
		storage:           storage,
		getMemFileTotal:   getMemFileTotal,
		deleteTable:       deleteTable,
		mergeTable:        mergeTable,
		requestBlockTable: requestBlockTable,

		shutdownCh:    make(chan struct{}),
		stoppedTaskCh: make(chan stoppedTask, threadCount),
		triggerCh:     make(chan chainUpdate, 1),
		threads:       make([]mergeThread, threadCount),
	}
}

func (mc *mergeControl) start() {
	mc.mergeWg.Add(mc.threadCount + 1)
	for i := range mc.threads {
		mc.threads[i].startTaskCh = make(chan tableOperation, 1)
		mc.threads[i].suspendCh = make(chan bool, 1)
		go mc.threadLoop(i)
	}
	go mc.controlLoop()
}

func (mc *mergeControl) stop() {
	close(mc.shutdownCh)
	mc.mergeWg.Wait()
}

type mergeThread struct {
	currentOp, lastOp tableOperation
	startTaskCh       chan tableOperation
	suspendCh         chan bool
}

type stoppedTask struct {
	threadIndex int
	op          tableOperation
}

func (mt *mergeThread) canStartTask() bool {
	return mt.lastOp == (tableOperation{})
}

func (mt *mergeThread) runningTask(op tableOperation) bool {
	return mt.currentOp == op || mt.lastOp == op
}

func (mt *mergeThread) startTask(op tableOperation) {
	select {
	case removed := <-mt.startTaskCh:
		if removed != mt.currentOp {
			panic("unknown tableOperation removed")
		}
		mt.currentOp = tableOperation{}
	default:
	}
	mt.startTaskCh <- op
	mt.lastOp, mt.currentOp = mt.currentOp, op
}

func (mt *mergeThread) stoppedTask(op tableOperation) {
	switch op {
	case mt.lastOp:
		mt.lastOp = tableOperation{}
	case mt.currentOp:
		if mt.lastOp != (tableOperation{}) {
			panic("tableOperation stopped out of order")
		}
		mt.currentOp = tableOperation{}
	default:
		panic("unknown tableOperation stopped")
	}
}

func (mt *mergeThread) suspend(active bool) {
	select {
	case <-mt.suspendCh:
	default:
	}
	mt.suspendCh <- active
}

func (mc *mergeControl) update() {
	completeSet, partialSet, initPhase := mc.storage.tables()
	if initPhase {
		panic("merge control loop started before tableStorage initialized")
	}
	// update thread count assigned to merging low level tables
	memFileTotal := mc.getMemFileTotal()
	tableCountTotal := completeSet.count() + partialSet.count()
	t := int((max(min(memFileTotal, memFileHighThreshold), memFileLowThreshold) - memFileLowThreshold) * uint64(mc.threadCount+1) / (memFileHighThreshold - memFileLowThreshold))
	t2 := int((max(min(tableCountTotal, tableCountHighThreshold), tableCountLowThreshold) - tableCountLowThreshold) * uint64(mc.threadCount+1) / (tableCountHighThreshold - tableCountLowThreshold))
	t = max(t, t2)
	mc.lowLevelThreadCount = min(max(mc.lowLevelThreadCount+1, t)-1, t)
	// determine priority of available operations
	targetSet := mc.makeTargetSet(completeSet)
	tableOps, requiredBlockTables := mc.params.nextTableOperations(completeSet, partialSet, targetSet, mc.lowLevelThreadCount, mc.threadCount)
	//fmt.Println("updateTableOperations   complete", completeSet.count(), "partial", partialSet.count(), "target", targetSet.count())
	suspendDelivery := (memFileTotal >= memFileSuspendThreshold || tableCountTotal >= tableCountSuspendThreshold) && len(tableOps) != 0
	if !suspendDelivery {
		// request more blocks for indexing
		request := requiredBlockTables.Difference(mc.expectedBlockTables)
		for !request.IsEmpty() {
			var sent bool
			mc.expectedBlockTables, sent = mc.requestBlockTable(request.Last())
			if !sent {
				break
			}
			//fmt.Println("requesting table block", request.Last())
			requested := common.SingleRangeSet[uint64](common.NewRange[uint64](request.Last(), 1))
			request = request.Difference(requested)
		}
	}
	// start table operations
	threadPriority := make([]int, mc.threadCount) // higher is better; 0 means cannot start new operation
	for i, mt := range mc.threads {
		if mt.canStartTask() {
			if mt.currentOp == (tableOperation{}) {
				threadPriority[i] = math.MaxInt // best case: thread is idle
				continue
			}
			threadPriority[i] = math.MaxInt - 1 // second best case: thread is running something that is not on the current priority list
		innerLoop:
			for j, op := range tableOps { // iterate in order of decreasing priority
				if op == mt.currentOp {
					threadPriority[i] = j // stop lowest priority task if necessary
					break innerLoop
				}
			}
		}
	}
	for _, op := range tableOps { // iterate in order of decreasing priority
		var bestThread, bestPri int
	innerLoop2:
		for i, mt := range mc.threads {
			if mt.runningTask(op) {
				bestPri = 0 // already running, do not start anywhere
				break innerLoop2
			}
			if pri := threadPriority[i]; pri > bestPri {
				bestThread, bestPri = i, pri
			}
		}
		if bestPri > 0 {
			mc.threads[bestThread].startTask(op)
			threadPriority[bestThread] = 0
		}
	}

}
func (mc *mergeControl) makeTargetSet(complete tableSet) tableSet {
	if mc.headBlock == 0 {
		return mc.params.newTableSet()
	}
	rangeStart := mc.tailBlock
	maxDelay := mc.params.tableLevels[len(mc.params.protocolLevels)-1].blockCount // highest in-protocol table size
	rangeEnd := max(rangeStart+maxDelay, mc.finalBlock+maxDelay, mc.headBlock) - maxDelay
	//fmt.Println("makeTargetSet  headBlock", headBlock, "rangeStart", rangeStart, "rangeEnd", rangeEnd)
	target := mc.params.rangeTarget(complete, common.SingleRangeSet[uint64](common.NewRange[uint64](rangeStart, rangeEnd+1-rangeStart)))
	//fmt.Println(" rangeTarget", target)
	for i, pl := range mc.params.protocolLevels {
		first := (max(mc.headBlock, pl.tailAge) - pl.tailAge) / mc.params.tableLevels[i].blockCount
		afterLast := (max(mc.headBlock+1, pl.headAge) - pl.headAge) / mc.params.tableLevels[i].blockCount
		//fmt.Println(" protocolLevel", i, first, afterLast)
		target[i] = target[i].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](first, afterLast-first)))
	}
	//fmt.Println(" finalTarget", target)
	return target
}

func (mc *mergeControl) trigger(headBlock, tailBlock, finalBlock uint64) {
	select {
	case <-mc.triggerCh:
	default:
	}
	mc.triggerCh <- chainUpdate{headBlock: headBlock, tailBlock: tailBlock, finalBlock: finalBlock}
}

func (mc *mergeControl) suspend(suspend bool) {
	if suspend == mc.suspended {
		return
	}
	mc.suspended = suspend
	for _, mt := range mc.threads {
		mt.suspend(suspend)
	}
}

func (mc *mergeControl) controlLoop() {
	defer mc.mergeWg.Done()

	for {
		select {
		case st := <-mc.stoppedTaskCh:
			mc.threads[st.threadIndex].stoppedTask(st.op)
		case c := <-mc.triggerCh:
			mc.headBlock, mc.tailBlock, mc.finalBlock = c.headBlock, c.tailBlock, c.finalBlock
		case <-mc.shutdownCh:
			return
		}
		mc.update()
	}
}

func (mc *mergeControl) threadLoop(threadIndex int) {
	defer mc.mergeWg.Done()

	mt := &mc.threads[threadIndex]
	var startOp tableOperation
	for {
		for startOp == (tableOperation{}) {
			select {
			case startOp = <-mt.startTaskCh:
			case suspend := <-mt.suspendCh:
				if suspend {
				suspendLoop:
					for {
						select {
						case suspend := <-mt.suspendCh:
							if !suspend {
								break suspendLoop
							}
						case <-mc.shutdownCh:
							return
						}
					}
				}
			case <-mc.shutdownCh:
				return
			}
		}
		currentOp := startOp
		startOp = tableOperation{}
		switch currentOp.operation {
		case opDelete:
			if err := mc.deleteTable(currentOp.id); err != nil {
				r := mc.params.blockRange(currentOp.id)
				log.Error("Failed to delete index table", "start", r.First(), "count", r.Count(), "error", err)
			}
		case opMerge:
			err := mc.mergeTable(currentOp.id, func() bool {
				for {
					select {
					case startOp = <-mt.startTaskCh:
						return true
					case suspend := <-mt.suspendCh:
						if suspend {
						suspendLoop2:
							for {
								select {
								case suspend := <-mt.suspendCh:
									if !suspend {
										break suspendLoop2
									}
								case <-mc.shutdownCh:
									return true
								}
							}
						}
					case <-mc.shutdownCh:
						return true
					default:
						return false
					}
				}
			})
			if err != nil {
				r := mc.params.blockRange(currentOp.id)
				log.Error("Failed to merge index table", "start", r.First(), "count", r.Count(), "error", err)
			}
		}
		mc.stoppedTaskCh <- stoppedTask{threadIndex: threadIndex, op: currentOp}
	}
}

type tableSet []common.RangeSet[uint64]

type tableLevel struct {
	blockCount  uint64
	leanStorage bool //TODO
}

type protocolLevel struct {
	tailAge, headAge uint64
}

type tableID struct {
	level int
	index uint64
}

func (p *Params) newTableSet() tableSet {
	return make(tableSet, len(p.tableLevels))
}

func (ts tableSet) add(id tableID) {
	ts[id.level] = ts[id.level].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](id.index, 1)))
}

func (ts tableSet) remove(id tableID) {
	ts[id.level] = ts[id.level].Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](id.index, 1)))
}

func (ts tableSet) includes(id tableID) bool {
	return ts[id.level].Includes(id.index)
}

func (ts tableSet) isEmpty() bool {
	for _, rs := range ts {
		if !rs.IsEmpty() {
			return false
		}
	}
	return true
}

func (ts tableSet) count() uint64 {
	var count uint64
	for _, rs := range ts {
		count += rs.Count()
	}
	return count
}

func (p *Params) blockRange(id tableID) common.Range[uint64] {
	return common.NewRange[uint64](p.tableLevels[id.level].blockCount*id.index, p.tableLevels[id.level].blockCount)
}

func (p *Params) rangeID(r common.Range[uint64]) (tableID, bool) {
	for i, tl := range p.tableLevels {
		if tl.blockCount == r.Count() {
			if r.First()%tl.blockCount != 0 {
				return tableID{}, false
			}
			return tableID{level: i, index: r.First() / tl.blockCount}, true
		}
	}
	return tableID{}, false
}

const (
	opNone = iota
	opMerge
	opDelete
)

type tableOperation struct {
	operation int
	id        tableID
}

func (p *Params) compareOps(a, b tableOperation, lowLevelFirst bool) int {
	switch {
	case a.operation > b.operation:
		return 1
	case a.operation < b.operation:
		return -1
	}
	if lowLevelFirst {
		switch {
		case a.id.level < b.id.level:
			return 1
		case a.id.level > b.id.level:
			return -1
		}
	}
	aa := p.blockRange(a.id).First()
	bb := p.blockRange(b.id).First()
	switch {
	case aa > bb:
		return 1
	case aa < bb:
		return -1
	}
	if !lowLevelFirst {
		switch {
		case a.id.level < b.id.level:
			return 1
		case a.id.level > b.id.level:
			return -1
		}
	}
	return 0
}

func (p *Params) addToOps(ops *[]tableOperation, newOp tableOperation, maxThreads int, lowLevelFirst bool) bool {
	pos := len(*ops)
loop:
	for i, op := range *ops {
		switch p.compareOps(newOp, op, lowLevelFirst) {
		case 0:
			return true
		case 1:
			pos = i
			break loop
		}
	}
	if pos >= maxThreads {
		return false
	}
	if len(*ops) < maxThreads {
		*ops = append(*ops, tableOperation{})
	}
	copy((*ops)[pos+1:len(*ops)], (*ops)[pos:len(*ops)-1])
	(*ops)[pos] = newOp
	return true
}

func (p *Params) nextTableOperations(complete, partial, target tableSet, lowLevelMergeThreadCount, mergeThreadCount int) ([]tableOperation, common.RangeSet[uint64]) {
	var (
		bestLowLevelOps, bestOps []tableOperation
		required                 common.RangeSet[uint64]
	)
	for i := len(p.tableLevels) - 1; i >= 0; i-- {
		required = required.Union(target[i])
		//fmt.Println("level", i, "complete", complete[i], "partial", partial[i], "target", target[i], "required", required)
		if remove := complete[i].Union(partial[i]).Difference(required); !remove.IsEmpty() {
			//fmt.Println(" remove", remove)
			// Note that we deliberately only add one delete operation candidate
			// per level in order to avoid delete operations always interrupting
			// all merge operations
			op := tableOperation{
				operation: opDelete,
				id: tableID{
					level: i,
					index: remove.Last(),
				},
			}
			p.addToOps(&bestLowLevelOps, op, lowLevelMergeThreadCount, true)
			p.addToOps(&bestOps, op, mergeThreadCount, false)
		}
		required = required.Difference(complete[i])
		if i > 0 {
			merge := required.Intersection(shiftRangeSetLevel(complete[i-1], p.tableLevels[i-1], p.tableLevels[i], false))
			for !merge.IsEmpty() {
				//fmt.Println(" merge", merge)
				op := tableOperation{
					operation: opMerge,
					id: tableID{
						level: i,
						index: merge.Last(),
					},
				}
				add1 := p.addToOps(&bestLowLevelOps, op, lowLevelMergeThreadCount, true)
				add2 := p.addToOps(&bestOps, op, mergeThreadCount, false)
				if !add1 && !add2 {
					break
				}
				merge = merge.Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](merge.Last(), 1)))
			}
			required = shiftRangeSetLevel(required /*.Difference(merge)*/, p.tableLevels[i], p.tableLevels[i-1], false)
		}
	}
	//fmt.Println("nextTableOperations  best", bestOps, "bestLL", bestLowLevelOps, "required blocks", required)
	ops := bestLowLevelOps
	for _, op := range bestOps {
		if !p.addToOps(&ops, op, mergeThreadCount, true) {
			break
		}
	}
	//fmt.Println(" final", ops)
	return ops, required
}

// TODO partial helyett vmi jobb nev
func shiftRangeSetLevel(rs common.RangeSet[uint64], from, to tableLevel, partial bool) (result common.RangeSet[uint64]) {
	for _, r := range rs {
		rr := shiftRangeLevel(r, from, to, partial)
		if !rr.IsEmpty() {
			result = result.Union(common.SingleRangeSet[uint64](rr))
		}
	}
	return result
}

func shiftRangeLevel(r common.Range[uint64], from, to tableLevel, partial bool) common.Range[uint64] {
	first := r.First() * from.blockCount
	if !partial {
		first += to.blockCount - 1
	}
	first /= to.blockCount
	afterLast := r.AfterLast() * from.blockCount
	if partial {
		afterLast += to.blockCount - 1
	}
	afterLast /= to.blockCount
	if afterLast <= first {
		return common.Range[uint64]{}
	}
	return common.NewRange[uint64](first, afterLast-first)
}

func (p *Params) rangeTarget(avail tableSet, blockRange common.RangeSet[uint64]) tableSet {
	target := p.newTableSet()
	for i := len(p.tableLevels) - 1; i >= 0; i-- {
		fullTables := shiftRangeSetLevel(blockRange, p.tableLevels[0], p.tableLevels[i], false)
		partialTables := shiftRangeSetLevel(blockRange, p.tableLevels[0], p.tableLevels[i], true)
		target[i] = fullTables.Union(partialTables.Intersection(avail[i]))
		blockRange = blockRange.Difference(shiftRangeSetLevel(target[i], p.tableLevels[i], p.tableLevels[0], false))
	}
	return target
}
