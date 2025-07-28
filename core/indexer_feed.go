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
	indexersLock sync.Mutex
	indexers     []*indexServer
	chain        *BlockChain

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
	f.indexersLock.Lock()
	defer f.indexersLock.Unlock()

	server := &indexServer{
		parent:    f,
		indexer:   indexer,
		sendTimer: time.NewTimer(0),
	}
	f.indexers = append(i.indexers, server)
	f.closeWg.Add(1)
	indexer.MissingBlocks(common.NewRange[uint64](0, f.historyCutoff))
	go server.eventLoop()
}

func (f *indexerFeed) stop() {
	close(f.closeCh)
	f.closeWg.Wait()
}

func (f *indexerFeed) broadcastHead(header *types.Header, head bool) {
	f.indexersLock.Lock()
	defer f.indexersLock.Unlock()

	receipts := s.parent.chain.GetReceiptsByHash(header.Hash()) //TODO number and hash
	if receipts == nil {
		log.Error("Receipts belonging to new head are missing", "number", header.Number, "hash", header.Hash())
		return
	}
	f.headers = append(f.headers, header)
	f.receipts = append(f.receipts, receipts)
	if head || len(f.headers) >= maxBatchLength {
		for _, indexer := range f.indexers {
			indexer.sendBlockData(f.headers, f.receipts)
		}
		f.headers = f.headers[:0]
		f.receipts = f.receipts[:0]
	}
}

type indexServer struct {
	lock    sync.Mutex
	parent  *indexerFeed
	indexer Indexer // always call under mutex lock; never call after stopped
	stopped bool

	requestNumber uint64
	requestValid  bool

	ready      bool
	sendTimer  *time.Timer
	needBlocks common.Range[uint64]
}

func (s *indexServer) eventLoop() {
	for {
		select {
		case <-s.sendTimer.C:
			s.lock.Lock()
			first := max(s.needBlocks.First(), s.historyCutoff)
			afterLast := min(first+maxBatchLength, s.needBlocks.AfterLast())
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

func (s *indexServer) sendBlockData(headers []*types.Header, receipts []types.Receipts) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if !s.stopped {
		s.ready, s.needBlocks = s.indexer.AddBlockData(headers, receipts)
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

func (s *indexServer) historicBlockData(first, afterLast uint64) {
	xxx
}
