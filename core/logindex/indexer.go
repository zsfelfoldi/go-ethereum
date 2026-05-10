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
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	headerCacheSize     = 100
	maxSingleBlockRange = 8
	blockRequestLevels  = 2 // priority levels where block requests are processed
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
}

var DefaultParams = &Params{
	tableLevels: []tableLevel{
		{blockCount: 0x1},
		{blockCount: 0x4},
		{blockCount: 0x10},
		{blockCount: 0x40},
		{blockCount: 0x100},
		{blockCount: 0x400},
		{blockCount: 0x1000},
		{blockCount: 0x4000},
		{blockCount: 0x10000},
		{blockCount: 0x40000},
		{blockCount: 0x100000},
		{blockCount: 0x200000, leanStorage: true},
	},
	protocolLevels: []protocolLevel{
		{tailAge: 5, headAge: 0},
		{tailAge: 20, headAge: 0},
		{tailAge: 80, headAge: 1},
		{tailAge: 320, headAge: 4},
		{tailAge: 8192, headAge: 16},
	},
	fileStorageThresholdHeight: 12,
}

type Indexer struct {
	lock               sync.Mutex
	params             *Params
	config             Config
	requestBlock       func(uint64, bool, bool, int) bool
	setIndexerPriority func(int)

	storage                                   *tableStorage
	currentOp                                 tableOperation
	requiredBlockTables, requestedBlockTables common.RangeSet[uint64]
	indexerPriority                           int
	headBlock, finalBlock, cutoffBlock        uint64
	blockRequests                             map[blockRequest][]blockDeliverFn
	queuedBlockRequests                       [blockRequestLevels][]blockRequest
	requestedInitBlock                        uint64

	hasProcessedBlock    bool
	processedBlockNumber uint64
	processedIndexRoot   common.Hash
	recentHeaders        *lru.Cache[uint64, *types.Header]

	shutdown      bool
	updateMergeCh chan struct{}
	mergeWg       sync.WaitGroup
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
		params:             params,
		config:             config,
		storage:            storage,
		indexerPriority:    2,
		requestedInitBlock: math.MaxUint64,
		updateMergeCh:      make(chan struct{}, 1),
		recentHeaders:      lru.NewCache[uint64, *types.Header](headerCacheSize),
		blockRequests:      make(map[blockRequest][]blockDeliverFn),
	}
	ix.mergeWg.Add(1)
	go ix.mergeLoop()
	return ix
}

type blockRequest struct {
	number                 uint64
	needBody, needReceipts bool
}

type blockDeliverFn = func(blockRequest, *types.Header, *types.Body, types.Receipts)

func (ix *Indexer) getBlockData(number uint64, needBody, needReceipts bool, priority int, deliverFn blockDeliverFn) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	ix.getBlockDataLocked(number, needBody, needReceipts, priority, deliverFn)
}

func (ix *Indexer) getBlockDataLocked(number uint64, needBody, needReceipts bool, priority int, deliverFn blockDeliverFn) {
	req := blockRequest{
		number:       number,
		needBody:     needBody,
		needReceipts: needReceipts,
	}
	ix.blockRequests[req] = append(ix.blockRequests[req], deliverFn)
	if ix.requestBlock == nil || !ix.requestBlock(number, needBody, needReceipts, priority) {
		ix.queuedBlockRequests[priority] = append(ix.queuedBlockRequests[priority], req)
	}
}

func (ix *Indexer) processBlockRequests(header *types.Header, body *types.Body, receipts types.Receipts) {
	req := blockRequest{
		number:       header.Number.Uint64(),
		needBody:     body != nil,
		needReceipts: receipts != nil,
	}
	if deliverFns, ok := ix.blockRequests[req]; ok {
		for _, deliverFn := range deliverFns {
			deliverFn(req, header, body, receipts)
		}
		delete(ix.blockRequests, req)
	}
	ix.sendQueuedBlockRequests()
}

func (ix *Indexer) sendQueuedBlockRequests() {
	if ix.requestBlock == nil {
		return
	}
	for priority, reqList := range ix.queuedBlockRequests {
		for len(reqList) > 0 {
			if _, ok := ix.blockRequests[reqList[0]]; ok && !ix.requestBlock(reqList[0].number, reqList[0].needBody, reqList[0].needReceipts, priority) {
				break
			}
			if len(reqList) > 1 {
				reqList = reqList[1:]
			} else {
				reqList = nil
			}
			ix.queuedBlockRequests[priority] = reqList
		}
	}
}

func (ix *Indexer) filterBlockRequests() {
	for req, deliverFns := range ix.blockRequests {
		if req.number < ix.tailBlock() || req.number > ix.headBlock {
			for _, deliverFn := range deliverFns {
				deliverFn(req, nil, nil, nil)
			}
			delete(ix.blockRequests, req)
		}
	}
}

func (ix *Indexer) updateTableOperations() {
	completeSet, partialSet, initPhase := ix.storage.tables()
	fmt.Println("completeSet:", completeSet)
	fmt.Println("partialSet:", partialSet)
	fmt.Println("initPhase:", initPhase)
	if initPhase {
		reqNumber, ok := ix.storage.requestInitBlockHash()
		if !ok {
			panic("cannot initialize index table storage")
		}
		if ix.requestedInitBlock == reqNumber {
			return
		}
		fmt.Println("requesting init block", reqNumber)
		ix.getBlockDataLocked(reqNumber, false, false, 0, func(req blockRequest, header *types.Header, body *types.Body, receipts types.Receipts) {
			if header != nil {
				ix.storage.deliverInitBlockHash(reqNumber, header.Hash())
			} else {
				ix.storage.deliverInitBlockHash(reqNumber, common.Hash{})
			}
		})
		ix.requestedInitBlock = reqNumber
		return
	}
	indexerPriority := 2
	for _, r := range completeSet[0] {
		if r.Count() >= maxSingleBlockRange {
			indexerPriority = 1
			break
		}
	}
	if indexerPriority != ix.indexerPriority {
		ix.indexerPriority = indexerPriority
		if ix.setIndexerPriority != nil {
			ix.setIndexerPriority(indexerPriority)
		}
	}
	currentOp, requiredBlockTables := ix.params.nextTableOperations(completeSet, partialSet, ix.makeTargetSet(completeSet))
	if currentOp != ix.currentOp {
		fmt.Println("currentOp:", currentOp)
		ix.currentOp = currentOp
		select {
		case ix.updateMergeCh <- struct{}{}:
		default:
		}
	}
	fmt.Println("requiredBlockTables:", requiredBlockTables, "requestedBlockTables", ix.requestedBlockTables)
	ix.requiredBlockTables = requiredBlockTables
	if ix.requestBlock != nil {
		request := ix.requiredBlockTables.Difference(ix.requestedBlockTables)
		for !request.IsEmpty() && ix.requestBlock(request.Last(), true, true, 2) {
			fmt.Println("requesting table block", request.Last())
			requested := common.SingleRangeSet[uint64](common.NewRange[uint64](request.Last(), 1))
			ix.requestedBlockTables = ix.requestedBlockTables.Union(requested)
			request = request.Difference(requested)
		}
	}
}

func (ix *Indexer) makeTargetSet(complete tableSet) tableSet {
	if ix.headBlock == 0 {
		return ix.params.newTableSet()
	}
	rangeStart := ix.tailBlock()
	maxDelay := ix.params.tableLevels[len(ix.params.protocolLevels)-1].blockCount // highest in-protocol table size
	rangeEnd := max(rangeStart+maxDelay, ix.finalBlock+maxDelay, ix.headBlock) - maxDelay
	fmt.Println("makeTargetSet  headBlock", ix.headBlock, "rangeStart", rangeStart, "rangeEnd", rangeEnd)
	target := ix.params.rangeTarget(complete, common.SingleRangeSet[uint64](common.NewRange[uint64](rangeStart, rangeEnd+1-rangeStart)))
	fmt.Println(" rangeTarget", target)
	for i, pl := range ix.params.protocolLevels {
		first := (max(ix.headBlock, pl.tailAge) - pl.tailAge) / ix.params.tableLevels[i].blockCount
		afterLast := (max(ix.headBlock+1, pl.headAge) - pl.headAge) / ix.params.tableLevels[i].blockCount
		fmt.Println(" protocolLevel", i, first, afterLast)
		target[i] = target[i].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](first, afterLast-first)))
	}
	fmt.Println(" finalTarget", target)
	return target
}

func sortedBlockEntries(blockNumber uint64, parentHash common.Hash, txs types.Transactions, receipts types.Receipts) indexEntries {
	var entries indexEntries
	if blockNumber > 0 {
		entries = append(entries, indexEntry{
			indexValue:  ([32]byte)(parentHash),
			blockNumber: blockNumber - 1,
			entryType:   ieBlock,
		})
	}
	for txi, tx := range txs {
		entries = append(entries, indexEntry{
			indexValue:  ([32]byte)(tx.Hash()),
			blockNumber: blockNumber,
			txIndex:     uint32(txi),
			entryType:   ieTransaction,
		})
	}
	for txi, receipt := range receipts {
		for li, log := range receipt.Logs {
			var addr32 [32]byte
			copy(addr32[32-common.AddressLength:], log.Address[:])
			entries = append(entries, indexEntry{
				indexValue:  addr32,
				blockNumber: blockNumber,
				txIndex:     uint32(txi),
				logIndex:    uint32(li),
				entryType:   ieAddress,
			})
			for ti, topic := range log.Topics {
				entries = append(entries, indexEntry{
					indexValue:  ([32]byte)(topic),
					blockNumber: blockNumber,
					txIndex:     uint32(txi),
					logIndex:    uint32(li),
					entryType:   ieTopic0 + uint32(ti),
				})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].compare(&entries[j]) < 0
	})
	return entries
}

func (ix *Indexer) GetIndexRoots(blockNumber uint64, parentHash common.Hash, transactions types.Transactions, receipts types.Receipts) ([]byte, error) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	indexRoots := make([]byte, common.HashLength*len(ix.params.protocolLevels))
	if blockNumber > 0 {
		parentHeader, ok := ix.recentHeaders.Get(blockNumber - 1)
		for !ok {
			headerCh := make(chan *types.Header, 1)
			ix.getBlockData(blockNumber-1, false, false, 0, func(req blockRequest, header *types.Header, body *types.Body, receipts types.Receipts) {
				headerCh <- header
			})
			ix.lock.Unlock()
			parentHeader = <-headerCh
			ix.lock.Lock()
			if parentHeader == nil {
				return nil, errors.New("could not retrieve parent header")
			}
		}
		if parentHeader.Hash() != parentHash {
			return nil, errors.New("parent header hash mismatch")
		}
		if len(parentHeader.BloomOrIndex) == len(indexRoots) {
			copy(indexRoots, parentHeader.BloomOrIndex)
		}
	}

	if ix.hasProcessedBlock {
		if err := ix.storage.deleteTable(tableID{level: 0, index: ix.processedBlockNumber}); err != nil {
			return nil, err
		}
		ix.hasProcessedBlock = false
	}

	ix.storage.deleteTablesFromBlock(blockNumber)
	entries := sortedBlockEntries(blockNumber, parentHash, transactions, receipts)
	tw, err := ix.storage.addNewTableWriter(tableID{level: 0, index: blockNumber}, uint64(len(entries)))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := tw.addEntry(&entry); err != nil {
			ix.storage.deleteTable(tableID{level: 0, index: blockNumber})
			return nil, err
		}
	}
	tableRoot := tw.getTableRoot()
	updateIndexRoot(indexRoots[:common.HashLength], tableRoot[:])
	for i := 1; i < len(ix.params.protocolLevels); i++ {
		blockCount := ix.params.tableLevels[i].blockCount
		headAge := ix.params.protocolLevels[i].headAge
		if blockNumber >= headAge && (blockNumber-headAge)%blockCount == blockCount-1 { //TODO fork block
			id := tableID{level: i, index: (blockNumber - headAge) / blockCount}
			tr, err := ix.storage.waitForTableReader(id)
			if err != nil {
				return nil, err
			}
			updateIndexRoot(indexRoots[common.HashLength*i:common.HashLength*(i+1)], tr.tableRoot[:])
		}
	}

	ix.hasProcessedBlock = true
	ix.processedBlockNumber = blockNumber
	copy(ix.processedIndexRoot[:], indexRoots[:common.HashLength])
	return indexRoots, nil
}

func updateIndexRoot(rootSection, newTableRoot []byte) {
	hasher := sha256.New()
	hasher.Write(rootSection)
	hasher.Write(newTableRoot)
	var result common.Hash
	hasher.Sum(result[:0])
	copy(rootSection, result[:])
}

func (ix *Indexer) Register(requestBlock func(uint64, bool, bool, int) bool, setIndexerPriority func(int)) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	ix.requestBlock, ix.setIndexerPriority = requestBlock, setIndexerPriority
	ix.sendQueuedBlockRequests()
	ix.setIndexerPriority(ix.indexerPriority)
}

func (ix *Indexer) AddBlockData(header *types.Header, body *types.Body, receipts types.Receipts) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	blockNumber := header.Number.Uint64()
	fmt.Println("AddBlockData", blockNumber, body != nil, receipts != nil)
	if blockNumber >= ix.headBlock {
		ix.headBlock = blockNumber
		ix.recentHeaders.Add(blockNumber, header)
	}
	ix.processBlockRequests(header, body, receipts)
	if err := ix.processBlockTables(header, body, receipts); err != nil {
		log.Error("Failed to add block data to log index", "number", header.Number.Uint64(), "error", err)
	}
}

func (ix *Indexer) processBlockTables(header *types.Header, body *types.Body, receipts types.Receipts) error {
	var tw *tableWriter
	blockNumber := header.Number.Uint64()
	if blockNumber < ix.headBlock && !ix.requiredBlockTables.Includes(blockNumber) {
		fmt.Println(" unexpected", blockNumber, "required", ix.requiredBlockTables)
		return nil // unexpected block, do not create table
	}
	ix.requestedBlockTables = ix.requestedBlockTables.Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](blockNumber, 1)))
	if ix.hasProcessedBlock && ix.processedBlockNumber == blockNumber &&
		len(header.BloomOrIndex) == common.HashLength*len(ix.params.protocolLevels) {
		var indexRoot common.Hash
		copy(indexRoot[:], header.BloomOrIndex[:common.HashLength])
		if indexRoot == ix.processedIndexRoot {
			var err error
			if tw, err = ix.storage.getTableWriter(tableID{level: 0, index: ix.processedBlockNumber}); err != nil {
				return err
			}
		} else {
			if err := ix.storage.deleteTable(tableID{level: 0, index: ix.processedBlockNumber}); err != nil {
				return err
			}
		}
		ix.hasProcessedBlock = false
	}
	if tw == nil {
		entries := sortedBlockEntries(blockNumber, header.ParentHash, body.Transactions, receipts)
		var err error
		if tw, err = ix.storage.addNewTableWriter(tableID{level: 0, index: blockNumber}, uint64(len(entries))); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := tw.addEntry(&entry); err != nil {
				ix.storage.deleteTable(tableID{level: 0, index: blockNumber})
				return err
			}
		}
	}
	tw.setMeta(tableMeta{
		LastBlockNumber: blockNumber,
		BlockCount:      1,
		LastBlockHash:   header.Hash(),
		ParentHash:      header.ParentHash,
	})
	for {
		done, err := tw.finalize()
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	if err := ix.storage.finalizeTableWriter(tableID{level: 0, index: blockNumber}); err != nil {
		return err
	}
	fmt.Println(" success")
	ix.updateTableOperations()
	return nil
}

func (ix *Indexer) Revert(blockNumber uint64) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	if blockNumber >= ix.headBlock {
		log.Error("Invalid indexer revert", "from", ix.headBlock, "to", blockNumber)
		return
	}
	ix.headBlock = blockNumber
	ix.filterBlockRequests()
	ix.storage.deleteTablesFromBlock(blockNumber + 1)
	ix.updateTableOperations()
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
	ix.cutoffBlock = blockNumber
	ix.filterBlockRequests()
	ix.updateTableOperations()
}

func (ix *Indexer) SetFinalized(blockNumber uint64) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	if ix.shutdown {
		return
	}
	ix.finalBlock = blockNumber
	ix.updateTableOperations()
}

func (ix *Indexer) Suspended() {
	ix.lock.Lock()
	defer ix.lock.Unlock()

}

func (ix *Indexer) Stop() {
	fmt.Println("Stop")
	ix.lock.Lock()
	if ix.shutdown {
		return
	}
	ix.shutdown = true
	ix.lock.Unlock()
	select {
	case ix.updateMergeCh <- struct{}{}:
	default:
	}
	ix.mergeWg.Wait()
	ix.storage.close()
}
