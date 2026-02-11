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
	"math/bits"

	"github.com/ethereum/go-ethereum/common"
)

type tableProof struct {
	public  tableProofPublic
	private tableProofPrivate
}

type tableProofPublic struct {
	tableChains   []tableChainHead // ordered by tableSize
	partialTables []partialTable
}

type tableChainHead struct {
	tableSize uint64
	lastBlock uint64
	headHash  common.Hash
}

type partialTable struct {
	tableSize    uint64
	lastBlock    uint64
	tableRoot    common.Hash
	entryCount   uint64
	provenRanges []common.Range[uint64]
}

type tableProofPrivate struct {
	recursiveProofs []recursiveProof
	tableRoots      [][]common.Hash // same length as tableProofPublic.tableChains
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
	firstEntry  uint64
	entries     [][64]byte
	entryCount  uint64
	leftBranch  []common.Hash
	rightBranch []common.Hash
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

type tableChainState struct {
	lastProven uint64
	recent     []common.Hash
}

type partialTableState struct {
	entryCount       uint64
	expected, proven []common.Range[uint64]
}

type verifyProofState struct {
	tableChains   []*tableChainState
	partialTables map[common.Hash]*verifyTableState
}

func (t *tableProof) verify() bool {
	if len(t.private.tableRoots) != len(t.public.tableChains) {
		return false
	}
	for i := 0; i < len(t.public.tableChains)-1; i++ {
		if t.public.tableChains[i].tableSize >= t.public.tableChains[i+1].tableSize {
			return false
		}
	}
	state := &verifyProofState{
		tableChains:   make([]*tableChainState, len(t.public.tableChains)),
		partialTables: make(map[common.Hash]*verifyTableState),
	}
	for i, tr := range t.private.tableRoots {
		if len(tr) != 0 {
			state.tableChains[i] = &tableChainState{
				recent: make([]common.Hash, len(tr)),
			}
			lastHash := tr[0]
			state.tableChains[i].recent[0] = lastHash
			for j := 1; j < len(tr); j++ {
				lastHash = binaryHash(lastHash, tr[j])
				state.tableChains[i].recent[j] = lastHash
			}
			if lastHash != t.public.tableChains[i].headHash {
				return false
			}
		}
	}
	state.tableChains[0].lastProven = t.public.tableChains[0].lastBlock
	for _, proof := range t.private.recursiveProofs {
		t.applyRecursiveProof(state, proof)
	}
	for _, mp := range t.private.mergeProofs {
		if !t.verifyMergeProof(mp) {
			return false
		}
	}
	return true
}

func (t *tableProof) applyRecursiveProof(state *verifyProofState, proof recursiveProof) bool {
	//TODO verify ZKP
	for _, tc := range proof.public.tableChains {
		index := -1
		for i, ptc := range t.public.tableChains {
			if ptc.tableSize >= tc.tableSize {
				if ptc.tableSize == tc.tableSize {
					index = i
				}
				break
			}
		}

		t.public.tableChains[index].lastBlock
	}
}

func (t *tableProof) verifyMergeProof(mp *tableMergeProof) bool {
	if len(mp.inputs) == 0 {
		return false
	}
	var rangeFirst, rangeLast uint64
	for i, tr := range mp.inputs {
		tableSize, lastBlock, ok := t.findTableRoot(tr.rootHash(), tr.firstEntry, tr.firstEntry+uint64(len(tr.entries)))
		if !ok {
			return false
		}
		if i == 0 {
			rangeFirst = lastBlock + 1 - tableSize
		} else if rangeLast+tableSize != lastBlock {
			return false
		}
		rangeLast = lastBlock
	}
	tableSize, lastBlock, ok := t.findTableRoot(mp.output.rootHash(), tr.firstEntry, tr.firstEntry+uint64(len(tr.entries)))

}

func (t *tableProof) findTableRoot(rootHash common.Hash, firstEntry, afterLastEntry uint64) (tableSize, lastBlock uint64, ok bool) {

}

func (tr *tableRangeProof) rootHash() common.Hash {
	var listTreeRoot, countNode common.Hash
	if tr.entryCount > 0 {
		listTreeRoot = tr.listTreeHash(0, 64-bits.LeadingZeros64(tr.entryCount-1))
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
		return entryHash(&tr.entries[index-tr.firstEntry])
	}
	return binaryHash(tr.listTreeHash(index*2, height+1), tr.listTreeHash(index*2+1, height+1))
}
