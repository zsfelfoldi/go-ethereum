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
	//"errors"
	//"math/bits"
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

const maxHistoricTreeDistance = 3

type HistoricTree struct {
	bc                               *BeaconChain
	HeadBlock                        *BlockData
	tailSlot, tailPeriod, nextPeriod uint64
	block, state, historic           *merkleListHasher
}

func (bc *BeaconChain) newHistoricTree(tailSlot, tailPeriod, nextPeriod uint64) *HistoricTree {
	return &HistoricTree{
		bc:         bc,
		HeadBlock:  bc.storedHead,
		block:      newMerkleListHasher(bc.blockRoots, 1),
		state:      newMerkleListHasher(bc.stateRoots, 1),
		historic:   newMerkleListHasher(bc.historicRoots, 0),
		tailSlot:   tailSlot,
		tailPeriod: tailPeriod,
		nextPeriod: nextPeriod,
	}
}

func (bc *BeaconChain) GetHistoricTree(blockHash common.Hash) *HistoricTree {
	bc.historicMu.RLock()
	ht := bc.historicTrees[blockHash]
	bc.historicMu.RUnlock()
	return ht
}

//func (ht *HistoricTree) makeChildTree(newHead *BlockData) (*HistoricTree, error) {
func (ht *HistoricTree) makeChildTree() *HistoricTree {
	return &HistoricTree{
		bc:         ht.bc,
		HeadBlock:  ht.HeadBlock,
		tailSlot:   ht.tailSlot,
		tailPeriod: ht.tailPeriod,
		nextPeriod: ht.nextPeriod,
		block:      newMerkleListHasher(ht.block.list, 1),
		state:      newMerkleListHasher(ht.state.list, 1),
		historic:   newMerkleListHasher(ht.historic.list, 0),
	}
}

// update HeadBlock after addRoots if necessary
func (ht *HistoricTree) addRoots(firstSlot uint64, blockRoots, stateRoots MerkleValues, deleteAfter bool, tailProof MultiProof) {
	afterLastSlot := firstSlot + uint64(len(blockRoots))
	var headSlot uint64
	if ht.HeadBlock != nil {
		headSlot = uint64(ht.HeadBlock.Header.Slot)
	}

	for slot := firstSlot; slot < afterLastSlot; slot++ {
		period, index := slot>>13, 0x2000+slot&0x1fff
		i := int(slot - firstSlot)
		ht.block.put(period, index, blockRoots[i])
		ht.state.put(period, index, stateRoots[i])
	}
	if afterLastSlot > headSlot {
		headSlot = afterLastSlot
	}
	if deleteAfter && afterLastSlot < headSlot {
		for slot := afterLastSlot; slot < headSlot; slot++ {
			period, index := slot>>13, 0x2000+slot&0x1fff
			ht.block.put(period, index, MerkleValue{})
			ht.state.put(period, index, MerkleValue{})
		}
		headSlot = afterLastSlot
	}

	newNextPeriod := headSlot >> 13
	if newNextPeriod < ht.nextPeriod {
		for period := newNextPeriod; period < ht.nextPeriod; period++ {
			ht.deleteHistoricRoots(period)
		}
		ht.nextPeriod = newNextPeriod
		if ht.nextPeriod < ht.tailPeriod {
			ht.reset()
		}
	}

	oldTailPeriod := ht.tailPeriod
	tailProofPeriod := firstSlot >> 13
	if (ht.nextPeriod == 0 || tailProofPeriod < ht.tailPeriod) && tailProof.Format != nil {
		if verifyTailProof(tailProof, tailProofPeriod) {
			ht.historic.addMultiProof(0, tailProof, limitLeft, 0x2000000+tailProofPeriod)
			ht.tailPeriod = tailProofPeriod
			if ht.nextPeriod == 0 {
				ht.nextPeriod = tailProofPeriod
				oldTailPeriod = tailProofPeriod
			}
			for period := ht.tailPeriod; period < oldTailPeriod; period++ {
				ht.setHistoricRoots(period)
			}
		} else {
			log.Error("Invalid historic tail proof")
		}
	}
	firstUpdate, afterUpdate := tailProofPeriod, afterLastSlot>>13
	if oldTailPeriod > firstUpdate {
		firstUpdate = oldTailPeriod
	}
	if ht.nextPeriod < afterUpdate {
		afterUpdate = ht.nextPeriod
	}
	for period := firstUpdate; period < afterUpdate; period++ {
		ht.setHistoricRoots(period)
	}
	for period := ht.nextPeriod; period < newNextPeriod; period++ {
		ht.setHistoricRoots(period)
	}
	ht.nextPeriod = newNextPeriod
}

func verifyTailProof(proof MultiProof, tailPeriod uint64) bool {
	if tailPeriod == 0 {
		return true
	}
	var values [2]MerkleValue
	TraverseProof(proof.Reader(nil), NewValueWriter(proof.Format, values[:], func(index uint64) int {
		if index == 0x2000000+tailPeriod-1 {
			return 0
		}
		if index == 0x2000000+tailPeriod {
			return 1
		}
		return -1
	}))
	return values[0] == (MerkleValue{}) && values[1] != (MerkleValue{})
}

func (ht *HistoricTree) setHistoricRoots(period uint64) {
	ht.historic.put(0, 0x4000000+period*2, ht.block.get(period, 1))
	ht.historic.put(0, 0x4000000+period*2+1, ht.state.get(period, 1))
}

func (ht *HistoricTree) deleteHistoricRoots(period uint64) {
	ht.historic.put(0, 0x4000000+period*2, MerkleValue{})
	ht.historic.put(0, 0x4000000+period*2+1, MerkleValue{})
}

func (ht *HistoricTree) isValid() bool {
	return ht.nextPeriod > ht.tailPeriod && ht.nextPeriod == uint64(ht.HeadBlock.Header.Slot)>>13
}

func (ht *HistoricTree) reset() {
	for period := ht.tailPeriod; period < ht.nextPeriod; period++ {
		ht.deleteHistoricRoots(period)
	}
	ht.tailPeriod, ht.nextPeriod = 0, 0
}

func (bc *BeaconChain) commitHistoricTree(batch ethdb.Batch, ht *HistoricTree) {
	bc.blockRoots.commit(batch, ht.block.list)
	bc.stateRoots.commit(batch, ht.state.list)
	bc.historicRoots.commit(batch, ht.historic.list)
	bc.blockRoots, bc.stateRoots, bc.historicRoots = ht.block.list, ht.state.list, ht.historic.list
	ht.block = newMerkleListHasher(bc.blockRoots, 1)
	ht.state = newMerkleListHasher(bc.stateRoots, 1)
	ht.historic = newMerkleListHasher(bc.historicRoots, 0)
	bc.headTree = ht
}

// call after the batch of commitHistoricTree has been written
func (bc *BeaconChain) updateTreeMap() {
	newTreeMap := make(map[common.Hash]*HistoricTree)
	newTreeMap[bc.storedHead.BlockRoot] = bc.headTree
	for _, path := range bc.findCloseBlocks(bc.storedHead, maxHistoricTreeDistance) {
		ht := bc.headTree.makeChildTree()
		firstSlot, blockRoots, stateRoots := blockAndStateRoots(path[0], path[1:])
		ht.addRoots(firstSlot, blockRoots, stateRoots, true, MultiProof{})
		ht.HeadBlock = path[len(path)-1]
		newTreeMap[ht.HeadBlock.BlockRoot] = ht
	}

	bc.historicMu.Lock()
	bc.historicTrees = newTreeMap
	bc.historicMu.Unlock()
}

func blockAndStateRoots(parent *BlockData, blocks []*BlockData) (firstSlot uint64, blockRoots, stateRoots MerkleValues) {
	firstSlot = uint64(blocks[0].Header.Slot) - uint64(len(blocks[0].StateRootDiffs))
	if parent != nil {
		firstSlot--
	}
	rootCount := uint64(blocks[len(blocks)-1].Header.Slot) - firstSlot
	blockRoots, stateRoots = make(MerkleValues, rootCount), make(MerkleValues, rootCount)
	var rootIndex int
	for _, block := range blocks {
		if parent != nil {
			blockRoots[rootIndex] = MerkleValue(block.Header.ParentRoot)
			stateRoots[rootIndex] = MerkleValue(parent.StateRoot)
			rootIndex++
		}
		parent = block
		for _, stateRoot := range block.StateRootDiffs {
			blockRoots[rootIndex] = MerkleValue(block.Header.ParentRoot)
			stateRoots[rootIndex] = stateRoot
			rootIndex++
		}
	}
	if rootIndex != len(blockRoots) {
		panic(nil)
	}
	return
}

func (bc *BeaconChain) findCloseBlocks(block *BlockData, maxDistance int) (res [][]*BlockData) {
	type distanceWithPath struct {
		distance int
		path     []*BlockData
	}
	dist := make(map[common.Hash]distanceWithPath)
	dist[block.BlockRoot] = distanceWithPath{0, []*BlockData{block}}
	b := block
	firstSlot := b.Header.Slot
	for i := 1; i <= maxDistance; i++ {
		if b = bc.GetParent(b); b == nil {
			break
		}
		res = append(res, []*BlockData{b})
		firstSlot = b.Header.Slot
		dist[b.BlockRoot] = distanceWithPath{i, []*BlockData{b}}
	}

	var slotEnc [8]byte
	binary.BigEndian.PutUint64(slotEnc[:], firstSlot)
	iter := bc.db.NewIterator(blockDataKey, slotEnc[:])
	for iter.Next() {
		block := new(BlockData)
		if err := rlp.DecodeBytes(iter.Value(), block); err == nil {
			block.CalculateRoots()
			if _, ok := dist[block.BlockRoot]; ok {
				continue
			}
			if d, ok := dist[block.Header.ParentRoot]; ok && d.distance < maxDistance {
				path := append(d.path, block)
				dist[block.BlockRoot] = distanceWithPath{d.distance + 1, path}
				res = append(res, path)
			}
		} else {
			log.Error("Error decoding beacon block found by iterator", "key", iter.Key(), "value", iter.Value(), "error", err)
		}
	}
	iter.Release()
	return res
}

func (ht *HistoricTree) initRecentRoots(ctx context.Context, dataSource beaconData) error {
	period, index := uint64(ht.HeadBlock.Header.Slot)>>13, uint64(ht.HeadBlock.Header.Slot)&0x1fff
	blockRootsProof, stateRootsProof, err := dataSource.GetRootsProof(ctx, ht.HeadBlock)
	if err != nil {
		return err
	}
	ht.block.addMultiProof(period, blockRootsProof, limitLeft, 0x2000+index)
	ht.state.addMultiProof(period, stateRootsProof, limitLeft, 0x2000+index)

	if uint64(ht.HeadBlock.Header.Slot) >= 0x2000 {
		ht.tailSlot = uint64(ht.HeadBlock.Header.Slot) - 0x2000
	} else {
		ht.tailSlot = 0
	}

	if period > 0 {
		ht.tailPeriod, ht.nextPeriod = period, period
		//period--
		historicRootsProof, err := dataSource.GetHistoricRootsProof(ctx, ht.HeadBlock, period)
		if err != nil {
			return err
		}
		ht.historic.addMultiProof(0, historicRootsProof, limitNone, 0)

		period--
		ht.block.addMultiProof(period, blockRootsProof, limitRight, 0x2000+index)
		ht.state.addMultiProof(period, stateRootsProof, limitRight, 0x2000+index)

		/*		// move state_roots items beyond index to previous period
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
				}*/
	}
	return nil
}

func (ht *HistoricTree) GetStateRoot(slot uint64) common.Hash {
	headSlot := ht.HeadBlock.Header.Slot
	if slot > headSlot {
		return common.Hash{}
	}
	if slot == headSlot {
		return ht.HeadBlock.StateRoot
	}
	return common.Hash(ht.state.get(slot>>13, (slot&0x1fff)+0x2000))
}

func (ht *HistoricTree) HistoricStateReader() ProofReader {
	return ht.HeadBlock.Proof().Reader(ht.rootSubtrees)
}

func (ht *HistoricTree) rootSubtrees(index uint64) ProofReader {
	switch index {
	case BsiStateRoots:
		return stateRootsReader{ht: ht, period: ht.HeadBlock.Header.Slot >> 13, index: 1}
	case BsiHistoricRoots:
		return historicRootsReader{ht: ht, index: 1}
	default:
		return nil
	}
}

type stateRootsReader struct { // implements ProofReader
	ht            *HistoricTree
	period, index uint64
}

func (sr stateRootsReader) children() (left, right ProofReader) {
	if sr.index < 0x2000 {
		return stateRootsReader{ht: sr.ht, period: sr.period, index: sr.index * 2}, stateRootsReader{ht: sr.ht, period: sr.period, index: sr.index*2 + 1}
	}
	headSlot := sr.ht.HeadBlock.Header.Slot
	var slot uint64
	if sr.period == headSlot>>13 {
		slot = headSlot - 1 - (headSlot-1-sr.index)&0x1fff
	} else {
		slot = sr.period<<13 + sr.index - 0x2000
	}
	blockData := sr.ht.bc.GetBlockData(slot, common.Hash(sr.ht.state.get(sr.period, sr.index)), false)
	if blockData == nil {
		return nil, nil //empty slot
	}
	return blockData.Proof().Reader(nil).children()
}

func (sr stateRootsReader) readNode() (MerkleValue, bool) {
	return sr.ht.state.get(sr.period, sr.index), true
}

type historicRootsReader struct { // implements ProofReader
	ht    *HistoricTree
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

func SlotRangeFormat(headSlot, begin uint64, stateProofFormatTypes []byte) ProofFormat {
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
func SlotProofIndex(headSlot, slot uint64) uint64 {
	if slot > headSlot {
		panic(nil)
	}
	if slot == headSlot {
		return 1
	}
	if slot+0x2000 >= headSlot {
		return ChildIndex(BsiStateRoots, 0x2000+(slot&0x1fff))
	}
	return ChildIndex(ChildIndex(BsiHistoricRoots, 0x4000000+(slot>>13)*2+1), 0x2000+(slot&0x1fff))
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
