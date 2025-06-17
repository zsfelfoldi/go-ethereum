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
	hasher      *Hasher
}

func NewLogIndexHasher() *LogIndexHasher {
	mt := &memTree{roots: make(map[uint64]uint32)}
	tree := mt.newWriter(0)
	return &LogIndexHasher{
		headerCache: lru.NewCache[common.Hash, *types.Header](100),
		hasher: &Hasher{
			tree:            tree,
			params:          &DefaultParams,
			rowMappingCache: lru.NewCache[common.Hash, lvPosition](cachedRowMappings),
		},
	}
}

func (h *LogIndexHasher) AddHeader(header *types.Header) {
	h.headerCache.Add(header.Hash(), header)
}

func (h *LogIndexHasher) AddReceipts(parentHash common.Hash, receipts types.Receipts) (logRoot common.Hash) {
	//parentHeader := h.headerCache.Get(parentHash)
	return common.Hash{42}
}
