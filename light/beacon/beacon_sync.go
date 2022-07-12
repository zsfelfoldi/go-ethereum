// Copyright 2022 The go-ethereum Authors
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

package beacon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const (
	firstRollback    = 4
	rollbackMulStep  = 4
	syncWorkerCount  = 4
	ReverseSyncLimit = 64
	MaxHeaderFetch   = 192
)

// part of BeaconChain, separated for better readability
type beaconSyncer struct {
	storedSection, syncHeadSection *chainSection
	syncHeader                     Header
	newHeadCh                      chan struct{} // closed and replaced when head is changed
	latestHeadCounter              uint64        // +1 when head is changed
	newHeadReqCancel               []func()      // called shortly after head is changed
	nextRollback                   uint64

	processedCallback                               func(common.Hash)
	waitForExecHead                                 bool
	lastProcessedBeaconHead, lastReportedBeaconHead common.Hash
	lastProcessedExecHead, expectedExecHead         common.Hash

	stopSyncCh chan struct{}
	syncWg     sync.WaitGroup
}

func (bc *BeaconChain) SyncToHead(head Header) {
	bc.chainMu.Lock()
	defer bc.chainMu.Unlock()

	bc.syncHeader = head
	cs := &chainSection{
		tailSlot:   uint64(head.Slot) + 1,
		headSlot:   uint64(head.Slot),
		parentHash: head.Hash(),
	}
	if bc.syncHeadSection != nil {
		cs.headCounter = bc.syncHeadSection.headCounter
		if bc.syncHeadSection.prev != nil {
			bc.syncHeadSection.prev.next = cs
			cs.prev = bc.syncHeadSection.prev
		}
	}
	cs.headCounter++
	bc.syncHeadSection = cs
	bc.cancelRequests(time.Second)
	close(bc.newHeadCh)
	bc.newHeadCh = make(chan struct{})
}

func (bc *BeaconChain) StartSyncing() {
	if bc.storedHead != nil {
		bc.storedSection = &chainSection{headSlot: uint64(bc.storedHead.Header.Slot), tailSlot: bc.tailLongTerm}
	}
	bc.newHeadCh = make(chan struct{})
	bc.stopSyncCh = make(chan struct{})
	bc.nextRollback = firstRollback

	bc.syncWg.Add(syncWorkerCount)
	for i := 0; i < syncWorkerCount; i++ {
		go bc.syncWorker()
	}
}

func (bc *BeaconChain) StopSyncing() {
	bc.chainMu.Lock()
	bc.cancelRequests(0)
	bc.chainMu.Unlock()
	close(bc.stopSyncCh)
	bc.syncWg.Wait()
}

func (bc *BeaconChain) syncWorker() {
	defer bc.syncWg.Done()

	bc.chainMu.Lock()
	for {
		if cs := bc.nextRequest(); cs != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
			bc.newHeadReqCancel = append(bc.newHeadReqCancel, cancel)
			cs.requesting = true
			head := bc.syncHeader
			var (
				blocks []*BlockData
				proof  MultiProof
				err    error
			)
			newHeadCh := bc.newHeadCh
			bc.chainMu.Unlock()
			if bc.dataSource != nil && cs.tailSlot+MaxHeaderFetch-1 >= uint64(head.Slot) {
				blocks, err = bc.dataSource.GetBlocksFromHead(ctx, head, uint64(head.Slot)+1-cs.tailSlot)
			} else if bc.historicSource != nil {
				blocks, proof, err = bc.historicSource.GetHistoricBlocks(ctx, head, cs.headSlot, cs.headSlot+1-cs.tailSlot)
			} else {
				log.Error("Historic data source not available") //TODO print only once, ?reset chain?
			}
			if err != nil /*&& err != light.ErrNoPeers && err != context.Canceled*/ {
				log.Warn("Beacon data source failed", "error", err)
			}
			if blocks == nil {
				select {
				case <-newHeadCh:
				case <-bc.stopSyncCh:
					return
				}
			}
			bc.chainMu.Lock()
			if blocks != nil {
				cs.requesting = false
				bc.addBlocksToSection(cs, blocks, proof)
			} else {
				cs.remove()
			}
		} else {
			newHeadCh := bc.newHeadCh
			bc.chainMu.Unlock()
			select {
			case <-newHeadCh:
			case <-bc.stopSyncCh:
				return
			}
			bc.chainMu.Lock()
		}
	}
}

func (bc *BeaconChain) reset() {
	bc.storedSection.remove()
	bc.storedHead, bc.headTree, bc.storedSection = nil, nil, nil
	bc.cancelRequests(0)
	bc.tailShortTerm, bc.tailLongTerm = 0, 0
	bc.initHistoricStructures()
	bc.clearDb()
}

func (bc *BeaconChain) initHistoricStructures() {
	bc.blockRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: blockRootsKey, zeroLevel: 13}}
	bc.stateRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: stateRootsKey, zeroLevel: 13}}
	bc.historicRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: historicRootsKey, zeroLevel: 25}}
	bc.historicTrees = make(map[common.Hash]*HistoricTree)
}

func (bc *BeaconChain) cancelRequests(dt time.Duration) {
	cancelList := bc.newHeadReqCancel
	bc.newHeadReqCancel = nil
	if cancelList != nil {
		time.AfterFunc(dt, func() {
			for _, cancel := range cancelList {
				cancel()
			}
		})
	}
}

func (bc *BeaconChain) rollback(slot uint64) {
	if slot >= uint64(bc.storedHead.Header.Slot) {
		log.Error("Cannot roll back beacon chain", "slot", slot, "head", uint64(bc.storedHead.Header.Slot))
	}
	var block *BlockData
	for {
		block = bc.GetBlockData(slot, bc.headTree.GetStateRoot(slot), false)
		if block != nil {
			return
		}
		slot--
		if slot < bc.tailLongTerm {
			bc.reset()
			return
		}
	}

	headTree := bc.headTree.makeChildTree()
	headTree.addRoots(slot, nil, nil, true, MultiProof{})
	headTree.HeadBlock = block
	batch := bc.db.NewBatch()
	bc.commitHistoricTree(batch, headTree)
	bc.storedHead = block
	bc.storeHeadTail(batch)
	batch.Write()
	bc.updateTreeMap()
}

func (bc *BeaconChain) blockRootAt(slot uint64) common.Hash {
	if bc.headTree == nil {
		return common.Hash{}
	}
	if block := bc.GetBlockData(slot, bc.headTree.GetStateRoot(slot), false); block != nil {
		return block.BlockRoot
	}
	return common.Hash{}
}

type chainSection struct {
	headSlot, tailSlot, headCounter uint64 // tail is parent slot + 1 or 0 if no parent (genesis)
	requesting                      bool
	blocks                          []*BlockData // nil for stored section, sync head section and sections being requested
	tailProof                       MultiProof   // optional merkle proof including path leading to the first root in the historical_roots structure
	parentHash                      common.Hash  // empty for stored section, section starting at genesis and sections being requested
	prev, next                      *chainSection
}

func (cs *chainSection) blockIndex(slot uint64) int {
	if slot > cs.headSlot || slot < cs.tailSlot {
		return -1
	}

	min, max := 0, len(cs.blocks)-1
	for min < max {
		mid := (min + max) / 2
		if uint64(cs.blocks[min].Header.Slot) < slot {
			min = mid + 1
		} else {
			max = mid
		}
	}
	return max
}

// returns empty hash for missed slots and slots outside the parent..head range
func (cs *chainSection) blockRootAt(slot uint64) common.Hash {
	if slot+1 == cs.tailSlot {
		return cs.parentHash
	}
	if index := cs.blockIndex(slot); index != -1 {
		block := cs.blocks[index]
		if uint64(block.Header.Slot) == slot {
			return block.BlockRoot
		}
	}
	return common.Hash{}
}

func (cs *chainSection) blockRange(begin, end uint64) []*BlockData {
	return cs.blocks[cs.blockIndex(begin) : cs.blockIndex(end)+1]
}

func (cs *chainSection) trim(front bool) {
	length := cs.headSlot + 1 - cs.tailSlot
	length /= ((length + MaxHeaderFetch - 1) / MaxHeaderFetch)
	if front {
		cs.headSlot = cs.tailSlot + length - 1
	} else {
		cs.tailSlot = cs.headSlot + 1 - length
	}
}

func (cs *chainSection) remove() {
	if cs.prev != nil {
		cs.prev.next = cs.next
	}
	if cs.next != nil {
		cs.next.prev = cs.prev
	}
}

// chainMu locked
func (bc *BeaconChain) nextRequest() *chainSection {
	origin := bc.storedSection
	if origin == nil {
		origin = bc.syncHeadSection
	}
	if origin == nil {
		return nil
	}
	cs := origin
	for cs != bc.syncHeadSection && cs.next != nil {
		if cs.next.tailSlot > cs.headSlot+1 {
			req := &chainSection{
				headSlot:    cs.next.tailSlot - 1,
				tailSlot:    cs.headSlot + 1,
				headCounter: bc.latestHeadCounter,
				prev:        cs,
				next:        cs.next,
			}
			cs.next.prev = req
			cs.next = req
			req.trim(true)
			return req
		}
		cs = cs.next
	}
	cs = origin
	for cs.prev != nil {
		if cs.tailSlot > cs.prev.headSlot+1 {
			req := &chainSection{
				headSlot:    cs.tailSlot - 1,
				tailSlot:    cs.prev.headSlot + 1,
				headCounter: bc.latestHeadCounter,
				prev:        cs.prev,
				next:        cs,
			}
			cs.prev.next = req
			cs.prev = req
			req.trim(false)
			return req
		}
		cs = cs.prev
	}
	if cs.tailSlot > bc.tailLongTerm {
		req := &chainSection{
			headSlot:    cs.tailSlot - 1,
			tailSlot:    bc.tailLongTerm,
			headCounter: bc.latestHeadCounter,
			next:        cs,
		}
		cs.prev = req
		req.trim(false)
		return req
	}
	return nil
}

// assumes continuity; overwrite allowed
func (bc *BeaconChain) addCanonicalBlocks(blocks []*BlockData, setHead bool, tailProof MultiProof) error {
	eh, ok := blocks[0].GetStateValue(BsiExecHead)
	if !ok { // should not happen, backend should check proof format
		return errors.New("exec header root not found in beacon state")
	}
	execHeader := bc.execChain.GetHeaderByHash(common.Hash(eh))
	if execHeader == nil {
		return errors.New("cannot find exec header")
	}
	execNumber := execHeader.Number.Uint64()
	for _, block := range blocks {
		if !bc.pruneBlockFormat(block) {
			return errors.New("fetched state proofs insufficient")
		}
		bc.storeBlockData(block)
		bc.storeSlotByBlockRoot(block)
		bc.storeBlockDataByExecNumber(execNumber, block)
		execNumber++
	}

	parent := bc.GetParent(blocks[0])
	firstSlot, blockRoots, stateRoots := blockAndStateRoots(parent, blocks)
	if parent == nil && firstSlot > bc.tailLongTerm {
		log.Error("Missing parent block", "firstSlot", firstSlot-1)
	}

	headTree := bc.headTree.makeChildTree()
	headTree.addRoots(firstSlot, blockRoots, stateRoots, setHead, tailProof)
	if setHead {
		bc.storedHead = blocks[len(blocks)-1]
		headTree.HeadBlock = bc.storedHead
	}
	batch := bc.db.NewBatch()
	bc.commitHistoricTree(batch, headTree)
	bc.storeHeadTail(batch)
	batch.Write()
	bc.updateTreeMap()
	log.Info("Successful BeaconChain insert")
	return nil
}

func (bc *BeaconChain) initWithSection(cs *chainSection) bool { // ha a result true, a chain inicializalva van, storedSection != nil
	if bc.syncHeadSection == nil || cs.next != bc.syncHeadSection || cs.headSlot != uint64(bc.syncHeader.Slot) || cs.blockRootAt(uint64(bc.syncHeader.Slot)) != bc.syncHeader.Hash() {
		return false
	}

	headTree := bc.newHistoricTree(uint64(bc.syncHeader.Slot)+1, 0, 0)
	if bc.dataSource != nil {
		headTree := bc.headTree.makeChildTree()
		ctx, _ := context.WithTimeout(context.Background(), time.Second*2) // API backend should respond immediately so waiting here once during init is acceptable
		if err := headTree.initRecentRoots(ctx, bc.dataSource); err == nil {
			batch := bc.db.NewBatch()
			bc.commitHistoricTree(batch, headTree)
			bc.storeHeadTail(batch)
			batch.Write()
			bc.updateTreeMap()
		} else {
			log.Error("Error retrieving recent roots from beacon API", "error", err)
			return false
		}
	}
	bc.storedHead = cs.blocks[len(cs.blocks)-1]
	bc.headTree = headTree
	bc.addCanonicalBlocks(cs.blocks, true, MultiProof{})
	bc.storedSection = cs
	return true
}

// storedSection is expected to exist
func (bc *BeaconChain) mergeWithStoredSection(cs *chainSection) bool { // ha a result true, ezutan cs eldobhato
	if cs.tailSlot > bc.storedSection.headSlot+1 || cs.headSlot+1 < bc.storedSection.tailSlot {
		return false
	}

	if bc.storedSection == nil {
		// try to init the chain with the current section

	}

	if cs.tailSlot < bc.storedSection.tailSlot {
		if cs.blockRootAt(bc.storedSection.tailSlot-1) == bc.blockRootAt(bc.storedSection.tailSlot-1) {
			//bc.addToTail(cs.blockRange(cs.tailSlot, bc.storedSection.tailSlot-1), cs.tailProof)
			bc.addCanonicalBlocks(cs.blockRange(cs.tailSlot, bc.storedSection.tailSlot-1), false, cs.tailProof)
		} else {
			if cs.headCounter <= bc.storedSection.headCounter {
				return true
			}
			bc.reset()
			return false
		}
	}

	if cs.headCounter <= bc.storedSection.headCounter {
		if cs.headSlot > bc.storedSection.headSlot && cs.blockRootAt(bc.storedSection.headSlot) == bc.blockRootAt(bc.storedSection.headSlot) {
			//bc.addToHead(cs.blockRange(bc.storedSection.headSlot+1, cs.headSlot))
			bc.addCanonicalBlocks(cs.blockRange(bc.storedSection.headSlot+1, cs.headSlot), true, MultiProof{})
			bc.storedSection.headCounter = cs.headCounter
			bc.nextRollback = firstRollback
		}
		return true
	}

	lastCommon := cs.headSlot
	if bc.storedSection.headSlot < lastCommon {
		lastCommon = bc.storedSection.headSlot
	}
	for cs.blockRootAt(lastCommon) != bc.blockRootAt(lastCommon) {
		if lastCommon == 0 || lastCommon < cs.tailSlot || lastCommon < bc.storedSection.tailSlot {
			rollback := bc.nextRollback
			bc.nextRollback *= rollbackMulStep
			if lastCommon >= bc.storedSection.tailSlot+rollback {
				bc.rollback(lastCommon - rollback)
			} else {
				bc.reset()
			}
			return false
		}
		lastCommon--
	}
	if lastCommon < bc.storedSection.headSlot {
		bc.rollback(lastCommon)
	}
	if lastCommon < cs.headSlot {
		//bc.addToHead(cs.blockRange(lastCommon+1, cs.headSlot))
		bc.addCanonicalBlocks(cs.blockRange(lastCommon+1, cs.headSlot), true, MultiProof{})
		bc.nextRollback = firstRollback
	}
	return true
}

func (bc *BeaconChain) addBlocksToSection(cs *chainSection, blocks []*BlockData, tailProof MultiProof) {
	headSlot, tailSlot := blocks[len(blocks)-1].Header.Slot, blocks[0].Header.Slot+1-blocks[0].ParentSlotDiff
	if headSlot < cs.headSlot || tailSlot > cs.tailSlot {
		panic(nil)
	}
	cs.headSlot, cs.tailSlot = headSlot, tailSlot
	cs.blocks, cs.tailProof = blocks, tailProof

	for {
		if bc.storedSection == nil && (bc.syncHeadSection == nil || bc.syncHeadSection.prev == nil || !bc.initWithSection(bc.syncHeadSection.prev)) {
			return
		}
		if bc.storedSection.next != nil && bc.mergeWithStoredSection(bc.storedSection.next) && bc.storedSection.next != bc.syncHeadSection {
			bc.storedSection.next.remove()
			continue
		}
		if bc.storedSection == nil {
			continue
		}
		if bc.storedSection.prev != nil && bc.mergeWithStoredSection(bc.storedSection.prev) {
			bc.storedSection.prev.remove()
		}
		if bc.storedSection != nil {
			return
		}
	}
}

func (bc *BeaconChain) SubscribeToProcessedHeads(processedCallback func(common.Hash), waitForExecHead bool) {
	// Note: called during init phase, after that these variables are read-only
	bc.processedCallback = processedCallback
	bc.waitForExecHead = waitForExecHead
}

// called under chainMu
// returns true if cb should be called with beaconHead as parameter after releasing lock
func (bc *BeaconChain) processedHead(beaconHead, execHead common.Hash) bool {
	if bc.processedCallback == nil {
		return false
	}
	bc.lastProcessedBeaconHead, bc.expectedExecHead = beaconHead, execHead
	if bc.lastReportedBeaconHead != beaconHead && (!bc.waitForExecHead || bc.lastProcessedExecHead == execHead) {
		bc.lastReportedBeaconHead = beaconHead
		return true
	}
	return false
}

func (bc *BeaconChain) ProcessedExecHead(execHead common.Hash) {
	if bc.processedCallback == nil || !bc.waitForExecHead {
		return
	}
	bc.chainMu.Lock()
	var reportHead common.Hash
	if bc.expectedExecHead == execHead && bc.lastReportedBeaconHead != bc.lastProcessedBeaconHead {
		reportHead = bc.lastProcessedBeaconHead
		bc.lastReportedBeaconHead = reportHead
	}
	bc.lastProcessedExecHead = execHead
	bc.chainMu.Unlock()

	if reportHead != (common.Hash{}) {
		bc.processedCallback(reportHead)
	}
}
