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
	if head.Hash() == f.chainView.getBlockHash(f.chainView.headNumber()) {
		return
	}

	f.indexLock.Lock()
	defer f.indexLock.Unlock()

	newView := newChainView(f.chain, head.Number.Uint64(), head.Hash())
	headNum := f.headBlockNumber
	if newHeadNum := newView.headNumber(); newHeadNum < headNum {
		headNum = newHeadNum
	}
	for newView.getBlockHash(headNum) != f.chainView.getBlockHash(headNum) {
		if headNum == 0 {
			return errors.New("no common ancestor found")
		}
		headNum--
	}
	f.chainView = newView
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
		delete(f.revertPoints, blockNumber)
	}
	for mapIndex := newRange.headMapIndex + 1; mapIndex <= f.headMapIndex; mapIndex++ {
		f.deleteMapBlockPtr(batch, mapIndex)
	}
	f.setRange(batch, newRange)
	return batch.Write()
}
