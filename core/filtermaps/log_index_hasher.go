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
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
)

type LogIndexHasher struct {
	headerCache *lru.Cache[common.Hash, *types.Header]
	idCache     *lru.Cache[common.Hash, common.Hash]
	memTree     *memTree
	hasher      *Hasher
}

func NewLogIndexHasher() *LogIndexHasher {
	mt := &memTree{roots: make(map[uint64]memTreeRoot)}
	hasher := &Hasher{
		//tree:            mt.newWriter(0, common.Hash{}, common.Hash{}),
		params:          &DefaultParams,
		rowMappingCache: lru.NewCache[common.Hash, lvPosition](cachedRowMappings),
	}
	hasher.params.deriveFields()
	return &LogIndexHasher{
		headerCache: lru.NewCache[common.Hash, *types.Header](100),
		idCache:     lru.NewCache[common.Hash, common.Hash](100),
		memTree:     mt,
		hasher:      hasher,
	}
}

func (h *LogIndexHasher) AddHeader(header *types.Header, blockId common.Hash) {
	h.headerCache.Add(header.Hash(), header)
	h.idCache.Add(header.Hash(), blockId)
}

func (h *LogIndexHasher) AddReceipts(parentHash, blockId common.Hash, receipts types.Receipts) common.Hash {
	var (
		blockNumber  uint64
		parentHeader *types.Header
		parentId     common.Hash
	)
	if parentHash != (common.Hash{}) {
		parentHeader, _ = h.headerCache.Get(parentHash)
		blockNumber = parentHeader.Number.Uint64() + 1
		var ok bool
		parentId, ok = h.idCache.Get(parentHash)
		if !ok {
			panic("xxx")
		}
	}
	tree := h.memTree.newWriter(blockNumber, parentId, blockId)
	h.hasher.tree = tree
	if parentHeader != nil {
		h.hasher.AddBlockDelimiter(parentHeader)
	} else {
		h.hasher.InitGenesis()
	}
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			h.hasher.AddLogEvent(log)
		}
	}
	return tree.rootHash()
}
