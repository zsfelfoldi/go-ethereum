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
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

var (
	mapCountGauge           = metrics.NewRegisteredGauge("filtermaps/maps/count", nil)          // actual number of rendered maps
	mapLogValueMeter        = metrics.NewRegisteredMeter("filtermaps/maps/logvalues", nil)      // number of log values processed
	mapBlockMeter           = metrics.NewRegisteredMeter("filtermaps/maps/blocks", nil)         // number of block delimiters processed
	mapRenderTimer          = metrics.NewRegisteredTimer("filtermaps/maps/rendertime", nil)     // time elapsed while rendering a single map
	mapWriteTimer           = metrics.NewRegisteredTimer("filtermaps/maps/writetime", nil)      // time elapsed while writing a batch of finished maps to db
	matchRequestTimer       = metrics.NewRegisteredTimer("filtermaps/match/requesttime", nil)   // processing time a matching request in a single epoch
	matchEpochTimer         = metrics.NewRegisteredTimer("filtermaps/match/epochtime", nil)     // total processing time a matching request
	matchBaseRowAccessMeter = metrics.NewRegisteredMeter("filtermaps/match/baserowaccess", nil) // number of accessed rows on layer 0
	matchBaseRowSizeMeter   = metrics.NewRegisteredMeter("filtermaps/match/baserowsize", nil)   // size of accessed rows on layer 0
	matchExtRowAccessMeter  = metrics.NewRegisteredMeter("filtermaps/match/extrowaccess", nil)  // number of accessed rows on higher layers
	matchExtRowSizeMeter    = metrics.NewRegisteredMeter("filtermaps/match/extrowsize", nil)    // size of accessed rows on higher layers
	matchLogLookup          = metrics.NewRegisteredMeter("filtermaps/match/loglookup", nil)     // number of log lookups based on potential matches
	matchAllMeter           = metrics.NewRegisteredMeter("filtermaps/match/matchall", nil)      // number of requests returned with ErrMatchAll
)

const (
	databaseVersion       = 3    // reindexed if database version does not match
	cachedLastBlocks      = 1000 // last block of map pointers
	cachedLvPointers      = 1000 // first log value pointer of block pointers
	cachedFilterMaps      = 3    // complete filter maps (cached by map renderer)
	cachedRenderSnapshots = 8    // saved map renderer data at block boundaries
)

// FilterMaps is the in-memory representation of the log index structure that is
// responsible for building and updating the index according to the canonical
// chain.
//
// Note that FilterMaps implements the same data structure as proposed in EIP-7745
// without the tree hashing and consensus changes:
// https://eips.ethereum.org/EIPS/eip-7745
type FilterMaps struct {
	// If disabled is set, log indexing is fully disabled.
	// This is configured by the --history.logs.disable Geth flag.
	// We chose to implement disabling this way because it requires less special
	// case logic in eth/filters.
	disabled   bool
	disabledCh chan struct{} // closed by indexer if disabled

	closeCh        chan struct{}
	closeWg        sync.WaitGroup
	history        uint64
	hashScheme     bool // use hashdb-safe delete range method
	exportFileName string
	Params

	db ethdb.KeyValueStore

	// fields only accessed by the indexer (no mutex required).
	startedHeadIndex, startedTailIndex, startedTailUnindex       bool
	startedHeadIndexAt, startedTailIndexAt, startedTailUnindexAt time.Time
	loggedHeadIndex, loggedTailIndex                             bool
	lastLogHeadIndex, lastLogTailIndex                           time.Time
	ptrHeadIndex, ptrTailIndex, ptrTailUnindexBlock              uint64
	ptrTailUnindexMap                                            uint32

	historyCutoff         uint64
	finalBlock, lastFinal uint64
	lastFinalEpoch        uint32
	stop                  bool
	blockProcessingCh     chan bool
	blockProcessing       bool
	waitIdleCh            chan chan bool

	// test hooks
	testDisableSnapshots, testSnapshotUsed bool
	testProcessEventsHook                  func()
}

// FilterRow encodes a single row of a filter map as a list of column indices.
// Note that the values are always stored in the same order as they were added
// and if the same column index is added twice, it is also stored twice.
// Order of column indices and potential duplications do not matter when searching
// for a value but leaving the original order makes reverting to a previous state
// simpler.
type FilterRow []uint32

// Equal returns true if the given filter rows are equivalent.
func (a FilterRow) Equal(b FilterRow) bool {
	return slices.Equal(a, b)
}

// lastBlockOfMap is used for caching the (number, id) pairs belonging to the
// last block of each map.
type lastBlockOfMap struct {
	number uint64
	id     common.Hash
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

// NewFilterMaps creates a new FilterMaps and starts the indexer.
func NewFilterMaps(db ethdb.KeyValueStore, initView *ChainView, historyCutoff, finalBlock uint64, params Params, config Config) (*FilterMaps, error) {
	rs, initialized, err := rawdb.ReadFilterMapsRange(db)
	if err != nil || (initialized && rs.Version != databaseVersion) {
		rs, initialized = rawdb.FilterMapsRange{}, false
		log.Warn("Invalid log index database version; resetting log index")
	}
	if err := params.sanitize(); err != nil {
		return nil, err
	}
	f := &FilterMaps{
		db:                db,
		closeCh:           make(chan struct{}),
		waitIdleCh:        make(chan chan bool),
		blockProcessingCh: make(chan bool, 1),
		history:           config.History,
		disabled:          config.Disabled,
		hashScheme:        config.HashScheme,
		disabledCh:        make(chan struct{}),
		exportFileName:    config.ExportFileName,
		Params:            params,
		historyCutoff:     historyCutoff,
		finalBlock:        finalBlock,
	}
	f.checkRevertRange() // revert maps that are inconsistent with the current chain view

	/*	if f.indexedRange.hasIndexedBlocks() {
		log.Info("Initialized log indexer",
			"firstblock", f.indexedRange.blocks.First(), "lastblock", f.indexedRange.blocks.Last(),
			"firstmap", f.indexedRange.maps.First(), "lastmap", f.indexedRange.maps.Last(),
			"headindexed", f.indexedRange.headIndexed)
	}*/
	return f, nil
}

// Start starts the indexer.
func (f *FilterMaps) Start() {
	f.closeWg.Add(1)
	//go f.removeBloomBits()
}

// Stop ensures that the indexer is fully stopped before returning.
func (f *FilterMaps) Stop() {
	close(f.closeCh)
	f.closeWg.Wait()
}

// checkRevertRange checks whether the existing index is consistent with the
// current indexed view and reverts inconsistent maps if necessary.
func (f *FilterMaps) checkRevertRange() {
	/*
	   	if f.indexedRange.maps.Count() == 0 {
	   		return
	   	}

	   lastMap := f.indexedRange.maps.Last()
	   lastBlockNumber, lastBlockId, err := f.getLastBlockOfMap(lastMap)

	   	if err != nil {
	   		log.Error("Error initializing log index database; resetting log index", "error", err)
	   		f.reset()
	   		return
	   	}

	   	for lastBlockNumber > f.indexedView.HeadNumber() || f.indexedView.BlockId(lastBlockNumber) != lastBlockId {
	   		// revert last map
	   		if f.indexedRange.maps.Count() == 1 {
	   			f.reset() // reset database if no rendered maps remained
	   			return
	   		}
	   		lastMap--
	   		newRange := f.indexedRange
	   		newRange.maps.SetLast(lastMap)
	   		lastBlockNumber, lastBlockId, err = f.getLastBlockOfMap(lastMap)
	   		if err != nil {
	   			log.Error("Error initializing log index database; resetting log index", "error", err)
	   			f.reset()
	   			return
	   		}
	   		newRange.blocks.SetAfterLast(lastBlockNumber) // lastBlockNumber is probably partially indexed
	   		newRange.headIndexed = false
	   		newRange.headDelimiter = 0
	   		// only shorten range and leave map data; next head render will overwrite it
	   		f.setRange(f.db, f.indexedView, newRange, false)
	   	}
	*/
}

// isShuttingDown returns true if FilterMaps is shutting down.
func (f *FilterMaps) isShuttingDown() bool {
	select {
	case <-f.closeCh:
		return true
	default:
		return false
	}
}

// init initializes an empty log index according to the current targetView.
/*func (f *FilterMaps) init() error {
	// ensure that there is no remaining data in the filter maps key range
	if err := f.safeDeleteWithLogs(rawdb.DeleteFilterMapsDb, "Resetting log index database", f.isShuttingDown); err != nil {
		return err
	}

	f.indexLock.Lock()
	defer f.indexLock.Unlock()

	var bestIdx, bestLen int
	for idx, checkpointList := range checkpoints {
		// binary search for the last matching epoch head
		min, max := 0, len(checkpointList)
		for min < max {
			mid := (min + max + 1) / 2
			cp := checkpointList[mid-1]
			if cp.BlockNumber <= f.targetView.HeadNumber() && f.targetView.BlockId(cp.BlockNumber) == cp.BlockId {
				min = mid
			} else {
				max = mid - 1
			}
		}
		if max > bestLen {
			bestIdx, bestLen = idx, max
		}
	}
	var initBlockNumber uint64
	if bestLen > 0 {
		initBlockNumber = checkpoints[bestIdx][bestLen-1].BlockNumber
	}
	if initBlockNumber < f.historyCutoff {
		return errors.New("cannot start indexing before history cutoff point")
	}
	batch := f.db.NewBatch()
	for epoch := range bestLen {
		cp := checkpoints[bestIdx][epoch]
		f.storeLastBlockOfMap(batch, f.lastEpochMap(uint32(epoch)), cp.BlockNumber, cp.BlockId)
		f.storeBlockLvPointer(batch, cp.BlockNumber, cp.FirstIndex)
	}
	fmr := filterMapsRange{
		initialized: true,
	}
	if bestLen > 0 {
		cp := checkpoints[bestIdx][bestLen-1]
		fmr.blocks = common.NewRange(cp.BlockNumber+1, 0)
		fmr.maps = common.NewRange(f.firstEpochMap(uint32(bestLen)), 0)
	}
	f.setRange(batch, f.targetView, fmr, false)
	return batch.Write()
}

// removeBloomBits removes old bloom bits data from the database.
func (f *FilterMaps) removeBloomBits() {
	f.safeDeleteWithLogs(rawdb.DeleteBloomBitsDb, "Removing old bloom bits database", f.isShuttingDown)
	f.closeWg.Done()
}*/

// exportCheckpoints exports epoch checkpoints in the format used by checkpoints.go.
//
// Note: acquiring the indexLock read lock is unnecessary here, as this function
// is always called within the indexLoop.
/*func (f *FilterMaps) exportCheckpoints() {
	finalLvPtr, err := f.getBlockLvPointer(f.finalBlock + 1)
	if err != nil {
		log.Error("Error fetching log value pointer of finalized block", "block", f.finalBlock, "error", err)
		return
	}
	epochCount := uint32(finalLvPtr >> (f.logValuesPerMap + f.logMapsPerEpoch))
	if epochCount == f.lastFinalEpoch {
		return
	}
	w, err := os.Create(f.exportFileName)
	if err != nil {
		log.Error("Error creating checkpoint export file", "name", f.exportFileName, "error", err)
		return
	}
	defer w.Close()

	log.Info("Exporting log index checkpoints", "epochs", epochCount, "file", f.exportFileName)
	w.WriteString("[\n")
	comma := ","
	for epoch := uint32(0); epoch < epochCount; epoch++ {
		lastBlock, lastBlockId, err := f.getLastBlockOfMap(f.lastEpochMap(epoch))
		if err != nil {
			log.Error("Error fetching last block of epoch", "epoch", epoch, "error", err)
			return
		}
		lvPtr, err := f.getBlockLvPointer(lastBlock)
		if err != nil {
			log.Error("Error fetching log value pointer of last block", "block", lastBlock, "error", err)
			return
		}
		if epoch == epochCount-1 {
			comma = ""
		}
		w.WriteString(fmt.Sprintf("{\"blockNumber\": %d, \"blockId\": \"0x%064x\", \"firstIndex\": %d}%s\n", lastBlock, lastBlockId, lvPtr, comma))
	}
	w.WriteString("]\n")
	f.lastFinalEpoch = epochCount
}*/
