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
	"errors"
	"math"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	cachedRevertPoints = 16              // revert points for most recent blocks in memory
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

	f.indexLock.Lock()
	f.updateMapCache()
	f.indexLock.Unlock()
	f.headEventCh = make(chan core.ChainEvent, 10)
	sub := f.chain.SubscribeChainEvent(headEventCh)
	defer sub.Unsubscribe()
	f.setTargetHead(f.chain.CurrentBlock())

	for !f.stop {
		if !f.targetHeadIndexed() {
			f.tryUpdateHead()
		} else {
			if f.tryUpdateTail() {
				f.waitForEvent()
			}
		}
	}
}

func (f *FilterMaps) tryUpdateHead() {
	for f.targetView == nil {
		f.waitForEvent()
		if f.stop {
			return
		}
	}
	headRenderer := f.tryMakeHeadRendererFromSnapshot()
	if headRenderer == nil {
		headRenderer = f.tryMakeHeadRendererFromMapBoundary()
	}
	if headRenderer == nil {
		if f.initialized {
			log.Warn("Could not render log index head; resetting index database")
			f.reset()
		}
		headRenderer = f.tryMakeHeadRendererFromCheckpoint()
	}
	if headRenderer == nil {
		log.Error("Could not initialize log index")
		f.waitForEvent()
		return
	}
	if _, err := headRenderer.renderMaps(func() bool {
		f.processEvents()
		return f.stop
	}); err != nil {
		log.Error("Log index head rendering failed", "error", err)
		f.waitForEvent()
	}
}

func (f *FilterMaps) tryMakeHeadRendererFromSnapshot() *mapRenderer {
	if !f.initialized {
		return nil
	}
	commonAncestor := f.targetView.commonAncestor(f.indexedView)
	if cp := f.renderSnapshots[commonAncestor]; cp != nil {
		if r, err := f.renderMapsFromSnapshot(cp); err == nil {
			return r
		} else {
			log.Error("Error initializing head map renderer at checkpoint", "commonAncestor", commonAncestor, "error", err)
		}
	}
	return nil
}

func (f *FilterMaps) tryMakeHeadRendererFromMapBoundary() *mapRenderer {
	if !f.initialized {
		return nil
	}
	var firstMap uint32 // first map to be rendered or re-rendered
	commonAncestor := f.targetView.commonAncestor(f.indexedView)
	if commonAncestor > f.lastIndexedBlock {
		// partially rendered block is unchanged, rendering can continue from next unrendered map
		firstMap = f.lastRenderedMap + 1
	} else if commonAncestor == f.lastIndexedBlock {
		// last fully rendered map is unchanged but the rest of the last rendered map is changed
		firstMap = f.lastRenderedMap
	} else {
		lvPtr, err := f.getBlockLvPointer(commonAncestor + 1)
		if err != nil {
			log.Error("Error retrieving blockLvPtr after common ancestor ", "blockNumber", commonAncestor+1, "error", err)
			return nil
		}
		firstMap = uint32(lvPtr >> f.logValuesPerMap)
	}
	if firstMap < f.firstRenderedMap {
		return nil
	}
	if r, err := f.renderMapsFromMapBoundary(firstMap, math.MaxUint32); err == nil {
		return r
	} else {
		log.Error("Error initializing head map renderer at map boundary", "firstMap", firstMap, "error", err)
	}
	return nil
}

func (f *FilterMaps) tryMakeHeadRendererFromCheckpoint() *mapRenderer {

}

func (f *FilterMaps) tryUpdateTail() bool {
	for f.firstIndexedBlock > f.tailTarget() {
		tailRenderer := f.tryMakeTailRenderer()
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
			return false
		}
	}
}

func (f *FilterMaps) tryMakeTailRenderer() *mapRenderer {

}

func (f *FilterMaps) waitForEvent() {
	for f.targetHeadIndexed() {
		if f.matcherSyncRequest != nil {
			f.matcherSyncRequest.synced(f.targetHead)
			f.matcherSyncRequest = nil
		}
		select {
		case ev := <-f.headEventCh:
			f.setTargetHead(ev.Header)
		case f.matcherSyncRequest = <-f.matcherSyncCh:
			f.setTargetHead(f.chain.CurrentBlock())
		case <-f.closeCh:
			f.stop = true
			return
		case ch := <-f.waitIdleCh:
			f.setTargetHead(f.chain.CurrentBlock())
			ch <- f.targetHeadIndexed()
		case <-time.After(time.Second * 20):
			// keep updating log index during syncing
			f.setTargetHead(f.chain.CurrentBlock())
		}
	}
}

func (f *FilterMaps) processEvents() {
	for {
		if f.matcherSyncRequest != nil && f.targetHeadIndexed() {
			f.matcherSyncRequest.synced(f.targetHead)
			f.matcherSyncRequest = nil
		}
		select {
		case ev := <-f.headEventCh:
			f.setTargetHead(ev.Header)
		case f.matcherSyncRequest = <-f.matcherSyncCh:
			f.setTargetHead(f.chain.CurrentBlock())
		case <-f.closeCh:
			f.stop = true
			return
		default:
			f.setTargetHead(f.chain.CurrentBlock())
			return
		}
	}
}

func (f *FilterMaps) setTargetHead(head *types.Header) {
	if head == nil || (f.targetHead != nil && head.Hash() == f.targetHead.Hash()) {
		return
	}
	f.targetHead = head
	f.targetView = newChainView(f.chain, head.Number.Uint64(), head.Hash())
}

func (f *FilterMaps) targetHeadIndexed() bool {
	return f.initialized && f.targetHead != nil &&
		f.targetHead.Hash() == f.headBlockHash && f.lastIndexedBlock == f.headBlockNumber
}

//-------------------------------------

// updateLoop initializes and updates the log index structure according to the
// canonical chain.
func (f *FilterMaps) updateLoop() {
	defer f.closeWg.Done()

	if f.noHistory {
		f.reset()
		return
	}

	f.indexLock.Lock()
	f.updateMapCache()
	f.indexLock.Unlock()

	var (
		headEventCh = make(chan core.ChainEvent, 10)
		sub         = f.chain.SubscribeChainEvent(headEventCh)
		head        = f.chain.CurrentBlock()
		stop        bool
		syncMatcher *FilterMapsMatcherBackend
	)
	f.setTargetHead(head)

	matcherSync := func() {
		if syncMatcher != nil && f.initialized && f.headBlockHash == head.Hash() {
			syncMatcher.synced(head)
			syncMatcher = nil
		}
	}

	defer func() {
		sub.Unsubscribe()
		matcherSync()
	}()

	wait := func() {
		matcherSync()
		if stop {
			return
		}
	loop:
		for {
			select {
			case ev := <-headEventCh:
				head = ev.Header
			case syncMatcher = <-f.matcherSyncCh:
				head = f.chain.CurrentBlock()
			case <-f.closeCh:
				stop = true
			case ch := <-f.waitIdleCh:
				head = f.chain.CurrentBlock()
				if head.Hash() == f.headBlockHash {
					ch <- true
					continue loop
				}
				ch <- false
			case <-time.After(time.Second * 20):
				// keep updating log index during syncing
				head = f.chain.CurrentBlock()
			}
			break
		}
		f.setTargetHead(head)
	}
	for head == nil {
		wait()
		if stop {
			return
		}
	}

	for !stop {
		if !f.initialized {
			if !f.tryInit(head) {
				return
			}
			if !f.initialized {
				wait()
				continue
			}
		}
		// log index is initialized
		if f.headBlockHash != head.Hash() {
			// log index head need to be updated
			f.tryUpdateHead(func() *types.Header {
				// return nil if head processing needs to be stopped
				select {
				case ev := <-headEventCh:
					head = ev.Header
				case syncMatcher = <-f.matcherSyncCh:
					head = f.chain.CurrentBlock()
				case <-f.closeCh:
					stop = true
					return nil
				default:
					head = f.chain.CurrentBlock()
				}
				return head
			})
			if stop {
				return
			}
			if !f.initialized {
				continue
			}
			if f.headBlockHash != head.Hash() {
				// if head processing stopped without reaching current head then
				// something went wrong; tryUpdateHead prints an error log in
				// this case and there is nothing better to do here than retry
				// later. Wait for an event though in order to avoid the retry
				// loop spinning at full power.
				wait()
				continue
			}
		}
		// log index is synced to the latest known chain head
		matcherSync()
		// process tail blocks if possible
		if f.tryUpdateTail(func() bool {
			// return true if tail processing needs to be stopped
			select {
			case ev := <-headEventCh:
				head = ev.Header
			case syncMatcher = <-f.matcherSyncCh:
				head = f.chain.CurrentBlock()
			case <-f.closeCh:
				stop = true
				return true
			default:
				head = f.chain.CurrentBlock()
			}
			// stop if there is a new chain head (always prioritize head updates)
			return f.headBlockHash != head.Hash() || syncMatcher != nil
		}) && f.headBlockHash == head.Hash() {
			// if tail processing reached its final state and there is no new
			// head then wait for more events
			wait()
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

func (f *FilterMaps) setTargetHead(head *types.Header) error {
	if head.Hash() == f.indexedView.getBlockHash(f.indexedView.headNumber()) {
		return
	}

	f.indexLock.Lock()
	defer f.indexLock.Unlock()

	newView := newChainView(f.chain, head.Number.Uint64(), head.Hash())
	headNum := f.headBlockNumber
	if newHeadNum := newView.headNumber(); newHeadNum < headNum {
		headNum = newHeadNum
	}
	for newView.getBlockHash(headNum) != f.indexedView.getBlockHash(headNum) {
		if headNum == 0 {
			return errors.New("no common ancestor found")
		}
		headNum--
	}
	f.indexedView = newView
	if headNum == f.headBlockNumber {
		return nil
	}
	nextLvPointer, err := f.getBlockLvPointer(headNum + 1)
	if err != nil {
		return err
	}
	newRange := f.filterMapsRange
	newRange.headBlockNumber = headNum
	newRange.headBlockHash = newView.getBlockHash(headNum)
	newRange.headLvPointer = nextLvPointer - 1
	newRange.headMapIndex = uint32(newRange.headLvPointer >> f.logValuesPerMap)
	batch := f.db.NewBatch()
	for blockNumber := headNum + 1; blockNumber <= oldHeadNum; blockNumber++ {
		f.deleteBlockLvPointer(batch, blockNumber)
		delete(f.renderSnapshots, blockNumber)
	}
	for mapIndex := newRange.headMapIndex + 1; mapIndex <= f.headMapIndex; mapIndex++ {
		f.deleteMapBlockPtr(batch, mapIndex)
	}
	f.setRange(batch, newRange)
	return batch.Write()
}
