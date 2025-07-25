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
	AddBlockData(header *types.Header, block *types.Block, receipts types.Receipts) bool
	Revert(blockNumber uint64)
	NeedBlocks() (common.Range[uint64], bool)
}

type indexerFeed struct {
	indexers []*indexServer
	chain    *BlockChain
}

func (f *indexerFeed) register(indexer Indexer, sendBlocks, sendReceipts bool) {
	server := &indexServer{
		parent:       f,
		indexer:      indexer,
		sendBlocks:   sendBlocks,
		sendReceipts: sendReceipts,
	}
	server.start()
	f.indexers = append(i.indexers, server)
}

func (f *indexerFeed) addBlockData(block *types.Block, receipts types.Receipts) {}

type indexServer struct {
	lock                     sync.Mutex
	parent                   *indexerFeed
	indexer                  Indexer
	sendBlocks, sendReceipts bool

	isReady    bool
	needBlocks common.Range[uint64]
}

func (s *indexServer) start() {

}

func (s *indexServer) addBlockData(block *types.Block, receipts types.Receipts) {
	s.lock.Lock()
	defer s.lock.Unlock()

	header := block.Header()
	if !s.sendBlocks {
		block = nil
	}
	if !s.sendReceipts {
		receipts = nil
	}
	s.ready = s.indexer.AddBlockData(header, block, receipts)
}
