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
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const (
	cachedRevertPoints = 8                // revert points for most recent blocks in memory
	logFrequency       = time.Second * 20 // log info frequency during long indexing/unindexing process
	headLogDelay       = time.Second      // head indexing log info delay (do not log if finished faster)
)

// updateLoop initializes and updates the log index structure according to the
// canonical chain.
func (f *FilterMaps) indexerLoop() {
	defer f.closeWg.Done()

	if f.noHistory {
		f.reset()
		return
	}
	log.Info("Started log indexer")

	for !f.stop {
		if !f.initialized {
			if err := f.init(); err != nil {
				log.Error("Error initializing log index", "error", err)
				f.waitForEvent()
				continue
			}
		}
		if !f.targetHeadIndexed() {
			if !f.tryIndexHead() {
				f.waitForEvent()
			}
		} else {
			if f.tryIndexTail() && f.tryUnindexTail() {
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

func (f *FilterMaps) tryIndexHead() bool {
	if f.targetView == nil {
		return false
	}
	headRenderer, err := f.renderMapsBefore(math.MaxUint32)
	if err != nil {
		log.Error("Error creating log index head renderer", "error", err)
		return false
	}
	if headRenderer == nil {
		return true
	}
	if !f.startedHeadIndex {
		f.lastLogHeadIndex = time.Now()
		f.startedHeadIndexAt = f.lastLogHeadIndex
		f.startedHeadIndex = true
		f.ptrHeadIndex = f.afterLastIndexedBlock
	}
	if _, err := headRenderer.renderMaps(func() bool {
		f.processEvents()
		if f.hasIndexedBlocks() && (time.Since(f.lastLogHeadIndex) > logFrequency ||
			(!f.loggedHeadIndex && time.Since(f.startedHeadIndexAt) > headLogDelay)) {
			log.Info("Log index head rendering in progress",
				"first block", f.firstIndexedBlock, "last block", f.afterLastIndexedBlock-1,
				"processed", f.afterLastIndexedBlock-f.ptrHeadIndex,
				"remaining", f.targetBlockNumber+1-f.afterLastIndexedBlock,
				"elapsed", common.PrettyDuration(time.Since(f.startedHeadIndexAt)))
			f.loggedHeadIndex = true
			f.lastLogHeadIndex = time.Now()
		}
		f.tryUnindexTail()
		return f.stop
	}); err != nil {
		log.Error("Log index head rendering failed", "error", err)
		return false
	}
	if f.loggedHeadIndex {
		log.Info("Log index head rendering finished",
			"first block", f.firstIndexedBlock, "last block", f.afterLastIndexedBlock-1,
			"processed", f.afterLastIndexedBlock-f.ptrHeadIndex,
			"elapsed", common.PrettyDuration(time.Since(f.startedHeadIndexAt)))
		f.loggedHeadIndex, f.startedHeadIndex = false, false
	}
	return true
}

func (f *FilterMaps) tryIndexTail() bool {
	for firstEpoch := f.firstRenderedMap >> f.logMapsPerEpoch; firstEpoch > 0 && f.needTailEpoch(firstEpoch-1); {
		f.processEvents()
		if f.stop || !f.targetHeadIndexed() {
			return false
		}
		// resume process if tail rendering was interrupted because of head rendering
		tailRenderer := f.tailRenderer
		f.tailRenderer = nil
		if tailRenderer != nil && tailRenderer.afterLastMap != f.firstRenderedMap {
			tailRenderer = nil
		}
		if tailRenderer == nil {
			var err error
			tailRenderer, err = f.renderMapsBefore(f.firstRenderedMap)
			if err != nil {
				log.Error("Error creating log index tail renderer", "error", err)
				return false
			}
		}
		if tailRenderer == nil {
			return true
		}
		if !f.startedTailIndex {
			f.lastLogTailIndex = time.Now()
			f.startedTailIndexAt = f.lastLogTailIndex
			f.startedTailIndex = true
			f.ptrTailIndex = f.firstIndexedBlock
		}
		done, err := tailRenderer.renderMaps(func() bool {
			f.processEvents()
			if f.hasIndexedBlocks() && (time.Since(f.lastLogTailIndex) > logFrequency || !f.loggedTailIndex) {
				log.Info("Log index tail rendering in progress",
					"first block", f.firstIndexedBlock, "last block", f.afterLastIndexedBlock-1,
					"processed", f.ptrTailIndex-f.firstIndexedBlock+f.tailPartialBlocks(),
					"remaining", f.firstIndexedBlock-f.tailTargetBlock(),
					"next tail epoch percentage", f.tailPartialEpoch*100/f.mapsPerEpoch,
					"elapsed", common.PrettyDuration(time.Since(f.startedTailIndexAt)))
				f.loggedTailIndex = true
				f.lastLogTailIndex = time.Now()
			}
			return f.stop || !f.targetHeadIndexed()
		})
		if err != nil {
			log.Error("Log index tail rendering failed", "error", err)
		}
		if !done {
			f.tailRenderer = tailRenderer // only keep tail renderer if interrupted by stopFn
			return false
		}
	}
	if f.loggedTailIndex {
		log.Info("Log index tail rendering finished",
			"first block", f.firstIndexedBlock, "last block", f.afterLastIndexedBlock-1,
			"processed", f.ptrTailIndex-f.firstIndexedBlock,
			"elapsed", common.PrettyDuration(time.Since(f.startedTailIndexAt)))
		f.loggedTailIndex = false
	}
	return true
}

func (f *FilterMaps) tailPartialBlocks() uint64 {
	if f.tailPartialEpoch == 0 {
		return 0
	}
	end, _, err := f.getLastBlockOfMap(f.firstRenderedMap - f.mapsPerEpoch + f.tailPartialEpoch - 1)
	if err != nil {
		log.Error("Error fetching last block of map", "mapIndex", f.firstRenderedMap-f.mapsPerEpoch+f.tailPartialEpoch-1, "error", err)
	}
	var start uint64
	if f.firstRenderedMap-f.mapsPerEpoch > 0 {
		start, _, err = f.getLastBlockOfMap(f.firstRenderedMap - f.mapsPerEpoch - 1)
		if err != nil {
			log.Error("Error fetching last block of map", "mapIndex", f.firstRenderedMap-f.mapsPerEpoch-1, "error", err)
		}
	}
	return end - start
}

func (f *FilterMaps) tryUnindexTail() bool {
	for {
		firstEpoch := (f.firstRenderedMap - f.tailPartialEpoch) >> f.logMapsPerEpoch
		if f.needTailEpoch(firstEpoch) {
			break
		}
		f.processEvents()
		if f.stop {
			return false
		}
		if !f.startedTailUnindex {
			f.startedTailUnindexAt = time.Now()
			f.startedTailUnindex = true
			f.ptrTailUnindexMap = f.firstRenderedMap - f.tailPartialEpoch
			f.ptrTailUnindexBlock = f.firstIndexedBlock
		}
		if err := f.deleteTailEpoch(firstEpoch); err != nil {
			log.Error("Log index tail epoch unindexing failed", "error", err)
			return false
		}
	}
	if f.startedTailUnindex {
		log.Info("Log index tail unindexing finished",
			"first block", f.firstIndexedBlock, "last block", f.afterLastIndexedBlock-1,
			"removed maps", f.ptrTailUnindexMap-f.firstRenderedMap,
			"removed blocks", f.ptrTailUnindexBlock-f.firstIndexedBlock,
			"elapsed", common.PrettyDuration(time.Since(f.startedTailUnindexAt)))
		f.startedTailUnindex = false
	}
	return true
}

func (f *FilterMaps) needTailEpoch(epoch uint32) bool {
	tailTarget := f.tailTargetBlock()
	if tailTarget < f.firstIndexedBlock {
		return true
	}
	tailLvIndex, err := f.getBlockLvPointer(tailTarget)
	if err != nil {
		log.Error("Could not get log value index of tail block", "error", err)
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
}

func (f *FilterMaps) targetHeadIndexed() bool {
	return equalViews(f.targetView, f.indexedView) && f.afterLastIndexedBlock == f.targetBlockNumber+1
}

func (f *FilterMaps) exportCheckpoints() {
	if f.exportFileName == "" {
		return
	}
	w, err := os.Create(f.exportFileName)
	if err != nil {
		log.Error("Error creating checkpoint export file", "name", f.exportFileName, "error", err)
		return
	}
	defer w.Close()

	epochCount := f.afterLastRenderedMap >> f.logMapsPerEpoch
	log.Info("Exporting log index checkpoints", "epochs", epochCount, "file", f.exportFileName)
	w.WriteString("\t{\n")
	for epoch := uint32(0); epoch < epochCount; epoch++ {
		lastBlock, lastBlockId, err := f.getLastBlockOfMap((epoch+1)<<f.logMapsPerEpoch - 1)
		if err != nil {
			log.Error("Error fetching last block of epoch", "epoch", epoch, "error", err)
			return
		}
		lvPtr, err := f.getBlockLvPointer(lastBlock)
		if err != nil {
			log.Error("Error fetching log value pointer of last block", "block", lastBlock, "error", err)
			return
		}
		w.WriteString(fmt.Sprintf("\t\t{%d, common.HexToHash(\"0x%064x\"), %d},\n", lastBlock, lastBlockId, lvPtr))
	}
	w.WriteString("\t},\n")
}
