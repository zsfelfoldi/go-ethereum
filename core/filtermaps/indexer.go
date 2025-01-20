// Copyright 2024 The go-ethereum Authors
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

package filtermaps

import (
	"fmt"
	"math"
	"time"

	"github.com/ethereum/go-ethereum/log"
)

const (
	cachedRevertPoints = 8               // revert points for most recent blocks in memory
	logFrequency       = time.Second * 8 // log info frequency during long indexing/unindexing process
)

// updateLoop initializes and updates the log index structure according to the
// canonical chain.
func (f *FilterMaps) indexerLoop() {
	defer f.closeWg.Done()

	if f.noHistory {
		f.reset()
		return
	}

	for !f.stop {
		fmt.Println("loop")
		if !f.initialized {
			fmt.Println("init")
			if err := f.init(); err != nil {
				log.Error("Error initializing log index", "error", err)
				fmt.Println(" fail", err)
				f.waitForEvent()
				continue
			}
			fmt.Println(" success")
		}
		if !f.targetHeadIndexed() {
			fmt.Println("tryUpdateHead")
			if !f.tryUpdateHead() {
				fmt.Println(" fail; wait")
				panic(nil) //TODO
				f.waitForEvent()
			} else {
				fmt.Println(" success")
			}
		} else {
			fmt.Println("tryUpdateTail")
			if f.tryUpdateTail() {
				fmt.Println(" success; wait")
				f.waitForEvent()
			} else {
				fmt.Println(" interrupted") //TODO keep renderer
			}
		}
	}
}

func (f *FilterMaps) SetTargetView(targetView chainView) {
	if f.noHistory {
		return
	}
	fmt.Println("SetTargetView", targetView.headNumber(), targetView.getBlockId(targetView.headNumber()))
	f.targetViewCh <- targetView
}

// WaitIdle blocks until the indexer is in an idle state while synced up to the
// latest chain head.
func (f *FilterMaps) WaitIdle() {
	if f.noHistory {
		f.closeWg.Wait()
		return
	}
	for {
		ch := make(chan bool)
		f.waitIdleCh <- ch
		if <-ch {
			return
		}
	}
}

func (f *FilterMaps) tryUpdateHead() bool {
	if f.targetView == nil {
		return false
	}
	fmt.Println(" headRenderer create")
	headRenderer, err := f.renderMapsBefore(math.MaxUint32)
	if err != nil {
		fmt.Println(" headRenderer create err", err)
		log.Error("Error creating log index head renderer", "error", err)
		return false
	}
	fmt.Println(" headRenderer created")
	if headRenderer == nil {
		return true
	}
	fmt.Println(" headRenderer start")
	if _, err := headRenderer.renderMaps(func() bool {
		fmt.Println(" headRenderer events")
		f.processEvents()
		return f.stop
	}); err != nil {
		fmt.Println(" headRenderer err", err)
		log.Error("Log index head rendering failed", "error", err)
		return false
	}
	fmt.Println(" headRenderer stop")
	return true
}

func (f *FilterMaps) tryUpdateTail() bool {
	for {
		f.processEvents()
		if f.stop || !f.targetHeadIndexed() {
			return false
		}
		firstEpoch := f.firstRenderedMap >> f.logMapsPerEpoch
		fmt.Println(" tail 1", firstEpoch)
		if firstEpoch > 0 {
			fmt.Println(" tail 2")
			if f.needTailEpoch(firstEpoch - 1) {
				fmt.Println(" tail 3")
				tailRenderer, err := f.renderMapsBefore(f.firstRenderedMap)
				if tailRenderer == nil {
					fmt.Println(" tail 4")
					return false
				}
				done, err := tailRenderer.renderMaps(func() bool {
					f.processEvents()
					return f.stop || !f.targetHeadIndexed()
				})
				if err != nil {
					log.Error("Log index tail rendering failed", "error", err)
				}
				if !done {
					fmt.Println(" tail 5")
					return false
				}
				continue
			} else if f.tailPartialEpoch > 0 {
				fmt.Println(" tail 6 del")
				if err := f.deleteTailEpoch(firstEpoch - 1); err != nil {
					log.Error("Log index partial tail epoch unindexing failed", "error", err)
					fmt.Println("t8 err", err)
					return false
				}
				continue
			}
		}
		fmt.Println(" tail 7")
		if !f.needTailEpoch(firstEpoch) {
			fmt.Println(" tail 8 del")
			if err := f.deleteTailEpoch(firstEpoch); err != nil {
				log.Error("Log index tail epoch unindexing failed", "error", err)
				fmt.Println("t10", err)
				return false
			}
			continue
		}
		return true
	}
}

func (f *FilterMaps) needTailEpoch(epoch uint32) bool {
	tailTarget := f.tailTargetBlock()
	fmt.Println("needTailEpoch", epoch, tailTarget, f.firstIndexedBlock)
	if tailTarget < f.firstIndexedBlock {
		fmt.Println(" 1 true")
		return true
	}
	tailLvIndex, err := f.getBlockLvPointer(tailTarget)
	if err != nil {
		log.Error("Could not get lv index of tail block", "error", err)
		fmt.Println(" 2 true")
		return true
	}
	fmt.Println(" 3", uint64(epoch+1)<<(f.logValuesPerMap+f.logMapsPerEpoch) >= tailLvIndex)
	return uint64(epoch+1)<<(f.logValuesPerMap+f.logMapsPerEpoch) >= tailLvIndex
}

// tailTargetBlock returns the target value for the tail block number according to the
// log history parameter and the current index head.
func (f *FilterMaps) tailTargetBlock() uint64 {
	if f.history == 0 || f.targetBlockNumber < f.history {
		return 0
	}
	return f.targetBlockNumber + 1 - f.history
}

func (f *FilterMaps) waitForEvent() {
	for f.targetHeadIndexed() {
		if f.matcherSyncRequest != nil {
			f.matcherSyncRequest.synced(f.targetBlockNumber)
			f.matcherSyncRequest = nil
		}
		select {
		case targetView := <-f.targetViewCh:
			f.setTargetView(targetView)
		case f.matcherSyncRequest = <-f.matcherSyncCh:
		case <-f.closeCh:
			f.stop = true
			return
		case ch := <-f.waitIdleCh:
			ch <- f.targetHeadIndexed()
		}
	}
}

func (f *FilterMaps) processEvents() {
	for {
		if f.matcherSyncRequest != nil && f.targetHeadIndexed() {
			f.matcherSyncRequest.synced(f.targetBlockNumber)
			f.matcherSyncRequest = nil
		}
		select {
		case targetView := <-f.targetViewCh:
			f.setTargetView(targetView)
		case f.matcherSyncRequest = <-f.matcherSyncCh:
		case <-f.closeCh:
			f.stop = true
			return
		default:
			return
		}
	}
}

func (f *FilterMaps) setTargetView(targetView chainView) {
	fmt.Println("setTargetView", targetView.headNumber(), targetView.getBlockId(targetView.headNumber()))
	if equalViews(f.targetView, targetView) {
		return
	}
	f.targetView = targetView
}

func (f *FilterMaps) targetHeadIndexed() bool {
	return equalViews(f.targetView, f.indexedView) && f.afterLastIndexedBlock == f.targetBlockNumber+1
}
