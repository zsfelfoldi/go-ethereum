// Copyright 2025 The go-ethereum Authors
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
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

const (
	maxCanonicalSnapshots = 4
	maxRecentSnapshots    = 4
	maxIndexViewMaps      = 2
)

var ( //TODO
	mapCountGauge    = metrics.NewRegisteredGauge("filtermaps/maps/count", nil)      // actual number of rendered maps
	mapLogValueMeter = metrics.NewRegisteredMeter("filtermaps/maps/logvalues", nil)  // number of log values processed
	mapBlockMeter    = metrics.NewRegisteredMeter("filtermaps/maps/blocks", nil)     // number of block delimiters processed
	mapRenderTimer   = metrics.NewRegisteredTimer("filtermaps/maps/rendertime", nil) // time elapsed while rendering a single map
	mapWriteTimer    = metrics.NewRegisteredTimer("filtermaps/maps/writetime", nil)  // time elapsed while writing a batch of finished maps to db
)

type Indexer struct {
	storage                    *mapStorage
	missingBlocks              common.Range[uint64]
	checkpoints                []checkpointList
	headRenderer, tailRenderer *renderState
	headNumber, tailLastNumber uint64
	lastCanonical              uint64
	canonicalHashes            []common.Hash // last one belongs to lastCanonical
	recentHashes               []common.Hash // last one is the most recently saved
	snapshotsLock              sync.RWMutex
	snapshots                  map[common.Hash]*IndexView
	headMapsCache              *lru.Cache[uint32, *finishedMap]
}

// Config contains the configuration options for NewFilterMaps.
type Config struct {
	History  uint64 // number of historical blocks to index
	Disabled bool   // disables indexing completely

	// This option enables the checkpoint JSON file generator.
	// If set, the given file will be updated with checkpoint information.
	ExportFileName string

	// expect trie nodes of hash based state scheme in the filtermaps key range;
	// use safe iterator based implementation of DeleteRange that skips them
	HashScheme bool
}

// TODO blockId vs blockHash?
// TODO disable, export, history, finalized
func NewIndexer(db ethdb.KeyValueStore, params *Params, config Config) *Indexer {
	mapDb := newMapDatabase(params, db, config.HashScheme)
	ix := &Indexer{
		storage:       newMapStorage(&DefaultParams, mapDb),
		checkpoints:   checkpoints,
		snapshots:     make(map[common.Hash]*IndexView),
		headMapsCache: lru.NewCache[uint32, *finishedMap](maxIndexViewMaps),
	}
	ix.headRenderer = ix.initMapBoundary(ix.storage.lastBoundaryBefore(math.MaxUint32), math.MaxUint32)
	return ix
}

func (ix *Indexer) initMapBoundary(nextMap, limitMap uint32) *renderState {
	rs := &renderState{
		params:      ix.storage.params,
		renderRange: common.NewRange[uint32](nextMap, limitMap-nextMap),
		currentMap:  ix.storage.params.newMemoryMap(),
	}
	for nextMap > 0 {
		nextMap = ix.storage.lastBoundaryBefore(nextMap)
		lastNumber, lastHash, err := ix.storage.getLastBlockOfMap(nextMap - 1)
		if err != nil {
			log.Error("Last block of map not found, reverting database", "mapIndex", nextMap)
			nextMap = ix.storage.lastBoundaryBefore(nextMap - 1)
			ix.revertMaps(nextMap)
			continue
		}
		lvPointer, err := ix.storage.getBlockLvPointer(lastNumber)
		if err != nil {
			log.Error("Block pointer of last block of map not found, reverting database", "mapIndex", nextMap, "blockNumber", lastNumber)
			nextMap = ix.storage.lastBoundaryBefore(nextMap - 1)
			ix.revertMaps(nextMap)
			continue
		}
		rs.lvPointer = lvPointer
		rs.mapIndex = uint32(lvPointer >> ix.storage.params.logValuesPerMap)
		rs.nextBlock = lastNumber
		rs.partialBlock = true
		rs.partialBlockHash = lastHash
		return rs
	}
	// initialize at genesis
	return rs
}

func (ix *Indexer) initSnapshot(snapshot *IndexView) *renderState {
	mapIndex := ix.storage.lastBoundaryBefore(snapshot.firstMapIndex)
	ix.revertMaps(mapIndex)
	if snapshot.checkInvalid() {
		log.Error("Failed to revert to invalidated snapshot", "blockNumber", snapshot.headBlockNumber)
		return nil
	}

	return &renderState{
		params:      ix.storage.params,
		renderRange: common.NewRange[uint32](snapshot.headMapIndex, math.MaxUint32-snapshot.headMapIndex),
		currentMap:  snapshot.headMap.clone(),
		mapIndex:    snapshot.headMapIndex,
		lvPointer:   snapshot.headLvPointer,
	}
}

func (ix *Indexer) revertMaps(mapIndex uint32) {
	if mapIndex < ix.storage.lastBoundaryBefore(math.MaxUint32) {
		for hash, iv := range ix.snapshots {
			if iv.firstMapIndex > mapIndex {
				iv.invalidate()
				ix.snapshotsLock.Lock()
				delete(ix.snapshots, hash)
				ix.snapshotsLock.Unlock()
			}
		}
		ix.storage.revert(mapIndex)
		ix.headMapsCache.Purge() // invalidate all maps cached by index
	}
	if mapIndex <= ix.headRenderer.mapIndex {
		ix.headRenderer = nil
	}
	if mapIndex <= ix.tailRenderer.mapIndex {
		ix.tailRenderer = nil
	}
}

func (ix *Indexer) AddBlockData(headers []*types.Header, receipts []types.Receipts) (ready bool, needBlocks common.Range[uint64]) {
	if len(headers) == 0 {
		return ix.Status()
	}
	ix.headNumber = max(ix.headNumber, headers[len(headers)-1].Number.Uint64())
	for i, header := range headers {
		number, hash := header.Number.Uint64(), header.Hash()
		if number > ix.headRenderer.nextBlock {
			ix.tryCheckpointInit(number, hash)
		}
		if number == ix.headRenderer.nextBlock {
			if ix.headRenderer.checkNextHash(hash) {
				ix.headRenderer.addReceipts(receipts[i])
				ix.headRenderer.addHeader(header)
				firstMapIndex, finishedMaps := ix.headRenderer.getFinishedMaps()
				ix.storeFinishedMaps(firstMapIndex, finishedMaps, i == len(headers)-1, true)
				if number+maxCanonicalSnapshots > ix.headNumber {
					ix.storeHeadIndexView(number, hash)
				}
			} else {
				ix.headRenderer = ix.initMapBoundary(max(ix.headRenderer.renderRange.First(), 1)-1, math.MaxUint32)
			}
		}
		if ix.tailRenderer != nil && number == ix.tailRenderer.nextBlock {
			if ix.tailRenderer.checkNextHash(hash) {
				ix.tailRenderer.addReceipts(receipts[i])
				ix.tailRenderer.addHeader(header)
				firstMapIndex, finishedMaps := ix.tailRenderer.getFinishedMaps()
				ix.storeFinishedMaps(firstMapIndex, finishedMaps, false, false)
			} else {
				// Note that if there is a canonical hash mismatch at the tail epoch then we need to revert the head renderer before this point.
				ix.headRenderer = ix.initMapBoundary(max(ix.tailRenderer.renderRange.First(), 1)-1, math.MaxUint32)
			}
		}
	}
	return ix.Status()
}

func (cpList checkpointList) epochsUntilBlock(number uint64) uint32 {
	first, afterLast := uint32(0), uint32(len(cpList))
	for first+1 < afterLast {
		mid := (first + afterLast) / 2
		if cpList[mid].BlockNumber > number {
			afterLast = mid
		} else {
			first = mid
		}
	}
	return first
}

func (ix *Indexer) tryCheckpointInit(number uint64, id common.Hash) {
	var ci int
	for ci < len(ix.checkpoints) {
		cpList := ix.checkpoints[ci]
		epochs := cpList.epochsUntilBlock(number)
		if epochs == 0 || cpList[epochs-1].BlockNumber != number {
			// no matching block number, skip list (a relevant block might match later)
			ci++
			continue
		}
		if cpList[epochs-1].BlockId == id {
			// apply matching checkpoint, discard other lists
			if err := ix.storage.addKnownEpochs(cpList[:epochs]); err == nil {
				ix.checkpoints = []checkpointList{cpList}
				ix.headRenderer = ix.initMapBoundary(epochs*ix.storage.params.mapsPerEpoch, math.MaxUint32)
				return
			} else {
				log.Error("Error initializing epoch boundaries", "error", err)
			}
		}
		// checkpoint does not match, discard list
		ix.checkpoints[ci] = ix.checkpoints[len(ix.checkpoints)-1]
		ix.checkpoints = ix.checkpoints[:len(ix.checkpoints)-1]
	}
}

// TODO single block number? also supply finalized block? combine with AddBlockData/Status?
func (ix *Indexer) MissingBlocks(missing common.Range[uint64]) {
	ix.missingBlocks = missing
}

func (ix *Indexer) Revert(blockNumber uint64) {
	firstCanonical := ix.lastCanonical + 1 - uint64(len(ix.canonicalHashes))
	if blockNumber >= firstCanonical && blockNumber <= ix.lastCanonical {
		blockHash := ix.canonicalHashes[blockNumber-firstCanonical]
		if snapshot, ok := ix.snapshots[blockHash]; ok {
			ix.headRenderer = ix.initSnapshot(snapshot)
			if ix.headRenderer != nil {
				return
			}
		}
	}
	mapIndex := uint32(math.MaxUint32)
	for mapIndex > 0 {
		mapIndex = ix.storage.lastBoundaryBefore(mapIndex)
		lastNumber, _, err := ix.storage.getLastBlockOfMap(mapIndex - 1)
		if err != nil {
			log.Error("Last block of map not found, reverting database", "mapIndex", mapIndex)
			mapIndex--
			continue
		}
		if lastNumber < blockNumber {
			break
		}
	}
	ix.revertMaps(mapIndex)
	ix.headRenderer = ix.initMapBoundary(mapIndex, math.MaxUint32)
	ix.headNumber = blockNumber
}

func (ix *Indexer) Status() (bool, common.Range[uint64]) {
	if ix.headNumber > ix.headRenderer.nextBlock { //TODO head -> finalized
		// request potential checkpoint in this range if available
		for _, cpList := range ix.checkpoints {
			if epochs := cpList.epochsUntilBlock(ix.headNumber); epochs > 0 {
				blockNumber := cpList[epochs-1].BlockNumber
				if ix.storage.lastBoundaryBefore(math.MaxUint32) >= epochs*ix.storage.params.mapsPerEpoch ||
					blockNumber <= ix.headRenderer.nextBlock || ix.missingBlocks.Includes(blockNumber) {
					continue
				}
				return true, common.NewRange[uint64](blockNumber, 1)
			}
		}
	}
	if ix.headRenderer.nextBlock <= ix.headNumber && ix.headRenderer.nextBlock >= ix.missingBlocks.AfterLast() {
		return ix.storage.busyLevel() < 2, common.NewRange[uint64](ix.headRenderer.nextBlock, ix.headNumber+1-ix.headRenderer.nextBlock)
	}
	if ix.tailRenderer.nextBlock <= ix.tailLastNumber && ix.tailRenderer.nextBlock >= ix.missingBlocks.AfterLast() {
		return ix.storage.busyLevel() < 1, common.NewRange[uint64](ix.tailRenderer.nextBlock, ix.tailLastNumber+1-ix.tailRenderer.nextBlock)
	}
	return ix.storage.busyLevel() < 2, common.Range[uint64]{}
}

func (ix *Indexer) Stop() {
	ix.storage.stop()
}

func (ix *Indexer) releaseView(hash common.Hash) {
	iv := ix.snapshots[hash]
	if iv == nil {
		return
	}
	if iv.addRefCount(-1) {
		iv.invalidate()
		ix.snapshotsLock.Lock()
		delete(ix.snapshots, hash)
		ix.snapshotsLock.Unlock()
	}
}

func (ix *Indexer) GetIndexView(hash common.Hash) *IndexView {
	ix.snapshotsLock.RLock()
	iv := ix.snapshots[hash]
	ix.snapshotsLock.RUnlock()
	if iv == nil || iv.checkReleased() {
		return nil
	}
	iv.addRefCount(1)
	return iv
}

func (ix *Indexer) storeFinishedMaps(firstMapIndex uint32, maps []*finishedMap, forceCommit, cacheHeadMaps bool) {
	if len(maps) == 0 {
		return
	}
	for i, fm := range maps {
		ix.storage.addMap(firstMapIndex+uint32(i), fm, forceCommit && i == len(maps)-1)
		if cacheHeadMaps {
			ix.headMapsCache.Add(firstMapIndex+uint32(i), fm)
		}
	}
}

func (ix *Indexer) getFilterMap(mapIndex uint32) (*finishedMap, error) {
	if fm, ok := ix.headMapsCache.Get(mapIndex); ok {
		return fm, nil
	}
	fm, err := ix.storage.getFilterMap(mapIndex)
	if err != nil {
		return nil, err
	}
	ix.headMapsCache.Add(mapIndex, fm)
	return fm, nil
}

func (ix *Indexer) checkReleasedViews() {
	for hash, iv := range ix.snapshots {
		if iv.checkReleased() {
			iv.invalidate()
			ix.snapshotsLock.Lock()
			delete(ix.snapshots, hash)
			ix.snapshotsLock.Unlock()
		}
	}
}

func (ix *Indexer) storeHeadIndexView(number uint64, hash common.Hash) {
	ix.checkReleasedViews()
	firstMapIndex := max(ix.headRenderer.mapIndex, maxIndexViewMaps) - maxIndexViewMaps
	finishedMaps := make([]*finishedMap, 0, ix.headRenderer.mapIndex-firstMapIndex)
	for mapIndex := firstMapIndex; mapIndex < ix.headRenderer.mapIndex; mapIndex++ {
		fm, err := ix.getFilterMap(mapIndex)
		if err != nil {
			log.Error("Error loading recent filter map", "mapIndex", mapIndex, "error", err)
		}
		if fm != nil && err == nil {
			finishedMaps = append(finishedMaps, fm)
		} else {
			finishedMaps = finishedMaps[:0]
			firstMapIndex = mapIndex + 1
		}
	}
	var firstBlockNumber uint64
	if len(finishedMaps) > 0 {
		firstBlockNumber = finishedMaps[0].firstBlock()
	} else {
		firstBlockNumber = ix.headRenderer.currentMap.firstBlock()
	}
	ix.snapshotsLock.Lock()
	ix.snapshots[hash] = &IndexView{
		refCount:         2,
		storage:          ix.storage,
		headBlockNumber:  number,
		headBlockHash:    hash,
		headLvPointer:    ix.headRenderer.lvPointer,
		headMap:          ix.headRenderer.currentMap.clone(),
		headMapIndex:     ix.headRenderer.mapIndex,
		firstMapIndex:    firstMapIndex,
		firstBlockNumber: firstBlockNumber,
		finishedMaps:     finishedMaps,
	}
	ix.snapshotsLock.Unlock()
	if number == ix.lastCanonical+1 {
		if len(ix.canonicalHashes) == maxCanonicalSnapshots {
			ix.releaseView(ix.canonicalHashes[0])
			copy(ix.canonicalHashes[0:maxCanonicalSnapshots-1], ix.canonicalHashes[1:maxCanonicalSnapshots])
			ix.canonicalHashes[maxCanonicalSnapshots-1] = hash
		} else {
			ix.canonicalHashes = append(ix.canonicalHashes, hash)
		}
	} else {
		for _, oldHash := range ix.canonicalHashes {
			ix.releaseView(oldHash)
		}
		ix.canonicalHashes = []common.Hash{hash}
	}
	ix.lastCanonical = number
	if len(ix.recentHashes) == maxRecentSnapshots {
		ix.releaseView(ix.recentHashes[0])
		copy(ix.recentHashes[0:maxRecentSnapshots-1], ix.recentHashes[1:maxRecentSnapshots])
		ix.recentHashes[maxRecentSnapshots-1] = hash
	} else {
		ix.recentHashes = append(ix.recentHashes, hash)
	}
}
