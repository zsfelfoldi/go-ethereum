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
	"fmt"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type QueryProof struct {
	Query            FilterQuery
	RefHeader        types.Header
	IndexContracts   []common.Address
	IndexTablesProof [][]byte
	TableQueryProofs []tableQueryProof
}

type tableQueryProof struct {
	IndexContract         uint
	FirstBlock, TableSize uint64
	ProvenEntries         entriesForStorage
	EntryIndices          []uint64
	ProofHashes           []merkle.Value
	BlockResults          []blockResults
}

type blockResults struct {
	Header         types.Header
	ProvenReceipts []uint
	ReceiptsProof  [][]byte // receipts trie nodes in ascending order of node hash
}

func (qp *QueryProof) addOrGetIndexContract(address common.Address) uint {
	for i, addr := range qp.IndexContracts {
		if addr == address {
			return uint(i)
		}
	}
	qp.IndexContracts = append(qp.IndexContracts, address)
	return uint(len(qp.IndexContracts) - 1)
}

func (qp *QueryProof) Verify() error {
	return nil //TODO
}

func (qp *QueryProof) printStats() {
	fmt.Println("* Reference block number:", qp.RefHeader.Number.Uint64())
	fmt.Println("* Unique index contracts:", len(qp.IndexContracts))
	fmt.Println("* Table root MPT proof nodes:", len(qp.IndexTablesProof))
	for i, tqp := range qp.TableQueryProofs {
		fmt.Println("*** Table query proof", i)
		fmt.Println("  * Contract index:", tqp.IndexContract)
		fmt.Println("  * First block:", tqp.FirstBlock)
		fmt.Println("  * Table size:", tqp.TableSize)
		fmt.Println("  * Number of proven entries:", len(tqp.ProvenEntries))
		fmt.Println("  * Number of IXT proof hashes:", len(tqp.ProofHashes))
		fmt.Println("  * Number of block results:", len(tqp.BlockResults))
		for j, br := range tqp.BlockResults {
			fmt.Println("  *** Block result", j)
			fmt.Println("    * Block number:", br.Header.Number.Uint64())
			fmt.Println("    * Number of proven receipts:", len(br.ProvenReceipts))
			fmt.Println("    * Number of receipt MPT proof nodes:", len(br.ReceiptsProof))
		}
	}
}
