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
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

type indexer struct {
	lock                      sync.RWMutex
	needBlocks, missingBlocks rangeSet[uint64]
	needInitBlock             uint64
	initBlockCh               chan *types.Header
	storage                   *mapStorage
	tailView                  *indexView
	headNumber                uint64
	headViews                 map[common.Hash]*indexView
}

func (i *indexer) AddBlockData(header *types.Header, receipts types.Receipts) {
	number := header.Number.Uint64()
	i.headNumber = max(i.headNumber, number)
	if i.initBlockCh != nil && number == i.needInitBlock {
		i.initBlockCh <- header // should have a capacity of 1
		i.initBlockCh = nil
	}
	if indexView := i.getindexView(header.ParentHash); indexView != nil {
		indexView = indexView.clone()
		indexView.addReceipts(receipts)
		indexView.addHeader(header)
		i.registerindexView(header.Hash(), indexView)
		return
	}

}

func (i *indexer) MissingBlocks(missing []common.Range[uint64]) {
	i.missingBlocks = rangeSet[uint64](missing)
	if i.initBlockCh != nil && missing.Includes(i.needInitBlock) {
		i.initBlockCh <- nil // should have a capacity of 1
		i.initBlockCh = nil
	}
}

func (i *indexer) Revert(newHead uint64) {
	i.headNumber = max(i.headNumber, newHead)

}

func (i *indexer) NeedBlocks() []common.Range[uint64] {
	if initBlockCh != nil {
		return []common.Range[uint64]{common.NewRange[uint64](i.needInitBlock, 1)}
	}
	return []common.Range[uint64](i.needBlocks)
}

func (i *indexer) checkpointInit() {
	waitForHeadInit()

	initBlockCh := make(chan *types.Header, 1)
	var stopped bool
	blockId := func(number uint64) common.Hash {
		i.needInitBlock = number
		i.initBlockCh = initBlockCh
		select {
		case header := <-initBlockCh:
			if header != nil {
				return header.Hash()
			}
		case <-i.closeCh:
			stopped = true
		}
		return common.Hash{}
	}

	for age := 0; ; age++ {
		var more bool
		for idx, cpList := range checkpoints {
			if age >= len(cpList) {
				continue
			}
			cp := cpList[len(cpList)-1-age]
			if _, missing := i.missingBlocks.closestGte(cp.BlockNumber); missing {
				continue // missing blocks at or after checkpoint
			}
			if cp.BlockNumber <= i.headNumber && blockId(cp.BlockNumber) == cp.BlockId { // most recent match found
				return i.storage.checkpointInit(cpList[:len(cpList)-age])
			}
			if stopped {
				return errors.New("indexer closed during checkpoint initialization")
			}
			more = true
		}
		if !more {
			break
		}
	}
	return i.storage.genesisInit()
}
