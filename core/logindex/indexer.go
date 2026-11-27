// Copyright 2026 The go-ethereum Authors
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

package logindex

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	headCacheSize              = 4
	blockRequestLevels         = 2 // priority levels where block requests are processed
	maxMergeThreads            = 4 //TODO config
	mergeEntryPrefetch         = 20000
	memFileLowThreshold        = 10000000 //TODO config
	memFileHighThreshold       = 15000000 //TODO config
	memFileSuspendThreshold    = 20000000 //TODO config
	tableCountLowThreshold     = 200      //TODO config, +protocol tables
	tableCountHighThreshold    = 250      //TODO config
	tableCountSuspendThreshold = 300      //TODO config
)

// Config contains the configuration options for NewIndexer.
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

type Params struct {
	tableLevels                []tableLevel //TODO config?
	protocolLevels             []protocolLevel
	fileStorageThresholdHeight uint
	topLevelMinUnique          uint
}

var DefaultParams = &Params{
	tableLevels: []tableLevel{
		{blockCount: 0x1},
		//	{blockCount: 0x4},
		{blockCount: 0x10},
		//	{blockCount: 0x40},
		{blockCount: 0x100},
		//	{blockCount: 0x400},
		{blockCount: 0x1000},
		//	{blockCount: 0x4000},
		{blockCount: 0x10000},
		//	{blockCount: 0x40000},
		{blockCount: 0x100000},
		{blockCount: 0x400000, leanStorage: true},
	},
	protocolLevels: []protocolLevel{
		{tailAge: 5, headAge: 0},
		{tailAge: 20, headAge: 0},
		{tailAge: 80, headAge: 1},
		{tailAge: 320, headAge: 4},
		{tailAge: 8192, headAge: 16},
	},
	fileStorageThresholdHeight: 12,
	topLevelMinUnique:          3, //TODO
}

type Indexer struct {
	lock         sync.Mutex
	params       *Params
	config       Config
	requestBlock func(uint64) bool

	files                              *tableFiles
	storage                            *tableStorage
	expectedBlockTables                common.RangeSet[uint64]
	headBlock, finalBlock, cutoffBlock uint64
	headBlockHash                      common.Hash
	queuedBlockRequests                []uint64
	requestedInitBlock                 uint64

	hasProcessedBlock    bool
	processedBlockNumber uint64
	processedBlockId     common.Hash
	recentHeads          *lru.Cache[common.Hash, *cachedBlockData]

	shutdown           bool
	storageInitialized bool

	mergeControl *mergeControl
}

func NewIndexer(params *Params, config Config, path string) *Indexer {
	fmt.Println("*** PATH", path)
	files, err := newTableFiles(path, 2000000000, 16) //TODO
	if err != nil {
		log.Crit("Could not open index table file manager", "error", err) //TODO return?
	}
	storage, err := newTableStorage(params, files)
	if err != nil {
		log.Crit("Could not open index table storage", "error", err)
	}
	ix := &Indexer{
		params:      params,
		config:      config,
		files:       files,
		storage:     storage,
		recentHeads: lru.NewCache[common.Hash, *cachedBlockData](headCacheSize),
	}
	ix.mergeControl = newMergeControl(
		maxMergeThreads,
		storage,
		ix.files.getMemFileTotal,
		storage.deleteTable,
		ix.mergeTable,
		ix.requestBlockTable,
	)
	ix.requestInitBlock()
	return ix
}

func (ix *Indexer) requestInitBlock() {
	if ix.storageInitialized {
		return
	}
	reqNumber, ok := ix.storage.requestInitBlockHash()
	if !ok {
		_, _, initPhase := ix.storage.tables()
		if !initPhase {
			ix.storageInitialized = true
			return
		}
		panic("cannot initialize index table storage")
	}
	fmt.Println("requesting init block", reqNumber)
	if ix.requestBlock(reqNumber) {
		ix.requestedInitBlock = reqNumber
	}
	return
}

type cachedBlockData struct {
	header         *types.Header
	body           *types.Body
	receipts       types.Receipts
	canonicalUntil uint64
}

func (ix *Indexer) GetRangeReaders(refBlockHash common.Hash, blockRange common.Range[uint64]) []*TableReader {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	//fmt.Println("GetRangeReaders", blockRange, refBlockHash)
	refBd, ok := ix.recentHeads.Get(refBlockHash) //TODO give useful error message
	if !ok {
		return nil
	}
	if refBd.canonicalUntil < blockRange.Last() {
		blockRange.SetLast(refBd.canonicalUntil) //TODO keep non-canonical tables for a short time?
	}
	if blockRange.IsEmpty() {
		return nil
	}
	return ix.storage.getRangeReaders(blockRange)
}

func getBlockEntries(blockNumber uint64, parentHash common.Hash, txs types.Transactions, receipts types.Receipts) IndexEntries {
	entryCount := len(txs) + 1
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			entryCount += len(log.Topics) + 1
		}
	}

	entries := make(IndexEntries, 0, entryCount)

	if blockNumber > 0 {
		entries = append(entries, IndexEntry{
			IndexValue: IndexValue{
				EntryType: IeBlock,
				Value:     ([32]byte)(parentHash),
			},
			IndexPosition: IndexPosition{
				BlockNumber: blockNumber - 1,
			},
		})
	}
	var cli uint32
	for txi, tx := range txs {
		entries = append(entries, IndexEntry{
			IndexValue: IndexValue{
				EntryType: IeTransaction,
				Value:     ([32]byte)(tx.Hash()),
			},
			IndexPosition: IndexPosition{
				BlockNumber: blockNumber,
				TxIndex:     uint32(txi),
				LogIndex:    cli,
			},
		})
		cli += uint32(len(receipts[txi].Logs))
	}
	for txi, receipt := range receipts {
		for li, log := range receipt.Logs {
			var addr32 [32]byte
			copy(addr32[32-common.AddressLength:], log.Address[:])
			entries = append(entries, IndexEntry{
				IndexValue: IndexValue{
					EntryType: IeAddress,
					Value:     addr32,
				},
				IndexPosition: IndexPosition{
					BlockNumber: blockNumber,
					TxIndex:     uint32(txi),
					LogIndex:    uint32(li),
				},
			})
			for ti, topic := range log.Topics {
				entries = append(entries, IndexEntry{
					IndexValue: IndexValue{
						EntryType: IeTopic0 + uint16(ti),
						Value:     ([32]byte)(topic),
					},
					IndexPosition: IndexPosition{
						BlockNumber: blockNumber,
						TxIndex:     uint32(txi),
						LogIndex:    uint32(li),
					},
				})
			}
		}
	}
	return entries
}

func (ix *Indexer) Register(requestBlock func(uint64) bool) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	ix.requestBlock = requestBlock
}

var (
	abStatCount                                                                                        uint64
	abStatWaitLock, abStatOther, abStatGetEntries, abStatSortEntries, abStatAddEntries, abStatFinalize mclock.AbsTime
	abStatCurrent                                                                                      *mclock.AbsTime
)

func abStatSet(s *mclock.AbsTime) {
	now := mclock.Now()
	if abStatCurrent != nil {
		(*abStatCurrent) += now
	}
	abStatCurrent = s
	if abStatCurrent != nil {
		(*abStatCurrent) -= now
	}
}

func (ix *Indexer) requestBlockTable(blockNumber uint64) (common.RangeSet[uint64], bool) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	sent := ix.requestBlock(blockNumber)
	if sent {
		ix.expectedBlockTables = ix.expectedBlockTables.Union(common.SingleRangeSet[uint64](common.NewRange[uint64](blockNumber, 1)))
	}
	return ix.expectedBlockTables, sent // never modified, can return without copying
}

func (ix *Indexer) AddBlockData(header *types.Header, body *types.Body, receipts types.Receipts) {
	abStatCount++
	abStatSet(&abStatWaitLock)
	defer abStatSet(nil)

	ix.lock.Lock()
	abStatSet(&abStatOther)
	if ix.shutdown {
		ix.lock.Unlock()
		return
	}
	ix.mergeControl.suspend(false)
	blockNumber := header.Number.Uint64()
	if ix.requestedInitBlock == blockNumber {
		if ix.storage.deliverInitBlockHash(blockNumber, header.Hash()) {
			ix.storageInitialized = true
			ix.mergeControl.start()
		} else {
			ix.requestInitBlock()
		}
	}
	expected := ix.expectedBlockTables.Includes(blockNumber)
	if blockNumber >= ix.headBlock {
		ix.headBlock = blockNumber
		ix.headBlockHash = header.Hash()
		ix.recentHeads.Add(header.Hash(), &cachedBlockData{header: header, body: body, receipts: receipts, canonicalUntil: blockNumber})
	}
	//fmt.Println(" processBlockTables")
	if blockNumber < ix.headBlock && !expected {
		//fmt.Println(" unexpected", blockNumber, "required", ix.requiredBlockTables)
		ix.lock.Unlock()
		return // unexpected block, do not create table
	}
	if expected {
		ix.expectedBlockTables = ix.expectedBlockTables.Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](blockNumber, 1)))
	}
	tw, err := ix.getPartialBlockTableWriter(header)
	var expectedBlockTables common.RangeSet[uint64]
	if abStatCount%1000 == 0 {
		expectedBlockTables = slices.Clone(ix.expectedBlockTables)
	}
	headBlock, tailBlock, finalBlock := ix.headBlock, ix.tailBlock(), ix.finalBlock
	ix.lock.Unlock()
	if err == nil {
		err = ix.processBlockTable(tw, header, body, receipts)
	}
	if err == nil {
		ix.mergeControl.trigger(headBlock, tailBlock, finalBlock)
	} else {
		log.Error("Failed to add block data to log index", "number", header.Number.Uint64(), "error", err)
	}
	//fmt.Println(" done")
	if abStatCount%1000 == 0 {
		completeSet, partialSet, _ := ix.storage.tables()
		fmt.Println("--------- AddBlockData stats ----------")
		fmt.Println(" wait for lock", time.Duration(abStatWaitLock)/time.Duration(abStatCount))
		fmt.Println(" get entries", time.Duration(abStatGetEntries)/time.Duration(abStatCount))
		fmt.Println(" sort entries", time.Duration(abStatSortEntries)/time.Duration(abStatCount))
		fmt.Println(" add entries", time.Duration(abStatAddEntries)/time.Duration(abStatCount))
		fmt.Println(" finalize", time.Duration(abStatFinalize)/time.Duration(abStatCount))
		fmt.Println(" other", time.Duration(abStatOther)/time.Duration(abStatCount))
		fmt.Println("---------------------------------------")
		fmt.Println("+ tables: complete", completeSet.count(), "partial", partialSet.count())
		fmt.Println("+  complete:", completeSet)
		fmt.Println("+  partial:", partialSet)
		fmt.Println("+  expectedBlockTables:", expectedBlockTables.Count(), expectedBlockTables)
		fmt.Println("---------------------------------------")
	}
}

func (ix *Indexer) getPartialBlockTableWriter(header *types.Header) (*tableWriter, error) {
	var tw *tableWriter
	if ix.hasProcessedBlock && ix.processedBlockNumber == header.Number.Uint64() {
		if header.Hash() == ix.processedBlockId {
			var err error
			if tw, err = ix.storage.getTableWriter(tableID{level: 0, index: ix.processedBlockNumber}); err != nil {
				return nil, err
			}
		} else {
			if err := ix.storage.deleteTable(tableID{level: 0, index: ix.processedBlockNumber}); err != nil {
				return nil, err
			}
		}
		ix.hasProcessedBlock = false
	}
	return tw, nil
}

func (ix *Indexer) processBlockTable(tw *tableWriter, header *types.Header, body *types.Body, receipts types.Receipts) error {
	blockNumber := header.Number.Uint64()
	id := tableID{level: 0, index: blockNumber}
	if tw == nil {
		complete, partial, initPhase := ix.storage.tables()
		if initPhase {
			return nil
		}
		if partial.includes(id) {
			if err := ix.storage.deleteTable(id); err != nil {
				return err
			}
		}
		if complete.includes(id) {
			return nil
		}
		abStatSet(&abStatGetEntries)
		entries := getBlockEntries(blockNumber, header.ParentHash, body.Transactions, receipts)
		abStatSet(&abStatSortEntries)
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Compare(&entries[j]) < 0
		})
		abStatSet(&abStatAddEntries)
		var err error
		if tw, err = ix.storage.addNewTableWriter(id, uint64(len(entries))); err != nil {
			complete, partial, _ := ix.storage.tables()
			fmt.Println(" complete", complete[0], "partial", partial[0])
			return err
		}
		for _, entry := range entries {
			if err := tw.addEntry(&entry); err != nil {
				ix.storage.deleteTable(id)
				return err
			}
		}
	}
	tw.setMeta(TableMeta{
		LastBlockNumber: blockNumber,
		BlockCount:      1,
		LastBlockHash:   header.Hash(),
		ParentHash:      header.ParentHash,
	})
	abStatSet(&abStatFinalize)
	for {
		done, err := tw.finalize()
		if err != nil {
			ix.storage.deleteTable(id)
			return err
		}
		if done {
			break
		}
	}
	if err := ix.storage.finalizeTableWriter(tableID{level: 0, index: blockNumber}); err != nil {
		return err
	}
	abStatSet(&abStatOther)
	//fmt.Println(" success")
	return nil
}

func (ix *Indexer) Revert(header *types.Header) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	ix.mergeControl.suspend(false)
	blockNumber := header.Number.Uint64()
	if blockNumber >= ix.headBlock {
		log.Error("Invalid indexer revert", "from", ix.headBlock, "to", blockNumber)
		return
	}
	ix.headBlock = blockNumber
	ix.headBlockHash = header.Hash()
	//ix.recentHeads.Add(blockHash, cachedBlockData{header: header, body: body, receipts: receipts, canonicalUntil: blockNumber})
	ix.cutoffBlock = min(ix.cutoffBlock, ix.headBlock+1)
	ix.storage.deleteTablesFromBlock(blockNumber + 1)
	for _, key := range ix.recentHeads.Keys() {
		bd, _ := ix.recentHeads.Get(key)
		if bd.canonicalUntil > blockNumber {
			bd.canonicalUntil = blockNumber
		}
	}
	if ix.requestedInitBlock != math.MaxUint64 && ix.requestedInitBlock > blockNumber {
		ix.storage.deliverInitBlockHash(ix.requestedInitBlock, common.Hash{})
		ix.requestInitBlock()
	}
	ix.mergeControl.trigger(ix.headBlock, ix.tailBlock(), ix.finalBlock)
}

func (ix *Indexer) tailBlock() uint64 {
	return max(ix.cutoffBlock, max(ix.headBlock, ix.config.History-1)-ix.config.History+1)
}

func (ix *Indexer) SetHistoryCutoff(blockNumber uint64) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	ix.cutoffBlock = min(blockNumber, ix.headBlock+1)
	if ix.requestedInitBlock < blockNumber {
		ix.storage.deliverInitBlockHash(ix.requestedInitBlock, common.Hash{})
		ix.requestInitBlock()
	}
	ix.mergeControl.trigger(ix.headBlock, ix.tailBlock(), ix.finalBlock)
}

func (ix *Indexer) SetFinalized(blockNumber uint64) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	ix.finalBlock = blockNumber
	ix.mergeControl.trigger(ix.headBlock, ix.tailBlock(), ix.finalBlock)
}

func (ix *Indexer) Suspend() {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	ix.mergeControl.suspend(true)
}

func (ix *Indexer) Stop() {
	fmt.Println("Stop")
	ix.lock.Lock()
	if ix.shutdown {
		ix.lock.Unlock()
		return
	}
	ix.shutdown = true
	ix.lock.Unlock()
	if ix.storageInitialized {
		ix.mergeControl.stop()
		fmt.Println(" merge loop stopped")
	}
	ix.storage.close()
	fmt.Println(" table storage stopped")
	ix.files.close()
	fmt.Println(" closed table file manager")
}

func (ix *Indexer) mergeTable(id tableID, stopFn func() bool) error {
	//fmt.Println("mergeTable", id)
	if id.level == 0 {
		panic("cannot merge table on the lowest level")
	}
	sourceIndices := shiftRangeLevel(common.NewRange[uint64](id.index, 1), ix.params.tableLevels[id.level], ix.params.tableLevels[id.level-1], false)
	readers := make([]*TableReader, sourceIndices.Count())
	var entryCount uint64
	for i := range readers {
		tr, err := ix.storage.getTableReader(tableID{level: id.level - 1, index: sourceIndices.First() + uint64(i)})
		if err != nil {
			return err
		}
		entryCount += tr.EntryCount
		readers[i] = tr
	}
	tw, err := ix.storage.getTableWriter(id)
	/*if err == nil && id.level > 4 {
		fmt.Println("*** Resuming merge", id)
		ix.mergeStatTime -= mclock.Now()
	}*/
	if err == errTableNotFound {
		/*if id.level > 4 {
			fmt.Println("*** Starting merge", id)
			ix.mergeStatTime = -mclock.Now()
			ix.mergeStatCount = 0
		}*/
		tw, err = ix.storage.addNewTableWriter(id, entryCount)
		if err != nil {
			return err
		}
		// initialize table metadata
		r := ix.params.blockRange(id)
		tw.setMeta(TableMeta{
			LastBlockNumber: r.Last(),
			BlockCount:      r.Count(),
			LastBlockHash:   readers[len(readers)-1].Meta.LastBlockHash,
			ParentHash:      readers[0].Meta.ParentHash,
		})
	}
	if err != nil {
		return err
	}
	if tw.entryCount != entryCount {
		return errors.New("entry count mismatch")
	}
	lastEntry, nextEntry := tw.lastAndNextEntry()
	if tw.getPhase() == wpWriteEntries && nextEntry != entryCount {
		if lastEntry == nil {
			//fmt.Println("lastAndNextEntry", "nil", nextEntry)
		} else {
			//fmt.Println("lastAndNextEntry", *lastEntry, nextEntry)
		}
		nextReadPosition := make([]uint64, len(readers))
		nextReadEntries := make([]IndexEntries, len(readers))
		if nextEntry != 0 {
			// find next read position in each source reader
			if lastEntry == nil {
				return errors.New("last entry not found")
			}
			var checkNextEntry uint64
			for i, tr := range readers {
				pos, found, err := tr.SeekEntry(lastEntry, false)
				if err != nil {
					return err
				}
				if found {
					pos++
				}
				nextReadPosition[i] = pos
				checkNextEntry += pos
				//fmt.Println(" reader", i, "pos", pos)
			}
			if checkNextEntry != nextEntry {
				//fmt.Println(" checkNextEntry", checkNextEntry)
				panic("xxx")
				return errors.New("next entry mismatch")
			}
		}
		for i, tr := range readers {
			if nextReadPosition[i] < tr.EntryCount {
				var err error
				nextReadEntries[i], err = tr.getEntries(common.NewRange[uint64](nextReadPosition[i], min(mergeEntryPrefetch, tr.EntryCount-nextReadPosition[i])))
				if err != nil {
					return err
				}
			}
		}
		for nextEntry < entryCount {
			if nextEntry%10000 == 0 && stopFn() {
				return nil
			}
			var (
				bestIndex int
				bestEntry *IndexEntry
			)
			for i, entries := range nextReadEntries {
				if len(entries) > 0 && (bestEntry == nil || entries[0].Compare(bestEntry) < 0) {
					bestIndex, bestEntry = i, &entries[0]
				}
			}
			if err := tw.addEntry(bestEntry); err != nil {
				return err
			}
			//ix.mergeStatCount++
			nextEntry++
			nextReadPosition[bestIndex]++
			if tr, pos := readers[bestIndex], nextReadPosition[bestIndex]; pos < tr.EntryCount {
				nextReadEntries[bestIndex] = nextReadEntries[bestIndex][1:]
				if len(nextReadEntries[bestIndex]) == 0 {
					var err error
					nextReadEntries[bestIndex], err = tr.getEntries(common.NewRange[uint64](pos, min(mergeEntryPrefetch, tr.EntryCount-pos)))
					if err != nil {
						return err
					}
				}
			} else {
				nextReadEntries[bestIndex] = nil
			}
			//fmt.Println("merge", id, "write", nextEntry, "read", nextReadPosition, "entry", *bestEntry)
		}
	}
	for {
		if stopFn() {
			return nil
		}
		done, err := tw.finalize()
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	if tw.getPhase() != wpFinalized {
		panic("invalid table write phase")
	}
	if err := ix.storage.finalizeTableWriter(id); err != nil {
		return err
	}
	return nil
}
