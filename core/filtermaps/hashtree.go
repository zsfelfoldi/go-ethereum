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
	"errors"
	"math/bits"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
)

const treeLevelCacheSize = 16

type treeHashed interface {
	leafLevel() int
	getLeaf(leafIndex uint64) (common.Hash, error)
	isEmpty(firstIndex, lastIndex uint64) bool
	//invalidateFrom() uint64
	emptyHash(level int) common.Hash
}

type treeHasher struct {
	hashed treeHashed
	cache  []*lru.Cache[uint64, common.Hash]
}

func newTreeHasher(hashed treeHashed) *treeHasher {
	h := &treeHasher{
		hashed: hashed,
		cache:  make([]*lru.Cache[uint64, common.Hash], hashed.leafLevel()+1),
	}
	for i := range h.cache {
		h.cache[i] = lru.NewCache[uint64, common.Hash](treeLevelCacheSize)
	}
	return h
}

func (t *treeHasher) getNode(index uint64) (common.Hash, error) {
	//t.checkInvalidate()
	level := 63 - bits.LeadingZeros64(index)
	leafLevel := t.hashed.leafLevel()
	if level > leafLevel || index == 0 {
		return common.Hash{}, errors.New("invalid hash tree index")
	}
	if node, ok := t.cache[level].Get(index); ok {
		return node, nil
	}
	leafOffset := uint64(1) << leafLevel
	if level < leafLevel { // internal hash node
		// check if subtree range is empty
		firstLeaf := index<<(leafLevel-level) - leafOffset
		lastLeaf := (index+1)<<(leafLevel-level) - leafOffset - 1
		if t.hashed.isEmpty(firstLeaf, lastLeaf) {
			// entire subtree is empty, no recursion, no need to cache
			return t.hashed.emptyHash(level), nil
		}
		left, err := t.getNode(index * 2)
		if err != nil {
			return common.Hash{}, err
		}
		right, err := t.getNode(index*2 + 1)
		if err != nil {
			return common.Hash{}, err
		}
		result := binaryHash(left, right)
		t.cache[level].Add(index, result)
		return result, nil
	}
	// leaf node
	result, err := t.hashed.getLeaf(index - leafOffset)
	if err != nil {
		return common.Hash{}, err
	}
	t.cache[level].Add(index, result)
	return result, nil
}

type filterMapsTree struct {
	*FilterMaps
	hc            *headerChain
	headLvPointer uint64
}

func (f *FilterMaps) newFilterMapsTree() *filterMapsTree {
	f.indexLock.Lock()
	defer f.indexLock.Unlock()

	return &filterMapsTree{
		FilterMaps:    f,
		hc:            newHeaderChain(f.chain, f.headBlockNumber, f.headBlockHash),
		headLvPointer: f.headLvPointer,
	}
}

func (t *filterMapsTree) leafLevel() int { return int(t.logMaxEpochs) }

func (t *filterMapsTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	return newTreeHasher(epochTree{filterMapsTree: t, epochIndex: leafIndex}).getNode(1)
}

func (t *filterMapsTree) isEmpty(firstIndex, lastIndex uint64) bool {
	return firstIndex<<(t.logMapsPerEpoch+t.logValuesPerMap) >= t.headLvPointer
}

func (t *filterMapsTree) emptyHash(level int) common.Hash { return t.filterMapsTreeEmptyHashes[level] }

type epochTree struct {
	*filterMapsTree
	epochIndex uint32
}

func (t epochTree) leafLevel() int { return 1 }

func (t epochTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	switch leafIndex {
	case 0:
		return newTreeHasher(mapRowsTree(t)).getNode(1)
	case 1:
		return newTreeHasher(&logIndexTree{epochTree: t}).getNode(1)
	}
}

func (t epochTree) isEmpty(firstIndex, lastIndex uint64) bool { return false }

func (t epochTree) emptyHash(level int) common.Hash {
	panic("epochTree.emptyHash should never be called")
}

type mapRowsTree epochTree

func (t mapRowsTree) leafLevel() int { return t.logMapHeight + t.logMapsPerEpoch }

func (t mapRowsTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	mapIndex := t.epochIndex<<t.logMapsPerEpoch + leafIndex%t.mapsPerEpoch
	rowIndex := leafIndex >> t.logMapsPerEpoch
	row, err := t.getFilterMapRow(mapIndex, rowIndex)
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

func (t mapRowsTree) isEmpty(firstIndex, lastIndex uint64) bool {
	firstMap := t.epochIndex << t.logMapsPerEpoch
	firstEmptyMap := (t.headLvIndex + t.valuesPerMap - 1) >> t.logValuesPerMap
	if firstEmptyMap <= firstMap {
		return true
	}
	if firstEmptyMap >= firstMap+t.mapsPerEpoch {
		return false
	}
	return firstIndex>>t.logsMapPerEpoch == lastIndex>>t.logsMapPerEpoch &&
		firstIndex%t.mapsPerEpoch >= firstEmptyMap-firstMap
}

func (t mapRowsTree) emptyHash(level int) common.Hash { return t.mapRowsTreeEmptyHashes[level] }

type logIndexTree struct {
	epochTree
	// block receipts iterator fields
	blockNumber                   uint64
	blockHash                     common.Hash
	receipts                      types.Receipts
	txIndex, logIndex, valueIndex int
	lvIndex                       uint64
}

func (t *logIndexTree) leafLevel() int { return t.logMapsPerEpoch + t.logValuesPerMap + 1 }

func (t *logIndexTree) getLeaf(leafIndex uint64) (common.Hash, error) {
	lvIndex := uint64(t.epochIndex)<<(t.logMapsPerEpoch+t.logValuesPerMap) + leafIndex>>1
	if ok, err := t.findLvIndex(lvIndex); !ok {
		return common.Hash{}, err
	}
	switch leafIndex & 1 {
	case 0:
		return t.blockHash, nil
	case 1:
		var result common.Hash
		binary.LittleEndian.PutUint64(result[0:8], t.blockNumber)
		binary.LittleEndian.PutUint32(result[8:12], uint32(t.txIndex))
		binary.LittleEndian.PutUint32(result[12:16], uint32(t.logIndex))
		binary.LittleEndian.PutUint32(result[16:20], uint32(t.valueIndex))
		return result, nil
	default:
		panic("invalid refSubIndex")
	}
}

func (t *logIndexTree) isEmpty(firstIndex, lastIndex uint64) bool {
	return uint64(t.epochIndex)<<(t.logMapsPerEpoch+t.logValuesPerMap)+firstIndex>>1 >= t.headLvPointer
}

func (t *logIndexTree) emptyHash(level int) common.Hash { return t.logIndexTreeEmptyHashes[level] }

func (t *logIndexTree) findLvIndex(lvIndex uint64) (bool, error) {
	if t.iterateTo(lvIndex) {
		return true, nil
	}
	if lvIndex < f.tailBlockLvPointer {
		return false, errors.New("not indexed")
	}
	if lvIndex >= f.headLvPointer {
		return false, nil
	}
	blockNumber, lvPointer, err := t.getBlockByLvIndex(lvIndex)
	if err != nil {
		return false, err
	}
	blockHash := t.hc.getBlockHash(blockNumber)
	receipts := t.chain.GetReceiptsByHash(blockHash)
	if receipts == nil {
		return false, errors.New("receipts not found")
	}
	t.blockNumber, t.blockHash, t.receipts, t.lvIndex = blockNumber, blockHash, receipts, lvPointer
	t.txIndex, t.logIndex, t.valueIndex = 0, 0, 0
	if !t.iterateTo(lvIndex) {
		log.Error("Could not iterate to log value index", "blockNumber", blockNumber, "first lvIndex", lvPointer, "target lvIndex", lvIndex)
		return false, errors.New("could not iterate to log value index")
	}
	return true, nil
}

func (t *logIndexTree) iterateTo(lvTarget uint64) bool {
	if t.receipts == nil {
		return false
	}
	for ; t.txIndex < len(t.receipts); t.txIndex++ {
		receipt := t.receipts[t.txIndex]
		for ; t.logIndex < len(receipt.Logs); t.logIndex++ {
			log := receipt.Logs[t.logIndex]
			for ; t.valueIndex <= len(log.Topics); t.valueIndex++ {
				if t.lvIndex == lvTarget {
					return true
				}
				t.lvIndex++
			}
		}
	}
	return false
}

func makeEmptyHashes(leafLevel int, leafDefault common.Hash) []common.Hash {
	hashes := make([]common.Hash, leafLevel+1)
	hashes[leafLevel] = leafDefault
	for i := leafLevel - 1; i >= 0; i-- {
		hashes[i] = binaryHash(hashes[i+1], hashes[i+1])
	}
	return hashes
}

func binaryHash(left, right common.Hash) common.Hash {
	var result common.Hash
	hasher := sha256.New()
	hasher.Write(left[:])
	hasher.Write(right[:])
	hasher.Sum(result[:0])
	return result
}

type emptyHashes struct {
	filterMapsTreeEmptyHashes,
	mapRowsTreeEmptyHashes,
	logIndexTreeEmptyHashes []common.Hash
}

func (f *FilterMaps) initEmptyHashes() {
	var emptyHash common.Hash
	hasher := sha256.New()
	hasher.Sum(emptyHash[:0])
	f.mapRowsTreeEmptyHashes = makeEmptyHashes(f.logMapHeight+f.logMapsPerEpoch, emptyHash)
	f.logIndexTreeEmptyHashes = makeEmptyHashes(f.logMapsPerEpoch+f.logValuesPerMap+1, common.Hash{})
	f.filterMapsTreeEmptyHashes = makeEmptyHashes(f.logMaxEpochs, binaryHash(f.mapRowsTreeEmptyHashes[0], f.logIndexTreeEmptyHashes[0]))
}
