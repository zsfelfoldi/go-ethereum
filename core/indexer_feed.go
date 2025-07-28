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

type Indexer interface {
	AddBlockData(headers []*types.Header, receipts []types.Receipts) (bool, common.Range[uint64])
	Revert(blockNumber uint64)
	MissingBlocks(missing common.Range[uint64])
	Status() (bool, common.Range[uint64])
	Stop()
}

type indexerFeed struct {
	serversLock sync.Mutex
	servers     []*indexServer
	chain       *BlockChain

	headers  []*types.Header  // broadcast head header batch
	receipts []types.Receipts // broadcast head receipts batch

	historyCutoff uint64
	closeCh       chan struct{}
	closeWg       sync.WaitGroup
}

func (f *indexerFeed) init(chain *BlockChain) {
	f.chain = chain
	f.closeCh = make(chan struct{})
}

func (f *indexerFeed) register(indexer Indexer) {
	f.serversLock.Lock()
	defer f.serversLock.Unlock()

	server := &indexServer{
		parent:    f,
		indexer:   indexer,
		sendTimer: time.NewTimer(0),
		lastHead:  f.chain.CurrentBlock(),
	}
	f.servers = append(i.servers, server)
	f.closeWg.Add(1)
	indexer.MissingBlocks(common.NewRange[uint64](0, f.historyCutoff))
	receipts := f.chain.GetReceiptsByHash(server.lastHead.Hash()) //TODO number and hash
	if receipts != nil {
		indexer.AddBlockData([]*types.Header{server.lastHead}, []types.Receipts{receipts})
	} else {
		log.Error("Receipts belonging to init head are missing", "number", server.lastHead.Number, "hash", server.lastHead.Hash())
	}
	go server.eventLoop()
}

func (f *indexerFeed) stop() {
	close(f.closeCh)
	f.closeWg.Wait()
}

func (f *indexerFeed) broadcast(header *types.Header, head bool) {
	f.serversLock.Lock()
	defer f.serversLock.Unlock()

	receipts := s.parent.chain.GetReceiptsByHash(header.Hash()) //TODO number and hash
	if receipts == nil {
		log.Error("Receipts belonging to new head are missing", "number", header.Number, "hash", header.Hash())
		return
	}
	f.headers = append(f.headers, header)
	f.receipts = append(f.receipts, receipts)
	if head || len(f.headers) >= maxBatchLength {
		for _, server := range f.servers {
			server.sendBlockData(f.headers, f.receipts)
		}
		f.headers = f.headers[:0]
		f.receipts = f.receipts[:0]
	}
}

func (f *indexerFeed) revert(header *types.Header) {
	for _, server := range f.servers {
		server.revert(header)
	}
}

type indexServer struct {
	lock    sync.Mutex
	parent  *indexerFeed
	indexer Indexer // always call under mutex lock; never call after stopped
	stopped bool

	lastHead   *types.Header
	ready      bool
	sendTimer  *time.Timer
	needBlocks common.Range[uint64]
}

//TODO Status
func (s *indexServer) eventLoop() {
	for {
		select {
		case <-s.sendTimer.C:
			s.lock.Lock()
			first := max(s.needBlocks.First(), s.historyCutoff)
			afterLast := min(first+maxBatchLength, s.needBlocks.AfterLast(), s.lastHead.Number.Uint64()+1)
			s.lock.Unlock()
			if first < afterLast {
				headers, receipts := s.historicBlockData(first, afterLast)
				s.sendBlockData(headers, receipts)
			}
		case <-s.parent.closeCh:
			s.lock.Lock()
			s.indexer.Stop()
			s.stopped = true
			s.lock.Unlock()
			s.parent.closeWg.Done()
			return
		}
	}
}

func (s *indexServer) revert(header *types.Header) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if !s.stopped {
		s.indexer.Revert(header.Number.Uint64())
		s.lastHead = header
	}
}

func (s *indexServer) sendBlockData(headers []*types.Header, receipts []types.Receipts) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if !s.stopped {
		s.ready, s.needBlocks = s.indexer.AddBlockData(headers, receipts)
		s.lastHead = headers[len(headers)-1]
		if s.needBlocks.IsEmpty() {
			s.sendTimer.Stop()
		} else {
			if s.ready {
				s.sendTimer.Reset(0)
			} else {
				s.sendTimer.Reset(busyDelay)
			}
		}
	}
}

func (s *indexServer) historicBlockData(first, afterLast uint64) (headers []*types.Header, receipts []types.Receipts) {
	s.lock.Lock()
	head := s.lastHead
	s.lock.Unlock()
	numbers := make([]uint64, afterLast+1-first)
	for number := first; number < afterLast; number++ {
		numbers[i-first] = number
	}
	numbers[afterLast-first] = head.Number.Uint64()
	hashes, err := s.parent.chain.GetCanonicalHashes(numbers)
	if err != nil || len(hashes) != afterLast+1-first || hashes[afterLast-first] != head.Hash() {
		return
	}
	headers = make([]*types.Header, 0, afterLast-first)
	receipts = make([]types.Receipts, 0, afterLast-first)
	for number := first; number < afterLast; number++ {
		hash := hashes[i-first]
		header := s.parent.chain.GetHeader(hash, number)
		if header == nil {
			log.Error("Historical header missing", "number", number, "hash", hash)
			continue
		}
		receipts := s.parent.chain.GetReceiptsByHash(hash)
		if receipts == nil {
			log.Error("Historical receipts are missing", "number", number, "hash", hash)
			continue
		}

	}
}
