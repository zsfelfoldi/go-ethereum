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
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	firstRollback    = 4
	rollbackMulStep  = 4
	syncWorkerCount  = 1 //TODO 4
	ReverseSyncLimit = 64
	MaxHeaderFetch   = 192
)

// part of BeaconChain, separated for better readability
type beaconSyncer struct {
	storedSection, syncHeadSection *chainSection
	syncHeader                     Header
	syncTail                       uint64
	newHeadCh                      chan struct{} // closed and replaced when head is changed
	syncedCh                       chan bool
	latestHeadCounter              uint64   // +1 when head is changed
	newHeadReqCancel               []func() // called shortly after head is changed
	nextRollback                   uint64

	processedCallback                               func(common.Hash)
	waitForExecHead                                 bool
	lastProcessedBeaconHead, lastReportedBeaconHead common.Hash
	lastProcessedExecHead, expectedExecHead         common.Hash

	stopSyncCh chan struct{}
	syncWg     sync.WaitGroup
}

func (bc *BeaconChain) SyncToHead(head Header, syncedCh chan bool) {
	bc.chainMu.Lock()
	defer bc.chainMu.Unlock()

	fmt.Println("SyncToHead", head.Slot)
	if bc.syncedCh != nil {
		bc.syncedCh <- false
	}
	bc.syncedCh = syncedCh
	bc.syncHeader = head
	bc.latestHeadCounter++
	cs := &chainSection{
		headCounter: bc.latestHeadCounter,
		tailSlot:    uint64(head.Slot) + 1,
		headSlot:    uint64(head.Slot),
		parentHash:  head.Hash(),
	}
	if bc.syncHeadSection != nil && bc.syncHeadSection.prev != nil {
		bc.syncHeadSection.prev.next = cs
		cs.prev = bc.syncHeadSection.prev
	}
	if bc.historicSource == nil && cs.headSlot > bc.syncTail+32 {
		bc.syncTail = cs.headSlot - 32 //TODO named constant
	}
	if cs.prev == nil && bc.storedSection != nil {
		cs.prev = bc.storedSection
		bc.storedSection.next = cs
	}
	bc.syncHeadSection = cs
	bc.cancelRequests(time.Second)
	close(bc.newHeadCh)
	bc.newHeadCh = make(chan struct{})
}

func (bc *BeaconChain) StartSyncing() {
	if bc.storedHead != nil {
		bc.storedSection = &chainSection{headSlot: uint64(bc.storedHead.Header.Slot), tailSlot: bc.tailLongTerm}
		bc.updateConstraints(bc.tailLongTerm, uint64(bc.storedHead.Header.Slot))
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
		bc.debugPrint("before nextRequest")
		if cs := bc.nextRequest(); cs != nil && bc.syncHeader.StateRoot != (common.Hash{}) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
			bc.newHeadReqCancel = append(bc.newHeadReqCancel, cancel)
			cs.requesting = true
			bc.debugPrint("after nextRequest")
			head := bc.syncHeader
			var (
				blocks []*BlockData
				proof  MultiProof
				err    error
			)
			newHeadCh := bc.newHeadCh
			cb := bc.constraintCallbacks()
			bc.chainMu.Unlock()
			cb()
			if bc.dataSource != nil && cs.tailSlot+MaxHeaderFetch-1 >= uint64(head.Slot) {
				fmt.Println("SYNC dataSource request", head.Slot, cs.tailSlot, cs.headSlot)
				blocks, err = bc.dataSource.GetBlocksFromHead(ctx, head, uint64(head.Slot)+1-cs.tailSlot)
			} else if bc.historicSource != nil {
				fmt.Println("SYNC historicSource request", head.Slot, cs.tailSlot, cs.headSlot)
				blocks, proof, err = bc.historicSource.GetHistoricBlocks(ctx, head, cs.headSlot, cs.headSlot+1-cs.tailSlot)
			} else {
				log.Error("Historic data source not available") //TODO print only once, ?reset chain?
			}
			if err != nil /*&& err != light.ErrNoPeers && err != context.Canceled*/ {
				blocks = nil
				log.Warn("Beacon data source failed", "error", err)
			}
			if blocks != nil && (len(blocks) == 0 || blocks[0].Header.Slot+1-blocks[0].ParentSlotDiff > cs.tailSlot || blocks[len(blocks)-1].Header.Slot < cs.headSlot) {
				blocks = nil
				log.Error("Retrieved beacon block range insufficient")
			}
			fmt.Println(" blocks", len(blocks), "err", err)
			if blocks == nil {
				bc.chainMu.Lock()
				if bc.syncedCh != nil {
					bc.syncedCh <- true
					bc.syncedCh = nil
				}
				bc.chainMu.Unlock()
				select {
				case <-newHeadCh:
				case <-bc.stopSyncCh:
					return
				}
			}
			bc.chainMu.Lock()
			if blocks != nil {
				cs.requesting = false
				bc.debugPrint("before addBlocksToSection")
				bc.addBlocksToSection(cs, blocks, proof)
				bc.debugPrint("after addBlocksToSection")
			} else {
				bc.debugPrint("before remove")
				cs.remove()
				bc.debugPrint("after remove")
			}
		} else {
			newHeadCh := bc.newHeadCh
			cb := bc.constraintCallbacks()
			if bc.syncedCh != nil {
				bc.syncedCh <- true
				bc.syncedCh = nil
			}
			bc.chainMu.Unlock()
			cb()
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

	headTree := bc.makeChildTree()
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
	if slot == bc.headTree.HeadBlock.Header.Slot { //TODO is this special case needed?
		return bc.headTree.HeadBlock.BlockRoot
	}
	if slot+1 == bc.tailLongTerm {
		for s := slot + 1; s <= bc.headTree.HeadBlock.Header.Slot; s++ {
			if block := bc.GetBlockData(s, bc.headTree.GetStateRoot(s), false); block != nil {
				if block.Header.Slot+1-block.ParentSlotDiff == slot {
					return block.Header.ParentRoot
				}
				log.Error("Could not find parent of tail block")
				return common.Hash{}
			}

		}
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
	if bc.syncHeadSection == nil {
		return nil
	}
	fmt.Println("SYNC nextRequest")
	origin := bc.storedSection
	if origin == nil {
		origin = bc.syncHeadSection
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
			fmt.Println(" fwd", req.tailSlot, req.headSlot)
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
			fmt.Println(" rev", req.tailSlot, req.headSlot)
			return req
		}
		cs = cs.prev
	}
	if cs.tailSlot > bc.syncTail {
		req := &chainSection{
			headSlot:    cs.tailSlot - 1,
			tailSlot:    bc.syncTail,
			headCounter: bc.latestHeadCounter,
			next:        cs,
		}
		cs.prev = req
		req.trim(false)
		fmt.Println(" tail", req.tailSlot, req.headSlot)
		return req
	}
	return nil
}

// assumes continuity; overwrite allowed
func (bc *BeaconChain) addCanonicalBlocks(blocks []*BlockData, setHead bool, tailProof MultiProof) {
	fmt.Println("SYNC addCanonicalBlocks", len(blocks), blocks[0].Header.Slot, blocks[len(blocks)-1].Header.Slot, setHead, tailProof.Format != nil)
	eh, ok := blocks[len(blocks)-1].GetStateValue(BsiExecHead)
	if !ok { // should not happen, backend should check proof format
		log.Error("exec header root not found in beacon state", "slot", blocks[0].Header.Slot)
	}
	var (
		execHeader     *types.Header
		lastExecNumber uint64
	)
	if eh != (MerkleValue{}) {
		if execHeader = bc.execChain.GetHeaderByHash(common.Hash(eh)); execHeader != nil {
			lastExecNumber = execHeader.Number.Uint64()
		} else {
			log.Error("Exec block header not found", "slot", blocks[len(blocks)-1].Header.Slot) //TODO is it too wasteful to always look up by hash?
		}
	}
	for i, block := range blocks {
		if !bc.pruneBlockFormat(block) {
			log.Error("fetched state proofs insufficient", "slot", block.Header.Slot)
		}
		bc.storeBlockData(block)
		bc.storeSlotByBlockRoot(block)
		if execHeader == nil && (eh != MerkleValue{}) {
			if execHeader = bc.execChain.GetHeaderByHash(common.Hash(eh)); execHeader != nil {
				lastExecNumber = execHeader.Number.Uint64()
			} else {
				log.Error("Exec block header not found", "slot", block.Header.Slot) //TODO is it too wasteful to always look up by hash?
			}
		}
		if execHeader != nil { //TODO do not store pre-TTD entries
			bc.storeBlockDataByExecNumber(lastExecNumber-uint64(len(blocks)-1-i), block)
		}
	}

	parent := bc.GetParent(blocks[0])
	firstSlot, blockRoots, stateRoots := blockAndStateRoots(parent, blocks)
	if parent == nil && blocks[0].Header.Slot > bc.tailLongTerm {
		log.Error("Missing parent block", "slot", blocks[0].Header.Slot)
	}
	tailSlot := blocks[0].Header.Slot - uint64(len(blocks[0].StateRootDiffs))
	fmt.Println("addCanonicalBlocks", tailSlot, bc.tailLongTerm)
	if tailSlot < bc.tailLongTerm {
		bc.tailLongTerm = tailSlot
	}

	headTree := bc.makeChildTree()
	headTree.addRoots(firstSlot, blockRoots, stateRoots, setHead, tailProof)
	afterLastSlot := firstSlot + uint64(len(blockRoots))
	if setHead {
		bc.storedHead = blocks[len(blocks)-1]
		if uint64(bc.storedHead.Header.Slot) > afterLastSlot {
			afterLastSlot = uint64(bc.storedHead.Header.Slot)
		}
		headTree.HeadBlock = bc.storedHead
	}
	if afterLastSlot > firstSlot {
		bc.updateConstraints(firstSlot, afterLastSlot-1)
	}
	batch := bc.db.NewBatch()
	bc.commitHistoricTree(batch, headTree)
	bc.storeHeadTail(batch)
	batch.Write()
	bc.updateTreeMap()
	if bc.storedSection != nil {
		bc.storedSection.tailSlot, bc.storedSection.headSlot = bc.tailLongTerm, bc.storedHead.Header.Slot
	}
	if execHeader != nil && bc.processedHead(blocks[len(blocks)-1].BlockRoot, execHeader.Hash()) {
		bc.callProcessedBeaconHead = blocks[len(blocks)-1].BlockRoot
	}
	log.Info("Successful BeaconChain insert")
}

func (bc *BeaconChain) initWithSection(cs *chainSection) bool { // ha a result true, a chain inicializalva van, storedSection != nil
	bc.debugPrint("before initWithSection")
	defer bc.debugPrint("after initWithSection")

	fmt.Println("SYNC initWithSection", cs.tailSlot, cs.headSlot)
	if bc.syncHeadSection == nil || cs.next != bc.syncHeadSection || cs.headSlot != uint64(bc.syncHeader.Slot) || cs.blockRootAt(uint64(bc.syncHeader.Slot)) != bc.syncHeader.Hash() {
		return false
	}

	bc.storedHead = cs.blocks[len(cs.blocks)-1]
	bc.tailLongTerm = uint64(bc.storedHead.Header.Slot) + 1
	//bc.tailShortTerm = bc.tailLongTerm
	bc.headTree = bc.newHistoricTree(bc.tailLongTerm, 0, 0)
	if bc.dataSource != nil {
		headTree := bc.makeChildTree()
		ctx, _ := context.WithTimeout(context.Background(), time.Second*2) // API backend should respond immediately so waiting here once during init is acceptable
		if err := headTree.initRecentRoots(ctx, bc.dataSource); err == nil {
			batch := bc.db.NewBatch()
			bc.commitHistoricTree(batch, headTree)
			bc.storeHeadTail(batch)
			batch.Write()
			//bc.updateTreeMap()
		} else {
			log.Error("Error retrieving recent roots from beacon API", "error", err)
			bc.storedHead, bc.headTree = nil, nil
			return false
		}
	}
	bc.addCanonicalBlocks(cs.blocks, true, MultiProof{})
	bc.storedSection = cs
	cs.blocks, cs.tailProof = nil, MultiProof{}
	fmt.Println(" init success")
	return true
}

// storedSection is expected to exist
func (bc *BeaconChain) mergeWithStoredSection(cs *chainSection) bool { // ha a result true, ezutan cs eldobhato
	bc.debugPrint("before mergeWithStoredSection")
	defer bc.debugPrint("after mergeWithStoredSection")

	fmt.Println("SYNC mergeWithStoredSection", bc.storedSection.tailSlot, bc.storedSection.headSlot, cs.tailSlot, cs.headSlot)
	if cs.tailSlot > bc.storedSection.headSlot+1 || cs.headSlot+1 < bc.storedSection.tailSlot {
		fmt.Println(" 1 false")
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
				fmt.Println(" 2 true")
				return true
			}
			bc.reset()
			fmt.Println(" 3 false")
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
		fmt.Println(" 4 true")
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
			fmt.Println(" 5 false")
			return false
		}
		lastCommon--
	}
	bc.storedSection.headCounter = cs.headCounter
	if lastCommon < bc.storedSection.headSlot {
		bc.rollback(lastCommon)
	}
	if lastCommon < cs.headSlot {
		//bc.addToHead(cs.blockRange(lastCommon+1, cs.headSlot))
		bc.addCanonicalBlocks(cs.blockRange(lastCommon+1, cs.headSlot), true, MultiProof{})
		bc.nextRollback = firstRollback
	}
	fmt.Println(" 6 true")
	return true
}

func (bc *BeaconChain) addBlocksToSection(cs *chainSection, blocks []*BlockData, tailProof MultiProof) {
	fmt.Println("SYNC addBlocksToSection", cs.tailSlot, cs.headSlot, len(blocks), blocks[0].Header.Slot, blocks[len(blocks)-1].Header.Slot, tailProof.Format != nil)
	headSlot, tailSlot := blocks[len(blocks)-1].Header.Slot, blocks[0].Header.Slot+1-blocks[0].ParentSlotDiff
	if headSlot < cs.headSlot || tailSlot > cs.tailSlot {
		panic(nil)
	}
	cs.headSlot, cs.tailSlot = headSlot, tailSlot
	cs.blocks, cs.parentHash, cs.tailProof = blocks, blocks[0].Header.ParentRoot, tailProof

	for {
		if bc.storedSection == nil && (bc.syncHeadSection == nil || bc.syncHeadSection.prev == nil || !bc.initWithSection(bc.syncHeadSection.prev)) {
			return
		}
		if bc.storedSection.next != nil && !bc.storedSection.next.requesting && bc.mergeWithStoredSection(bc.storedSection.next) && bc.storedSection.next != bc.syncHeadSection {
			bc.storedSection.next.remove()
			continue
		}
		if bc.storedSection == nil {
			continue
		}
		if bc.storedSection.prev != nil && !bc.storedSection.prev.requesting && bc.mergeWithStoredSection(bc.storedSection.prev) {
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
	fmt.Println("processedHead", beaconHead, execHead)
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

func (bc *BeaconChain) debugPrint(id string) {
	if bc.storedHead != nil {
		fmt.Println("***", id, "*** storedHead", bc.storedHead.Header.Slot)
	} else {
		fmt.Println("***", id, "*** no storedHead")
	}
	cs := bc.syncHeadSection
	for cs != nil {
		var bs, be uint64
		if len(cs.blocks) > 0 {
			bs, be = cs.blocks[0].Header.Slot, cs.blocks[len(cs.blocks)-1].Header.Slot
		}
		fmt.Println("***", id, "*** cs  headCounter", cs.headCounter, "requesting", cs.requesting, "stored", cs == bc.storedSection, "tail", cs.tailSlot, "head", cs.headSlot, "blocks", len(cs.blocks), bs, be)
		cs = cs.prev
	}
	fmt.Println("***", id, "*** tail", bc.tailLongTerm)
}
