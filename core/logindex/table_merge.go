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
	//"fmt"
	//"time"

	"github.com/ethereum/go-ethereum/common"
	//"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
)

func (ix *Indexer) mergeLoop(threadIndex int) {
	defer ix.mergeWg.Done()

	for {
		ix.lock.Lock()
		currentOp, shutdown := ix.currentOps[threadIndex], ix.shutdown
		ix.lock.Unlock()
		for !shutdown && currentOp.operation == opNone {
			<-ix.updateMergeCh[threadIndex]
			ix.lock.Lock()
			currentOp, shutdown = ix.currentOps[threadIndex], ix.shutdown
			ix.lock.Unlock()
		}
		if shutdown {
			return
		}
		switch currentOp.operation {
		case opDelete:
			if err := ix.storage.deleteTable(currentOp.id); err != nil {
				r := ix.params.blockRange(currentOp.id)
				log.Error("Failed to delete index table", "start", r.First(), "count", r.Count(), "error", err)
			}
			ix.lock.Lock()
			ix.updateTableOperations()
			ix.lock.Unlock()
		case opMerge:
			done, err := ix.mergeTable(currentOp.id, func() bool {
				select {
				case <-ix.updateMergeCh[threadIndex]:
					return true
				default:
					return false
				}
			})
			if err != nil {
				r := ix.params.blockRange(currentOp.id)
				log.Error("Failed to merge index table", "start", r.First(), "count", r.Count(), "error", err)
			}
			if done {
				ix.lock.Lock()
				ix.updateTableOperations()
				ix.lock.Unlock()
			}
		}
	}
}

func (ix *Indexer) mergeTable(id tableID, stopFn func() bool) (finalized bool, finalErr error) {
	//fmt.Println("mergeTable", id)
	if id.level == 0 {
		panic("cannot merge table on the lowest level")
	}
	sourceIndices := shiftRangeLevel(common.NewRange[uint64](id.index, 1), ix.params.tableLevels[id.level], ix.params.tableLevels[id.level-1], false)
	readers := make([]*tableReader, sourceIndices.Count())
	var entryCount uint64
	for i := range readers {
		tr, err := ix.storage.getTableReader(tableID{level: id.level - 1, index: sourceIndices.First() + uint64(i)})
		if err != nil {
			return false, err
		}
		entryCount += tr.entryCount
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
		tw.setMeta(tableMeta{
			LastBlockNumber: r.Last(),
			BlockCount:      r.Count(),
			LastBlockHash:   readers[len(readers)-1].meta.LastBlockHash,
			ParentHash:      readers[0].meta.ParentHash,
		})
	}
	if err != nil {
		return false, err
	}
	defer func() {
		if !finalized {
			err := ix.storage.releaseTableWriter(id)
			if finalErr == nil {
				finalErr = err
			}
		}
		/*if id.level > 4 {
			switch {
			case finalized:
				fmt.Println("*** Finished merge", id)
			case err != nil:
				fmt.Println("*** Failed merge", id, finalErr)
			default:
				fmt.Println("*** Suspending merge", id)
			}
			ix.mergeStatTime += mclock.Now()
			fmt.Println(" dt", time.Duration(ix.mergeStatTime), "entries", ix.mergeStatCount, "entries/sec", ix.mergeStatCount*1000000000/max(1, uint64(ix.mergeStatTime)))
		}*/
	}()
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
		nextReadEntry := make([]*indexEntry, len(readers))
		if nextEntry != 0 {
			// find next read position in each source reader
			if lastEntry == nil {
				return false, errors.New("last entry not found")
			}
			var checkNextEntry uint64
			for i, tr := range readers {
				pos, found, err := tr.seekEntry(lastEntry)
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
			if nextReadPosition[i] < tr.entryCount {
				var err error
				nextReadEntry[i], err = tr.getEntry(nextReadPosition[i])
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
				bestEntry *indexEntry
			)
			for i, entry := range nextReadEntry {
				if entry != nil && (bestEntry == nil || entry.compare(bestEntry) < 0) {
					bestIndex, bestEntry = i, entry
				}
			}
			if err := tw.addEntry(bestEntry); err != nil {
				return false, err
			}
			ix.mergeStatCount++
			nextEntry++
			nextReadPosition[bestIndex]++
			if nextReadPosition[bestIndex] < readers[bestIndex].entryCount {
				var err error
				nextReadEntry[bestIndex], err = readers[bestIndex].getEntry(nextReadPosition[bestIndex])
				if err != nil {
					return false, err
				}
			} else {
				nextReadEntry[bestIndex] = nil
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
