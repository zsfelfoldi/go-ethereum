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
	AddBlockData(header *types.Header, receipts types.Receipts) (bool, common.Range[uint64])
	Revert(blockNumber uint64)
	MissingBlocks(missing common.Range[uint64])
	Status() (bool, common.Range[uint64])
	Stop()
}

type indexerFeed struct {
	indexersLock sync.Mutex
	indexers     []*indexServer
	chain        *BlockChain

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
	indexer.MissingBlocks(common.NewRange[uint64](0, f.historyCutoff)
	go server.eventLoop()
}

func (f *indexerFeed) stop() {
	close(f.closeCh)
	f.closeWg.Wait()
}

func (f *indexerFeed) broadcastHead(header *types.Header) {
	f.indexersLock.Lock()
	defer f.indexersLock.Unlock()

	for _, indexer := range f.indexers {
		indexer.sendHead(header)
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

			blockNumber, ok := s.needBlocks.First(), !s.needBlocks.IsEmpty()
			s.lock.Unlock()
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

func (s *indexServer) sendHead(header *types.Header) {
	receipts := s.parent.chain.GetReceiptsByHash(header.Hash()) //TODO number and hash
	if receipts == nil {
		log.Error("Receipts belonging to new head are missing", "number", header.Number, "hash", header.Hash())
		return
	}
	s.lock.Lock()
	if !s.stopped {
		s.ready, s.needBlocks = s.indexer.AddBlockData(header, receipts)
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
	s.lock.Unlock()
}

func (s *indexServer) sendBlockData(header *types.Header) {
	receipts := s.parent.chain.GetReceiptsByHash(header.Hash()) //TODO number and hash
	if receipts == nil {
		return //TODO ???
	}
	ready := s.indexer.AddBlockData(header, receipts)
}

func (s *indexServer) sendByNumber(number uint64) {
	s.lock.Lock()
	s.requestNumber, s.requestValid = number, true
	s.lock.Unlock()

	var (
		header *types.Header
		block  *types.Block
	)
	if s.sendBlocks {
		block = s.parent.chain.GetBlockByNumber(number)
		if block == nil {
			return //TODO ???
		}
		header = block.Header()
	} else {
		header = s.parent.chain.GetHeaderByNumber(number)
		if header == nil {
			return //TODO ???
		}
	}

	s.lock.Lock()
	if s.requestValid {
		s.sendBlockData(header, block)
	}
	s.lock.Unlock()
}

/*	header := block.Header()
	if !s.sendBlocks {
		block = nil
	}
	if !s.sendReceipts {
		receipts = nil
	}
	s.ready = s.indexer.AddBlockData(header, block, receipts)
}
*/
