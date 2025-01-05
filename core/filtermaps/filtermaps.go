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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

// checkpoint allows the log indexer to start indexing from the given block
// instead of genesis at the correct absolute log value index.
type checkpointList []epochCheckpoint

type epochCheckpoint struct {
	blockNumber  uint64 // block that generated the last log value of the given epoch
	blockHash    common.Hash
	firstLvIndex uint64 // first log value index of the given block
}

var checkpoints = []checkpointList{}

const headCacheSize = 4 // maximum number of recent filter maps cached in memory

// blockchain defines functions required by the FilterMaps log indexer.
type blockchain interface {
	CurrentBlock() *types.Header
	SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription
	GetHeader(hash common.Hash, number uint64) *types.Header
	GetCanonicalHash(number uint64) common.Hash
	GetReceiptsByHash(hash common.Hash) types.Receipts
}

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
	chain blockchain

	db ethdb.KeyValueStore

	// fields written by the indexer and read by matcher backend. Indexer can
	// read them without a lock and write them under indexLock write lock.
	// Matcher backend can read them under indexLock read lock.
	indexLock sync.RWMutex
	filterMapsRange
	indexedView *chainView // always consistent with the log index
	// filterMapCache caches certain filter maps (headCacheSize most recent maps
	// and one tail map) that are expected to be frequently accessed and modified
	// while updating the structure. Note that the set of cached maps depends
	// only on filterMapsRange and rows of other maps are not cached here.
	filterMapCache map[uint32]filterMap

	// also accessed by indexer and matcher backend but no locking needed.
	blockPtrCache  *lru.Cache[uint32, uint64]
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

	targetHead         *types.Header
	targetView         *chainView
	matcherSyncRequest *FilterMapsMatcherBackend
	stop               bool
	headEventCh        chan core.ChainEvent
	matcherSyncCh      chan *FilterMapsMatcherBackend
	waitIdleCh         chan chan bool
}

// filterMap is a full or partial in-memory representation of a filter map where
// rows are allowed to have a nil value meaning the row is not stored in the
// structure. Note that therefore a known empty row should be represented with
// a zero-length slice.
// It can be used as a memory cache or an overlay while preparing a batch of
// changes to the structure. In either case a nil value should be interpreted
// as transparent (uncached/unchanged).
type filterMap []FilterRow

// FilterRow encodes a single row of a filter map as a list of column indices.
// Note that the values are always stored in the same order as they were added
// and if the same column index is added twice, it is also stored twice.
// Order of column indices and potential duplications do not matter when searching
// for a value but leaving the original order makes reverting to a previous state
// simpler.
type FilterRow []uint32

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
	headBlockNumber    uint64
	headBlockHash      common.Hash
	headBlockDelimiter uint64 // zero if lastIndexedBlock != headBlockNumber
	// if initialized then all maps are rendered between firstRenderedMap and
	// lastRenderedMap
	// some rendered maps might exist between tailMapLimit and firstRenderedMap-1
	firstRenderedMap, lastRenderedMap uint32
	// if initialized then all log values belonging to blocks between
	// firstIndexedBlock and lastIndexedBlock are fully rendered
	// blockLvPointers are available between firstIndexedBlock and lastIndexedBlock
	firstIndexedBlock, lastIndexedBlock uint64
}

// mapCount returns the number of maps fully or partially included in the range.
func (fmr *filterMapsRange) mapCount() uint32 {
	if !fmr.initialized {
		return 0
	}
	return fmr.lastRenderedMap + 1 - fmr.firstRenderedMap
}

// NewFilterMaps creates a new FilterMaps and starts the indexer in order to keep
// the structure in sync with the given blockchain.
func NewFilterMaps(db ethdb.KeyValueStore, chain blockchain, params Params, history, unindexLimit uint64, noHistory bool) *FilterMaps {
	rs, err := rawdb.ReadFilterMapsRange(db)
	if err != nil {
		log.Error("Error reading log index range", "error", err)
	}
	params.deriveFields()
	fm := &FilterMaps{
		db:           db,
		chain:        chain,
		closeCh:      make(chan struct{}),
		waitIdleCh:   make(chan chan bool),
		history:      history,
		noHistory:    noHistory,
		unindexLimit: unindexLimit,
		Params:       params,
		filterMapsRange: filterMapsRange{
			initialized:     rs.Initialized,
			headLvPointer:   rs.HeadLvPointer,
			tailLvPointer:   rs.TailLvPointer,
			headBlockNumber: rs.HeadBlockNumber,
			tailBlockNumber: rs.TailBlockNumber,
			headBlockHash:   rs.HeadBlockHash,
			tailParentHash:  rs.TailParentHash,
		},
		matcherSyncCh:   make(chan *FilterMapsMatcherBackend),
		matchers:        make(map[*FilterMapsMatcherBackend]struct{}),
		filterMapCache:  make(map[uint32]filterMap),
		blockPtrCache:   lru.NewCache[uint32, uint64](1000),
		lvPointerCache:  lru.NewCache[uint64, uint64](1000),
		renderSnapshots: lru.NewCache[uint64, *renderedMap](cachedRevertPoints),
	}
	if fm.initialized {
		fm.tailBlockLvPointer, err = fm.getBlockLvPointer(fm.tailBlockNumber)
		if err != nil {
			log.Error("Error fetching tail block pointer, resetting log index", "error", err)
			fm.filterMapsRange = filterMapsRange{} // updateLoop resets the database
		}
		log.Trace("Log index head", "number", fm.headBlockNumber, "hash", fm.headBlockHash.String(), "log value pointer", fm.headLvPointer)
		log.Trace("Log index tail", "number", fm.tailBlockNumber, "parentHash", fm.tailParentHash.String(), "log value pointer", fm.tailBlockLvPointer)
	}
	return fm
}

// Start starts the indexer.
func (f *FilterMaps) Start() {
	f.closeWg.Add(1)
	go f.updateLoop()
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
	f.filterMapCache = make(map[uint32]filterMap)
	f.renderSnapshots.Purge()
	f.blockPtrCache.Purge()
	f.lvPointerCache.Purge()
	f.indexLock.Unlock()
	// deleting the range first ensures that resetDb will be called again at next
	// startup and any leftover data will be removed even if it cannot finish now.
	rawdb.DeleteFilterMapsRange(f.db)
	return f.removeDbWithPrefix(rawdb.FilterMapsPrefix, "Resetting log index database")
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
	if f.indexedView != nil && f.indexedView.getHash(newRange.headBlockNumber) != newRange.headBlockHash {
		panic("indexed range inconsistent with canonical chain")
	}
	f.filterMapsRange = newRange
	rs := rawdb.FilterMapsRange{
		//TODO
		Initialized:     newRange.initialized,
		HeadLvPointer:   newRange.headLvPointer,
		TailLvPointer:   newRange.tailLvPointer,
		HeadBlockNumber: newRange.headBlockNumber,
		TailBlockNumber: newRange.tailBlockNumber,
		HeadBlockHash:   newRange.headBlockHash,
		TailParentHash:  newRange.tailParentHash,
	}
	rawdb.WriteFilterMapsRange(batch, rs)
	f.updateMapCache()
	f.updateMatchersValidRange()
}

// updateMapCache updates the maps covered by the filterMapCache according to the
// covered range.
// Note that this function assumes that the index write lock is being held.
func (f *FilterMaps) updateMapCache() {
	if !f.initialized {
		return
	}
	newFilterMapCache := make(map[uint32]filterMap)
	firstMap, afterLastMap := uint32(f.tailBlockLvPointer>>f.logValuesPerMap), uint32((f.headLvPointer+f.valuesPerMap-1)>>f.logValuesPerMap)
	headCacheFirst := firstMap + 1
	if afterLastMap > headCacheFirst+headCacheSize {
		headCacheFirst = afterLastMap - headCacheSize
	}
	fm := f.filterMapCache[firstMap]
	if fm == nil {
		fm = make(filterMap, f.mapHeight)
	}
	newFilterMapCache[firstMap] = fm
	for mapIndex := headCacheFirst; mapIndex < afterLastMap; mapIndex++ {
		fm := f.filterMapCache[mapIndex]
		if fm == nil {
			fm = make(filterMap, f.mapHeight)
		}
		newFilterMapCache[mapIndex] = fm
	}
	f.filterMapCache = newFilterMapCache
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
	iter, err := f.newLogIterator(lvIndex)
	if err != nil {
		return nil, err
	}
	return iter.log(), nil
}

// getFilterMapRow returns the given row of the given map. If the row is empty
// then a non-nil zero length row is returned.
// Note that the returned slices should not be modified, they should be copied
// on write.
// Note that the function assumes that the indexLock is not being held (should
// only be called from the updateLoop goroutine).
func (f *FilterMaps) getFilterMapRow(mapIndex, rowIndex uint32) (FilterRow, error) {
	fm := f.filterMapCache[mapIndex]
	if fm != nil && fm[rowIndex] != nil {
		return fm[rowIndex], nil
	}
	row, err := rawdb.ReadFilterMapRow(f.db, f.mapRowIndex(mapIndex, rowIndex))
	if err != nil {
		return nil, err
	}
	if fm != nil {
		f.indexLock.Lock()
		fm[rowIndex] = FilterRow(row)
		f.indexLock.Unlock()
	}
	return FilterRow(row), nil
}

// getFilterMapRowUncached returns the given row of the given map. If the row is
// empty then a non-nil zero length row is returned.
// This function bypasses the memory cache which is mostly useful for processing
// the head and tail maps during the indexing process and should be used by the
// matcher backend which rarely accesses the same row twice and therefore does
// not really benefit from caching anyways.
// The function is unaffected by the indexLock mutex.
func (f *FilterMaps) getFilterMapRowUncached(mapIndex, rowIndex uint32) (FilterRow, error) {
	row, err := rawdb.ReadFilterMapRow(f.db, f.mapRowIndex(mapIndex, rowIndex))
	return FilterRow(row), err
}

// storeFilterMapRow stores a row at the given row index of the given map and also
// caches it in filterMapCache if the given map is cached.
// Note that empty rows are not stored in the database and therefore there is no
// separate delete function; deleting a row is the same as storing an empty row.
// Note that this function assumes that the indexer write lock is being held.
func (f *FilterMaps) storeFilterMapRow(batch ethdb.Batch, mapIndex, rowIndex uint32, row FilterRow) {
	if fm := f.filterMapCache[mapIndex]; fm != nil {
		fm[rowIndex] = row
	}
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
	if blockNumber > f.headBlockNumber {
		return f.headLvPointer, nil
	}
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

// getMapBlockPtr returns the number of the block that generated the first log
// value entry of the given map.
func (f *FilterMaps) getMapBlockPtr(mapIndex uint32) (uint64, error) {
	if blockPtr, ok := f.blockPtrCache.Get(mapIndex); ok {
		return blockPtr, nil
	}
	blockPtr, err := rawdb.ReadFilterMapBlockPtr(f.db, mapIndex)
	if err != nil {
		return 0, err
	}
	f.blockPtrCache.Add(mapIndex, blockPtr)
	return blockPtr, nil
}

// storeMapBlockPtr stores the number of the block that generated the first log
// value entry of the given map.
func (f *FilterMaps) storeMapBlockPtr(batch ethdb.Batch, mapIndex uint32, blockPtr uint64) {
	f.blockPtrCache.Add(mapIndex, blockPtr)
	rawdb.WriteFilterMapBlockPtr(batch, mapIndex, blockPtr)
}

// deleteMapBlockPtr deletes the number of the block that generated the first log
// value entry of the given map.
func (f *FilterMaps) deleteMapBlockPtr(batch ethdb.Batch, mapIndex uint32) {
	f.blockPtrCache.Remove(mapIndex)
	rawdb.DeleteFilterMapBlockPtr(batch, mapIndex)
}
