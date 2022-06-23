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
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	lru "github.com/hashicorp/golang-lru"
)

var (
	blockRootsKey           = []byte("br-")
	stateRootsKey           = []byte("sr-")
	historicRootsKey        = []byte("hr-")
	beaconHeadTailKey       = []byte("ht")  // -> head slot and block root, tail slot values (short term, long term, init data)
	blockDataKey            = []byte("b-")  // bigEndian64(slot) + stateRoot -> RLP(BlockData)  (available starting from tailLongTerm)
	blockDataByBlockRootKey = []byte("bb-") // bigEndian64(slot) + blockRoot -> RLP(BlockData)  (not stored in db, only for caching)
	execNumberKey           = []byte("e-")  // bigEndian64(execNumber) + stateRoot -> RLP(slot)  (available starting from tailLongTerm)
	slotByBlockRootKey      = []byte("sb-") // blockRoot -> RLP(slot)  (available for init data blocks and all blocks starting from tailShortTerm)

	StateProofFormats   [HspFormatCount]indexMapFormat
	stateProofIndexMaps [HspFormatCount]map[uint64]int
	beaconHeaderFormat  indexMapFormat
)

const (
	HspLongTerm    = 1 << iota // state proof long term fields (latest_block_header.root, exec_head)
	HspShortTerm               // state proof short term fields (block_roots, state_roots, historical_roots, finalized.root)
	HspInitData                // state proof init data fields (genesis_time, genesis_validators_root, sync_committee_root, next_sync_committee_root)
	HspFormatCount             // number of possible format configurations

	HspAll = HspFormatCount - 1

	// beacon header fields
	BhiSlot          = 8
	BhiProposerIndex = 9
	BhiParentRoot    = 10
	BhiStateRoot     = 11
	BhiBodyRoot      = 12

	// beacon state fields		//TODO ??? fork-ot nem kene state-bol ellenorizni?
	BsiGenesisTime       = 32
	BsiGenesisValidators = 33
	BsiForkVersion       = 141 //TODO ??? osszes fork field? long vagy short term?
	BsiLatestHeader      = 36
	BsiBlockRoots        = 37
	BsiStateRoots        = 38
	BsiHistoricRoots     = 39
	BsiFinalBlock        = 105
	BsiSyncCommittee     = 54
	BsiNextSyncCommittee = 55
	BsiExecHead          = 908 // ??? 56

	ReverseSyncLimit = 64
	MaxHeaderFetch   = 192
)

var BsiFinalExecHash = ChildIndex(ChildIndex(BsiFinalBlock, BhiStateRoot), BsiExecHead)

func init() {
	// initialize header proof format
	beaconHeaderFormat = NewIndexMapFormat()
	for i := uint64(8); i < 13; i++ {
		beaconHeaderFormat.AddLeaf(i, nil)
	}

	// initialize beacon state proof formats and index maps
	for i := range StateProofFormats {
		StateProofFormats[i] = NewIndexMapFormat()
		if i&HspLongTerm != 0 {
			StateProofFormats[i].AddLeaf(BsiLatestHeader, nil)
			StateProofFormats[i].AddLeaf(BsiExecHead, nil)
		}
		if i&HspShortTerm != 0 {
			StateProofFormats[i].AddLeaf(BsiStateRoots, nil)
			StateProofFormats[i].AddLeaf(BsiHistoricRoots, nil)
			StateProofFormats[i].AddLeaf(BsiFinalBlock, nil)
		}
		if i&HspInitData != 0 {
			StateProofFormats[i].AddLeaf(BsiGenesisTime, nil)
			StateProofFormats[i].AddLeaf(BsiGenesisValidators, nil)
			StateProofFormats[i].AddLeaf(BsiForkVersion, nil)
			StateProofFormats[i].AddLeaf(BsiSyncCommittee, nil)
			StateProofFormats[i].AddLeaf(BsiNextSyncCommittee, nil)
		}
		stateProofIndexMaps[i] = proofIndexMap(StateProofFormats[i])
	}
}

type beaconData interface {
	GetBlocksFromHead(ctx context.Context, head Header, amount uint64) ([]*BlockData, error)
	//
	GetRootsProof(ctx context.Context, block *BlockData) (MultiProof, MultiProof, error)
	GetHistoricRootsProof(ctx context.Context, block *BlockData, period uint64) (MultiProof, error)
}

type historicData interface { // only supported by ODR
	GetHistoricBlocks(ctx context.Context, head Header, lastSlot, amount uint64) ([]*BlockData, error)
}

type historicInitData interface { // only supported by beacon node API; ODR supports retrieving 8k historic slots instead
	GetRootsProof(ctx context.Context, block *BlockData) (MultiProof, MultiProof, error)
	GetHistoricRootsProof(ctx context.Context, block *BlockData, period uint64) (MultiProof, error)
}

type execChain interface {
	// GetHeader(common.Hash, uint64) *types.Header   ???
	GetHeaderByHash(common.Hash) *types.Header
}

type BeaconChain struct {
	dataSource     beaconData
	historicSource historicData

	//historicInitSource historicInitData
	execChain      execChain
	db             ethdb.Database
	failCounter    int
	blockDataCache *lru.Cache // string(dbKey) -> *BlockData  (either blockDataKey, slotByBlockRootKey or blockDataByBlockRootKey)  //TODO use separate cache?
	historicCache  *lru.Cache // string(dbKey) -> MerkleValue (either stateRootsKey or historicRootsKey)

	bellatrixSlot uint64

	execNumberCacheMu sync.RWMutex //TODO ???
	execNumberCache   *lru.Cache   // uint64(execNumber) -> []struct{slot, stateRoot}

	chainMu                               sync.RWMutex
	storedSection, syncHeadSection        *chainSection
	storedHead                            Header
	newHeadCh                             chan struct{} // closed and replaced when head is changed
	headCounter                           uint64        // +1 when head is changed
	newHeadReqCancel                      []func()      // called shortly after head is changed
	tailShortTerm, tailLongTerm           uint64        // shortTerm >= longTerm
	blockRoots, stateRoots, historicRoots *merkleListVersion

	historicMu    sync.RWMutex
	historicTrees map[common.Hash]*HistoricTree
}

type Header struct {
	Slot          common.Decimal `json:"slot"` //TODO SignedHead RLP encoding is jo??
	ProposerIndex common.Decimal `json:"proposer_index"`
	ParentRoot    common.Hash    `json:"parent_root"`
	StateRoot     common.Hash    `json:"state_root"`
	BodyRoot      common.Hash    `json:"body_root"`
}

func (bh *Header) Hash() common.Hash {
	var values [8]MerkleValue //TODO ezt lehetne szebben is
	binary.LittleEndian.PutUint64(values[0][:8], uint64(bh.Slot))
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = MerkleValue(bh.ParentRoot)
	values[3] = MerkleValue(bh.StateRoot)
	values[4] = MerkleValue(bh.BodyRoot)
	//fmt.Println("hashing full header", bh, values)
	return MultiProof{Format: NewRangeFormat(8, 15, nil), Values: values[:]}.rootHash()
}

type HeaderWithoutState struct {
	Slot                 uint64
	ProposerIndex        uint
	ParentRoot, BodyRoot common.Hash
}

func (bh *HeaderWithoutState) Hash(stateRoot common.Hash) common.Hash {
	return bh.Proof(stateRoot).rootHash()
}

func (bh *HeaderWithoutState) Proof(stateRoot common.Hash) MultiProof {
	var values [8]MerkleValue //TODO ezt lehetne szebben is
	binary.LittleEndian.PutUint64(values[0][:8], bh.Slot)
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = MerkleValue(bh.ParentRoot)
	values[3] = MerkleValue(stateRoot)
	values[4] = MerkleValue(bh.BodyRoot)
	return MultiProof{Format: NewRangeFormat(8, 15, nil), Values: values[:]}
}

func (bh *HeaderWithoutState) FullHeader(stateRoot common.Hash) Header {
	return Header{
		Slot:          common.Decimal(bh.Slot),
		ProposerIndex: common.Decimal(bh.ProposerIndex),
		ParentRoot:    bh.ParentRoot,
		StateRoot:     stateRoot,
		BodyRoot:      bh.BodyRoot,
	}
}

type BlockData struct {
	Header         HeaderWithoutState
	StateRoot      common.Hash `rlp:"-"` // calculated by CalculateRoots()
	BlockRoot      common.Hash `rlp:"-"` // calculated by CalculateRoots()
	ProofFormat    byte
	StateProof     MerkleValues
	ParentSlotDiff uint64       // slot-parentSlot; 0 if not initialized
	StateRootDiffs MerkleValues // only valid if ParentSlotDiff is initialized
}

func (block *BlockData) Proof() MultiProof {
	return MultiProof{Format: StateProofFormats[block.ProofFormat], Values: block.StateProof}
}

func (block *BlockData) GetStateValue(index uint64) (MerkleValue, bool) {
	proofIndex, ok := stateProofIndexMaps[block.ProofFormat][index]
	if !ok {
		return MerkleValue{}, false
	}
	return block.StateProof[proofIndex], true
}

func (block *BlockData) mustGetStateValue(index uint64) MerkleValue {
	v, ok := block.GetStateValue(index)
	if !ok {
		panic(nil)
	}
	return v
}

func (block *BlockData) CalculateRoots() {
	block.StateRoot = block.Proof().rootHash()
	block.BlockRoot = block.Header.Hash(block.StateRoot)
}

func NewBeaconChain(dataSource beaconData, execChain execChain /*sct *SyncCommitteeTracker, */, db ethdb.Database, forks Forks) *BeaconChain {
	chainDb := rawdb.NewTable(db, "bc-")
	blockDataCache, _ := lru.New(2000)
	historicCache, _ := lru.New(20000)
	execNumberCache, _ := lru.New(2000)
	bc := &BeaconChain{
		dataSource:      dataSource,
		execChain:       execChain,
		db:              chainDb,
		blockDataCache:  blockDataCache,
		historicCache:   historicCache,
		execNumberCache: execNumberCache,
	}
	if epoch, ok := forks.epoch("BELLATRIX"); ok {
		bc.bellatrixSlot = epoch << 5
	} else {
		log.Error("Bellatrix fork not found in beacon chain config")
		return nil
	}
	bc.reset()
	if enc, err := bc.db.Get(beaconHeadTailKey); err == nil {
		var ht beaconHeadTailInfo
		if rlp.DecodeBytes(enc, &ht) == nil {
			if block := bc.GetBlockData(ht.HeadSlot, ht.HeadHash, true); block != nil {
				bc.storedHead = block.Header
				bc.tailShortTerm, bc.tailLongTerm = ht.TailShortTerm, ht.TailLongTerm
				bc.storedSection = &chainSection{headSlot: ht.HeadSlot, tailSlot: ht.TailLongTerm}
			} else {
				log.Error("Head block data not found in database")
			}
		}
	}
	if bc.head.StateRoot == (common.Hash{}) {
		bc.clearDb()
	}

	return bc
}

// chainMu locked
func (bc *BeaconChain) setHead(head Header) {
	cs := &chainSection{
		tailSlot:   uint64(head.Slot) + 1,
		headSlot:   uint64(head.Slot),
		parentHash: head.Hash(),
	}
	if bc.syncHeadSection != nil && bc.syncHeadSection.prev != nil {
		bc.syncHeadSection.prev.next = cs
		cs.prev = bc.syncHeadSection.prev
	}
	bc.syncHeadSection = cs
	if bc.storedSection == nil {
		bc.storedSection = cs
	}

	close(bc.newHeadCh)
	bc.newHeadCh = make(chan struct{})
	bc.headCounter++
	cancelList := bc.newHeadReqCancel
	bc.newHeadReqCancel = nil
	if cancelList != nil {
		time.AfterFunc(time.Second, func() {
			for _, cancel := range cancelList {
				cancel()
			}
		})
	}
}

type chainSection struct {
	headSlot, tailSlot, headCounter uint64 // tail is parent slot + 1 or 0 if no parent (genesis)
	requesting                      bool
	blocks                          []*BlockData // nil for stored section, sync head section and sections being requested
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
func (cs *chainSection) blockHashAt(slot uint64) common.Hash {
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
	cs := bc.storedSection
	for cs != bc.syncHeadSection && cs.next != nil {
		if cs.next.tailSlot > cs.headSlot+1 {
			req := &chainSection{
				headSlot:    cs.next.tailSlot - 1,
				tailSlot:    cs.headSlot + 1,
				headCounter: bc.headCounter,
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
	cs = bc.storedSection
	for cs.prev != nil {
		if cs.parentSlot > cs.prev.headSlot {
			req := &chainSection{
				headSlot:    cs.parentSlot,
				parentSlot:  cs.prev.headSlot,
				headCounter: bc.headCounter,
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
	if cs.parentSlot > bc.tailLongTerm {
		req := &chainSection{
			headSlot:    cs.parentSlot,
			parentSlot:  bc.tailLongTerm,
			headCounter: bc.headCounter,
			next:        cs,
		}
		cs.prev = req
		req.trim(false)
		return req
	}
	return nil
}

func (bc *BeaconChain) mergeSection(cs *chainSection) bool { // ha a result true, ezutan cs eldobhato
	if cs.tail > dbc.head+1 || cs.head+1 < dbc.tail {
		return false
	}

	if cs.tail < dbc.tail {
		if cs.blockHashAt(dbc.tail-1) == dbc.blockHashAt(dbc.tail-1) {
			dbc.addToTail(cs.blockRange(cs.tail, dbc.tail-1))
		} else {
			if cs.headCounter <= dbc.headCounter {
				return true
			}
			dbc.reset()
			return false
		}
	}

	if cs.headCounter <= dbc.headCounter {
		if cs.head > dbc.head && cs.blockHashAt(dbc.head) == dbc.blockHashAt(dbc.head) {
			dbc.addToHead(cs.blockRange(dbc.head+1, cs.head))
		}
		return true
	}

	lastCommon := cs.head
	if dbc.head < lastCommon {
		lastCommon = dbc.head
	}
	for cs.blockHashAt(lastCommon) != dbc.blockHashAt(lastCommon) {
		if lastCommon == 0 || lastCommon < cs.tail || lastCommon < dbc.tail {
			rollback := dbc.nextRollback
			dbc.nextRollback += dbc.nextRollback // addToHead resets it to 1
			if lastCommon >= dbc.tail+rollback {
				dbc.rollback(lastCommon - rollback)
			} else {
				dbc.reset()
			}
			return false
		}
		lastCommon--
	}
	if lastCommon < dbc.head {
		dbc.rollback(lastCommon)
	}
	if lastCommon < cs.head {
		dbc.addToHead(cs.blockRange(lastCommon+1, cs.head))
	}
	return true
}

func (bc *BeaconChain) addBlocks(cs *chainSection, blocks []*BeaconData) {
	cs.headSlot = blocks[len(blocks)-1].Header.Slot
	cs.parentSlot = blocks[0].Header.Slot - blocks[0].ParentSlotDiff
	cs.blocks = blocks

	if deleted, _ := bc.matchNext(cs); deleted {
		return
	}
	for cs.prev != nil {
		if _, promoted := bc.matchNext(cs.prev); !promoted {
			return
		}
		cs = cs.prev
	}
}

func (bc *BeaconChain) matchNext(cs *chainSection) (deleted, promoted bool) {
	var connected bool
	if cs.next == nil {
		// match end to current head
		if cs.headSlot < uint64(bc.head.Slot) {
			// there is a gap, more requests are needed
			return false, false
		}
		connected = cs.headSlot == uint64(bc.head.Slot) && bc.blockRootAt(cs, cs.headSlot) == bc.head.Hash()
	} else {
		// match to next section
		if cs.headSlot < cs.next.parentSlot {
			// there is a gap, more requests are needed
			return false, false
		}
		checkSlot := cs.headSlot
		if cs.next.headSlot < checkSlot {
			checkSlot = cs.next.headSlot
		}
		connected = bc.blockRootAt(cs.next, checkSlot) == bc.blockRootAt(cs, checkSlot)
	}

	switch {
	case c.prev.headCounter < c.headCounter:
	case c.prev.headCounter > c.headCounter:
	default:
		if !connected {
			log.Error("Sections belonging to the same head are not connected")
		}
		return false, false
	}
}

func (bc *BeaconChain) requestWorker() {
	bc.chainMu.Lock()
	for {
		if cs := bc.nextRequest(); cs != nil {
			ctx, cancel := context.WithTimeout(ctx, time.Second*15)
			bc.newHeadReqCancel = append(bc.newHeadReqCancel, cancel)
			cs.requesting = true
			head := bc.head
			var (
				blocks []*BlockData
				err    error
			)
			bc.chainMu.Unlock()
			if bc.dataSource != nil && cs.parentSlot+MaxHeaderFetch >= uint64(head.Slot) {
				blocks, err = bc.dataSource.GetBlocksFromHead(ctx, head, uint64(head.Slot)-cs.parentSlot)
			} else if bc.historicSource != nil {
				blocks, err = bc.historicSource.GetHistoricBlocks(ctx, head, cs.headSlot, cs.headSlot-cs.parentSlot)
			} else {
				log.Error("Historic data source not available") //TODO print only once, ?reset chain?
			}
			if blocks == nil {
				select {
				case <-newHeadCh:
				case <-bc.stopCh:
					return
				}
			}
			bc.chainMu.Lock()
			if blocks != nil {
				cs.requesting = false
				bc.addBlocks(cs, blocks)
			} else {
				cs.remove()
			}
		} else {
			newHeadCh := bc.newHeadCh
			bc.chainMu.Unlock()
			select {
			case <-newHeadCh:
			case <-bc.stopCh:
				return
			}
			bc.chainMu.Lock()
		}
	}
}

//func (cs *chainSection) getBlockBySlot(slot uint64) *BlockData

type beaconHeadTailInfo struct {
	HeadSlot                    uint64
	HeadHash                    common.Hash
	TailShortTerm, TailLongTerm uint64
}

func (bc *BeaconChain) storeHeadTail(batch ethdb.Batch) {
	enc, _ := rlp.EncodeToBytes(&beaconHeadTailInfo{
		HeadSlot:      bc.headSlot,
		HeadHash:      bc.headHash,
		TailShortTerm: bc.tailShortTerm,
		TailLongTerm:  bc.tailLongTerm,
	})
	batch.Put(beaconHeadTailKey, enc)
}

func getBlockDataKey(slot uint64, root common.Hash, byBlockRoot, addRoot bool) []byte {
	var prefix []byte
	if byBlockRoot {
		prefix = blockDataByBlockRootKey
	} else {
		prefix = blockDataKey
	}
	p := len(prefix)
	keyLen := p + 8
	if addRoot {
		keyLen += 32
	}
	dbKey := make([]byte, keyLen)
	copy(dbKey[:p], prefix)
	binary.BigEndian.PutUint64(dbKey[p:p+8], slot)
	if addRoot {
		copy(dbKey[p+8:], root[:])
	}
	return dbKey
}

func (bc *BeaconChain) GetBlockData(slot uint64, hash common.Hash, byBlockRoot bool) *BlockData {
	fmt.Println("GetBlockData", slot, hash, byBlockRoot)
	key := getBlockDataKey(slot, hash, byBlockRoot, true)
	if bd, ok := bc.blockDataCache.Get(string(key)); ok {
		fmt.Println(" cached")
		return bd.(*BlockData)
	}
	var blockData *BlockData

	if byBlockRoot {
		iter := bc.db.NewIterator(getBlockDataKey(slot, common.Hash{}, false, false), nil)
		for iter.Next() {
			blockData = new(BlockData)
			if err := rlp.DecodeBytes(iter.Value(), blockData); err == nil {
				blockData.CalculateRoots()
				if blockData.BlockRoot == hash {
					break
				} else {
					blockData = nil
				}
			} else {
				blockData = nil
				log.Error("Error decoding stored beacon slot data", "slot", slot, "blockRoot", hash, "error", err)
			}
		}
		iter.Release()
	} else {
		if blockDataEnc, err := bc.db.Get(key); err == nil {
			fmt.Println(" found in db")
			blockData = new(BlockData)
			if err := rlp.DecodeBytes(blockDataEnc, blockData); err == nil {
				blockData.CalculateRoots()
				fmt.Println(" decoded")
			} else {
				fmt.Println(" decode err", err)
				blockData = nil
				log.Error("Error decoding stored beacon slot data", "slot", slot, "stateRoot", hash, "error", err)
			}
		} else {
			fmt.Println(" db err", err)
		}
	}

	bc.blockDataCache.Add(string(key), blockData)
	if blockData != nil {
		if byBlockRoot {
			bc.blockDataCache.Add(string(getBlockDataKey(slot, blockData.StateRoot, false, true)), blockData)
		} else {
			bc.blockDataCache.Add(string(getBlockDataKey(slot, blockData.BlockRoot, true, true)), blockData)
		}
	}
	return blockData
}

func (bc *BeaconChain) storeBlockData(blockData *BlockData) {
	fmt.Println("storeBlockData", blockData.Header.Slot, blockData.StateRoot)
	key := getBlockDataKey(blockData.Header.Slot, blockData.StateRoot, false, true)
	bc.blockDataCache.Add(string(key), blockData)
	bc.blockDataCache.Add(string(getBlockDataKey(blockData.Header.Slot, blockData.BlockRoot, true, true)), blockData)
	enc, err := rlp.EncodeToBytes(blockData)
	if err != nil {
		fmt.Println(" encode err", err)
		log.Error("Error encoding beacon slot data for storage", "slot", blockData.Header.Slot, "blockRoot", blockData.BlockRoot, "error", err)
		return
	}
	fmt.Println(" store err", bc.db.Put(key, enc))
}

func (bc *BeaconChain) GetParent(block *BlockData) *BlockData {
	if block.ParentSlotDiff == 0 {
		return nil
	}
	return bc.GetBlockData(block.Header.Slot-block.ParentSlotDiff, block.Header.ParentRoot, true)
}

func getExecNumberKey(execNumber uint64, stateRoot common.Hash, addRoot bool) []byte {
	p := len(execNumberKey)
	keyLen := p + 8
	if addRoot {
		keyLen += 32
	}
	dbKey := make([]byte, keyLen)
	copy(dbKey[:p], execNumberKey)
	binary.BigEndian.PutUint64(dbKey[p:p+8], execNumber)
	if addRoot {
		copy(dbKey[p+8:], stateRoot[:])
	}
	return dbKey
}

type slotAndStateRoot struct {
	slot      uint64
	stateRoot common.Hash
}

type slotsAndStateRoots []slotAndStateRoot

func (bc *BeaconChain) getSlotsAndStateRoots(execNumber uint64) slotsAndStateRoots {
	//bc.execNumberCacheMu.RLock() //TODO
	if v, ok := bc.execNumberCache.Get(execNumber); ok {
		return v.(slotsAndStateRoots)
	}

	var list slotsAndStateRoots
	prefix := getExecNumberKey(execNumber, common.Hash{}, false)
	prefixLen := len(prefix)
	iter := bc.db.NewIterator(prefix, nil)
	for iter.Next() {
		var entry slotAndStateRoot
		if len(iter.Key()) != prefixLen+32 {
			log.Error("Invalid exec number entry key length", "execNumber", execNumber, "length", len(iter.Key()), "expected", prefixLen+32)
			continue
		}
		copy(entry.stateRoot[:], iter.Key()[prefixLen:])
		if err := rlp.DecodeBytes(iter.Value(), &entry.slot); err != nil {
			log.Error("Error decoding stored exec number entry", "execNumber", execNumber, "error", err)
			continue
		}
		list = append(list, entry)
	}
	iter.Release()
	bc.execNumberCache.Add(execNumber, list)
	return list
}

func (bc *BeaconChain) GetBlockDataByExecNumber(ht *HistoricTree, execNumber uint64) *BlockData {
	fmt.Println("GetBlockDataByExecNumber", execNumber)
	list := bc.getSlotsAndStateRoots(execNumber)
	fmt.Println(" list", list)
	for _, entry := range list {
		fmt.Println("  check", ht.GetStateRoot(entry.slot), entry.stateRoot)
		if ht.GetStateRoot(entry.slot) == entry.stateRoot {
			fmt.Println("  GetBlockData", bc.GetBlockData(entry.slot, entry.stateRoot, false))
			return bc.GetBlockData(entry.slot, entry.stateRoot, false)
		}
	}
	return nil
}

func (bc *BeaconChain) storeBlockDataByExecNumber(execNumber uint64, blockData *BlockData) {
	bc.execNumberCache.Remove(execNumber)
	slotEnc, _ := rlp.EncodeToBytes(&blockData.Header.Slot)
	bc.db.Put(getExecNumberKey(execNumber, blockData.StateRoot, true), slotEnc)
}

func getSlotByBlockRootKey(blockRoot common.Hash) []byte {
	p := len(slotByBlockRootKey)
	dbKey := make([]byte, p+32)
	copy(dbKey[:p], slotByBlockRootKey)
	copy(dbKey[p+8:], blockRoot[:])
	return dbKey
}

func (bc *BeaconChain) GetBlockDataByBlockRoot(blockRoot common.Hash) *BlockData {
	dbKey := getSlotByBlockRootKey(blockRoot)
	var slot uint64
	if enc, err := bc.db.Get(dbKey); err == nil {
		if rlp.DecodeBytes(enc, &slot) != nil {
			return nil //TODO error log
		}
	} else {
		bc.blockDataCache.Add(string(dbKey), nil)
		return nil
	}
	blockData := bc.GetBlockData(slot, blockRoot, true)
	bc.blockDataCache.Add(string(dbKey), blockData)
	return blockData
}

func (bc *BeaconChain) storeSlotByBlockRoot(blockData *BlockData) {
	dbKey := getSlotByBlockRootKey(blockData.BlockRoot)
	enc, _ := rlp.EncodeToBytes(&blockData.Header.Slot)
	bc.db.Put(dbKey, enc)
	bc.blockDataCache.Add(string(dbKey), blockData)
}

func (block *BlockData) firstInPeriod() bool {
	newPeriod, oldPeriod := block.Header.Slot>>13, (block.Header.Slot-block.ParentSlotDiff)>>13
	if newPeriod > oldPeriod+1 {
		log.Crit("More than an entire period skipped", "oldSlot", block.Header.Slot-block.ParentSlotDiff, "newSlot", block.Header.Slot)
	}
	return newPeriod > oldPeriod
}

func (block *BlockData) firstInEpoch() bool {
	return block.Header.Slot>>5 > (block.Header.Slot-block.ParentSlotDiff)>>5
}

func ProofFormatForBlock(block *BlockData) byte {
	format := byte(HspLongTerm + HspShortTerm)
	if block.firstInEpoch() {
		format += HspInitData
	}
	return format
}

// proofFormatForBlock returns the minimal required set of state proof fields for a
// given slot according to the current chain tail values. Stored format equals to or
// is a superset of this.
func (bc *BeaconChain) proofFormatForBlock(block *BlockData) byte {
	if block.ParentSlotDiff == 0 {
		return HspLongTerm + HspShortTerm + HspInitData
	}
	var format byte
	if block.Header.Slot >= bc.tailShortTerm {
		format = HspShortTerm
	}
	if block.Header.Slot >= bc.tailLongTerm {
		format += HspLongTerm
		if block.firstInEpoch() {
			format += HspInitData
		}
	}
	return format
}

func (bc *BeaconChain) GetTailSlots() (longTerm, shortTerm uint64) {
	bc.chainMu.RLock()
	longTerm, shortTerm = bc.tailLongTerm, bc.tailShortTerm
	bc.chainMu.RUnlock()
	return
}

func (bc *BeaconChain) pruneBlockFormat(block *BlockData) bool {
	if block.ParentSlotDiff == 0 && block.Header.Slot > bc.tailLongTerm {
		return false
	}
	format := bc.proofFormatForBlock(block)
	if format == block.ProofFormat {
		return true
	}

	var values MerkleValues
	if _, ok := TraverseProof(block.Proof().Reader(nil), NewMultiProofWriter(StateProofFormats[format], &values, nil)); ok {
		block.ProofFormat, block.StateProof = format, values
		if format&HspShortTerm == 0 {
			block.StateRootDiffs = nil
		}
		return true
	}
	return false
}

func (bc *BeaconChain) clearDb() {
	iter := bc.db.NewIterator(nil, nil)
	for iter.Next() {
		bc.db.Delete(iter.Key())
	}
	iter.Release()
	bc.blockDataCache.Purge()
	bc.historicCache.Purge()
	bc.execNumberCache.Purge()
	bc.storedSection = bc.syncHeadSection
}

func (bc *BeaconChain) reset() {
	bc.headSlot, bc.headHash = 0, common.Hash{}
	bc.tailShortTerm, bc.tailLongTerm = 0, 0
	bc.failCounter = 0
	bc.blockRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: blockRootsKey, zeroLevel: 13}}
	bc.stateRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: stateRootsKey, zeroLevel: 13}}
	bc.historicRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: historicRootsKey, zeroLevel: 25}}
	bc.historicTrees = make(map[common.Hash]*HistoricTree)
}

func (bc *BeaconChain) insertBlocks(lastHead *BlockData, blocks []*BlockData) (bool, error) {
	if len(blocks) == 0 {
		return false, nil
	}
	if lastHead != nil {
		fmt.Println("insertBlocks  lastHead.Slot", lastHead.Header.Slot, "blocks[0]", blocks[0].Header.Slot, blocks[0].ParentSlotDiff, "blocks[x]", blocks[len(blocks)-1].Header.Slot)
		oldBlock := lastHead
		index := len(blocks) - 1
		newBlock := blocks[index]
		for oldBlock.BlockRoot != newBlock.Header.ParentRoot {
			if oldBlock.Header.Slot < newBlock.Header.Slot {
				if index == 0 {
					return false, nil
				}
				index--
				newBlock = blocks[index]
			}
			if oldBlock.Header.Slot >= newBlock.Header.Slot {
				if oldBlock = bc.GetParent(oldBlock); oldBlock == nil {
					return false, nil
				}
			}
		}
		blocks = blocks[index:]
	}

	eh, ok := blocks[0].GetStateValue(BsiExecHead)
	if !ok { // should not happen, backend should check proof format
		fmt.Println("proofFormat", blocks[0].ProofFormat)
		return false, errors.New("exec header root not found in beacon state")
	}
	execHeader := bc.execChain.GetHeaderByHash(common.Hash(eh))
	if execHeader == nil {
		return false, errors.New("cannot find exec header")
	}
	execNumber := execHeader.Number.Uint64()
	for _, block := range blocks {
		if !bc.pruneBlockFormat(block) { //TODO itt viszont a tail-ben mindig hagyjuk benne a HspInitData-t
			return false, errors.New("fetched state proofs insufficient")
		}
		bc.storeBlockData(block)
		bc.storeSlotByBlockRoot(block)
		bc.storeBlockDataByExecNumber(execNumber, block)
		execNumber++
	}
	log.Info("Successful BeaconChain insert")
	return true, nil
}

func (bc *BeaconChain) findCloseBlocks(block *BlockData, maxDistance int) (res []*BlockData) {
	dist := make(map[common.Hash]int)
	dist[block.BlockRoot] = 0
	b := block
	firstSlot := b.Header.Slot
	for i := 1; i <= maxDistance; i++ {
		if b = bc.GetParent(b); b == nil {
			break
		}
		res = append(res, b)
		firstSlot = b.Header.Slot
		dist[b.BlockRoot] = i
	}

	var slotEnc [8]byte
	binary.BigEndian.PutUint64(slotEnc[:], firstSlot)
	iter := bc.db.NewIterator(blockDataKey, slotEnc[:])
	for iter.Next() {
		block := new(BlockData)
		if err := rlp.DecodeBytes(iter.Value(), block); err == nil {
			block.CalculateRoots()
			if _, ok := dist[block.BlockRoot]; ok {
				continue
			}
			if d, ok := dist[block.Header.ParentRoot]; ok && d < maxDistance {
				dist[block.BlockRoot] = d + 1
				res = append(res, block)
			}
		} else {
			//TODO error log
		}
	}
	iter.Release()
	return res
}
