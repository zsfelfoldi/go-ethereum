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

package les

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
	blockDataKey            = []byte("b-")  // bigEndian64(slot) + stateRoot -> RLP(beaconBlockData)  (available starting from tailLongTerm)
	blockDataByBlockRootKey = []byte("bb-") // bigEndian64(slot) + blockRoot -> RLP(beaconBlockData)  (not stored in db, only for caching)
	execNumberKey           = []byte("e-")  // bigEndian64(execNumber) + stateRoot -> RLP(slot)  (available starting from tailLongTerm)
	slotByBlockRootKey      = []byte("sb-") // blockRoot -> RLP(slot)  (available for init data blocks and all blocks starting from tailShortTerm)

	stateProofFormats   [hspFormatCount]indexMapFormat
	stateProofIndexMaps [hspFormatCount]map[uint64]int
	beaconHeaderFormat  indexMapFormat
)

const (
	hspLongTerm    = 1 << iota // state proof long term fields (latest_block_header.root, exec_head)
	hspShortTerm               // state proof short term fields (block_roots, state_roots, historic_roots, finalized.root)
	hspInitData                // state proof init data fields (genesis_time, genesis_validators_root, sync_committee_root, next_sync_committee_root)
	hspFormatCount             // number of possible format configurations

	// beacon header fields
	/*	bhiHeaderSlot          = 8
		bhiHeaderProposerIndex = 9
		bhiHeaderParentRoot    = 10
		bhiHeaderStateRoot     = 11
		bhiHeaderBodyRoot      = 12*/

	// beacon state fields		//TODO ??? fork-ot nem kene state-bol ellenorizni?
	bsiGenesisTime       = 32
	bsiGenesisValidators = 33
	bsiForkVersion       = 141 //TODO ??? osszes fork field? long vagy short term?
	bsiLatestHeader      = 36
	bsiBlockRoots        = 37
	bsiStateRoots        = 38
	bsiHistoricRoots     = 39
	bsiFinalBlock        = 105
	bsiSyncCommittee     = 54
	bsiNextSyncCommittee = 55
	bsiExecHead          = 908 // ??? 56

	reverseSyncLimit = 128
)

func init() {
	// initialize header proof format
	beaconHeaderFormat = newIndexMapFormat()
	for i := uint64(8); i < 13; i++ {
		beaconHeaderFormat.addLeaf(i, nil)
	}

	// initialize beacon state proof formats and index maps
	for i := range stateProofFormats {
		stateProofFormats[i] = newIndexMapFormat()
		if i&hspLongTerm != 0 {
			stateProofFormats[i].addLeaf(bsiLatestHeader, nil)
			stateProofFormats[i].addLeaf(bsiExecHead, nil)
		}
		if i&hspShortTerm != 0 {
			stateProofFormats[i].addLeaf(bsiStateRoots, nil)
			stateProofFormats[i].addLeaf(bsiHistoricRoots, nil)
			stateProofFormats[i].addLeaf(bsiFinalBlock, nil)
		}
		if i&hspInitData != 0 {
			stateProofFormats[i].addLeaf(bsiGenesisTime, nil)
			stateProofFormats[i].addLeaf(bsiGenesisValidators, nil)
			stateProofFormats[i].addLeaf(bsiForkVersion, nil)
			stateProofFormats[i].addLeaf(bsiSyncCommittee, nil)
			stateProofFormats[i].addLeaf(bsiNextSyncCommittee, nil)
		}
		stateProofIndexMaps[i] = proofIndexMap(stateProofFormats[i])
	}
}

type chainIterator interface { // no locking needed
	getParent(*beaconBlockData) *beaconBlockData
	proofFormatForBlock(*beaconBlockData) byte //TODO tail-tol fuggetlen legyen? mindenesetre lockolni ne kelljen
}

type beaconData interface {
	// if connected is false then first block is not expected to have ParentSlotDiff and StateRootDiffs set but is expected to have hspInitData
	getBlocksFromHead(ctx context.Context, head common.Hash, lastHead *beaconBlockData) (blocks []*beaconBlockData, connected bool, err error)
	getRootsProof(ctx context.Context, block *beaconBlockData) (multiProof, multiProof, error)
	getHistoricRootsProof(ctx context.Context, block *beaconBlockData, period uint64) (multiProof, error)
	getSyncCommittees(ctx context.Context, block *beaconBlockData) ([]byte, []byte, error)
	getBestUpdate(ctx context.Context, period uint64) (*lightClientUpdate, []byte, error)
}

/*type beaconBackfillData interface {
	// if connected is false then first block is not expected to have ParentSlotDiff and StateRootDiffs set
	getBlocks(ctx context.Context, head common.Hash, lastSlot, maxAmount uint64, lastHead *beaconBlockData, getParent func(*beaconBlockData) *beaconBlockData) (blocks []*beaconBlockData, connected bool, err error)
	getStateRoots(ctx context.Context, blockHash common.Hash, period uint64) (merkleValues, error)
	getHistoricRoots(ctx context.Context, blockHash common.Hash, period uint64) (blockRoots, stateRoots merkleValue, path merkleValues, err error)
	getSyncCommittee(ctx context.Context, period uint64, committeeRoot common.Hash) ([]byte, error)
	getBestUpdate(ctx context.Context, period uint64) (*lightClientUpdate, []byte, error)
	subscribeSignedHead(cb func(signedBeaconHead), bestHeads, minSignerCount, level uint)
}*/

type execChain interface {
	GetHeader(common.Hash, uint64) *types.Header
	GetHeaderByHash(common.Hash) *types.Header
}

type beaconChain struct {
	dataSource     beaconData
	execChain      execChain
	sct            *syncCommitteeTracker
	db             ethdb.Database
	failCounter    int
	blockDataCache *lru.Cache // string(dbKey) -> *beaconBlockData  (either blockDataKey, slotByBlockRootKey or blockDataByBlockRootKey)  //TODO use separate cache?
	historicCache  *lru.Cache // string(dbKey) -> merkleValue (either stateRootsKey or historicRootsKey)

	execNumberCacheMu sync.RWMutex //TODO ???
	execNumberCache   *lru.Cache   // uint64(execNumber) -> []struct{slot, stateRoot}

	chainMu                                   sync.Mutex
	headSlot                                  uint64
	headHash                                  common.Hash
	tailShortTerm, tailLongTerm, tailInitData uint64 // shortTerm >= initData >= longTerm
	blockRoots, stateRoots, historicRoots     *merkleListVersion

	historicMu    sync.RWMutex
	historicTrees map[common.Hash]*historicTree
}

type beaconHeader struct {
	Slot          common.Decimal `json:"slot"` //TODO signedBeaconHead RLP encoding is jo??
	ProposerIndex common.Decimal `json:"proposer_index"`
	ParentRoot    common.Hash    `json:"parent_root"`
	StateRoot     common.Hash    `json:"state_root"`
	BodyRoot      common.Hash    `json:"body_root"`
}

func (bh *beaconHeader) hash() common.Hash {
	var values [8]merkleValue //TODO ezt lehetne szebben is
	binary.LittleEndian.PutUint64(values[0][:8], uint64(bh.Slot))
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = merkleValue(bh.ParentRoot)
	values[3] = merkleValue(bh.StateRoot)
	values[4] = merkleValue(bh.BodyRoot)
	//fmt.Println("hashing full header", bh, values)
	return multiProof{format: newRangeFormat(8, 15, nil), values: values[:]}.rootHash()
}

type beaconHeaderWithoutState struct {
	Slot                 uint64
	ProposerIndex        uint
	ParentRoot, BodyRoot common.Hash
}

func (bh *beaconHeaderWithoutState) hash(stateRoot common.Hash) common.Hash {
	var values [8]merkleValue //TODO ezt lehetne szebben is
	binary.LittleEndian.PutUint64(values[0][:8], bh.Slot)
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = merkleValue(bh.ParentRoot)
	values[3] = merkleValue(stateRoot)
	values[4] = merkleValue(bh.BodyRoot)
	return multiProof{format: newRangeFormat(8, 15, nil), values: values[:]}.rootHash()
}

type beaconBlockData struct {
	Header               beaconHeaderWithoutState
	stateRoot, blockRoot common.Hash // calculated by calculateRoots()
	ProofFormat          byte
	StateProof           merkleValues
	ParentSlotDiff       uint64       // slot-parentSlot; 0 if not initialized
	StateRootDiffs       merkleValues // only valid if ParentSlotDiff is initialized
}

func (block *beaconBlockData) proof() multiProof {
	return multiProof{format: stateProofFormats[block.ProofFormat], values: block.StateProof}
}

func (block *beaconBlockData) mustGetStateValue(index uint64) merkleValue {
	proofIndex, ok := stateProofIndexMaps[block.ProofFormat][index]
	if !ok {
		panic(nil)
	}
	return block.StateProof[proofIndex]
}

func (block *beaconBlockData) calculateRoots() {
	block.stateRoot = block.proof().rootHash()
	block.blockRoot = block.Header.hash(block.stateRoot)
}

func newBeaconChain(dataSource beaconData, execChain execChain, sct *syncCommitteeTracker, db ethdb.Database) *beaconChain {
	chainDb := rawdb.NewTable(db, "bc-")
	blockDataCache, _ := lru.New(2000)
	historicCache, _ := lru.New(20000)
	execNumberCache, _ := lru.New(2000)
	bc := &beaconChain{
		dataSource:      dataSource,
		execChain:       execChain,
		db:              chainDb,
		blockDataCache:  blockDataCache,
		historicCache:   historicCache,
		execNumberCache: execNumberCache,
		sct:             sct,
	}
	bc.reset()
	if enc, err := bc.db.Get(beaconHeadTailKey); err == nil {
		var ht beaconHeadTailInfo
		if rlp.DecodeBytes(enc, &ht) == nil {
			bc.headSlot, bc.headHash = ht.HeadSlot, ht.HeadHash
			bc.tailShortTerm, bc.tailLongTerm, bc.tailInitData = ht.TailShortTerm, ht.TailLongTerm, ht.TailInitData
		}
	}
	if bc.headHash == (common.Hash{}) {
		bc.clearDb()
	}
	return bc
}

type beaconHeadTailInfo struct {
	HeadSlot                                  uint64
	HeadHash                                  common.Hash
	TailShortTerm, TailLongTerm, TailInitData uint64
}

func (bc *beaconChain) storeHeadTail(batch ethdb.Batch) {
	enc, _ := rlp.EncodeToBytes(&beaconHeadTailInfo{
		HeadSlot:      bc.headSlot,
		HeadHash:      bc.headHash,
		TailShortTerm: bc.tailShortTerm,
		TailLongTerm:  bc.tailLongTerm,
		TailInitData:  bc.tailInitData,
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

func (bc *beaconChain) getBlockData(slot uint64, hash common.Hash, byBlockRoot bool) *beaconBlockData {
	key := getBlockDataKey(slot, hash, byBlockRoot, true)
	if bd, ok := bc.blockDataCache.Get(string(key)); ok {
		return bd.(*beaconBlockData)
	}
	var blockData *beaconBlockData

	if byBlockRoot {
		iter := bc.db.NewIterator(getBlockDataKey(slot, common.Hash{}, false, false), nil)
		for iter.Next() {
			blockData = new(beaconBlockData)
			if err := rlp.DecodeBytes(iter.Value(), blockData); err == nil {
				blockData.calculateRoots()
				if blockData.blockRoot == hash {
					break
				} else {
					blockData = nil
				}
			} else {
				blockData = nil
				log.Error("Error decoding stored beacon slot data", "slot", slot, "blockRoot", hash, "error", err)
			}
		}
	} else {
		if blockDataEnc, err := bc.db.Get(key); err != nil {
			blockData = new(beaconBlockData)
			if err := rlp.DecodeBytes(blockDataEnc, blockData); err == nil {
				blockData.calculateRoots()
			} else {
				blockData = nil
				log.Error("Error decoding stored beacon slot data", "slot", slot, "stateRoot", hash, "error", err)
			}
		}
	}

	bc.blockDataCache.Add(string(key), blockData)
	if byBlockRoot {
		bc.blockDataCache.Add(string(getBlockDataKey(slot, blockData.stateRoot, false, true)), blockData)
	} else {
		bc.blockDataCache.Add(string(getBlockDataKey(slot, blockData.blockRoot, true, true)), blockData)
	}
	return blockData
}

func (bc *beaconChain) storeBlockData(blockData *beaconBlockData) {
	key := getBlockDataKey(blockData.Header.Slot, blockData.stateRoot, false, true)
	bc.blockDataCache.Add(string(key), blockData)
	bc.blockDataCache.Add(string(getBlockDataKey(blockData.Header.Slot, blockData.blockRoot, true, true)), blockData)
	enc, err := rlp.EncodeToBytes(blockData)
	if err != nil {
		log.Error("Error encoding beacon slot data for storage", "slot", blockData.Header.Slot, "blockRoot", blockData.blockRoot, "error", err)
		return
	}
	bc.db.Put(key, enc)
}

func (bc *beaconChain) getParent(block *beaconBlockData) *beaconBlockData {
	if block.ParentSlotDiff == 0 {
		return nil
	}
	return bc.getBlockData(block.Header.Slot-block.ParentSlotDiff, block.Header.ParentRoot, true)
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

func (bc *beaconChain) getSlotsAndStateRoots(execNumber uint64) slotsAndStateRoots {
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
	bc.execNumberCache.Add(execNumber, list)
	return list
}

func (bc *beaconChain) getBlockDataByExecNumber(ht *historicTree, execNumber uint64) *beaconBlockData {
	list := bc.getSlotsAndStateRoots(execNumber)
	for _, entry := range list {
		if ht.getStateRoot(entry.slot) == entry.stateRoot {
			return bc.getBlockData(entry.slot, entry.stateRoot, false)
		}
	}
	return nil
}

func (bc *beaconChain) storeBlockDataByExecNumber(execNumber uint64, blockData *beaconBlockData) {
	bc.execNumberCache.Remove(execNumber)
	slotEnc, _ := rlp.EncodeToBytes(&blockData.Header.Slot)
	bc.db.Put(getExecNumberKey(execNumber, blockData.stateRoot, true), slotEnc)
}

func getSlotByBlockRootKey(blockRoot common.Hash) []byte {
	p := len(slotByBlockRootKey)
	dbKey := make([]byte, p+32)
	copy(dbKey[:p], slotByBlockRootKey)
	copy(dbKey[p+8:], blockRoot[:])
	return dbKey
}

func (bc *beaconChain) getBlockDataByBlockRoot(blockRoot common.Hash) *beaconBlockData {
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
	blockData := bc.getBlockData(slot, blockRoot, true)
	bc.blockDataCache.Add(string(dbKey), blockData)
	return blockData
}

func (bc *beaconChain) storeSlotByBlockRoot(blockData *beaconBlockData) {
	dbKey := getSlotByBlockRootKey(blockData.blockRoot)
	enc, _ := rlp.EncodeToBytes(&blockData.Header.Slot)
	bc.db.Put(dbKey, enc)
	bc.blockDataCache.Add(string(dbKey), blockData)
}

// proofFormatForBlock returns the minimal required set of state proof fields for a
// given slot according to the current chain tail values. Stored format equals to or
// is a superset of this.
func (bc *beaconChain) proofFormatForBlock(block *beaconBlockData) byte {
	if block.ParentSlotDiff == 0 {
		return hspLongTerm + hspShortTerm + hspInitData
	}
	slot := block.Header.Slot
	var format byte
	if slot >= bc.tailShortTerm {
		format += hspShortTerm
	}
	if slot >= bc.tailLongTerm {
		format += hspLongTerm
	}
	if slot >= bc.tailInitData && (slot>>5 > (slot-block.ParentSlotDiff)>>5) {
		format += hspInitData
	}
	return format
}

func (bc *beaconChain) pruneBlockFormat(block *beaconBlockData) bool {
	if block.ParentSlotDiff == 0 && block.Header.Slot > bc.tailLongTerm {
		return false
	}
	format := bc.proofFormatForBlock(block)
	if format == block.ProofFormat {
		return true
	}

	var values merkleValues
	if _, ok := traverseProof(block.proof().reader(nil), newMultiProofWriter(stateProofFormats[format], &values, nil)); ok {
		block.ProofFormat, block.StateProof = format, values
		if format&hspShortTerm == 0 {
			block.StateRootDiffs = nil
		}
		return true
	}
	return false
}

func (bc *beaconChain) clearDb() {
	iter := bc.db.NewIterator(nil, nil)
	for iter.Next() {
		bc.db.Delete(iter.Key())
	}
	bc.blockDataCache.Purge()
	bc.historicCache.Purge()
	bc.execNumberCache.Purge()
	bc.sct.clearDb()
}

func (bc *beaconChain) reset() {
	bc.headSlot, bc.headHash = 0, common.Hash{}
	bc.tailShortTerm, bc.tailLongTerm, bc.tailInitData = 0, 0, 0
	bc.failCounter = 0
	bc.blockRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: blockRootsKey, zeroLevel: 13}}
	bc.stateRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: stateRootsKey, zeroLevel: 13}}
	bc.historicRoots = &merkleListVersion{list: &merkleList{db: bc.db, cache: bc.historicCache, dbKey: historicRootsKey, zeroLevel: 25}}
	bc.historicTrees = make(map[common.Hash]*historicTree)
}

func (bc *beaconChain) setHead(hash common.Hash) {
	if err := bc.trySetHead(hash); err == nil {
		log.Info("beaconChain.setHead successful")
		bc.failCounter = 0
	} else {
		bc.failCounter++
		log.Error("Error setting beacon chain head", "attempt", bc.failCounter, "reset after", 100, "error", err)
		if bc.failCounter == 100 {
			bc.clearDb()
			bc.reset()
			log.Warn("Beacon chain has been reset to empty state")
		}
	}
}

func (bc *beaconChain) trySetHead(headHash common.Hash) error {
	ctx := context.Background() //TODO

	var lastHead *beaconBlockData
	bc.chainMu.Lock()
	if bc.headHash != (common.Hash{}) {
		lastHead = bc.getBlockData(bc.headSlot, bc.headHash, true)
		if lastHead == nil {
			log.Error("Head beacon block not found in database")
		}
	}
	log.Info("beaconChain.trySetHead", "old head slot", bc.headSlot, "old head hash", bc.headHash, "found in db", lastHead != nil)
	bc.chainMu.Unlock()

	blocks, connected, err := bc.dataSource.getBlocksFromHead(ctx, headHash, lastHead)
	if err != nil {
		return err
	}
	log.Info("Retrieved beaconBlockData", "len", len(blocks), "connected", connected)
	if lastHead != nil && !connected {
		return errors.New("fetched blocks not connected to the existing chain")
	}
	if len(blocks) == 0 {
		if connected {
			return nil
		}
		return errors.New("no blocks fetched and not connected to the existing chain")
	}
	var headTree *historicTree
	if lastHead == nil {
		if err := bc.initChain(ctx, blocks[0]); err != nil {
			bc.clearDb()
			return err
		}
		var err error
		if headTree, err = bc.initHistoricTrees(ctx, blocks[0]); err != nil {
			bc.clearDb()
			return err
		}
		log.Info("Successful beaconChain init")
	} else {
		headTree = bc.newHistoricTree(lastHead)
	}

	//fmt.Println("exec block hash", common.Hash(blocks[0].mustGetStateValue(bsiExecHead)))
	execHeader := bc.execChain.GetHeaderByHash(common.Hash(blocks[0].mustGetStateValue(bsiExecHead)))
	if execHeader == nil {
		return errors.New("cannot find exec header")
	}
	execNumber := execHeader.Number.Uint64()
	if lastHead == nil {
		tail := blocks[0].Header.Slot
		bc.tailInitData, bc.tailLongTerm, bc.tailShortTerm = tail, tail, tail
		log.Info("Tail slot set", "tail", tail)
	}
	for _, block := range blocks {
		if !bc.pruneBlockFormat(block) { //TODO itt viszont a tail-ben mindig hagyjuk benne a hspInitData-t
			return errors.New("fetched state proofs insufficient")
		}
		bc.storeBlockData(block)
		bc.storeSlotByBlockRoot(block)
		bc.storeBlockDataByExecNumber(execNumber, block)
		execNumber++
	}
	log.Info("Successful beaconChain insert")

	headBlock := blocks[len(blocks)-1]
	headSlot := headBlock.Header.Slot
	headHash = headBlock.blockRoot
	nextPeriod := bc.sct.getNextPeriod()
	nextPeriodStart := nextPeriod << 13
	if headSlot >= nextPeriodStart+8000 {
		fmt.Println("Fetching best update for next period", nextPeriod)
		if err := bc.fetchBestUpdate(ctx, nextPeriod); err != nil {
			return err
		}
	}
	if headSlot >= nextPeriodStart && bc.headSlot < nextPeriodStart && nextPeriod-1 > bc.sct.getInitData().Period {
		fmt.Println("Fetching best update again for last period", nextPeriod-1)
		if err := bc.fetchBestUpdate(ctx, nextPeriod-1); err != nil {
			return err
		}
	}

	//fmt.Println("headBlock.root", headBlock.blockRoot)
	if err := headTree.moveToHead(headBlock); err != nil {
		return err
	}
	newTreeMap := make(map[common.Hash]*historicTree)
	newTreeMap[headBlock.blockRoot] = headTree
	for _, block := range bc.findCloseBlocks(headBlock, maxHistoricTreeDistance) {
		//fmt.Println("close block.root", block.blockRoot)
		if newTreeMap[block.blockRoot], err = headTree.makeChildTree(block); err != nil {
			return err
		}
	}

	bc.chainMu.Lock()
	batch := bc.db.NewBatch()
	bc.commitHistoricTree(batch, headTree)
	// internal consistency check of state roots tree updates
	var period uint64
	if headSlot > 0 {
		period = (headSlot - 1) >> 13
	}
	//if headTree.state.get(period, 1) != headBlock.mustGetStateValue(bsiStateRoots) {
	fmt.Println("*** stateRoots", headBlock.mustGetStateValue(bsiStateRoots), "headTree.state root", headTree.state.get(period, 1))
	//}
	//if headTree.historic.get(0, 1) != headBlock.mustGetStateValue(bsiHistoricRoots) {
	fmt.Println("*** historicRoots", headBlock.mustGetStateValue(bsiHistoricRoots), "headTree.historic root", headTree.historic.get(0, 1))
	//}
	log.Info("Successful historicTree check, setting head")
	if headBlock.ProofFormat&hspInitData != 0 {
		fmt.Println("forkVersion", headBlock.mustGetStateValue(bsiForkVersion))
		fmt.Println("genesisValidatorsRoot", headBlock.mustGetStateValue(bsiGenesisValidators))
	}
	bc.headSlot, bc.headHash = headSlot, headHash
	bc.storeHeadTail(batch)
	batch.Write()
	bc.historicMu.Lock()
	bc.historicTrees = newTreeMap
	bc.historicMu.Unlock()
	bc.chainMu.Unlock()
	return nil
}

func (bc *beaconChain) fetchBestUpdate(ctx context.Context, period uint64) error {
	committee := bc.sct.getSyncCommittee(period)
	if committee == nil {
		return errors.New("missing sync committee for requested period")
	}
	if update, nextCommittee, err := bc.dataSource.getBestUpdate(ctx, period); err == nil {
		fmt.Println(" getBestUpdate", period, "update header period", update.Header.Slot>>13)
		if period != uint64(update.Header.Slot)>>13 {
			return errors.New("received best update for wrong period")
		}
		if bc.sct.insertUpdate(update, committee, nextCommittee) != sciSuccess {
			return errors.New("cannot insert best update")
		}
		return nil
	} else {
		return err
	}
}

func (bc *beaconChain) findCloseBlocks(block *beaconBlockData, maxDistance int) (res []*beaconBlockData) {
	dist := make(map[common.Hash]int)
	dist[block.blockRoot] = 0
	b := block
	firstSlot := b.Header.Slot
	for i := 1; i <= maxDistance; i++ {
		if b = bc.getParent(b); b == nil {
			break
		}
		res = append(res, b)
		firstSlot = b.Header.Slot
		dist[b.blockRoot] = i
	}

	var slotEnc [8]byte
	binary.BigEndian.PutUint64(slotEnc[:], firstSlot)
	iter := bc.db.NewIterator(blockDataKey, slotEnc[:])
	for iter.Next() {
		block := new(beaconBlockData)
		if err := rlp.DecodeBytes(iter.Value(), block); err == nil {
			block.calculateRoots()
			if _, ok := dist[block.blockRoot]; ok {
				continue
			}
			if d, ok := dist[block.Header.ParentRoot]; ok && d < maxDistance {
				dist[block.blockRoot] = d + 1
				res = append(res, block)
			}
		} else {
			//TODO error log
		}
	}
	return res
}

func (bc *beaconChain) initChain(ctx context.Context, block *beaconBlockData) error {
	if block.ProofFormat&hspInitData == 0 {
		return errors.New("init data not found in fetched beacon states")
	}
	bc.clearDb()
	initData, committee, nextCommittee, err := bc.fetchInitData(ctx, block)
	if err != nil {
		return err //TODO wrap the error message?
	}
	bc.sct.init(initData, committee, nextCommittee)
	return nil
}

func (bc *beaconChain) fetchInitData(ctx context.Context, block *beaconBlockData) (initData *lightClientInitData, committee, nextCommittee []byte, err error) {
	gt := block.mustGetStateValue(bsiGenesisTime)
	initData = &lightClientInitData{
		Checkpoint:            block.blockRoot,
		Period:                block.Header.Slot >> 13,
		GenesisTime:           binary.LittleEndian.Uint64(gt[:]), //TODO check if encoding is correct
		GenesisValidatorsRoot: common.Hash(block.mustGetStateValue(bsiGenesisValidators)),
		CommitteeRoot:         common.Hash(block.mustGetStateValue(bsiSyncCommittee)),
		NextCommitteeRoot:     common.Hash(block.mustGetStateValue(bsiNextSyncCommittee)),
	}
	if committee, nextCommittee, err = bc.dataSource.getSyncCommittees(ctx, block); err != nil {
		return nil, nil, nil, err
	}
	return
}

type beaconFork struct {
	epoch   uint64
	version []byte
	domain  merkleValue
}

type beaconForks []beaconFork

func (bf beaconForks) version(epoch uint64) []byte {
	for i := len(bf) - 1; i >= 0; i-- {
		if epoch >= bf[i].epoch {
			return bf[i].version
		}
	}
	log.Error("Fork version unknown", "epoch", epoch)
	return nil
}

func (bf beaconForks) domain(epoch uint64) merkleValue {
	for i := len(bf) - 1; i >= 0; i-- {
		if epoch >= bf[i].epoch {
			return bf[i].domain
		}
	}
	log.Error("Fork domain unknown", "epoch", epoch)
	return merkleValue{}
}

func (bf beaconForks) computeDomains(genesisValidatorsRoot common.Hash) {
	for i := range bf {
		bf[i].domain = computeDomain(bf[i].version, genesisValidatorsRoot)
	}
}

/*type GetBeaconSlotsPacket struct {
	ReqID           uint64
	BeaconHead      common.Hash // recent beacon block hash used as a reference to the canonical chain state (client already has the header)
	LastSlot        uint64      // last slot of requested range (<= beacon_head.slot)
	MaxSlots        uint64      // maximum number of retrieved slots
	ProofFormatMask byte        // requested state fields (where available); bits correspond to hsp* constants
	LastBeaconHead  common.Hash `rlp:"optional"` // optional beacon block hash; retrieval stops before the common ancestor
}

type BeaconSlotsPacket struct {
	ReqID, BV uint64
	// MultiProof contains state proofs for the requested blocks; included state fields for each block are defined in ProofFormat
	// - head block state is proven directly from beacon_head.state_root
	// - states not older than 8192 slots are proven from beacon_head.state_roots[slot % 8192]
	// - states older than 8192 slots are proven from beacon_head.historic_roots[slot / 8192].state_roots[slot % 8192]
	FirstSlot         uint64
	StateProofFormats []byte                        // slot index equals FirstSlot plus slice index
	ProofValues       merkleValues                  // external value multiproof for block and state roots (format is determined by BeaconHead.Slot, LastSlot and length of ProofFormat)
	FirstParentRoot   common.Hash                   // used for reconstructing all header parent roots
	Headers           []beaconHeaderForTransmission // one for each slot where state proof format includes hspLongTerm
}

type beaconBlockData struct {
	Header               beaconHeaderWithoutState
	stateRoot, blockRoot common.Hash // calculated by calculateRoots()
	ProofFormat          byte
	StateProof           merkleValues
	ParentSlotDiff       uint64       // slot-parentSlot; 0 if not initialized
	StateRootDiffs       merkleValues // only valid if ParentSlotDiff is initialized
}
*/
/*
func validateBeaconSlots(header *beaconHeader, request *GetBeaconSlotsPacket, reply *BeaconSlotsPacket, hasBlockData func(uint64, common.Hash) bool) (blocks []*beaconBlockData, connected bool, err error) {
	// check that the returned range is as expected
	firstSlot, lastSlot := reply.FirstSlot, reply.FirstSlot+uint64(len(reply.StateProofFormats))-1
	expLastSlot := request.LastSlot
	if expLastSlot > header.Slot {
		expLastSlot = header.Slot
	}
	var expFirstSlot uint64
	if request.MaxSlots <= expLastSlot {
		expFirstSlot = expLastSlot + 1 - request.MaxSlots
	}
	if request.ProofFormatMask == 0 {
		if firstSlot != expFirstSlot || lastSlot != expLastSlot {
			return nil, false, errors.New("Returned slot range incorrect")
		}
	} else {
		if lastSlot < expLastSlot || firstSlot > expLastSlot { //TODO ??? first check
			return nil, false, errors.New("Returned slot range incorrect")
		}
		for slot := expLastSlot; slot <= lastSlot; slot++ {
			format := reply.StateProofFormats[int(slot-firstSlot)]
			if (format != 0) != (slot == lastSlot) {
				return nil, false, errors.New("Returned slot range incorrect")
			}
		}
	}

	reader := multiProof{format: slotRangeFormat(header.Slot, reply.FirstSlot, reply.StateProofFormats), values: reply.ProofValues}.reader(nil)
	target := make([]*merkleValues, len(reply.StateProofFormats))
	for i := range target {
		target[i] = new(merkleValues)
	}
	writer := slotRangeWriter(header.Slot, reply.FirstSlot, reply.StateProofFormats, target)
	if stateRoot, ok := traverseProof(reader, writer); ok && reader.exhausted() {
		if stateRoot != header.StateRoot {
			return nil, false, errors.New("Multiproof root hash does not match")
		}
	} else {
		return nil, false, errors.New("Multiproof format error")
	}
	blocks = make([]*beaconBlockData, len(reply.Headers))
	lastRoot := reply.FirstParentRoot
	var (
		blockPtr       int
		stateRootDiffs merkleValues
	)
	slot := reply.FirstSlot
	for i, format := range reply.StateProofFormats {
		if format == 0 {
			stateRootDiffs = append(stateRootDiffs, (*target[i])[0])
		} else {
			if blockPtr >= len(reply.Headers) {
				return nil, false, errors.New("Not enough beacon headers")
			}
			header := reply.Headers[blockPtr]
			block := &beaconBlockData{
				Header: beaconHeaderWithoutState{
					Slot:          slot,
					ProposerIndex: header.ProposerIndex,
					BodyRoot:      header.BodyRoot,
					ParentRoot:    lastRoot,
				},
				ProofFormat:    format,
				StateProof:     *target[i],
				ParentSlotDiff: uint64(len(stateRootDiffs) + 1), //TODO first one?
				StateRootDiffs: stateRootDiffs,
			}
			block.calculateRoots()
			lastRoot = block.blockRoot
			blocks[blockPtr] = block
			blockPtr++
		}
		slot++
	}
	if blockPtr != len(reply.Headers) {
		return nil, false, errors.New("Too many beacon headers")
	}
	//TODO return state roots only
}
*/
