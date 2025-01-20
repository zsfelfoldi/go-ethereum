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
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/log"
)

// checkpoint allows the log indexer to start indexing from the given block
// instead of genesis at the correct absolute log value index.
type checkpointList []epochCheckpoint

type epochCheckpoint struct {
	blockNumber  uint64 // block that generated the last log value of the given epoch
	blockId    common.Hash
	firstLvIndex uint64 // first log value index of the given block
}

var checkpoints = []checkpointList{}

// FilterMaps is the in-memory representation of the log index structure that is
// responsible for building and updating the index according to the canonical
// chain.
// Note that FilterMaps implements the same data structure as proposed in EIP-7745
// without the tree hashing and consensus changes:
// https://eips.ethereum.org/EIPS/eip-7745
type FilterMaps struct {
	closeCh               chan struct{}
	closeWg               sync.WaitGroup
	history, unindexLimit uint64
	noHistory             bool
	Params

	db ethdb.KeyValueStore

	// fields written by the indexer and read by matcher backend. Indexer can
	// read them without a lock and write them under indexLock write lock.
	// Matcher backend can read them under indexLock read lock.
	indexLock sync.RWMutex
	filterMapsRange
	indexedView chainView // always consistent with the log index

	// also accessed by indexer and matcher backend but no locking needed.
	filterMapCache *lru.Cache[uint32, filterMap]
	lastBlockCache *lru.Cache[uint32, lastBlockOfMap]
	lvPointerCache *lru.Cache[uint64, uint64]

	// the matchers set and the fields of FilterMapsMatcherBackend instances are
	// read and written both by exported functions and the indexer.
	// Note that if both indexLock and matchersLock needs to be locked then
	// indexLock should be locked first.
	matchersLock sync.Mutex
	matchers     map[*FilterMapsMatcherBackend]struct{}

	// fields only accessed by the indexer (no mutex required).
	renderSnapshots                                                        *lru.Cache[uint64, *renderedMap]
	startHeadUpdate, loggedHeadUpdate, loggedTailExtend, loggedTailUnindex bool
	startedHeadUpdate, startedTailExtend, startedTailUnindex               time.Time
	lastLogHeadUpdate, lastLogTailExtend, lastLogTailUnindex               time.Time
	ptrHeadUpdate, ptrTailExtend, ptrTailUnindex                           uint64

	targetView         chainView
	matcherSyncRequest *FilterMapsMatcherBackend
	stop               bool
	targetViewCh       chan chainView
	matcherSyncCh      chan *FilterMapsMatcherBackend
	waitIdleCh         chan chan bool
	
	// test hooks
	testDisableSnapshots, testSnapshotUsed bool
}

// filterMap is a full or partial in-memory representation of a filter map where
// rows are allowed to have a nil value meaning the row is not stored in the
// structure. Note that therefore a known empty row should be represented with
// a zero-length slice.
// It can be used as a memory cache or an overlay while preparing a batch of
// changes to the structure. In either case a nil value should be interpreted
// as transparent (uncached/unchanged).
type filterMap []FilterRow

func (fm filterMap) copy() filterMap {
	c := make(filterMap, len(fm))
	copy(c, fm)
	return c
}

// FilterRow encodes a single row of a filter map as a list of column indices.
// Note that the values are always stored in the same order as they were added
// and if the same column index is added twice, it is also stored twice.
// Order of column indices and potential duplications do not matter when searching
// for a value but leaving the original order makes reverting to a previous state
// simpler.
type FilterRow []uint32

func (a FilterRow) Equal(b FilterRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if b[i] != v {
			return false
		}
	}
	return true
}

// filterMapsRange describes the block range that has been indexed and the log
// value index range it has been mapped to.
// Note that tailBlockLvPointer points to the earliest log value index belonging
// to the tail block while tailLvPointer points to the earliest log value index
// added to the corresponding filter map. The latter might point to an earlier
// index after tail blocks have been unindexed because we do not remove tail
// values one by one, rather delete entire maps when all blocks that had log
// values in those maps are unindexed.
type filterMapsRange struct {
	initialized        bool
	targetBlockNumber    uint64
	targetBlockId      common.Hash
	headBlockDelimiter uint64 // zero if afterLastIndexedBlock != targetBlockNumber
	// if initialized then all maps are rendered between firstRenderedMap and
	// afterLastRenderedMap-1
	firstRenderedMap, afterLastRenderedMap uint32
	// if tailPartialEpoch > 0 then maps between firstRenderedMap-mapsPerEpoch and
	// firstRenderedMap-mapsPerEpoch+tailPartialEpoch-1 are rendered
	tailPartialEpoch uint32
	// if initialized then all log values belonging to blocks between
	// firstIndexedBlock and afterLastIndexedBlock are fully rendered
	// blockLvPointers are available between firstIndexedBlock and afterLastIndexedBlock-1
	firstIndexedBlock, afterLastIndexedBlock uint64
}

func (fmr *filterMapsRange) hasIndexedBlocks() bool {
	return fmr.initialized && fmr.afterLastIndexedBlock > fmr.firstIndexedBlock
}

type lastBlockOfMap struct {
	number uint64
	id common.Hash
}

// NewFilterMaps creates a new FilterMaps and starts the indexer in order to keep
// the structure in sync with the given blockchain.
func NewFilterMaps(db ethdb.KeyValueStore, initView chainView, params Params, history, unindexLimit uint64, noHistory bool) *FilterMaps {
	fmt.Println("create")
	rs, initialized, err := rawdb.ReadFilterMapsRange(db)
	if err != nil {
		log.Error("Error reading log index range", "error", err)
	}
	params.deriveFields()
	f := &FilterMaps{
		db:           db,
		closeCh:      make(chan struct{}),
		waitIdleCh:   make(chan chan bool),
		targetViewCh: make(chan chainView),
		history:      history,
		noHistory:    noHistory,
		unindexLimit: unindexLimit,
		Params:       params,
		filterMapsRange: filterMapsRange{
			initialized:           initialized,
			targetBlockId:         rs.TargetBlockId,
			targetBlockNumber:     rs.TargetBlockNumber,
			headBlockDelimiter:    rs.HeadBlockDelimiter,
			firstIndexedBlock:     rs.FirstIndexedBlock,
			afterLastIndexedBlock: rs.AfterLastIndexedBlock,
			firstRenderedMap:      rs.FirstRenderedMap,
			afterLastRenderedMap:  rs.AfterLastRenderedMap,
			tailPartialEpoch:      rs.TailPartialEpoch,
		},
		matcherSyncCh:   make(chan *FilterMapsMatcherBackend),
		matchers:        make(map[*FilterMapsMatcherBackend]struct{}),
		filterMapCache:  lru.NewCache[uint32, filterMap](3),	//TODO named consts
		lastBlockCache:  lru.NewCache[uint32, lastBlockOfMap](1000),
		lvPointerCache:  lru.NewCache[uint64, uint64](1000),
		renderSnapshots: lru.NewCache[uint64, *renderedMap](cachedRevertPoints),
	}
	f.targetView = initView
	if f.initialized {
		f.indexedView = f.initChainView(f.targetView)
		if !f.testDisableSnapshots && f.afterLastIndexedBlock == f.targetBlockNumber+1 &&
		 f.firstRenderedMap < f.afterLastRenderedMap {
			// previous target head rendered; load last map as snapshot
			if err:=f.loadHeadSnapshot();err!=nil {
				log.Error("Could not load head filter map snapshot", "error", err)
			}
		}
	}
	if f.hasIndexedBlocks() {
		log.Trace("Log index head", "number", f.targetBlockNumber, "id", f.targetBlockId.String(), "log value pointer", f.headBlockDelimiter)
		log.Trace("Log index range", "first block", f.firstIndexedBlock, "last block", f.afterLastIndexedBlock-1, "first map", f.firstRenderedMap, "last map", f.afterLastRenderedMap-1)
	}
	fmt.Println("created")
	return f
}

// Start starts the indexer.
func (f *FilterMaps) Start() {
	f.closeWg.Add(1)
	go f.indexerLoop()
	fmt.Println("loop started")
}

func (f *FilterMaps) initChainView(chainView chainView) chainView {
	mapIndex := f.afterLastRenderedMap
	for {
		fmt.Println(" icv mapindex", mapIndex)
		var ok bool
		mapIndex, ok = f.lastMapBoundaryBefore(mapIndex)
		if !ok {
			break
		}
		lastBlockNumber, lastBlockId, err := f.getLastBlockOfMap(mapIndex)
		if err != nil {
			log.Error("Could not initialize indexed chain view", "error", err)
			break
		}
		if chainView.getBlockId(lastBlockNumber)==lastBlockId {
	fmt.Println(" icv done", lastBlockNumber)
			return newLimitedChainView(chainView, lastBlockNumber, f.targetBlockNumber)
		}
	}
	fmt.Println(" icv done genesis")
	return newLimitedChainView(chainView, 0, f.targetBlockNumber)
}

// Stop ensures that the indexer is fully stopped before returning.
func (f *FilterMaps) Stop() {
	close(f.closeCh)
	f.closeWg.Wait()
}

// reset un-initializes the FilterMaps structure and removes all related data from
// the database. The function returns true if everything was successfully removed.
func (f *FilterMaps) reset() bool {
	f.indexLock.Lock()
	f.filterMapsRange = filterMapsRange{}
	f.indexedView = nil
	f.filterMapCache.Purge()
	f.renderSnapshots.Purge()
	f.lastBlockCache.Purge()
	f.lvPointerCache.Purge()
	f.indexLock.Unlock()
	// deleting the range first ensures that resetDb will be called again at next
	// startup and any leftover data will be removed even if it cannot finish now.
	rawdb.DeleteFilterMapsRange(f.db)
	return f.removeDbWithPrefix(rawdb.FilterMapsPrefix, "Resetting log index database")
}

// expects targetView to be non-nil
func (f *FilterMaps) init() error {
	var bestIdx, bestLen int
	for idx, checkpointList := range checkpoints {
		// binary search for the last matching epoch head
		min, max := 0, len(checkpointList)
		for min < max {
			mid := (min + max + 1) / 2
			cp := checkpointList[mid-1]
			if cp.blockNumber <= f.targetView.headNumber() && f.targetView.getBlockId(cp.blockNumber) == cp.blockId {
				min = mid
			} else {
				max = mid - 1
			}
		}
		if max > bestLen {
			bestIdx, bestLen = idx, max
		}
	}
	batch := f.db.NewBatch()
	for epoch := 0; epoch < bestLen; epoch++ {
		cp := checkpoints[bestIdx][epoch]
		f.storeLastBlockOfMap(batch, (uint32(epoch+1)<<f.logMapsPerEpoch)-1, cp.blockNumber, cp.blockId)
		f.storeBlockLvPointer(batch, cp.blockNumber, cp.firstLvIndex)
	}
	fmr := filterMapsRange{
		initialized:     true,
		targetBlockId:   f.targetView.getBlockId(f.targetView.headNumber()),
		targetBlockNumber: f.targetView.headNumber(),
	}
	if bestLen > 0 {
		cp := checkpoints[bestIdx][bestLen-1]
		fmr.firstIndexedBlock = cp.blockNumber + 1
		fmr.afterLastIndexedBlock = cp.blockNumber + 1
		fmr.firstRenderedMap = uint32(bestLen) << f.logMapsPerEpoch
		fmr.afterLastRenderedMap = uint32(bestLen) << f.logMapsPerEpoch
	}
	f.indexedView = f.targetView
	f.setRange(batch, fmr)
	return batch.Write()
}

// removeDbWithPrefix removes data with the given prefix from the database and
// returns true if everything was successfully removed.
func (f *FilterMaps) removeDbWithPrefix(prefix []byte, action string) bool {
	it := f.db.NewIterator(prefix, nil)
	hasData := it.Next()
	it.Release()
	if !hasData {
		return true
	}

	end := bytes.Clone(prefix)
	end[len(end)-1]++
	start := time.Now()
	var retry bool
	for {
		err := f.db.DeleteRange(prefix, end)
		if err == nil {
			log.Info(action+" finished", "elapsed", time.Since(start))
			return true
		}
		if err != leveldb.ErrTooManyKeys {
			log.Error(action+" failed", "error", err)
			return false
		}
		select {
		case <-f.closeCh:
			return false
		default:
		}
		if !retry {
			log.Info(action + " in progress...")
			retry = true
		}
	}
}

// setRange updates the covered range and also adds the changes to the given batch.
// Note that this function assumes that the index write lock is being held.
func (f *FilterMaps) setRange(batch ethdb.KeyValueWriter, newRange filterMapsRange) {
	if f.indexedView != nil && f.indexedView.getBlockId(newRange.targetBlockNumber) != newRange.targetBlockId {
		panic("indexed range inconsistent with canonical chain")
	}
	if newRange.firstRenderedMap > newRange.afterLastRenderedMap {
		panic("qqqq")
	}
	f.filterMapsRange = newRange
	f.updateMatchersValidRange()
	if newRange.initialized {
		rs := rawdb.FilterMapsRange{
			TargetBlockId:         newRange.targetBlockId,
			TargetBlockNumber:     newRange.targetBlockNumber,
			HeadBlockDelimiter:    newRange.headBlockDelimiter,
			FirstIndexedBlock:     newRange.firstIndexedBlock,
			AfterLastIndexedBlock: newRange.afterLastIndexedBlock,
			FirstRenderedMap:      newRange.firstRenderedMap,
			AfterLastRenderedMap:  newRange.afterLastRenderedMap,
			TailPartialEpoch:      newRange.tailPartialEpoch,
		}
		rawdb.WriteFilterMapsRange(batch, rs)
	} else {
		rawdb.DeleteFilterMapsRange(batch)
	}
}

// getLogByLvIndex returns the log at the given log value index. If the index does
// not point to the first log value entry of a log then no log and no error are
// returned as this can happen when the log value index was a false positive.
// Note that this function assumes that the log index structure is consistent
// with the canonical chain at the point where the given log value index points.
// If this is not the case then an invalid result or an error may be returned.
// Note that this function assumes that the indexer read lock is being held when
// called from outside the updateLoop goroutine.
func (f *FilterMaps) getLogByLvIndex(lvIndex uint64) (*types.Log, error) {
	mapIndex := uint32(lvIndex >> f.logValuesPerMap)
	if mapIndex < f.firstRenderedMap || mapIndex >= f.afterLastRenderedMap {
		return nil, nil
	}
	// find possible block range based on map to block pointers
	lastBlockNumber, _, err := f.getLastBlockOfMap(mapIndex)
	var firstBlockNumber uint64
	if mapIndex > 0 {
		firstBlockNumber, _, err = f.getLastBlockOfMap(mapIndex - 1)
		if err != nil {
			return nil, err
		}
	}
	if firstBlockNumber < f.firstIndexedBlock {
		firstBlockNumber = f.firstIndexedBlock
	}
	// find block with binary search based on block to log value index pointers
	for firstBlockNumber < lastBlockNumber {
		midBlockNumber := (firstBlockNumber + lastBlockNumber + 1) / 2
		midLvPointer, err := f.getBlockLvPointer(midBlockNumber)
		if err != nil {
			return nil, err
		}
		if lvIndex < midLvPointer {
			lastBlockNumber = midBlockNumber - 1
		} else {
			firstBlockNumber = midBlockNumber
		}
	}
	// get block receipts
	receipts := f.indexedView.getReceipts(firstBlockNumber)
	if receipts == nil {
		return nil, errors.New("receipts not found")
	}
	lvPointer, err := f.getBlockLvPointer(firstBlockNumber)
	if err != nil {
		return nil, err
	}
	// iterate through receipts to find the exact log starting at lvIndex
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			if lvPointer > lvIndex {
				// lvIndex does not point to the first log value (address value)
				// generated by a log as true matches should always do, so it
				// is considered a false positive (no log and no error returned).
				return nil, nil
			}
			if lvPointer == lvIndex {
				return log, nil // potential match
			}
			lvPointer += uint64(len(log.Topics) + 1)
		}
	}
	return nil, nil
}

func (f *FilterMaps) getFilterMap(mapIndex uint32) (filterMap, error) {
	if fm, ok := f.filterMapCache.Get(mapIndex); ok {
		return fm, nil
	}
	fm := make(filterMap, f.mapHeight)
	for rowIndex := range fm {
		var err error
		fm[rowIndex], err = f.getFilterMapRow(mapIndex, uint32(rowIndex))
		if err != nil {
			return nil, err
		}
	}
	f.filterMapCache.Add(mapIndex, fm)
	return fm, nil
}

func (f *FilterMaps) getFilterMapRow(mapIndex, rowIndex uint32) (FilterRow, error) {
	row, err := rawdb.ReadFilterMapRow(f.db, f.mapRowIndex(mapIndex, rowIndex))
	return FilterRow(row), err
}

func (f *FilterMaps) storeFilterMapRow(batch ethdb.Batch, mapIndex, rowIndex uint32, row FilterRow) {
	rawdb.WriteFilterMapRow(batch, f.mapRowIndex(mapIndex, rowIndex), []uint32(row))
}

// mapRowIndex calculates the unified storage index where the given row of the
// given map is stored. Note that this indexing scheme is the same as the one
// proposed in EIP-7745 for tree-hashing the filter map structure and for the
// same data proximity reasons it is also suitable for database representation.
// See also:
// https://eips.ethereum.org/EIPS/eip-7745#hash-tree-structure
func (f *FilterMaps) mapRowIndex(mapIndex, rowIndex uint32) uint64 {
	epochIndex, mapSubIndex := mapIndex>>f.logMapsPerEpoch, mapIndex&(f.mapsPerEpoch-1)
	return (uint64(epochIndex)<<f.logMapHeight+uint64(rowIndex))<<f.logMapsPerEpoch + uint64(mapSubIndex)
}

// getBlockLvPointer returns the starting log value index where the log values
// generated by the given block are located. If blockNumber is beyond the current
// head then the first unoccupied log value index is returned.
// Note that this function assumes that the indexer read lock is being held when
// called from outside the updateLoop goroutine.
func (f *FilterMaps) getBlockLvPointer(blockNumber uint64) (uint64, error) {
	if blockNumber > f.targetBlockNumber && f.targetBlockNumber+1 == f.afterLastIndexedBlock {
		return f.headBlockDelimiter, nil
	}
	/*if blockNumber < f.firstIndexedBlock || blockNumber >= f.afterLastIndexedBlock {
		return 0, errUnindexedRange
	}*/ //TODO
	if lvPointer, ok := f.lvPointerCache.Get(blockNumber); ok {
		return lvPointer, nil
	}
	lvPointer, err := rawdb.ReadBlockLvPointer(f.db, blockNumber)
	if err != nil {
		return 0, err
	}
	f.lvPointerCache.Add(blockNumber, lvPointer)
	return lvPointer, nil
}

// storeBlockLvPointer stores the starting log value index where the log values
// generated by the given block are located.
func (f *FilterMaps) storeBlockLvPointer(batch ethdb.Batch, blockNumber, lvPointer uint64) {
	f.lvPointerCache.Add(blockNumber, lvPointer)
	rawdb.WriteBlockLvPointer(batch, blockNumber, lvPointer)
}

// deleteBlockLvPointer deletes the starting log value index where the log values
// generated by the given block are located.
func (f *FilterMaps) deleteBlockLvPointer(batch ethdb.Batch, blockNumber uint64) {
	f.lvPointerCache.Remove(blockNumber)
	rawdb.DeleteBlockLvPointer(batch, blockNumber)
}

// getLastBlockOfMap returns the number and id of the block that generated the
// last log value entry of the given map.
func (f *FilterMaps) getLastBlockOfMap(mapIndex uint32) (uint64, common.Hash, error) {
	if lastBlock, ok := f.lastBlockCache.Get(mapIndex); ok {
		return lastBlock.number, lastBlock.id, nil
	}
	number, id, err := rawdb.ReadFilterMapLastBlock(f.db, mapIndex)
	if err != nil {
		return 0, common.Hash{}, err
	}
	f.lastBlockCache.Add(mapIndex, lastBlockOfMap{number:number, id:id})
	return number, id, nil
}

// storeLastBlockOfMap stores the number of the block that generated the last
// log value entry of the given map.
func (f *FilterMaps) storeLastBlockOfMap(batch ethdb.Batch, mapIndex uint32, number uint64, id common.Hash) {
	f.lastBlockCache.Add(mapIndex, lastBlockOfMap{number:number, id:id})
	rawdb.WriteFilterMapLastBlock(batch, mapIndex, number, id)
}

// deleteLastBlockOfMap deletes the number of the block that generated the last
// log value entry of the given map.
func (f *FilterMaps) deleteLastBlockOfMap(batch ethdb.Batch, mapIndex uint32) {
	f.lastBlockCache.Remove(mapIndex)
	rawdb.DeleteFilterMapLastBlock(batch, mapIndex)
}

func (f *FilterMaps) deleteTailEpoch(epoch uint32) error {
	fmt.Println("deleteTailEpoch", epoch)
	firstMap := epoch << f.logMapsPerEpoch
	lastBlock, _, err := f.getLastBlockOfMap(firstMap + f.mapsPerEpoch - 1)
	if err != nil {
		return err
	}
	var firstBlock uint64
	if epoch > 0 {
		firstBlock, _, err = f.getLastBlockOfMap(firstMap - 1)
		if err != nil {
			return err
		}
		firstBlock++
	}
	fmr := f.filterMapsRange
	if f.firstRenderedMap == firstMap && f.afterLastRenderedMap > firstMap+f.mapsPerEpoch && f.tailPartialEpoch == 0 {
		fmr.firstRenderedMap = firstMap + f.mapsPerEpoch
		fmr.firstIndexedBlock = lastBlock + 1
	} else if f.firstRenderedMap == firstMap+f.mapsPerEpoch {
		fmr.tailPartialEpoch = 0
	} else {
		return errors.New("invalid tail epoch number")
	}
	f.setRange(f.db, fmr)
	rawdb.DeleteFilterMapRows(f.db, f.mapRowIndex(firstMap, 0), f.mapRowIndex(firstMap+f.mapsPerEpoch, 0))
	for mapIndex := firstMap; mapIndex < firstMap+f.mapsPerEpoch; mapIndex++ {
		f.filterMapCache.Remove(mapIndex)
	}
	rawdb.DeleteFilterMapLastBlocks(f.db, firstMap, firstMap+f.mapsPerEpoch-1) // keep last enrty
	for mapIndex := firstMap; mapIndex < firstMap+f.mapsPerEpoch-1; mapIndex++ {
		f.lastBlockCache.Remove(mapIndex)
	}
	rawdb.DeleteBlockLvPointers(f.db, firstBlock, lastBlock)                   // keep last enrty
	for blockNumber := firstBlock; blockNumber < lastBlock; blockNumber++ {
		f.lvPointerCache.Remove(blockNumber)
	}
	return nil
}
