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
	"errors"
	"fmt"
	"math"

	//"time"

	"github.com/ethereum/go-ethereum/common"
	//"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
)

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

func (ix *Indexer) startTableMerge() {
	ix.mergeWg.Add(ix.mergeThreadCount + 1)
	ix.mergeThreads = make([]mergeThread, ix.mergeThreadCount)
	ix.stoppedTaskCh = make(chan stoppedTask, ix.mergeThreadCount)
	ix.mergeTriggerCh = make(chan struct{}, 1)
	ix.shutdownCh = make(chan struct{})
	for i := range ix.mergeThreads {
		ix.mergeThreads[i].startTaskCh = make(chan tableOperation, 1)
		ix.mergeThreads[i].suspendCh = make(chan bool, 1)
		go ix.mergeThreadLoop(i)
	}
	go ix.mergeControlLoop()
}

func (ix *Indexer) processInitPhase() {
	if ix.requestedInitBlock != math.MaxUint64 {
		return
	}
	reqNumber, ok := ix.storage.requestInitBlockHash()
	if !ok {
		panic("cannot initialize index table storage")
	}
	fmt.Println("requesting init block", reqNumber)
	if ix.requestBlock(reqNumber) {
		ix.requestedInitBlock = reqNumber
	}
	return
}

func (ix *Indexer) startTableOperations(completeSet, partialSet tableSet) { //TODO rename
	// update thread count assigned to merging low level tables
	memFileTotal := ix.files.getMemFileTotal()
	tableCountTotal := completeSet.count() + partialSet.count()
	t := int((max(min(memFileTotal, memFileHighThreshold), memFileLowThreshold) - memFileLowThreshold) * uint64(ix.mergeThreadCount+1) / (memFileHighThreshold - memFileLowThreshold))
	t2 := int((max(min(tableCountTotal, tableCountHighThreshold), tableCountLowThreshold) - tableCountLowThreshold) * uint64(ix.mergeThreadCount+1) / (tableCountHighThreshold - tableCountLowThreshold))
	t = max(t, t2)
	ix.lowLevelMergeThreadCount = min(max(ix.lowLevelMergeThreadCount+1, t)-1, t)
	// determine priority of available operations
	targetSet := ix.makeTargetSet(completeSet)
	tableOps, requiredBlockTables := ix.params.nextTableOperations(completeSet, partialSet, targetSet, ix.lowLevelMergeThreadCount, ix.mergeThreadCount)
	//fmt.Println("updateTableOperations   complete", completeSet.count(), "partial", partialSet.count(), "target", targetSet.count())
	ix.requiredBlockTables = requiredBlockTables
	suspendDelivery := (memFileTotal >= memFileSuspendThreshold || tableCountTotal >= tableCountSuspendThreshold) && len(tableOps) != 0
	if !suspendDelivery && ix.requestBlock != nil {
		// request more blocks for indexing
		request := ix.requiredBlockTables.Difference(ix.requestedBlockTables)
		for !request.IsEmpty() && ix.requestBlock(request.Last()) {
			//fmt.Println("requesting table block", request.Last())
			requested := common.SingleRangeSet[uint64](common.NewRange[uint64](request.Last(), 1))
			ix.requestedBlockTables = ix.requestedBlockTables.Union(requested)
			request = request.Difference(requested)
		}
	}
	// start table operations
	threadPriority := make([]int, ix.mergeThreadCount) // higher is better; 0 means cannot start new operation
	for i, mt := range ix.mergeThreads {
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
		for i, mt := range ix.mergeThreads {
			if mt.runningTask(op) {
				bestPri = 0 // already running, do not start anywhere
				break innerLoop2
			}
			if pri := threadPriority[i]; pri > bestPri {
				bestThread, bestPri = i, pri
			}
		}
		if bestPri > 0 {
			ix.mergeThreads[bestThread].startTask(op)
			threadPriority[bestThread] = 0
		}
	}
}

func (ix *Indexer) makeTargetSet(complete tableSet) tableSet {
	if ix.headBlock == 0 {
		return ix.params.newTableSet()
	}
	rangeStart := ix.tailBlock()
	maxDelay := ix.params.tableLevels[len(ix.params.protocolLevels)-1].blockCount // highest in-protocol table size
	rangeEnd := max(rangeStart+maxDelay, ix.finalBlock+maxDelay, ix.headBlock) - maxDelay
	//fmt.Println("makeTargetSet  headBlock", ix.headBlock, "rangeStart", rangeStart, "rangeEnd", rangeEnd)
	target := ix.params.rangeTarget(complete, common.SingleRangeSet[uint64](common.NewRange[uint64](rangeStart, rangeEnd+1-rangeStart)))
	//fmt.Println(" rangeTarget", target)
	for i, pl := range ix.params.protocolLevels {
		first := (max(ix.headBlock, pl.tailAge) - pl.tailAge) / ix.params.tableLevels[i].blockCount
		afterLast := (max(ix.headBlock+1, pl.headAge) - pl.headAge) / ix.params.tableLevels[i].blockCount
		//fmt.Println(" protocolLevel", i, first, afterLast)
		target[i] = target[i].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](first, afterLast-first)))
	}
	//fmt.Println(" finalTarget", target)
	return target
}

func (ix *Indexer) triggerMergeLoop() {
	select {
	case ix.mergeTriggerCh <- struct{}{}:
	default:
	}
}

// assumes ix.lock
func (ix *Indexer) suspendMerge(active bool) {
	if active == ix.mergeSuspended {
		return
	}
	ix.mergeSuspended = active
	for _, mt := range ix.mergeThreads {
		mt.suspend(active)
	}
}

func (ix *Indexer) mergeControlLoop() {
	defer ix.mergeWg.Done()

	for {
		select {
		case st := <-ix.stoppedTaskCh:
			ix.mergeThreads[st.threadIndex].stoppedTask(st.op)
		case <-ix.mergeTriggerCh:
		case <-ix.shutdownCh:
			return
		}
		ix.lock.Lock()
		completeSet, partialSet, initPhase := ix.storage.tables()
		/*fmt.Println("completeSet:", completeSet)
		fmt.Println("partialSet:", partialSet)
		fmt.Println("initPhase:", initPhase)*/
		if initPhase {
			ix.processInitPhase()
		} else {
			ix.startTableOperations(completeSet, partialSet)
		}
		ix.lock.Unlock()
	}
}

func (ix *Indexer) mergeThreadLoop(threadIndex int) {
	defer ix.mergeWg.Done()

	mt := &ix.mergeThreads[threadIndex]
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
						case <-ix.shutdownCh:
							return
						}
					}
				}
			case <-ix.shutdownCh:
				return
			}
		}
		currentOp := startOp
		startOp = tableOperation{}
		switch currentOp.operation {
		case opDelete:
			if err := ix.storage.deleteTable(currentOp.id); err != nil {
				r := ix.params.blockRange(currentOp.id)
				log.Error("Failed to delete index table", "start", r.First(), "count", r.Count(), "error", err)
			}
		case opMerge:
			_, err := ix.mergeTable(currentOp.id, func() bool {
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
								case <-ix.shutdownCh:
									return true
								}
							}
						}
					case <-ix.shutdownCh:
						return true
					default:
						return false
					}
				}
			})
			if err != nil {
				r := ix.params.blockRange(currentOp.id)
				log.Error("Failed to merge index table", "start", r.First(), "count", r.Count(), "error", err)
			}
		}
		ix.stoppedTaskCh <- stoppedTask{threadIndex: threadIndex, op: currentOp}
	}
}

func (ix *Indexer) mergeTable(id tableID, stopFn func() bool) (bool, error) {
	//fmt.Println("mergeTable", id)
	if id.level == 0 {
		panic("cannot merge table on the lowest level")
	}
	sourceIndices := shiftRangeLevel(common.NewRange[uint64](id.index, 1), ix.params.tableLevels[id.level], ix.params.tableLevels[id.level-1], false)
	readers := make([]*TableReader, sourceIndices.Count())
	var entryCount uint64
	for i := range readers {
		tr, err := ix.storage.getTableReader(tableID{level: id.level - 1, index: sourceIndices.First() + uint64(i)})
		if err != nil {
			return false, err
		}
		entryCount += tr.EntryCount
		readers[i] = tr
	}
	tw, err := ix.storage.getTableWriter(id)
	/*if err == nil && id.level > 4 {
		fmt.Println("*** Resuming merge", id)
		ix.mergeStatTime -= mclock.Now()
	}*/
	if err == errTableNotFound {
		/*if id.level > 4 {
			fmt.Println("*** Starting merge", id)
			ix.mergeStatTime = -mclock.Now()
			ix.mergeStatCount = 0
		}*/
		tw, err = ix.storage.addNewTableWriter(id, entryCount)
		if err != nil {
			return false, err
		}
		// initialize table metadata
		r := ix.params.blockRange(id)
		tw.setMeta(TableMeta{
			LastBlockNumber: r.Last(),
			BlockCount:      r.Count(),
			LastBlockHash:   readers[len(readers)-1].Meta.LastBlockHash,
			ParentHash:      readers[0].Meta.ParentHash,
		})
	}
	if err != nil {
		return false, err
	}
	if tw.entryCount != entryCount {
		return false, errors.New("entry count mismatch")
	}
	lastEntry, nextEntry := tw.lastAndNextEntry()
	if tw.getPhase() == wpWriteEntries && nextEntry != entryCount {
		if lastEntry == nil {
			//fmt.Println("lastAndNextEntry", "nil", nextEntry)
		} else {
			//fmt.Println("lastAndNextEntry", *lastEntry, nextEntry)
		}
		nextReadPosition := make([]uint64, len(readers))
		nextReadEntries := make([]IndexEntries, len(readers))
		if nextEntry != 0 {
			// find next read position in each source reader
			if lastEntry == nil {
				return false, errors.New("last entry not found")
			}
			var checkNextEntry uint64
			for i, tr := range readers {
				pos, found, err := tr.SeekEntry(lastEntry, false)
				if err != nil {
					return false, err
				}
				if found {
					pos++
				}
				nextReadPosition[i] = pos
				checkNextEntry += pos
				//fmt.Println(" reader", i, "pos", pos)
			}
			if checkNextEntry != nextEntry {
				//fmt.Println(" checkNextEntry", checkNextEntry)
				panic("xxx")
				return false, errors.New("next entry mismatch")
			}
		}
		for i, tr := range readers {
			if nextReadPosition[i] < tr.EntryCount {
				var err error
				nextReadEntries[i], err = tr.getEntries(common.NewRange[uint64](nextReadPosition[i], min(mergeEntryPrefetch, tr.EntryCount-nextReadPosition[i])))
				if err != nil {
					return false, err
				}
			}
		}
		for nextEntry < entryCount {
			if nextEntry%10000 == 0 && stopFn() {
				return false, nil
			}
			var (
				bestIndex int
				bestEntry *IndexEntry
			)
			for i, entries := range nextReadEntries {
				if len(entries) > 0 && (bestEntry == nil || entries[0].Compare(bestEntry) < 0) {
					bestIndex, bestEntry = i, &entries[0]
				}
			}
			if err := tw.addEntry(bestEntry); err != nil {
				return false, err
			}
			ix.mergeStatCount++
			nextEntry++
			nextReadPosition[bestIndex]++
			if tr, pos := readers[bestIndex], nextReadPosition[bestIndex]; pos < tr.EntryCount {
				nextReadEntries[bestIndex] = nextReadEntries[bestIndex][1:]
				if len(nextReadEntries[bestIndex]) == 0 {
					var err error
					nextReadEntries[bestIndex], err = tr.getEntries(common.NewRange[uint64](pos, min(mergeEntryPrefetch, tr.EntryCount-pos)))
					if err != nil {
						return false, err
					}
				}
			} else {
				nextReadEntries[bestIndex] = nil
			}
			//fmt.Println("merge", id, "write", nextEntry, "read", nextReadPosition, "entry", *bestEntry)
		}
	}
	for {
		if stopFn() {
			return false, nil
		}
		done, err := tw.finalize()
		if err != nil {
			return false, err
		}
		if done {
			break
		}
	}
	if tw.getPhase() != wpFinalized {
		panic("invalid table write phase")
	}
	if err := ix.storage.finalizeTableWriter(id); err != nil {
		return false, err
	}
	return true, nil
}
