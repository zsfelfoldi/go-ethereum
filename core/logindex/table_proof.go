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

type tableProof struct {
	public  tableProofPublic
	private tableProofPrivate
}

type tableProofPublic struct {
	tableChains   []tableChainHead // ordered by blockCount
	partialTables []partialTable
}

type tableChainHead struct {
	blockCount, lastBlock uint64
	headHash              common.Hash
}

type partialTable struct {
	blockCount, lastBlock uint64
	tableRoot             common.Hash
	entryCount            uint64
	provenRanges          rangeSet[uint64]
}

type tableProofPrivate struct {
	recursiveProofs []recursiveProof
	tableRootProofs [][]common.Hash // same length as tableProofPublic.tableChains
	mergeProofs     []tableMergeProof
}

type recursiveProof struct {
	public tableProofPublic
	proof  []byte
}

type tableMergeProof struct {
	inputs []tableRangeProof
	output tableRangeProof
}

type tableRangeProof struct {
	firstEntry, entryCount  uint64
	entries                 []provenEntry
	leftBranch, rightBranch []common.Hash
}

type provenEntry [64]byte

func (a *provenEntry) compare(b *provenEntry) int {
	return bytes.Compare((*a)[:], (*b)[:])
}

func (a *provenEntry) hash() (result common.Hash) {
	hasher := sha256.New()
	hasher.Write((*a)[:])
	hasher.Sum(result[:0])
	return
}

func binaryHash(left, right common.Hash) (result common.Hash) {
	hasher := sha256.New()
	hasher.Write(left[:])
	hasher.Write(right[:])
	hasher.Sum(result[:0])
	return
}

/*
- public TCL:
	- legalso TC a "feltetelezett"
	- bizonyitando TC parent-ek
	- bizonyitando teljes vagy reszleges table root-ok (ismert entry count, block range)
	!!! merge state a bizonyitando table root-okhoz rendel bizonyitott tartomanyt, majd vegul osszehasonlit
- recursive TCL:
	- akkor ervenyes az egesz, ha a public TCL legalso TC-je megtalalhato benne, azonos vagy korabbi head-del (a bizonyitott tartomanyban, parent es head kozott)
	- alsobb TC-k nem hasznalhatok
	- felsobbek bizonyitottnak tekinthetok
		- rTCL head a pTCL, bizonyitott tartomanyban, parent es head kozott
		- PT-kre is igaz, ha rTCL PT table root a pTCL bizonyitando rootok kozott van
- merge proof-ok:
	- a merge state-en operal
	- input-ok a legalso, "feltetelezett" TC bizonyitott table root-jai vagy a merge state mar (legalabb reszben) bizonyitott table root-jai
	- output a merge state egy bizonyitando table root-ja
	- output mindig foljebb, mint az input
	- feldolgozas input szint alapjan lentrol folfele
*/

type tableChainVerifier struct {
	blockCount, lastBlock, lastProven uint64
	chainHashes, tableRoots           []common.Hash
}

type tableProofVerifier struct {
	tableChains   []*tableChainVerifier
	partialTables map[common.Hash]*partialTable
}

func (t *tableProof) verify() bool {
	if len(t.private.tableRootProofs) != len(t.public.tableChains) {
		return false
	}
	for i := 0; i < len(t.public.tableChains)-1; i++ {
		if t.public.tableChains[i].blockCount >= t.public.tableChains[i+1].blockCount {
			return false
		}
	}
	verifier := tableProofVerifier{
		tableChains:   make([]*tableChainVerifier, len(t.public.tableChains)),
		partialTables: make(map[common.Hash]*partialTable),
	}
	for i, tc := range t.public.tableChains {
		tr := t.private.tableRootProofs[i]
		tcv := &tableChainVerifier{
			blockCount: tc.blockCount,
			lastBlock:  tc.lastBlock,
		}
		if len(tr) > 0 {
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
