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

package logquery

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/logindex"
)

const (
	nodeChunkSize  = 1024
	maxOutputCount = 4
)

type tableProver struct {
	reader       *logindex.TableReader
	optimizer    logicOptimizer
	treeHeight   int
	blockProofs  map[uint64]*blockProof
	validResults int
}

func newTableProver(reader *logindex.TableReader) *tableProver {
	return &tableProver{
		reader:     reader,
		treeHeight: 64 - bits.LeadingZeros64(max(reader.entryCount, 1)-1),
	}
}

func (tp *tableProver) addBlockProofs(newProofs map[uint64]*blockProof, newValidResults int) {
	//fmt.Println("addBlockProofs", len(newProofs), newValidResults)
	tp.validResults += newValidResults
	if tp.blockProofs == nil {
		tp.blockProofs = newProofs
		return
	}
	for number, newProof := range newProofs {
		if proof, ok := tp.blockProofs[number]; ok {
			proof.merge(newProof)
		} else {
			tp.blockProofs[number] = newProof
		}
	}
}

func (tp *tableProver) proofCost(a, b uint64) int {
	switch {
	case a == math.MaxUint64 && b == math.MaxUint64:
		// no entries remaining; proof is just a root hash
		return 1
	case a == math.MaxUint64:
		// b has no left neighbor; each 1 bit in b costs a proof hash
		return bits.OnesCount64(b)
	case b == math.MaxUint64:
		// a has no right neighbor; each 0 bit in a costs a proof hash
		return tp.treeHeight - bits.OnesCount64(a)
	default:
		if a >= b {
			panic("proofCost: invalid index order")
		}
		// we ignore the shared binary prefix plus the first different bit (0 in a, 1 in b)
		ignorePrefix := bits.LeadingZeros64(a^b) + 1
		// in the remaining lower bits, each 0 bit in a and each 1 bit in b costs a proof hash
		return 64 - ignorePrefix - bits.OnesCount64(a<<ignorePrefix) + bits.OnesCount64(b<<ignorePrefix)
	}
}

func (tp *tableProver) savedInputCost(a, b, c uint64) int {
	return tp.proofCost(a, b) + tp.proofCost(b, c) - tp.proofCost(a, c)
}

func (tp *tableProver) finalize(proveParentBlock common.Hash) (tableQueryProof, common.Hash, error) {
	//fmt.Println("tp.finalize", tp.reader.BlockRange(), "proveParentBlock", proveParentBlock)
	var proveLastBlock common.Hash
	blockNumbers := make([]uint64, 0, len(tp.blockProofs))
	for number := range tp.blockProofs {
		blockNumbers = append(blockNumbers, number)
	}
	sort.Slice(blockNumbers, func(i, j int) bool {
		return blockNumbers[i] < blockNumbers[j]
	})
	proof := tableQueryProof{
		FirstBlock:   tp.reader.BlockRange().First(),
		TableSize:    tp.reader.BlockRange().Count(),
		EntryCount:   tp.reader.entryCount,
		ResultCount:  uint64(tp.validResults),
		BlockResults: make([]blockResults, len(blockNumbers)),
	}
	if tp.optimizer.hasNodes() {
		var err error
		proof.EntryIndices, err = tp.optimizer.optimize(tp.savedInputCost)
		if err != nil {
			return tableQueryProof{}, common.Hash{}, err
		}
	}
	if proveParentBlock != (common.Hash{}) {
		entryIndex, found, err := tp.reader.seekEntry(&logindex.IndexEntry{
			logindex.IndexValue: logindex.IndexValue{
				entryType: ieBlock,
				value:     proveParentBlock,
			},
			logindex.IndexPosition: logindex.IndexPosition{
				blockNumber: tp.reader.BlockRange().First() - 1,
			},
		})
		fmt.Println(" fetch parent block entry", found, err)
		if err != nil {
			return tableQueryProof{}, common.Hash{}, err
		}
		if !found {
			return tableQueryProof{}, common.Hash{}, errors.New("parent block entry not found")
		}
		proof.EntryIndices = append(proof.EntryIndices, entryIndex)
	}
	for blockNumber, bp := range tp.blockProofs {
		//fmt.Println("block entry", bp.blockEntryIndex)
		if blockNumber != tp.reader.BlockRange().Last() {
			proof.EntryIndices = append(proof.EntryIndices, bp.blockEntryIndex)
		} else {
			proveLastBlock = bp.header.Hash()
		}
		for _, mtx := range bp.matchingTxs {
			//fmt.Println("tx entry", mtx.txEntryIndex)
			proof.EntryIndices = append(proof.EntryIndices, mtx.txEntryIndex)
		}
	}
	sort.Slice(proof.EntryIndices, func(i, j int) bool {
		return proof.EntryIndices[i] < proof.EntryIndices[j]
	})

	for i, number := range blockNumbers {
		bp := tp.blockProofs[number]
		br := blockResults{
			Header:         *bp.header,
			ProvenReceipts: make([]uint32, 0, len(bp.matchingTxs)),
			ReceiptsProof:  bp.receiptsProof.proofForStorage(),
		}
		for txi := range bp.matchingTxs {
			br.ProvenReceipts = append(br.ProvenReceipts, uint32(txi))
		}
		sort.Slice(br.ProvenReceipts, func(i, j int) bool {
			return br.ProvenReceipts[i] < br.ProvenReceipts[j]
		})
		proof.BlockResults[i] = br
	}

	entries := make(IndexEntries, len(proof.EntryIndices))
	lastIndex := uint64(math.MaxUint64)
	//fmt.Println("proof.EntryIndices", len(proof.EntryIndices))
	for i, entryIndex := range proof.EntryIndices {
		entry, err := tp.reader.getEntry(entryIndex)
		if err != nil {
			return tableQueryProof{}, common.Hash{}, err
		}
		//fmt.Println("entry", entryIndex, "hash", common.Hash(entry.hash()))
		entries[i] = *entry
		if proof.ProofHashes, err = tp.makeProofHashes(proof.ProofHashes, lastIndex, entryIndex); err != nil {
			return tableQueryProof{}, common.Hash{}, err
		}
		lastIndex = entryIndex
	}
	var err error
	if proof.ProofHashes, err = tp.makeProofHashes(proof.ProofHashes, lastIndex, uint64(math.MaxUint64)); err != nil {
		return tableQueryProof{}, common.Hash{}, err
	}
	proof.ProvenEntries = entries.toStorage()
	//treeRoot, _ := tp.reader.getHash(1)
	//fmt.Println("tree root", common.Hash(treeRoot))
	return proof, proveLastBlock, nil
}

func (tp *tableProver) makeProofHashes(hashes []merkle.Value, a, b uint64) ([]merkle.Value, error) {
	if err := iterateProofIndices(tp.treeHeight, a, b, func(gti uint64) error {
		hash, err := tp.reader.getHash(gti)
		if err != nil {
			return err
		}
		//fmt.Println("node", gti, "hash", common.Hash(hash))
		hashes = append(hashes, hash)
		return nil
	}); err != nil {
		return nil, err
	}
	return hashes, nil
}

func iterateProofIndices(treeHeight int, a, b uint64, callback func(uint64) error) error {
	switch {
	case a == math.MaxUint64 && b == math.MaxUint64:
		// no entries proven; merkle multiproof is just the root hash
		return callback(1)
	case a == math.MaxUint64:
		// b has no left neighbor; each 1 bit in b corresponds to a proven hash
		return iterateProofIndicesUp(treeHeight, 0, b, callback)
	case b == math.MaxUint64:
		// a has no right neighbor; each 0 bit in a corresponds to a proven hash
		return iterateProofIndicesDown(treeHeight, 0, a, callback)
	default:
		if a == b {
			return nil
		}
		if a > b {
			panic("iterateProofIndices: invalid index order")
		}
		// we ignore the shared binary prefix plus the first different bit (0 in a, 1 in b)
		splitHeight := treeHeight + bits.LeadingZeros64(a^b) - 63
		// in the remaining lower bits, each 0 bit in a and each 1 bit in b corresponds to a proven hash
		if err := iterateProofIndicesDown(treeHeight, splitHeight, a, callback); err != nil {
			return err
		}
		return iterateProofIndicesUp(treeHeight, splitHeight, b, callback)
	}
}

func iterateProofIndicesUp(treeHeight, fromHeight int, entryIndex uint64, callback func(uint64) error) error {
	for h := fromHeight; h < treeHeight; h++ { // h == 0 corresponds to entryIndex MSB
		if entryIndex&(uint64(1)<<(treeHeight-1-h)) != 0 {
			if err := callback((entryIndex>>(treeHeight-1-h) ^ 1) + uint64(1)<<(h+1)); err != nil {
				return err
			}
		}
	}
	return nil
}

func iterateProofIndicesDown(treeHeight, toHeight int, entryIndex uint64, callback func(uint64) error) error {
	for h := treeHeight - 1; h >= toHeight; h-- { // h == 0 corresponds to entryIndex MSB
		if entryIndex&(uint64(1)<<(treeHeight-1-h)) == 0 {
			if err := callback((entryIndex>>(treeHeight-1-h) ^ 1) + uint64(1)<<(h+1)); err != nil {
				return err
			}
		}
	}
	return nil
}

type trieProofWriter map[common.Hash][]byte

func (t trieProofWriter) Put(key []byte, value []byte) error {
	if len(key) != common.HashLength {
		panic("invalid proof database key")
	}
	var hash common.Hash
	copy(hash[:], key)
	t[hash] = slices.Clone(value)
	return nil
}

func (t trieProofWriter) Delete(key []byte) error { panic("not implemented") }

func (t trieProofWriter) proofForStorage() [][]byte {
	proof := make([][]byte, len(t))
	proofHashes := make([]common.Hash, 0, len(t))
	for hash := range t {
		proofHashes = append(proofHashes, hash)
	}
	sort.Slice(proofHashes, func(i, j int) bool {
		return proofHashes[i].Cmp(proofHashes[j]) < 0
	})
	for i, hash := range proofHashes {
		proof[i] = t[hash]
	}
	return proof
}
