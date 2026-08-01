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
	blockRequestLevels         = 2        // priority levels where block requests are processed
	maxMergeThreads            = 4        //TODO config
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
}

type Indexer struct {
	lock               sync.Mutex
	params             *Params
	config             Config
	requestBlock       func(uint64, bool, bool, int) bool
	setIndexerPriority func(int)

	files                                     *tableFiles
	storage                                   *tableStorage
	mergeThreads, lowLevelMergeThreads        int
	currentOps                                []tableOperation
	requiredBlockTables, requestedBlockTables common.RangeSet[uint64]
	indexerPriority                           int
	headBlock, finalBlock, cutoffBlock        uint64
	headBlockHash                             common.Hash
	blockRequests                             map[BlockRequest][]DeliverBlockFn
	queuedBlockRequests                       [blockRequestLevels][]BlockRequest
	requestedInitBlock                        uint64

	hasProcessedBlock    bool
	processedBlockNumber uint64
	processedBlockId     common.Hash
	recentHeads          *lru.Cache[common.Hash, *cachedBlockData]

	shutdown       bool
	updateMergeCh  []chan struct{}
	mergeWg        sync.WaitGroup
	mergeStatTime  mclock.AbsTime
	mergeStatCount uint64
}

func NewIndexer(params *Params, config Config, path string) *Indexer {
	fmt.Println("*** PATH", path)
	files, err := newTableFiles(path, 2000000000, 96) //TODO
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
		files:              files,
		storage:            storage,
		indexerPriority:    2,
		mergeThreads:       maxMergeThreads,
		requestedInitBlock: math.MaxUint64,
		updateMergeCh:      make([]chan struct{}, maxMergeThreads),
		currentOps:         make([]tableOperation, maxMergeThreads),
		recentHeads:        lru.NewCache[common.Hash, *cachedBlockData](headCacheSize),
		blockRequests:      make(map[BlockRequest][]DeliverBlockFn),
	}
	for i := range ix.updateMergeCh {
		ix.updateMergeCh[i] = make(chan struct{}, 1)
	}
	ix.mergeWg.Add(maxMergeThreads)
	for i := range maxMergeThreads {
		go ix.mergeLoop(i)
	}
	return ix
}

type BlockRequest struct {
	Number                 uint64
	NeedBody, NeedReceipts bool
}

type cachedBlockData struct {
	header         *types.Header
	body           *types.Body
	receipts       types.Receipts
	canonicalUntil uint64
}

type DeliverBlockFn = func(BlockRequest, *types.Header, *types.Body, types.Receipts)

func (ix *Indexer) RequestBlock(refBlockHash common.Hash, number uint64, deliverFn DeliverBlockFn) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	refBd, ok := ix.recentHeads.Get(refBlockHash)
	if !ok || number > refBd.header.Number.Uint64() {
		deliverFn(BlockRequest{Number: number, NeedBody: true, NeedReceipts: true}, nil, nil, nil) //TODO van olyan, hogy nem kell minden?
		return
	}
	if number+headCacheSize > refBd.header.Number.Uint64() {
		// look up in cache
		bd := refBd
		for bd != nil && bd.header.Number.Uint64() != number {
			bd, _ = ix.recentHeads.Get(bd.header.ParentHash)
		}
		if bd != nil {
			deliverFn(BlockRequest{Number: number, NeedBody: true, NeedReceipts: true},
				bd.header, bd.body, bd.receipts) //TODO van olyan, hogy nem kell minden?
			return
		}
	}
	if number > refBd.canonicalUntil {
		// not canonical anymore, also not cached
		deliverFn(BlockRequest{Number: number, NeedBody: true, NeedReceipts: true}, nil, nil, nil) //TODO van olyan, hogy nem kell minden?
		return
	}
	ix.getBlockData(number, true, true, 0 /*TODO*/, deliverFn)
}

func (ix *Indexer) GetRangeReaders(refBlockHash common.Hash, blockRange common.Range[uint64]) []*TableReader {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	refBd, ok := ix.recentHeads.Get(refBlockHash)
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

// needs ix.lock
func (ix *Indexer) getBlockData(number uint64, needBody, needReceipts bool, priority int, deliverFn DeliverBlockFn) {
	req := BlockRequest{
		Number:       number,
		NeedBody:     needBody,
		NeedReceipts: needReceipts,
	}
	ix.blockRequests[req] = append(ix.blockRequests[req], deliverFn)
	if ix.requestBlock == nil || !ix.requestBlock(number, needBody, needReceipts, priority) {
		ix.queuedBlockRequests[priority] = append(ix.queuedBlockRequests[priority], req)
	}
}

func (ix *Indexer) processBlockRequests(header *types.Header, body *types.Body, receipts types.Receipts) {
	req := BlockRequest{
		Number:       header.Number.Uint64(),
		NeedBody:     body != nil,
		NeedReceipts: receipts != nil,
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
			if _, ok := ix.blockRequests[reqList[0]]; ok && !ix.requestBlock(reqList[0].Number, reqList[0].NeedBody, reqList[0].NeedReceipts, priority) {
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
	tailBlock := ix.tailBlock()
	ix.requestedBlockTables = ix.requestedBlockTables.Intersection(common.SingleRangeSet[uint64](common.NewRange[uint64](tailBlock, ix.headBlock+1-tailBlock)))
	for req, deliverFns := range ix.blockRequests {
		if req.Number < tailBlock || req.Number > ix.headBlock {
			for _, deliverFn := range deliverFns {
				deliverFn(req, nil, nil, nil)
			}
			delete(ix.blockRequests, req)
		}
	}
}

func (ix *Indexer) updateTableOperations() {
	completeSet, partialSet, initPhase := ix.storage.tables()
	/*fmt.Println("completeSet:", completeSet)
	fmt.Println("partialSet:", partialSet)
	fmt.Println("initPhase:", initPhase)*/
	if initPhase {
		reqNumber, ok := ix.storage.requestInitBlockHash()
		if !ok {
			panic("cannot initialize index table storage")
		}
		if ix.requestedInitBlock == reqNumber {
			return
		}
		fmt.Println("requesting init block", reqNumber)
		ix.getBlockData(reqNumber, false, false, 0, func(req BlockRequest, header *types.Header, body *types.Body, receipts types.Receipts) {
			if header != nil {
				ix.storage.deliverInitBlockHash(reqNumber, header.Hash())
			} else {
				ix.storage.deliverInitBlockHash(reqNumber, common.Hash{})
			}
		})
		ix.requestedInitBlock = reqNumber
		return
	}
	memFileTotal := ix.files.getMemFileTotal()
	tableCountTotal := completeSet.count() + partialSet.count()
	t := int((max(min(memFileTotal, memFileHighThreshold), memFileLowThreshold) - memFileLowThreshold) * uint64(ix.mergeThreads+1) / (memFileHighThreshold - memFileLowThreshold))
	t2 := int((max(min(tableCountTotal, tableCountHighThreshold), tableCountLowThreshold) - tableCountLowThreshold) * uint64(ix.mergeThreads+1) / (tableCountHighThreshold - tableCountLowThreshold))
	t = max(t, t2)
	ix.lowLevelMergeThreads = min(max(ix.lowLevelMergeThreads+1, t)-1, t)
	targetSet := ix.makeTargetSet(completeSet)
	tableOps, requiredBlockTables := ix.params.nextTableOperations(completeSet, partialSet, targetSet, ix.lowLevelMergeThreads, ix.mergeThreads)
	//fmt.Println("updateTableOperations   complete", completeSet.count(), "partial", partialSet.count(), "target", targetSet.count())
	if ix.setIndexerPriority != nil {
		indexerPriority := 2
		if (memFileTotal >= memFileSuspendThreshold || tableCountTotal >= tableCountSuspendThreshold) && len(tableOps) != 0 {
			indexerPriority = 1
		}
		if indexerPriority != ix.indexerPriority {
			ix.indexerPriority = indexerPriority
			ix.setIndexerPriority(indexerPriority)
		}
	}
	updateOps := make([]bool, ix.mergeThreads)
	for i, co := range ix.currentOps {
		var found bool
	innerLoop:
		for j, to := range tableOps {
			if co == to {
				tableOps[j] = tableOperation{}
				found = true
				break innerLoop
			}
		}
		if !found {
			ix.currentOps[i] = tableOperation{}
			updateOps[i] = true
		}
	}
	var i int
	for _, to := range tableOps {
		if to == (tableOperation{}) {
			continue
		}
		for ix.currentOps[i] != (tableOperation{}) {
			i++
		}
		ix.currentOps[i] = to
		updateOps[i] = true
		i++
	}
	for i, update := range updateOps {
		if update {
			//fmt.Println("currentOp:", currentOp)
			select {
			case ix.updateMergeCh[i] <- struct{}{}:
			default:
			}
		}
	}
	//fmt.Println(" new currentOps", ix.currentOps)
	//fmt.Println("requiredBlockTables:", requiredBlockTables, "requestedBlockTables", ix.requestedBlockTables)
	ix.requiredBlockTables = requiredBlockTables
	if ix.requestBlock != nil {
		request := ix.requiredBlockTables.Difference(ix.requestedBlockTables)
		for !request.IsEmpty() && ix.requestBlock(request.Last(), true, true, 2) {
			//fmt.Println("requesting table block", request.Last())
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
	//fmt.Println("makeTargetSet  headBlock", ix.headBlock, "rangeStart", rangeStart, "rangeEnd", rangeEnd)
	target := ix.params.rangeTarget(complete, common.SingleRangeSet[uint64](common.NewRange[uint64](rangeStart, rangeEnd+1-rangeStart)))
	//fmt.Println(" rangeTarget", target)
	for i, pl := range ix.params.protocolLevels {
		first := (max(ix.headBlock, pl.tailAge) - pl.tailAge) / ix.params.tableLevels[i].blockCount
		afterLast := (max(ix.headBlock+1, pl.headAge) - pl.headAge) / ix.params.tableLevels[i].blockCount
		//fmt.Println(" protocolLevel", i, first, afterLast)
		target[i] = target[i].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](first, afterLast-first)))
	}
	//fmt.Println(" finalTarget", target)
	return target
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
						EntryType: IeTopic0 + uint32(ti),
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

func (ix *Indexer) GetProcessedTableRoot(blockId, parentHash common.Hash, transactions types.Transactions, receipts types.Receipts) (common.Hash, error) {
	ix.lock.Lock()
	if ix.hasProcessedBlock {
		if err := ix.storage.deleteTable(tableID{level: 0, index: ix.processedBlockNumber}); err != nil {
			return common.Hash{}, err
		}
		ix.hasProcessedBlock = false
	}
	indexHeadNumber, indexHeadHash := ix.headBlock, ix.headBlockHash
	ix.lock.Unlock()

	if parentHash != indexHeadHash {
		return common.Hash{}, errors.New("parent of processed table block does not match index head")
	}
	blockNumber := indexHeadNumber
	if indexHeadHash != (common.Hash{}) {
		blockNumber++
	}
	ix.storage.deleteTablesFromBlock(blockNumber)
	entries := getBlockEntries(blockNumber, parentHash, transactions, receipts)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Compare(&entries[j]) < 0
	})
	tw, err := ix.storage.addNewTableWriter(tableID{level: 0, index: blockNumber}, uint64(len(entries)))
	if err != nil {
		return common.Hash{}, err
	}
	for _, entry := range entries {
		if err := tw.addEntry(&entry); err != nil {
			ix.storage.deleteTable(tableID{level: 0, index: blockNumber})
			return common.Hash{}, err
		}
	}
	tableRoot := tw.getTableRoot()

	ix.lock.Lock()
	ix.hasProcessedBlock = true
	ix.processedBlockNumber = blockNumber
	ix.processedBlockId = blockId
	ix.lock.Unlock()

	return common.Hash(tableRoot), nil
}

func (ix *Indexer) GetAsyncTableRoot(parentHash common.Hash, firstBlock, tableSize uint64) (common.Hash, error) {
	/*	for i := 1; i < len(ix.params.protocolLevels); i++ {
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
	}*/
	panic("xxx")
}

/*func (ix *Indexer) GetIndexRoots(blockNumber uint64, parentHash common.Hash, transactions types.Transactions, receipts types.Receipts) ([]byte, error) {
	indexRoots := make([]byte, common.HashLength*len(ix.params.protocolLevels))
	if blockNumber > 0 {
		parentHeader, ok := ix.recentHeaders.Get(blockNumber - 1)
		for !ok {
			headerCh := make(chan *types.Header, 1)
			ix.getBlockDataLocked(blockNumber-1, false, false, 0, func(req BlockRequest, header *types.Header, body *types.Body, receipts types.Receipts) {
				headerCh <- header
			})
			parentHeader = <-headerCh
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

	ix.lock.Lock()
	if ix.hasProcessedBlock {
		if err := ix.storage.deleteTable(tableID{level: 0, index: ix.processedBlockNumber}); err != nil {
			return nil, err
		}
		ix.hasProcessedBlock = false
	}
	ix.lock.Unlock()

	ix.storage.deleteTablesFromBlock(blockNumber)
	entries := getBlockEntries(blockNumber, parentHash, transactions, receipts)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Compare(&entries[j]) < 0
	})
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

	ix.lock.Lock()
	ix.hasProcessedBlock = true
	ix.processedBlockNumber = blockNumber
	copy(ix.processedBlockId[:], indexRoots[:common.HashLength])
	ix.lock.Unlock()
	return indexRoots, nil
}*/

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

var (
	abStatCount                                                                                                         uint64
	abStatWaitLock, abStatOther, abStatGetEntries, abStatSortEntries, abStatAddEntries, abStatFinalize, abStatUpdateOps mclock.AbsTime
	abStatCurrent                                                                                                       *mclock.AbsTime
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
	blockNumber := header.Number.Uint64()
	ix.requestedBlockTables = ix.requestedBlockTables.Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](blockNumber, 1)))
	//fmt.Println("AddBlockData", blockNumber, body != nil, receipts != nil)
	if blockNumber >= ix.headBlock {
		ix.headBlock = blockNumber
		ix.headBlockHash = header.Hash()
		ix.recentHeads.Add(header.Hash(), &cachedBlockData{header: header, body: body, receipts: receipts, canonicalUntil: blockNumber})
	}
	//fmt.Println(" processBlockRequests")
	ix.processBlockRequests(header, body, receipts)
	//fmt.Println(" processBlockTables")
	if blockNumber < ix.headBlock && !ix.requiredBlockTables.Includes(blockNumber) {
		//fmt.Println(" unexpected", blockNumber, "required", ix.requiredBlockTables)
		ix.lock.Unlock()
		return // unexpected block, do not create table
	}
	tw, err := ix.getPartialBlockTableWriter(header)
	requested := slices.Clone(ix.requestedBlockTables)
	blockRequests := len(ix.blockRequests)
	ix.lock.Unlock()
	if err == nil {
		err = ix.processBlockTable(tw, header, body, receipts)
	}
	if err == nil {
		abStatSet(&abStatWaitLock)
		ix.lock.Lock()
		abStatSet(&abStatUpdateOps)
		ix.updateTableOperations()
		ix.lock.Unlock()
		abStatSet(nil)
	}
	if err != nil {
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
		fmt.Println(" update table ops", time.Duration(abStatUpdateOps)/time.Duration(abStatCount))
		fmt.Println(" other", time.Duration(abStatOther)/time.Duration(abStatCount))
		fmt.Println("---------------------------------------")
		fmt.Println("+ tables: complete", completeSet.count(), "partial", partialSet.count())
		fmt.Println("+  complete:", completeSet)
		fmt.Println("+  partial:", partialSet)
		fmt.Println("+  blockRequests:", blockRequests)
		fmt.Println("+  requestedBlockTables:", requested.Count(), requested)
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
	blockNumber := header.Number.Uint64()
	if blockNumber >= ix.headBlock {
		log.Error("Invalid indexer revert", "from", ix.headBlock, "to", blockNumber)
		return
	}
	ix.headBlock = blockNumber
	ix.headBlockHash = header.Hash()
	//ix.recentHeads.Add(blockHash, cachedBlockData{header: header, body: body, receipts: receipts, canonicalUntil: blockNumber})
	ix.cutoffBlock = min(ix.cutoffBlock, ix.headBlock+1)
	ix.filterBlockRequests()
	ix.storage.deleteTablesFromBlock(blockNumber + 1)
	ix.updateTableOperations()
	for _, key := range ix.recentHeads.Keys() {
		bd, _ := ix.recentHeads.Get(key)
		if bd.canonicalUntil > blockNumber {
			bd.canonicalUntil = blockNumber
		}
	}
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

func (ix *Indexer) Suspended() { //TODO
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
	for _, ch := range ix.updateMergeCh {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	ix.mergeWg.Wait()
	fmt.Println(" merge loop stopped")
	ix.storage.close()
	fmt.Println(" table storage stopped")
	ix.files.close()
	fmt.Println(" closed table file manager")
}
