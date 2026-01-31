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
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type Indexer struct {
}

func NewIndexer() *Indexer {
	return &Indexer{}
}

func (i *Indexer) GetIndexRoots(parentHash common.Hash, transactions types.Transactions, receipts types.Receipts) []byte {
	return nil
}

func (i *Indexer) AddBlockData(header *types.Header, body *types.Body, receipts types.Receipts) (ready bool, needBlocks common.Range[uint64]) {
	return false, common.Range[uint64]{}
}

func (i *Indexer) Revert(blockNumber uint64) {}

func (i *Indexer) Status() (ready bool, needBlocks common.Range[uint64]) {
	return false, common.Range[uint64]{}
}

func (i *Indexer) SetHistoryCutoff(blockNumber uint64) {}
func (i *Indexer) SetFinalized(blockNumber uint64)     {}
func (i *Indexer) Suspended()                          {}
func (i *Indexer) Stop()                               {}
