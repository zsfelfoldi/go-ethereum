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
	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type QueryProof struct {
	Query              FilterQuery
	RefHeader          types.Header
	HistoricTableProof []byte
	TableChainProofs   []TableChainProof
	TableQueryProofs   []TableQueryProof
}

type FilterQuery struct {
	FirstBlock, LastBlock uint64
	MaxResults, Reverse   uint
	Addresses             []common.Address
	Topics                [][]common.Hash
}

type TableChainProof struct {
	LastBlock, TableSize uint64
	LastChainHash        merkle.Value
	ProvenRoots          []merkle.Value
}

type TableQueryProof struct {
	FirstBlock, TableSize uint64
	ProvenEntries         entriesForStorage
	EntryIndices          []uint64
	ProofHashes           []merkle.Value
	BlockResults          []BlockResults
}

type BlockResults struct {
	Header         types.Header
	ProvenReceipts []uint64
	ReceiptProofs  [][][]byte
}

func (qp *QueryProof) Verify() error {
	return nil //TODO
}
