// Copyright 2024 The go-ethereum Authors
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
	"crypto/sha256"
	"encoding/binary"
	"math/bits"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type treeHashed interface {
	leafLevel() int
	getLeaf(leafIndex uint64) (common.Hash, error)
	emptyFrom() uint64
	//invalidateFrom() uint64
	emptyHash(level int) common.Hash
}

type treeHasher struct {
	hashed treeHashed
	cache  []*lru.Cache[uint64, common.Hash]
}

func (h *treeHasher) getNode(index uint64) (common.Hash, error) {
	//h.checkInvalidate()
	level := 63 - bits.LeadingZeros(index)
	leafLevel := h.hashed.leafLevel()
	if level > leafLevel || index == 0 {
		return common.Hash{}, errors.New("invalid hash tree index")
	}
	if node, ok := h.cache[level].Get(index); ok {
		return node, nil
	}
	leafOffset := uint64(1) << leafLevel
	if level < leafLevel { // internal hash node
		firstLeaf := index<<(leafLevel-level) - leafOffset // first leaf of subtree
		if firstLeaf >= h.hashed.emptyFrom() {
			// entire subtree is empty, no recursion, no need to cache
			return h.hashed.emptyHash(level), nil
		}
		left, err := h.getNode(index * 2)
		if err != nil {
			return common.Hash{}, err
		}
		right, err := h.getNode(index*2 + 1)
		if err != nil {
			return common.Hash{}, err
		}
		hasher := sha256.New()
		hasher.Write(left[:])
		hasher.Write(right[:])
		var result common.Hash
		hasher.Sum(result[:0])
		h.cache[level].Add(index, result)
		return result, nil
	}
	// leaf node
	result, err := h.hashed.getLeaf(index - leafOffset)
	if err != nil {
		return common.Hash{}, err
	}
	h.cache[level].Add(index, result)
	return result, nil
}

type fmTree struct {
	*FilterMaps
	hc *headerChain
}

func (t *fmTree) leafLevel() int { return t.logMaxEpochs }
func (t *fmTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	return newHasher(epochTree{fmTree: t, epochIndex: leafIndex}).getNode(1)
}
func (t *fmTree) emptyFrom() uint64 { //TODO lock
	return (t.headLvPointer + uint64(1)<<(t.logMapsPerEpoch+t.logValuesPerMap) - 1) >> (t.logMapsPerEpoch + t.logValuesPerMap)
}
func (t *fmTree) emptyHash(level int) common.Hash { return fmTreeEmptyHashes[level] }

type epochTree struct {
	*fmTree
	epochIndex uint32
}

func (t epochTree) leafLevel() int { return 1 }
func (t epochTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	switch leafIndex {
	case 0:
		return newHasher(mapRowsTree(t)).getNode(1)
	case 1:
		return newHasher(&logIndexTree{epochTree: t}).getNode(1)
	}
}
func (t epochTree) emptyFrom() uint64               { return 2 }
func (t epochTree) emptyHash(level int) common.Hash { return epochTreeEmptyHashes[level] }

type mapRowsTree epochTree

func (t mapRowsTree) leafLevel() int { return t.logMaxEpochs }
func (t mapRowsTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	return newHasher(epochTree{fmTree: t, epochIndex: leafIndex}).getNode(1)
}
func (t mapRowsTree) emptyFrom() uint64 { //TODO lock
	return (t.headLvPointer + uint64(1)<<(t.logMapsPerEpoch+t.logValuesPerMap) - 1) >> (t.logMapsPerEpoch + t.logValuesPerMap)
}
func (t mapRowsTree) emptyHash(level int) common.Hash { return epochTreeEmptyHashes[level] }

type logIndexTree struct {
	epochTree
	// block receipts iterator fields
	blockNumber                   uint64
	blockHash                     common.Hash
	receipts                      types.Receipts
	txIndex, logIndex, valueIndex int
	lvIndex                       uint64
}

func (t *logIndexTree) leafLevel() int { return t.logMaxEpochs }
func (t *logIndexTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	return newHasher(epochTree{fmTree: t, epochIndex: leafIndex}).getNode(1)
}
func (t *logIndexTree) emptyFrom() uint64 { //TODO lock
	return (t.headLvPointer + uint64(1)<<(t.logMapsPerEpoch+t.logValuesPerMap) - 1) >> (t.logMapsPerEpoch + t.logValuesPerMap)
}
func (t *logIndexTree) emptyHash(level int) common.Hash { return epochTreeEmptyHashes[level] }

func (h *treeHasher) mapRowHash(mapIndex, rowIndex uint32) (common.Hash, error) {
	row, err := h.getFilterMapRow(mapIndex, rowIndex)
	if err != nil {
		return common.Hash{}, err
	}
	encRow := make([]byte, len(row)*4)
	for i, c := range row {
		binary.LittleEndian.PutUint32(encRow[i*4:(i+1)*4], c)
	}
	hasher := sha256.New()
	hasher.Write(encRow)
	var result common.Hash
	hasher.Sum(result[:0])
	return result, nil
}

func (h *treeHasher) logIndexNode(lvIndex uint64, refSubIndex uint32) (common.Hash, error) {
	if !h.iterateTo(lvIndex) {
		if lvIndex < f.tailBlockLvPointer {
			return common.Hash{}, errors.New("not indexed")
		}
		if lvIndex >= f.headLvPointer {
			return common.Hash{}, nil
		}
		blockNumber, lvPointer, err := h.getBlockByLvIndex(lvIndex)
		if err != nil {
			return common.Hash{}, err
		}
		blockHash := h.hc.getBlockHash(blockNumber)
		receipts := h.chain.GetReceiptsByHash(blockHash)
		if receipts == nil {
			return common.Hash{}, errors.New("receipts not found")
		}
		h.blockNumber, h.blockHash, h.receipts, h.lvIndex = blockNumber, blockHash, receipts, lvPointer
		h.txIndex, h.logIndex, h.valueIndex = 0, 0, 0
		if !h.iterateTo(lvIndex) {
			log.Error("Could not iterate to log value index")
			return common.Hash{}, errors.New("could not iterate to log value index")
		}
	}
	switch refSubIndex {
	case 0:
		return h.blockHash, nil
	case 1:
		var result common.Hash
		binary.LittleEndian.PutUint64(result[0:8], h.blockNumber)
		binary.LittleEndian.PutUint32(result[8:12], uint32(h.txIndex))
		binary.LittleEndian.PutUint32(result[12:16], uint32(h.logIndex))
		binary.LittleEndian.PutUint32(result[16:20], uint32(h.valueIndex))
		return result, nil
	default:
		panic("invalid refSubIndex")
	}
}

func (h *treeHasher) iterateTo(lvTarget uint64) bool {
	if l.receipts == nil {
		return false
	}
	for ; l.txIndex < len(l.receipts); l.txIndex++ {
		receipt := l.receipts[l.txIndex]
		for ; l.logIndex < len(receipt.Logs); l.logIndex++ {
			log := receipt.Logs[l.logIndex]
			for ; l.valueIndex <= len(log.Topics); l.valueIndex++ {
				if l.lvIndex == lvTarget {
					return true
				}
				l.lvIndex++
			}
		}
	}
	return false
}
