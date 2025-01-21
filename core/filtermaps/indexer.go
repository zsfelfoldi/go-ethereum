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

	log.Info("Started log indexer")
	if f.noHistory {
		f.reset()
		return
	}

	for !f.stop {
		if !f.initialized {
			if err := f.init(); err != nil {
				log.Error("Error initializing log index", "error", err)
				f.waitForEvent()
				continue
			}
		}
		if !f.targetHeadIndexed() {
			if !f.tryUpdateHead() {
				f.waitForEvent()
			}
		} else {
			if f.tryUpdateTail() {
				f.waitForEvent()
			}
		}
	}
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
	log.Info("Log index head rendering started")
	headRenderer, err := f.renderMapsBefore(math.MaxUint32)
	if err != nil {
		log.Error("Error creating log index head renderer", "error", err)
		return false
	}
	if headRenderer == nil {
		return true
	}
	if _, err := headRenderer.renderMaps(func() bool {
		f.processEvents()
		return f.stop
	}); err != nil {
		log.Error("Log index head rendering failed", "error", err)
		return false
	}
	return true
}

func (f *FilterMaps) tryUpdateTail() bool {
	for {
		f.processEvents()
		if f.stop || !f.targetHeadIndexed() {
			return false
		}
		firstEpoch := f.firstRenderedMap >> f.logMapsPerEpoch

		tailRenderer := f.tailRenderer
		f.tailRenderer = nil
		if tailRenderer != nil && tailRenderer.afterLastMap != f.firstRenderedMap {
			tailRenderer = nil
		}

		if firstEpoch > 0 {
			if f.needTailEpoch(firstEpoch - 1) {
				if tailRenderer == nil {
					var err error
					tailRenderer, err = f.renderMapsBefore(f.firstRenderedMap)
					if err != nil {
						log.Error("Error creating log index tail renderer", "error", err)
						return false
					}
				}
				if tailRenderer == nil {
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
					f.tailRenderer = tailRenderer // only keep tail renderer if interrupted by stopFn
					return false
				}
				continue
			} else if f.tailPartialEpoch > 0 {
				if err := f.deleteTailEpoch(firstEpoch - 1); err != nil {
					log.Error("Log index partial tail epoch unindexing failed", "error", err)
					return false
				}
				continue
			}
		}
		if !f.needTailEpoch(firstEpoch) {
			if err := f.deleteTailEpoch(firstEpoch); err != nil {
				log.Error("Log index tail epoch unindexing failed", "error", err)
				return false
			}
			continue
		}
		return true
	}
}

func (f *FilterMaps) needTailEpoch(epoch uint32) bool {
	tailTarget := f.tailTargetBlock()
	if tailTarget < f.firstIndexedBlock {
		return true
	}
	tailLvIndex, err := f.getBlockLvPointer(tailTarget)
	if err != nil {
		log.Error("Could not get lv index of tail block", "error", err)
		return true
	}
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

func (f *FilterMaps) processSingleEvent(blocking bool) bool {
	if f.matcherSyncRequest != nil {
		f.matcherSyncRequest.synced(f.targetBlockNumber)
		f.matcherSyncRequest = nil
	}
	if blocking {
		select {
		case targetView := <-f.TargetViewCh:
			f.setTargetView(targetView)
		case f.matcherSyncRequest = <-f.matcherSyncCh:
		case f.blockProcessing = <-f.BlockProcessingCh:
		case <-f.closeCh:
			f.stop = true
		case ch := <-f.waitIdleCh:
			ch <- !f.blockProcessing && f.targetHeadIndexed()
		}
	} else {
		select {
		case targetView := <-f.TargetViewCh:
			f.setTargetView(targetView)
		case f.matcherSyncRequest = <-f.matcherSyncCh:
		case f.blockProcessing = <-f.BlockProcessingCh:
		case <-f.closeCh:
			f.stop = true
		default:
			return false
		}
	}
	return true
}

func (f *FilterMaps) waitForEvent() {
	for !f.stop && (f.blockProcessing || f.targetHeadIndexed()) {
		f.processSingleEvent(true)
	}
}

func (f *FilterMaps) processEvents() {
	for !f.stop && f.processSingleEvent(f.blockProcessing) {
	}
}

func (f *FilterMaps) setTargetView(targetView chainView) {
	if equalViews(f.targetView, targetView) {
		return
	}
	f.targetView = targetView
	log.Info("Updated log index target head", "number", targetView.headNumber())
}

func (f *FilterMaps) targetHeadIndexed() bool {
	return equalViews(f.targetView, f.indexedView) && f.afterLastIndexedBlock == f.targetBlockNumber+1
}
