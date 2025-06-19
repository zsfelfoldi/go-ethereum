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
	"encoding/binary"
	"fmt"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
)

type LogIndexHasher struct {
	headerCache *lru.Cache[common.Hash, *types.Header]
	idCache     *lru.Cache[common.Hash, common.Hash]
	memTree     *memTree
	lock        sync.RWMutex // currently only guarding blockPtrs
	blockPtrs   []uint64
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
			_, nextPtr := h.hasher.AddLogEvent(log)
			h.lock.Lock()
			if uint64(len(h.blockPtrs)) < blockNumber {
				panic("invalid block number")
			}
			h.blockPtrs = append(h.blockPtrs[:blockNumber], nextPtr)
			h.lock.Unlock()
		}
	}
	return tree.rootHash()
}

type ProverBackend interface {
	Prove(firstBlock, lastBlock uint64, addresses []common.Address, topics [][]common.Hash) ([]byte, error)
}

type logIndexProver struct {
	tree      *memTreeView
	blockPtrs []uint64
	params    *Params
}

func (h *LogIndexHasher) NewProverBackend(referenceBlock uint64) ProverBackend {
	h.lock.RLock()
	defer h.lock.RUnlock()

	return &logIndexProver{
		tree:      h.memTree.newReader(referenceBlock),
		blockPtrs: slices.Clone(h.blockPtrs),
		params:    h.hasher.params,
	}
}

func (p *logIndexProver) Prove(firstBlock, lastBlock uint64, addresses []common.Address, topics [][]common.Hash) ([]byte, error) {
	fmt.Println("Generating log query proof")
	fq := &filterQuery{
		firstBlock: firstBlock,
		lastBlock:  lastBlock,
		addresses:  addresses,
		topics:     topics,
	}
	var firstIndex uint64
	if fq.firstBlock > 0 {
		firstIndex = p.blockPtrs[fq.firstBlock-1]
	}
	mp := p.params.proveQuery(p.tree, fq, firstIndex, p.blockPtrs[fq.lastBlock])
	fmt.Println("  leaf nodes:", len(mp.leaves))
	fmt.Println("  proof nodes:", len(mp.proof))
	proofData := make([]byte, 16+48*len(mp.leaves)+32*len(mp.proof))
	binary.LittleEndian.PutUint64(proofData[0:8], uint64(len(mp.leaves)))
	binary.LittleEndian.PutUint64(proofData[8:16], uint64(len(mp.proof)))
	ptr := 16
	for _, index := range mp.leafIndices {
		binary.LittleEndian.PutUint64(proofData[ptr:ptr+8], index.lo)
		binary.LittleEndian.PutUint64(proofData[ptr+8:ptr+16], index.hi)
		ptr += 16
	}
	for _, node := range mp.leaves {
		copy(proofData[ptr:ptr+32], node[:])
		ptr += 32
	}
	for _, node := range mp.proof {
		copy(proofData[ptr:ptr+32], node[:])
		ptr += 32
	}
	fmt.Println("  proof size:", len(proofData), "bytes")
	return proofData, nil //TODO return errors instead of panic
}
