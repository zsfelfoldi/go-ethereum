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

// Package core implements the Ethereum consensus protocol.
package core

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	queueCapacity        = 8 // size of block data pre-fetch queue
	rawReceiptsCacheSize = 8
	priorityLevels       = 3
	logFrequency         = time.Second * 20 // log info frequency during long indexing/unindexing process
	headLogDelay         = time.Second      // head indexing log info delay (do not log if finished faster)

	enableIndexStats = true
)

type Indexer interface {
	Register(requestBlock func(uint64, bool, bool, int) bool, setIndexerPriority func(int))
	// AddBlockData delivers a header and receipts belonging to a block that is
	// either a direct descendant of the latest delivered head or the first one
	// in the last requested range.
	// The current ready/busy status and the requested historic range are returned.
	// Note that the indexer should never block even if it is busy processing.
	// It is allowed to re-request the delivered blocks later if the indexer could
	// not process them when first delivered.
	AddBlockData(header *types.Header, body *types.Body, receipts types.Receipts)
	// Revert rewinds the index to the given head block number. Subsequent
	// AddBlockData calls will deliver blocks starting from this point.
	Revert(blockNumber uint64)
	// SetHistoryCutoff signals the historical cutoff point to the indexer.
	// Note that any block number that is consistently being requested in the
	// needBlocks response that is not older than the cutoff point is guaranteed
	// to be delivered eventually. If the required data belonging to certain
	// block numbers is missing then the cutoff point is moved after the missing
	// section in order to maintain this guarantee.
	SetHistoryCutoff(blockNumber uint64)
	// SetFinalized signals the latest finalized block number to the indexer.
	SetFinalized(blockNumber uint64)
	// Suspended signals to the indexer that block processing has started and
	// any non-essential asynchronous tasks of the indexer should be suspended.
	// The next AddBlockData call signals the end of the suspended state.
	// Note that if multiple blocks are inserted then the indexer is only
	// suspended once, before the first block processing begins, so according
	// to the rule above it will not be suspended while processing the rest of
	// the batch. This behavior should be fine because indexing can happen in
	// parallel with forward syncing, the purpose of the suspend mechanism is
	// to handle historical index backfilling with a lower priority so that it
	// does not increase block latency.
	Suspended()
	// Stop initiates indexer shutdown. No subsequent calls are made through this
	// interface after Stop.
	Stop()
}

// indexServers operates as a part of BlockChain and can serve multiple chain
// indexers that implement the Indexer interface.
type indexServers struct {
	lock             sync.Mutex
	servers          []*indexServer
	chain            *BlockChain
	rawReceiptsCache *lru.Cache[common.Hash, []*types.Receipt]

	lastHead                  *types.Header
	lastHeadBody              *types.Body
	lastHeadReceipts          types.Receipts
	finalBlock, historyCutoff uint64

	closeCh chan struct{}
	closeWg sync.WaitGroup
}

// init initializes indexServers.
func (f *indexServers) init(chain *BlockChain) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.chain = chain
	f.lastHead = chain.CurrentBlock()
	if f.lastHead != nil {
		f.lastHeadBody = chain.GetBody(f.lastHead.Hash())
		f.lastHeadReceipts = chain.GetRawReceipts(f.lastHead.Hash(), f.lastHead.Number.Uint64())
	}
	f.closeCh = make(chan struct{})
	f.rawReceiptsCache = lru.NewCache[common.Hash, []*types.Receipt](rawReceiptsCacheSize)
}

// stop shuts down all registered Indexers and their serving goroutines.
func (f *indexServers) stop() {
	f.lock.Lock()
	defer f.lock.Unlock()

	close(f.closeCh)
	f.closeWg.Wait()
	f.servers = nil
}

// register adds a new Indexer to the chain.
func (f *indexServers) register(indexer Indexer, name string) {
	f.lock.Lock()
	defer f.lock.Unlock()

	server := &indexServer{
		parent:            f,
		indexer:           indexer,
		name:              name,
		revertBlocks:      make(map[uint64]uint64),
		revertCountCh:     make(chan uint64, 1),
		deliveredCountCh:  make(chan [priorityLevels]uint64, 1),
		readPriorityCh:    make(chan priorityUpdate, 1),
		deliverPriorityCh: make(chan int, 1),
		indexerPriority:   priorityLevels - 1,
		systemPriority:    priorityLevels - 1,
	}
	for i := range priorityLevels {
		server.blockRequestCh[i] = make(chan blockRequest, queueCapacity)
		server.blockDataCh[i] = make(chan blockData, queueCapacity)
	}
	f.closeWg.Add(2)
	if enableIndexStats {
		server.stats = &indexStats{}
		server.stats.total.set(mclock.Now(), true)
		f.closeWg.Add(1)
		go server.printStatsLoop()
	}
	f.servers = append(f.servers, server)
	indexer.Register(server.requestBlock, server.setIndexerPriority)
	indexer.SetHistoryCutoff(f.historyCutoff)
	indexer.SetFinalized(f.finalBlock)
	if f.lastHead != nil && f.lastHeadBody != nil && f.lastHeadReceipts != nil {
		server.sendHeadBlockData(f.lastHead, f.lastHeadBody, f.lastHeadReceipts)
	}
	go server.historicReadLoop()
	go server.historicDeliverLoop()
}

// cacheRawReceipts caches a set of raw receipts during block processing in order
// to avoid having to read it back from the database during broadcast.
func (f *indexServers) cacheRawReceipts(blockHash common.Hash, blockReceipts types.Receipts) {
	f.rawReceiptsCache.Add(blockHash, blockReceipts)
}

// broadcast sends a new head block to all registered Indexer instances.
func (f *indexServers) broadcast(block *types.Block) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// Note that individual Indexer servers might ignore block bodies and
	// receipts. We still always fetch receipts for simplicity because in the
	// typical case it is cached during block processing and costs nothing.
	blockHash := block.Hash()
	blockReceipts, _ := f.rawReceiptsCache.Get(blockHash)
	if blockReceipts == nil {
		blockReceipts = f.chain.GetRawReceipts(blockHash, block.NumberU64())
		if blockReceipts == nil {
			log.Error("Receipts belonging to new head are missing", "number", block.NumberU64(), "hash", block.Hash())
			return
		}
		f.rawReceiptsCache.Add(blockHash, blockReceipts)
	}
	f.lastHead, f.lastHeadBody, f.lastHeadReceipts = block.Header(), block.Body(), blockReceipts
	for _, server := range f.servers {
		server.sendHeadBlockData(block.Header(), block.Body(), blockReceipts)
	}
}

// revert notifies all registered Indexer instances about the chain being rolled
// back to the given head or last common ancestor.
func (f *indexServers) revert(header *types.Header) {
	f.lock.Lock()
	defer f.lock.Unlock()

	for _, server := range f.servers {
		server.revert(header)
	}
}

// setFinalBlock notifies all Indexer instances about the latest finalized block.
func (f *indexServers) setFinalBlock(blockNumber uint64) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.finalBlock == blockNumber {
		return
	}
	f.finalBlock = blockNumber
	for _, server := range f.servers {
		server.setFinalBlock(blockNumber)
	}
}

// setHistoryCutoff notifies all Indexer instances about the history cutoff point.
// The indexers cannot expect any data being delivered if needBlocks.First() is
// before this point.
func (f *indexServers) setHistoryCutoff(blockNumber uint64) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.historyCutoff == blockNumber {
		return
	}
	f.historyCutoff = blockNumber
	for _, server := range f.servers {
		server.setHistoryCutoff(blockNumber)
	}
}

// setBlockProcessing suspends serving historical blocks requested by the indexers
// while a chain segment is being processed and added to the chain.
func (f *indexServers) setBlockProcessing(processing bool) {
	f.lock.Lock()
	defer f.lock.Unlock()

	for _, server := range f.servers {
		server.setBlockProcessing(processing)
	}
}

// indexServer sends updates to a single Indexer instance. It sends all new heads
// and reorg events, and also sends historical block data upon request.
// It guarantees that Indexer functions are never called concurrently and also
// they always present a consistent view of the chain to the indexer.
type indexServer struct {
	indexerLock sync.Mutex
	parent      *indexServers
	indexer     Indexer // always call under mutex lock; never call after stopped
	stopped     bool

	lastHead                          *types.Header
	deliveredCount                    [priorityLevels]uint64
	revertCount, lastRevertCount      uint64
	revertBlocks                      map[uint64]uint64
	historyCutoff, missingBlockCutoff uint64

	blockRequestCh   [priorityLevels]chan blockRequest // Indexer => callback -> historicReadLoop (cap=queueCapacity)
	revertCountCh    chan uint64                       // indexServer -> historicReadLoop (cap=1)
	deliveredCountCh chan [priorityLevels]uint64       // historicDeliverLoop -> historicReadLoop (cap=1)
	blockDataCh      [priorityLevels]chan blockData    // historicReadLoop -> historicDeliverLoop => Indexer (cap=queueCapacity)

	readPriorityCh                  chan priorityUpdate // Indexer => callback -> historicReadLoop (cap=1)
	deliverPriorityCh               chan int            // Indexer => callback -> historicDeliverLoop (cap=1)
	priorityLock                    sync.Mutex
	indexerPriority, systemPriority int

	testSuspendHookCh       chan struct{} // initialized by test (cap=1)
	name                    string
	processed               uint64
	logged                  bool
	startedAt, lastLoggedAt time.Time
	lastHistoryErrorLog     time.Time
	stats                   *indexStats
}

type blockRequest struct {
	blockNumber            uint64
	needBody, needReceipts bool
}

// blockData represents the indexable data of a single block being sent from the
// reader to the sender goroutine and optionally queued in blockDataCh between.
// It also includes the latest revertCount known before reading the block data,
// which allows the sender to guarantee that all sent block data is always
// consistent with the indexer's canonical chain view while the reading of block
// data can still happen asynchronously.
type blockData struct {
	request     blockRequest
	valid       bool
	revertCount uint64
	header      *types.Header
	body        *types.Body
	receipts    types.Receipts
}

// sendHeadBlockData immediately sends the latest head block data to the indexer
// and updates the status of the historical block data serving mechanism
// accordingly.
func (s *indexServer) sendHeadBlockData(header *types.Header, body *types.Body, receipts types.Receipts) {
	s.indexerLock.Lock()
	defer s.indexerLock.Unlock()

	if s.stopped {
		return
	}
	if header.Hash() == s.lastHead.Hash() {
		return
	}
	s.lastHead = header
	s.indexer.AddBlockData(header, body, receipts)
}

// revert immediately reverts the indexer to the given block and updates the
// status of the historical block data serving mechanism accordingly.
func (s *indexServer) revert(header *types.Header) {
	s.indexerLock.Lock()
	defer s.indexerLock.Unlock()

	if s.stopped || s.lastHead == nil {
		return
	}
	if header.Hash() == s.lastHead.Hash() {
		return
	}
	blockNumber := header.Number.Uint64()
	if blockNumber >= s.lastHead.Number.Uint64() {
		panic("invalid indexer revert")
	}
	s.indexer.Revert(blockNumber)
	s.revertBlocks[s.revertCount] = blockNumber
	s.revertCount++
	s.lastHead = header
}

func (s *indexServer) requestBlock(blockNumber uint64, needBody, needReceipts bool, priority int) bool {
	req := blockRequest{
		blockNumber:  blockNumber,
		needBody:     needBody,
		needReceipts: needReceipts,
	}
	select {
	case s.blockRequestCh[priority] <- req:
		//fmt.Println("*** request", req, "success")
		return true
	default:
		//fmt.Println("*** request", req, "fail")
		return false
	}
}

func (s *indexServer) setIndexerPriority(priority int) {
	s.priorityLock.Lock()
	defer s.priorityLock.Unlock()

	if s.indexerPriority == priority {
		return
	}
	s.indexerPriority = priority
	s.updatePriority()
}

func (s *indexServer) setSystemPriority(priority int) {
	s.priorityLock.Lock()
	defer s.priorityLock.Unlock()

	if s.systemPriority == priority {
		return
	}
	s.systemPriority = priority
	s.updatePriority()
}

type priorityUpdate struct {
	system, indexer int
}

func (u priorityUpdate) value() int {
	return min(u.system, u.indexer)
}

func (s *indexServer) updatePriority() {
	//fmt.Println("*** updatePriority", s.indexerPriority, s.systemPriority)
	update := priorityUpdate{s.systemPriority, s.indexerPriority}
	select {
	case <-s.deliverPriorityCh:
	default:
	}
	s.deliverPriorityCh <- update.value()

	select {
	case <-s.readPriorityCh:
	default:
	}
	s.readPriorityCh <- update
}

func (s *indexServer) historicReadLoop() {
	defer s.parent.closeWg.Done()

	var (
		priority, priorityBefore          priorityUpdate
		canDeliverBelow, canDeliverBefore int
		revertCount                       uint64
		readCount, deliveredCount         [priorityLevels]uint64
	)
	handleRequest := func(priority int, req blockRequest) {
		if s.stats != nil {
			now := mclock.Now()
			s.stats.setWaiting(mclock.Now(), priorityBefore.system, priorityBefore.indexer, canDeliverBefore, false)
			s.stats.readBlockData[priority].set(now, true)
		}
		s.blockDataCh[priority] <- s.readBlockData(req, revertCount)
		if s.stats != nil {
			s.stats.readBlockData[priority].set(mclock.Now(), false)
		}
		readCount[priority]++
	}

	for {
		canDeliverBelow = priorityLevels
		for i := range priorityLevels {
			if readCount[i] >= deliveredCount[i]+queueCapacity {
				canDeliverBelow = i
				break
			}
		}
		waitRequestBelow := min(canDeliverBelow, priority.value()+1)
		if s.stats != nil {
			priorityBefore, canDeliverBefore = priority, canDeliverBelow
			s.stats.setWaiting(mclock.Now(), priority.system, priority.indexer, canDeliverBelow, true)
		}
		switch waitRequestBelow {
		case 0:
			select {
			case <-s.parent.closeCh:
				return
			case priority = <-s.readPriorityCh:
			case revertCount = <-s.revertCountCh:
			case deliveredCount = <-s.deliveredCountCh:
			}
		case 1:
			select {
			case <-s.parent.closeCh:
				return
			case priority = <-s.readPriorityCh:
			case revertCount = <-s.revertCountCh:
			case deliveredCount = <-s.deliveredCountCh:
			case req := <-s.blockRequestCh[0]:
				handleRequest(0, req)
			}
		case 2:
			select {
			case <-s.parent.closeCh:
				return
			case priority = <-s.readPriorityCh:
			case revertCount = <-s.revertCountCh:
			case deliveredCount = <-s.deliveredCountCh:
			case req := <-s.blockRequestCh[0]:
				handleRequest(0, req)
			case req := <-s.blockRequestCh[1]:
				handleRequest(1, req)
			}
		case 3:
			select {
			case <-s.parent.closeCh:
				return
			case priority = <-s.readPriorityCh:
			case revertCount = <-s.revertCountCh:
			case deliveredCount = <-s.deliveredCountCh:
			case req := <-s.blockRequestCh[0]:
				handleRequest(0, req)
			case req := <-s.blockRequestCh[1]:
				handleRequest(1, req)
			case req := <-s.blockRequestCh[2]:
				handleRequest(2, req)
			}
		default:
			panic("invalid priority value")
		}
		if s.stats != nil {
			s.stats.setWaiting(mclock.Now(), priorityBefore.system, priorityBefore.indexer, canDeliverBefore, false)
		}
	}

}

func (s *indexServer) readBlockData(req blockRequest, revertCount uint64) (bd blockData) {
	bd.request, bd.revertCount = req, revertCount
	if bd.header = s.parent.chain.GetHeaderByNumber(req.blockNumber); bd.header != nil {
		bd.valid = true
		blockHash := bd.header.Hash()
		if req.needBody {
			bd.body = s.parent.chain.GetBody(blockHash)
			if bd.body == nil {
				bd.valid = false
			}
		}
		if req.needReceipts {
			bd.receipts, _ = s.parent.rawReceiptsCache.Get(blockHash)
			if bd.receipts == nil {
				// Note: we do not cache historical receipts because the indexer
				// typically requests them once
				bd.receipts = s.parent.chain.GetRawReceipts(blockHash, bd.request.blockNumber)
				if bd.receipts == nil {
					bd.valid = false
				}
			}
		}
	}
	return
}

func (s *indexServer) historicDeliverLoop() {
	defer func() {
		s.indexerLock.Lock()
		s.stopped = true
		s.indexer.Stop()
		s.indexerLock.Unlock()
		s.parent.closeWg.Done()
	}()

	var maxPriority int
	for {
		switch maxPriority {
		case 0:
			select {
			case <-s.parent.closeCh:
				return
			case maxPriority = <-s.deliverPriorityCh:
			case bd := <-s.blockDataCh[0]:
				s.deliverBlockData(bd, 0)
			}
		case 1:
			select {
			case <-s.parent.closeCh:
				return
			case maxPriority = <-s.deliverPriorityCh:
			case bd := <-s.blockDataCh[0]:
				s.deliverBlockData(bd, 0)
			case bd := <-s.blockDataCh[1]:
				s.deliverBlockData(bd, 1)
			}
		case 2:
			select {
			case <-s.parent.closeCh:
				return
			case maxPriority = <-s.deliverPriorityCh:
			case bd := <-s.blockDataCh[0]:
				s.deliverBlockData(bd, 0)
			case bd := <-s.blockDataCh[1]:
				s.deliverBlockData(bd, 1)
			case bd := <-s.blockDataCh[2]:
				s.deliverBlockData(bd, 2)
			}
		default:
			panic("invalid priority value")
		}
	}
}

func (s *indexServer) deliverBlockData(bd blockData, priority int) {
	/*var number uint64
	if bd.header != nil {
		number = bd.header.Number.Uint64()
	}
	fmt.Println("*** deliverBlockData", number, bd.header != nil, bd.body != nil, bd.receipts != nil, priority)*/
	s.indexerLock.Lock()
	defer func() {
		s.deliveredCount[priority]++
	loop:
		for {
			select {
			case s.deliveredCountCh <- s.deliveredCount:
				break loop
			default:
			}
			select {
			case <-s.deliveredCountCh:
			default:
			}
		}
		s.indexerLock.Unlock()
		//fmt.Println("*** deliverBlockData done", number, bd.header != nil, bd.body != nil, bd.receipts != nil, priority)
	}()

	for i := s.lastRevertCount; i < bd.revertCount; i++ {
		delete(s.revertBlocks, i)
	}
	s.lastRevertCount = bd.revertCount
	for i := bd.revertCount; i < s.revertCount; i++ {
		if s.revertBlocks[i] <= bd.request.blockNumber {
			return
		}
	}
	if bd.request.blockNumber < max(s.historyCutoff, s.missingBlockCutoff) ||
		bd.request.blockNumber > s.lastHead.Number.Uint64() {
		return
	}
	if bd.valid {
		if s.stats != nil {
			s.stats.indexerProcessing[priority].set(mclock.Now(), true)
		}
		s.indexer.AddBlockData(bd.header, bd.body, bd.receipts)
		if s.stats != nil {
			s.stats.indexerProcessing[priority].set(mclock.Now(), false)
		}
	} else {
		// report error and update missingBlockCutoff in order to
		// avoid spinning forever on the same error.
		if time.Since(s.lastHistoryErrorLog) >= time.Second*10 {
			s.lastHistoryErrorLog = time.Now()
			if bd.header == nil {
				log.Error("Historical header is missing", "number", bd.request.blockNumber)
			} else if bd.request.needBody && bd.body == nil {
				log.Error("Historical block body is missing", "number", bd.request.blockNumber, "hash", bd.header.Hash())
			} else if bd.request.needReceipts && bd.receipts == nil {
				log.Error("Historical receipts are missing", "number", bd.request.blockNumber, "hash", bd.header.Hash())
			}
		}
		s.missingBlockCutoff = max(s.missingBlockCutoff, bd.request.blockNumber+1)
		s.indexer.SetHistoryCutoff(max(s.historyCutoff, s.missingBlockCutoff))
	}
}

// setBlockProcessing suspends serving historical blocks requested by the indexer
// while a chain segment is being processed and added to the chain.
func (s *indexServer) setBlockProcessing(processing bool) {
	if processing {
		s.setSystemPriority(1)
	} else {
		s.setSystemPriority(2)
	}
	if processing && s.testSuspendHookCh != nil {
		select {
		case s.testSuspendHookCh <- struct{}{}:
		default:
		}
	}
}

// logDelivered periodically prints log messages that report the current state
// of the indexing process. If should be called after processing each new block.
func (s *indexServer) logDelivered(position uint64) {
	if s.processed == 0 {
		s.startedAt = time.Now()
	}
	s.processed++
	if s.logged {
		if time.Since(s.lastLoggedAt) < logFrequency {
			return
		}
	} else {
		if time.Since(s.startedAt) < headLogDelay {
			return
		}
		s.logged = true
	}
	s.lastLoggedAt = time.Now()
	log.Info("Generating "+s.name, "block", position, "processed", s.processed, "elapsed", time.Since(s.startedAt))
}

// logFinished prints a log message that report the end of the indexing process.
// Note that any log message is only printed if the process took longer than
// headLogDelay.
func (s *indexServer) logFinished() {
	if s.logged {
		log.Info("Finished "+s.name, "processed", s.processed, "elapsed", time.Since(s.startedAt))
		s.logged = false
	}
	s.processed = 0
}

// setFinalBlock notifies the Indexer instance about the latest finalized block.
func (s *indexServer) setFinalBlock(blockNumber uint64) {
	s.indexerLock.Lock()
	defer s.indexerLock.Unlock()

	if s.stopped {
		return
	}
	s.indexer.SetFinalized(blockNumber)
}

// setHistoryCutoff notifies the Indexer instance about the history cutoff point.
// The indexer cannot expect any data being delivered if needBlocks.First() is
// before this point.
// Note that if some historical block data could not be loaded from the database
// then the historical cutoff point reported to the indexer might be modified by
// missingBlockCutoff. This workaround ensures that the indexing process does not
// get stuck permanently in case of missing data.
func (s *indexServer) setHistoryCutoff(blockNumber uint64) {
	s.indexerLock.Lock()
	defer s.indexerLock.Unlock()

	if s.stopped {
		return
	}
	s.historyCutoff = blockNumber
	s.indexer.SetHistoryCutoff(max(s.historyCutoff, s.missingBlockCutoff))
}

func (s *indexServer) printStatsLoop() {
	defer s.parent.closeWg.Done()
	for {
		select {
		case <-s.parent.closeCh:
			return
		case <-time.After(time.Second * 10):
			s.stats.printAndReset()
		}
	}
}
