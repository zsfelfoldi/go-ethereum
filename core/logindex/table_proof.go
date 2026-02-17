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
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
	"reflect"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

// tableProof represents the zero-knowledge prover input for recursive proofs
// of the validity of higher order index table chains based on the in-protocol
// table chains of a certain blockchain.
type tableProof struct {
	public  tableProofPublic
	private tableProofPrivate
}

// tableProofPublic is the public prover input and represents the facts that the
// proof is actually intended to prove:
//
// - tableChains[0] is the reference table chain that is assumed to be consistent
//   with the source blockchain
// - the proof proves that every tableChains[i] for i > 0 is consistent with the
//   reference table chain
// - also proves that the partially proven tables listed in partialTables are
//   consistent with the reference chain in the specified index entry ranges.
//
// Table chains are listed in an ascending order of the blockCount parameter and
// there can be only one chain with any given blockCount.
type tableProofPublic struct {
	tableChains   []tableChainHead
	partialTables []partialTable
}

// tableChainHead represents a single table chain. Each chain head hash is
// calculated as the hash of the previous chain head hash and the latest index
// table root. blockCount is the number of blocks indexed in each table and it
// is a constant for each table chain, while lastBlock is the last block number
// indexed, and it is increased by blockCount each time a new table is added.
//
// Note that a table chain is only listed in the proof if it has at least one
// table (in this case the head hash is a hash of a zero parent hash and the
// first table root).
type tableChainHead struct {
	blockCount, lastBlock uint64
	headHash              common.Hash
}

// partialTable represents a single, partially proven table that has not been
// appended to a table chain yet. provenRanges is a non-empty subset of the
// range [0; entryCount). Note that a table with a fully proven range can still
// be represented as a partialTable if there is not table chain listed in the
// same proof it could be appended to, and also it is not the first table of
// the indexed range of the reference table chain.
type partialTable struct {
	blockCount, lastBlock uint64
	tableRoot             common.Hash
	entryCount            uint64
	provenRanges          rangeSet[uint64]
}

// tableProofPrivate is the private prover input and provides the necessary
// proofs to verify the claims represented by the private input:
//
// - recursiveProofs lists recursive index table proofs of the same type; these
//   proofs can either prove a list of already fully proven table chains and/or
//   prove individual partial tables recursively.
// - tableRootProofs optionally proves the most recent table roots of certain
//   table chais. The outer slice is the same length as tableChains (one for
//   each chain); if the inner slice is not empty then it consists of one past
//   table chain hash and a number of subsequent index table roots, ending with
//   the root of the latest table.
// - mergeProofs lists full or partial proofs of index tables being merged into
//   bigger index tables.
type tableProofPrivate struct {
	recursiveProofs []recursiveProof
	tableRootProofs [][]common.Hash
	mergeProofs     []tableMergeProof
}

// recursiveProof represents an index table proof used as a recursive proof
// verified by the current one.
type recursiveProof struct {
	public tableProofPublic
	proof  []byte
}

// tableMergeProof proves a continuous section of index entries of the "output"
// table being correctly merged based on continuous sections of index entries
// from each of the "input" tables.
type tableMergeProof struct {
	inputs []tableRangeProof
	output tableRangeProof
}

// tableRangeProof proves a continuous section of entries of an index table.
type tableRangeProof struct {
	firstEntry, entryCount  uint64
	entries                 []provenEntry
	leftBranch, rightBranch []common.Hash
}

// provenEntry is the representation of an index entry used for tree hashing and
// Merkle proofs.
type provenEntry [64]byte

// compare compares two index entries according to the specified lexicographical
// ordering of index tables.
func (a *provenEntry) compare(b *provenEntry) int {
	return bytes.Compare((*a)[:], (*b)[:])
}

// hash calculates the binary Merkle tree node hash of an index entry leaf.
// Note that SHA256 is used here but the final specification might use a
// different hash function.
func (a *provenEntry) hash() (result common.Hash) {
	hasher := sha256.New()
	hasher.Write((*a)[:])
	hasher.Sum(result[:0])
	return
}

// binaryHash calculates the binary Merkle tree node hash of an inner tree node
// based on the two child node hashes.
// Note that SHA256 is used here but the final specification might use a
// different hash function.
func binaryHash(left, right common.Hash) (result common.Hash) {
	hasher := sha256.New()
	hasher.Write(left[:])
	hasher.Write(right[:])
	hasher.Sum(result[:0])
	return
}

// verify verifies the index table proofs and returns true if it is valid.
func (t *tableProof) verify() bool {
	// do basic sanity checks
	if len(t.private.tableRootProofs) != len(t.public.tableChains) {
		return false
	}
	for i := 0; i < len(t.public.tableChains)-1; i++ {
		if t.public.tableChains[i].blockCount >= t.public.tableChains[i+1].blockCount {
			return false
		}
	}
	// initialize the verifier state
	verifier := tableProofVerifier{
		tableChains:   make([]*tableChainVerifier, len(t.public.tableChains)),
		partialTables: make(map[common.Hash]*partialTable),
	}
	for i, tc := range t.public.tableChains {
		tr := t.private.tableRootProofs[i]
		// initialize table chains as not proven yet (lastProven == 0)
		tcv := &tableChainVerifier{
			blockCount: tc.blockCount,
			lastBlock:  tc.lastBlock,
		}
		if len(tr) > 0 {
			// reconstruct and verify proven table roots and table chain hashes
			tcv.chainHashes = make([]common.Hash, len(tr))
			tcv.tableRoots = make([]common.Hash, len(tr)-1)
			lastHash := tr[0]
			tcv.chainHashes[0] = lastHash
			for j := range tcv.tableRoots {
				tcv.tableRoots[j] = tr[j+1]
				lastHash = binaryHash(lastHash, tr[j+1])
				tcv.chainHashes[j+1] = lastHash
			}
			if lastHash != tc.headHash {
				return false
			}
		} else {
			tcv.chainHashes = []common.Hash{tc.headHash}
		}
		verifier.tableChains[i] = tcv
	}
	// reconstruct the proven table chains and partial tables in the verifier
	// state according to the provided proofs
	verifier.tableChains[0].lastProven = verifier.tableChains[0].lastBlock
	for _, rp := range t.private.recursiveProofs {
		//TODO verify ZKP
		if !verifier.applyRecursiveProof(rp.public) {
			return false
		}
	}
	for _, mp := range t.private.mergeProofs {
		if !verifier.applyMergeProof(&mp) {
			return false
		}
	}
	// check if all table chains and partial tables announced in the public
	// input are actually proven
	for _, tcv := range verifier.tableChains {
		if tcv.lastProven != tcv.lastBlock {
			return false
		}
	}
	for _, pt := range t.public.partialTables {
		if ptv, ok := verifier.partialTables[pt.tableRoot]; !ok || !reflect.DeepEqual(ptv, pt) {
			return false
		}
		delete(verifier.partialTables, pt.tableRoot)
	}
	if len(verifier.partialTables) != 0 {
		return false
	}
	return true
}

// tableProofVerifier is a mutable structure that mirrors the public proof input
// structure but it initialized in unproven
type tableProofVerifier struct {
	tableChains   []*tableChainVerifier
	partialTables map[common.Hash]*partialTable
}

type tableChainVerifier struct {
	blockCount, lastBlock, lastProven uint64
	chainHashes, tableRoots           []common.Hash
}

func (tv *tableProofVerifier) getTableChainIndex(blockCount uint64) (int, bool) {
	for i, tcv := range tv.tableChains {
		if tcv.blockCount == blockCount {
			return i, true
		}
	}
	return 0, false
}

func (tcv *tableChainVerifier) getChainHashAt(lastBlock uint64) (common.Hash, bool) {
	if lastBlock > tcv.lastBlock {
		return common.Hash{}, false
	}
	blockAge := (tcv.lastBlock) - lastBlock
	tableAge := blockAge / tcv.blockCount
	if blockAge != tableAge*tcv.blockCount || tableAge >= uint64(len(tcv.chainHashes)) {
		return common.Hash{}, false
	}
	return tcv.chainHashes[uint64(len(tcv.chainHashes))-1-tableAge], true
}

func (tv *tableProofVerifier) applyRecursiveProof(public tableProofPublic) bool {
	if len(public.tableChains) < 2 {
		return false
	}
	baseIndex, ok := tv.getTableChainIndex(public.tableChains[0].blockCount)
	if !ok {
		return false
	}
	if tv.tableChains[baseIndex].lastProven < public.tableChains[0].lastBlock {
		return false
	}
	matchHeadHash, ok := tv.tableChains[baseIndex].getChainHashAt(public.tableChains[0].lastBlock)
	if !ok || public.tableChains[0].headHash != matchHeadHash {
		return false
	}
	// base table chain of recursive proof matches existing proven chain, higher level chains can be considered proven too
	for i := 1; i < len(public.tableChains); i++ {
		tc := public.tableChains[i]
		chainIndex, ok := tv.getTableChainIndex(tc.blockCount)
		if !ok {
			continue // it is ok to not list some table chains proven by the recursive proof
		}
		tcv := tv.tableChains[chainIndex]
		tcv.lastProven = min(max(tcv.lastProven, tc.lastBlock), tcv.lastBlock)
	}
	for _, pt := range public.partialTables {
		chainIndex, ok := tv.getTableChainIndex(pt.blockCount)
		if !ok {
			continue
		}
		tcv := tv.tableChains[chainIndex]
		if tcv.lastProven+pt.blockCount != pt.lastBlock {
			continue
		}
		if ptv, ok := tv.partialTables[pt.tableRoot]; ok {
			ptv.provenRanges = ptv.provenRanges.merge(pt.provenRanges)
			if ptv.isComplete() {
				tcv.lastProven = pt.lastBlock
				delete(tv.partialTables, pt.tableRoot)
			}
		} else {
			tv.partialTables[pt.tableRoot] = &pt
		}
	}
	return true
}

func (tv *tableProofVerifier) findProvenTableRoot(tableRoot common.Hash) (uint64, uint64, bool) {
	for _, tcv := range tv.tableChains {
		for i, root := range tcv.tableRoots {
			if root == tableRoot {
				return tcv.blockCount, tcv.lastBlock - uint64(len(tcv.tableRoots)-1-i)*tcv.blockCount, true
			}
		}
	}
	return 0, 0, false
}

func (tv *tableProofVerifier) applyMergeProof(mp *tableMergeProof) bool {
	if !mp.verifyMerge() {
		return false
	}
	var rangeFirst, rangeLast uint64
	for i, tr := range mp.inputs {
		blockCount, lastBlock, ok := tv.findProvenTableRoot(tr.rootHash())
		if !ok {
			return false
		}
		if i == 0 {
			rangeFirst = lastBlock + 1 - blockCount
		} else if rangeLast+blockCount != lastBlock {
			return false
		}
		rangeLast = lastBlock
	}
	mergedRoot := mp.output.rootHash()
	provenRange := rangeSet[uint64]{common.NewRange[uint64](mp.output.firstEntry, uint64(len(mp.output.entries)))}
	if ptv, ok := tv.partialTables[mergedRoot]; ok {
		if ptv.blockCount != rangeLast+1-rangeFirst || ptv.lastBlock != rangeLast {
			return false
		}
		ptv.provenRanges = ptv.provenRanges.merge(provenRange)
	} else {
		tv.partialTables[mergedRoot] = &partialTable{
			blockCount:   rangeLast + 1 - rangeFirst,
			lastBlock:    rangeLast,
			tableRoot:    mergedRoot,
			provenRanges: provenRange,
		}
	}
	return true
}

func (pt *partialTable) isComplete() bool {
	return pt.entryCount == 0 || (len(pt.provenRanges) == 1 && pt.provenRanges[0] == common.NewRange[uint64](0, pt.entryCount))
}

func (mp *tableMergeProof) verifyMerge() bool {
	if len(mp.inputs) < 2 || len(mp.output.entries) == 0 {
		return false
	}
	inputIndices := make([]int, len(mp.inputs))
	lastEqual := -1
	var expFirstEntry uint64
	for i, input := range mp.inputs {
		if len(input.entries) == 0 {
			return false
		}
		expFirstEntry += input.firstEntry
		switch input.entries[0].compare(&mp.output.entries[0]) {
		case -1:
			if len(input.entries) < 1 || input.entries[1].compare(&mp.output.entries[0]) != 1 {
				return false
			}
			inputIndices[i] = 1
			expFirstEntry++
		case 0:
			if lastEqual != -1 {
				return false
			}
			lastEqual = i
		case 1:
			return false
		}
	}
	if lastEqual == -1 || expFirstEntry != mp.output.firstEntry {
		return false
	}
	for outputIndex := 1; outputIndex < len(mp.output.entries); outputIndex++ {
		inputIndices[lastEqual]++
		lastEqual = -1
		for i, input := range mp.inputs {
			if len(input.entries) <= inputIndices[i] {
				return false
			}
			switch input.entries[inputIndices[i]].compare(&mp.output.entries[outputIndex]) {
			case -1:
				return false
			case 0:
				if lastEqual != -1 {
					return false
				}
				lastEqual = i
			case 1:
			}
		}
		if lastEqual == -1 {
			return false
		}
	}
	for i, input := range mp.inputs {
		if len(input.entries) != inputIndices[i]+1 {
			return false
		}
	}
	return true
}

func (tr *tableRangeProof) rootHash() common.Hash {
	var listTreeRoot, countNode common.Hash
	if tr.entryCount > 0 {
		listTreeRoot = tr.listTreeHash(0, uint(64-bits.LeadingZeros64(tr.entryCount-1)))
		binary.LittleEndian.PutUint64(countNode[0:8], tr.entryCount)
	}
	return binaryHash(listTreeRoot, countNode)
}

func (tr *tableRangeProof) listTreeHash(index uint64, height uint) common.Hash {
	if (index+1)<<height <= tr.firstEntry {
		return tr.leftBranch[height]
	}
	if index<<height >= tr.firstEntry+uint64(len(tr.entries)) {
		return tr.rightBranch[height]
	}
	if height == 0 {
		return tr.entries[index-tr.firstEntry].hash()
	}
	return binaryHash(tr.listTreeHash(index*2, height+1), tr.listTreeHash(index*2+1, height+1))
}

type rangeSet[T uint32 | uint64] []common.Range[T]

func (a rangeSet[T]) merge(b rangeSet[T]) rangeSet[T] {
	m := make(rangeSet[T], len(a)+len(b))
	copy(m[:len(a)], a)
	copy(m[len(a):], b)
	m.normalize()
	return m
}

func (a *rangeSet[T]) normalize() {
	sort.Slice(*a, func(i, j int) bool { return (*a)[i].First() < (*a)[j].First() })
	// merge connecting/overlapping ranges
	var j int
	for i, next := range *a {
		if j == 0 || (*a)[j-1].AfterLast() < next.First() {
			// disjoint ranges, keep next range separate
			if j != i {
				(*a)[j] = next
			}
			j++
		} else {
			// connecting/overlapping ranges, merge with previous one
			(*a)[j-1] = (*a)[j-1].Union(next)
		}
	}
	*a = (*a)[:j]
}
