// Copyright 2022 The go-ethereum Authors
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

package beacon

import (
	"context"
	"encoding/binary"
	"errors"
	"math/bits"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

const maxHistoricTreeDistance = 3

type historicTree struct {
	bc                     *BeaconChain
	headBlock              *BlockData
	block, state, historic *merkleListHasher
}

func (bc *BeaconChain) newHistoricTree(headBlock *BlockData) *historicTree { //TODO inkabb headBlock legyen mindig bc-ben? szebb lenne, mint parameterben itt atadni, hiszen bc allapotahoz kotodik
	return &historicTree{
		bc:        bc,
		headBlock: headBlock,
		block:     newMerkleListHasher(bc.blockRoots, 1),
		state:     newMerkleListHasher(bc.stateRoots, 1),
		historic:  newMerkleListHasher(bc.historicRoots, 0),
	}
}

func (bc *BeaconChain) getHistoricTree(blockHash common.Hash) *historicTree {
	bc.historicMu.RLock()
	ht := bc.historicTrees[blockHash]
	bc.historicMu.RUnlock()
	return ht
}

func (block *BlockData) firstInPeriod() bool {
	newPeriod, oldPeriod := block.Header.Slot>>13, (block.Header.Slot-block.ParentSlotDiff)>>13
	if newPeriod > oldPeriod+1 {
		log.Crit("More than an entire period skipped", "oldSlot", block.Header.Slot-block.ParentSlotDiff, "newSlot", block.Header.Slot)
	}
	return newPeriod > oldPeriod
}

func (ht *historicTree) makeChildTree(newHead *BlockData) (*historicTree, error) {
	child := &historicTree{
		bc:        ht.bc,
		headBlock: ht.headBlock,
		block:     newMerkleListHasher(ht.block.list, 1),
		state:     newMerkleListHasher(ht.state.list, 1),
		historic:  newMerkleListHasher(ht.historic.list, 0),
	}
	if err := child.moveToHead(newHead); err == nil {
		return child, nil
	} else {
		return nil, err
	}
}

func (ht *historicTree) moveToHead(newHead *BlockData) error {
	var (
		rollbackHistoric, rollbackRoots int          // number of entries to roll back from old head to common ancestor
		blockRoots, stateRoots          MerkleValues // new entries to add from common ancestor to new head
		addHistoric                     int
	)
	block1 := ht.headBlock
	block2 := newHead

	//fmt.Println("mth1", block1.Header.Slot, block1.BlockRoot, block2.Header.Slot, block2.BlockRoot)
	for block1.BlockRoot != block2.BlockRoot {
		if block1.Header.Slot > block2.Header.Slot {
			if block1.firstInPeriod() {
				rollbackHistoric++
			}
			rollbackRoots += int(block1.ParentSlotDiff)
			block1 = ht.bc.GetParent(block1)
		} else {
			if block2.firstInPeriod() {
				addHistoric++
			}
			newBlockRoots := make(MerkleValues, len(block2.StateRootDiffs)+1+len(blockRoots))
			for i := 0; i <= len(block2.StateRootDiffs); i++ {
				newBlockRoots[i] = MerkleValue(block2.Header.ParentRoot)
			}
			copy(newBlockRoots[len(block2.StateRootDiffs)+1:], blockRoots)
			blockRoots = newBlockRoots

			newStateRoots := make(MerkleValues, len(block2.StateRootDiffs)+1+len(stateRoots))
			copy(newStateRoots[1:len(block2.StateRootDiffs)+1], block2.StateRootDiffs)
			copy(newStateRoots[len(block2.StateRootDiffs)+1:], stateRoots)
			block2 = ht.bc.GetParent(block2)
			newStateRoots[0] = MerkleValue(block2.StateRoot)
			stateRoots = newStateRoots
		}
		if block1 == nil || block2 == nil {
			return errors.New("common ancestor not found")
		}
		//fmt.Println(" mth1", block1.Header.Slot, block1.BlockRoot, block2.Header.Slot, block2.BlockRoot)
	}

	firstSlot := block1.Header.Slot
	firstPeriod := firstSlot >> 13
	newPeriod := newHead.Header.Slot >> 13
	//fmt.Println("mth2", firstSlot, stateRoots, rollbackState)

	for i, v := range blockRoots {
		ht.block.put((firstSlot+uint64(i))>>13, 0x2000+((firstSlot+uint64(i))&0x1fff), v)
	}
	for i := len(blockRoots); i < rollbackRoots; i++ {
		ht.block.put((firstSlot+uint64(i))>>13, 0x2000+((firstSlot+uint64(i))&0x1fff), MerkleValue{})
	}
	ht.block.get(newPeriod, 1) // force re-hashing

	for i, v := range stateRoots {
		ht.state.put((firstSlot+uint64(i))>>13, 0x2000+((firstSlot+uint64(i))&0x1fff), v)
	}
	for i := len(stateRoots); i < rollbackRoots; i++ {
		ht.state.put((firstSlot+uint64(i))>>13, 0x2000+((firstSlot+uint64(i))&0x1fff), MerkleValue{})
	}
	ht.state.get(newPeriod, 1) // force re-hashing

	firstHistoricIndex := 0x4000000 + 2*firstPeriod
	for i := 0; i < addHistoric; i++ {
		ht.historic.put(0, firstHistoricIndex+uint64(i)*2, ht.block.get(firstPeriod+uint64(i), 1))
		ht.historic.put(0, firstHistoricIndex+uint64(i)*2+1, ht.state.get(firstPeriod+uint64(i), 1))
	}
	for i := addHistoric; i < rollbackHistoric; i++ {
		ht.historic.put(0, firstHistoricIndex+uint64(i)*2, MerkleValue{})
		ht.historic.put(0, firstHistoricIndex+uint64(i)*2+1, MerkleValue{})
	}
	var listLength MerkleValue
	binary.LittleEndian.PutUint64(listLength[:8], newPeriod)
	ht.historic.put(0, 3, listLength)
	ht.historic.get(0, 1) // force re-hashing
	ht.headBlock = newHead
	return nil
}

func (bc *BeaconChain) commitHistoricTree(batch ethdb.Batch, ht *historicTree) {
	bc.blockRoots.commit(batch, ht.block.list)
	bc.stateRoots.commit(batch, ht.state.list)
	bc.historicRoots.commit(batch, ht.historic.list)
	bc.blockRoots, bc.stateRoots, bc.historicRoots = ht.block.list, ht.state.list, ht.historic.list
	ht.block = newMerkleListHasher(bc.blockRoots, 1)
	ht.state = newMerkleListHasher(bc.stateRoots, 1)
	ht.historic = newMerkleListHasher(bc.historicRoots, 0)
}

func (bc *BeaconChain) initHistoricTrees(ctx context.Context, block *BlockData) (*historicTree, error) {
	period, index := block.Header.Slot>>13, block.Header.Slot&0x1fff
	ht := bc.newHistoricTree(block)

	blockRootsProof, stateRootsProof, err := bc.dataSource.getRootsProof(ctx, block)
	if err != nil {
		return nil, err
	}
	ht.block.addMultiProof(period, blockRootsProof)
	ht.state.addMultiProof(period, stateRootsProof)

	if period > 0 {
		//period--
		historicRootsProof, err := bc.dataSource.getHistoricRootsProof(ctx, block, period)
		period--
		if err != nil {
			return nil, err
		}
		ht.historic.addMultiProof(0, historicRootsProof)

		// move state_roots items beyond index to previous period
		// (merkleListPeriodRepeat will still show them in current period until overwritten by new values)
		ht.block.get(period, 1) // calculate internal tree nodes
		for oi, value := range ht.block.list.diffs {
			if oi.period == period+1 {
				if oi.index > (0x2000+index)>>(bits.LeadingZeros64(oi.index)-50) {
					delete(ht.block.list.diffs, oi)
					ht.block.list.diffs[diffIndex{period, oi.index}] = value
				}
			}
		}
		ht.state.get(period, 1)
		for oi, value := range ht.state.list.diffs {
			if oi.period == period+1 {
				if oi.index > (0x2000+index)>>(bits.LeadingZeros64(oi.index)-50) {
					delete(ht.state.list.diffs, oi)
					ht.state.list.diffs[diffIndex{period, oi.index}] = value
				}
			}
		}
	}

	batch := bc.db.NewBatch()
	bc.commitHistoricTree(batch, ht)
	batch.Write()
	return ht, nil
}

func (ht *historicTree) getStateRoot(slot uint64) common.Hash {
	headSlot := ht.headBlock.Header.Slot
	if slot > headSlot {
		return common.Hash{}
	}
	if slot == headSlot {
		return ht.headBlock.StateRoot
	}
	return common.Hash(ht.state.get(slot>>13, (slot&0x1fff)+0x2000))
}

func (ht *historicTree) historicStateReader() ProofReader {
	return ht.headBlock.proof().Reader(ht.rootSubtrees)
}

func (ht *historicTree) rootSubtrees(index uint64) ProofReader {
	switch index {
	case BsiStateRoots:
		return stateRootsReader{ht: ht, period: ht.headBlock.Header.Slot >> 13, index: 1}
	case BsiHistoricRoots:
		return historicRootsReader{ht: ht, index: 1}
	default:
		return nil
	}
}

type stateRootsReader struct { // implements ProofReader
	ht            *historicTree
	period, index uint64
}

func (sr stateRootsReader) children() (left, right ProofReader) {
	if sr.index < 0x2000 {
		return stateRootsReader{ht: sr.ht, period: sr.period, index: sr.index * 2}, stateRootsReader{ht: sr.ht, period: sr.period, index: sr.index*2 + 1}
	}
	headSlot := sr.ht.headBlock.Header.Slot
	var slot uint64
	if sr.period == headSlot>>13 {
		slot = headSlot - 1 - (headSlot-1-sr.index)&0x1fff
	} else {
		slot = sr.period<<13 + sr.index - 0x2000
	}
	blockData := sr.ht.bc.getBlockData(slot, common.Hash(sr.ht.state.get(sr.period, sr.index)), false)
	if blockData == nil {
		return nil, nil //empty slot
	}
	return blockData.proof().Reader(nil).children()
}

func (sr stateRootsReader) readNode() (MerkleValue, bool) {
	return sr.ht.state.get(sr.period, sr.index), true
}

type historicRootsReader struct { // implements ProofReader
	ht    *historicTree
	index uint64
}

func (hr historicRootsReader) children() (left, right ProofReader) {
	if hr.index < 0x4000000 {
		return historicRootsReader{ht: hr.ht, index: hr.index * 2}, historicRootsReader{ht: hr.ht, index: hr.index*2 + 1}
	}
	if hr.index&1 == 0 {
		return nil, nil // block_roots subtree
	}
	period := (hr.index - 0x4000000) / 2
	return stateRootsReader{ht: hr.ht, period: period, index: 1}.children()
}

func (hr historicRootsReader) readNode() (MerkleValue, bool) {
	return hr.ht.historic.get(0, hr.index), true
}

func slotRangeFormat(headSlot, begin uint64, stateProofFormatTypes []byte) ProofFormat {
	end := begin + uint64(len(stateProofFormatTypes)) - 1
	if end > headSlot {
		panic(nil)
	}

	format := NewIndexMapFormat()
	headStateFormat := ProofFormat(format)
	if end == headSlot {
		// last state is the head state, prove directly in headStateFormat
		headStateFormat = MergedFormat{format, StateProofFormats[stateProofFormatTypes[len(stateProofFormatTypes)-1]]}
		stateProofFormatTypes = stateProofFormatTypes[:len(stateProofFormatTypes)-1]
		end--
	}
	//TODO ?? ha a state_roots-ba belelog, de nem fer bele, viszont az utolso historic-ba igen, akkor onnan bizonyitsuk?
	if end+0x2000 >= headSlot { //TODO check, ha csak a head-et kerjuk, vagy 0 hosszu a kert range
		var i int
		lpBegin := begin
		if begin+0x2000 < headSlot {
			i = int(headSlot - begin - 0x2000)
			lpBegin = headSlot - 0x2000
		}
		format.AddLeaf(BsiStateRoots, stateProofsRangeFormat(lpBegin, end, stateProofFormatTypes[i:]))
		stateProofFormatTypes = stateProofFormatTypes[:i]
		end = lpBegin - 1
	}
	if end >= begin {
		format.AddLeaf(BsiHistoricRoots, historicRootsRangeFormat(begin, end, stateProofFormatTypes))
	}
	return headStateFormat
}

// blocks[headSlot].StateRoot -> blocks[slot].StateRoot proof index
func slotProofIndex(headSlot, slot uint64) uint64 {
	if slot > headSlot {
		panic(nil)
	}
	if slot == headSlot {
		return 1
	}
	if slot+0x2000 >= headSlot {
		return childIndex(BsiStateRoots, 0x2000+(slot&0x1fff))
	}
	return childIndex(childIndex(BsiHistoricRoots, 0x4000000+(slot>>13)*2+1), 0x2000+(slot&0x1fff))
}

func stateProofsRangeFormat(begin, end uint64, stateProofFormatTypes []byte) ProofFormat {
	return StateRootsRangeFormat(begin, end, func(index uint64) ProofFormat {
		if format := stateProofFormatTypes[(index-begin)&0x1fff]; format > 0 {
			return StateProofFormats[format]
		}
		return nil
	})
}

func StateRootsRangeFormat(begin, end uint64, subtreeFn func(uint64) ProofFormat) ProofFormat {
	begin &= 0x1fff
	end &= 0x1fff
	if begin <= end {
		return NewRangeFormat(begin+0x2000, end+0x2000, subtreeFn)
	}
	return MergedFormat{
		NewRangeFormat(0x2000, end+0x2000, subtreeFn),
		NewRangeFormat(begin+0x2000, 0x3fff, subtreeFn),
	}
}

func historicRootsRangeFormat(begin, end uint64, stateProofFormatTypes []byte) ProofFormat {
	beginPeriod := begin >> 13
	endPeriod := end >> 13
	return NewRangeFormat(beginPeriod*2+0x2000001, endPeriod*2+0x2000001, func(index uint64) ProofFormat {
		if index&1 == 0 {
			return nil // block_roots entry
		}
		period := (index - 0x2000001) / 2
		if period < beginPeriod || period > endPeriod {
			return nil
		}
		periodBegin, periodEnd := period<<13, (period+1)<<13-1
		if periodBegin > end || periodEnd < begin {
			panic(nil)
		}
		rangeBegin, rangeEnd := begin, end
		if rangeBegin < periodBegin {
			rangeBegin = periodBegin
		}
		if rangeEnd > periodEnd {
			rangeEnd = periodEnd
		}
		return NewRangeFormat(rangeBegin-periodBegin+0x2000, rangeEnd-periodBegin+0x2000, func(index uint64) ProofFormat {
			slot := index - 0x2000 + periodBegin
			if format := stateProofFormatTypes[slot-begin]; format > 0 {
				return StateProofFormats[format]
			}
			return nil
		})
	})
}
