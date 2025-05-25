// Copyright 2025 The go-ethereum Authors
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
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// relative to root
	gtiEpochs    = 2
	gtiNextIndex = 3
	// relative to epoch root
	gtiFilterMaps = 2
	gtiLogEntries = 3
	// relative to progressive list root
	gtiProgListTree  = 2
	gtiProgListCount = 3
	// relative to progressive list tree root
	gtiProgListSubtree  = 2
	gtiProgListNextTree = 3

	cachedRowMappings = 10000 // log value to row mappings cached during rendering
)

type TreeNode [32]byte

type lvPosition struct{ rowIndex, layerIndex uint32 }

type logIndexData interface {
	get(treeIndex uint64) TreeNode
	set(treeIndex uint64, node TreeNode)
}

type Hasher struct {
	tree            logIndexData
	params          Params
	rowMappingCache *lru.Cache[common.Hash, lvPosition]
}

func (h *Hasher) AddLogEvent(log *types.Log) uint64 {
	nextIndex := nodeToUint64(h.tree.get(gtiNextIndex))
	valuesPerEpoch := uint64(h.params.mapsPerEpoch) * h.params.valuesPerMap
	if nextIndex%valuesPerEpoch == 0 {
		h.addNewEpoch(uint32(nextIndex / valuesPerEpoch))
	}
	panic(nil)
}

func (h *Hasher) AddBlockDelimiter(header *types.Header) uint64 {
	panic(nil)
}

func (h *Hasher) RenderLog(lvIndex uint64, log *types.Log) {
	panic(nil)
}

func (h *Hasher) InitGenesis() {
	h.tree.set(gtiEpochs, zeroHashes[h.params.logEpochHistory])
	h.tree.set(gtiNextIndex, uint64ToNode(0))
}

func (h *Hasher) InitWithProof([]byte) {
	panic(nil)
}

func (h *Hasher) MakeInitProof() []byte {
	panic(nil)
}

func (h *Hasher) gtiEpochRoot(epoch uint32) uint64 {
	return appendIndex(gtiEpochs, uint64(epoch), h.params.logEpochHistory)
}

func (h *Hasher) addNewEpoch(nextEpoch uint32) {
	epochRootIndex := h.expandVector(2, uint64(nextEpoch), h.params.logEpochHistory, true)
	h.tree.set(childIndex(epochRootIndex, gtiFilterMaps), zeroHashes[h.params.logMapHeight+h.params.logMapsPerEpoch])
	h.tree.set(childIndex(epochRootIndex, gtiLogEntries), zeroHashes[h.params.logMapsPerEpoch+h.params.logValuesPerMap])
}

func (h *Hasher) addNewMap(nextMap uint32) {
	epoch := nextMap >> h.params.logMapsPerEpoch
	mapSubIndex := nextMap % h.params.mapsPerEpoch
	if mapSubIndex == 0 {
		h.addNewEpoch(epoch)
	}
	filterMapsRootIndex := childIndex(h.gtiEpochRoot(epoch), gtiFilterMaps)
	for rowIndex := range h.params.mapHeight {
		epochRowRootIndex := appendIndex(filterMapsRootIndex, uint64(rowIndex), h.params.logMapHeight)
		mapRowRootIndex := h.expandVector(epochRowRootIndex, uint64(mapSubIndex), h.params.logMapsPerEpoch, true)
		h.tree.set(childIndex(mapRowRootIndex, gtiProgListTree), TreeNode{})
		h.tree.set(childIndex(mapRowRootIndex, gtiProgListCount), TreeNode{})
	}
	if h.rowMappingCache == nil {
		h.rowMappingCache = lru.NewCache[common.Hash, lvPosition](cachedRowMappings)
	} else {
		h.rowMappingCache.Purge()
	}
}

func (h *Hasher) addToMap(lvIndex uint64, logValue common.Hash) {
	mapIndex := uint32(lvIndex >> h.params.logValuesPerMap)
	lvp, cached := h.rowMappingCache.Get(logValue)
	if !cached {
		lvp = lvPosition{rowIndex: h.params.rowIndex(mapIndex, 0, logValue)}
	}
	columnIndex := h.params.columnIndex(lvIndex, &logValue)
	for !h.addToRow(mapIndex, lvp.rowIndex, columnIndex, h.params.maxRowLength(lvp.layerIndex)) {
		lvp.layerIndex++
		lvp.rowIndex = h.params.rowIndex(mapIndex, lvp.layerIndex, logValue)
		cached = false
	}
	if !cached {
		h.rowMappingCache.Add(logValue, lvp)
	}
}

func (h *Hasher) addToRow(mapIndex, rowIndex, entry, maxLen uint32) bool {
	epoch := mapIndex >> h.params.logMapsPerEpoch
	mapSubIndex := mapIndex % h.params.mapsPerEpoch
	filterMapsRootIndex := childIndex(h.gtiEpochRoot(epoch), gtiFilterMaps)
	epochRowRootIndex := appendIndex(filterMapsRootIndex, uint64(rowIndex), h.params.logMapHeight)
	mapRowRootIndex := h.expandVector(epochRowRootIndex, uint64(mapSubIndex), h.params.logMapsPerEpoch, true)
	countIndex := childIndex(mapRowRootIndex, gtiProgListCount)
	nextEntry := nodeToUint64(h.tree.get(countIndex))
	if nextEntry >= uint64(maxLen) {
		return false
	}
	h.tree.set(countIndex, uint64ToNode(nextEntry+1))
	nodeIndex, entrySubIndex := nextEntry/8, nextEntry%8
	progListSubtreeHeight := h.params.progListHeightFirst
	treeRoot := childIndex(mapRowRootIndex, gtiProgListTree)
	for uint64(1)<<progListSubtreeHeight <= nodeIndex {
		// move up to next proglist subtree
		nodeIndex -= uint64(1) << progListSubtreeHeight
		progListSubtreeHeight += h.params.progListHeightStep
		treeRoot = childIndex(treeRoot, gtiProgListNextTree)
	}
	if nodeIndex == 0 && entrySubIndex == 0 { // expand next proglist subtree
		h.tree.set(childIndex(treeRoot, gtiProgListNextTree), TreeNode{})
	}
	subtreeRoot := childIndex(treeRoot, gtiProgListSubtree)
	leafIndex := h.expandVector(subtreeRoot, nodeIndex, progListSubtreeHeight, false)
	var leaf TreeNode
	if entrySubIndex != 0 {
		leaf = h.tree.get(leafIndex)
	}
	binary.LittleEndian.PutUint32(leaf[entrySubIndex*4:entrySubIndex*4+4], entry)
	h.tree.set(leafIndex, leaf)
	return true
}

func (h *Hasher) expandVector(vectorRoot, nextIndex uint64, height uint, collapse bool) uint64 {
	tz := uint(bits.TrailingZeros64(nextIndex))
	if tz > height {
		tz = height
	}
	subtreeRoot := vectorRoot<<(height-tz) + nextIndex>>tz
	if collapse && tz > 0 && nextIndex > 0 {
		h.tree.set(subtreeRoot-1, h.tree.get(subtreeRoot-1)) // collapse finished subtree
	}
	for tz > 0 {
		tz--
		subtreeRoot *= 2
		// expanding left branch, add empty sibling node to the right
		h.tree.set(subtreeRoot+1, zeroHashes[tz])
	}
	return subtreeRoot
}

func nodeToUint64(node TreeNode) uint64 {
	return binary.LittleEndian.Uint64(node[:8])
}

func uint64ToNode(value uint64) (node TreeNode) {
	binary.LittleEndian.PutUint64(node[:8], value)
	return
}

func appendIndex(baseIndex, appendBits uint64, appendLen uint) uint64 {
	return baseIndex<<appendLen + appendBits
}

func childIndex(baseIndex, subIndex uint64) uint64 {
	appendLen := uint(63 - bits.LeadingZeros64(subIndex))
	return appendIndex(baseIndex, subIndex-uint64(1)<<appendLen, appendLen)
}

var zeroHashes [256]TreeNode

func init() {
	hasher := sha256.New()
	for i := 1; i < len(zeroHashes); i++ {
		hasher.Write(zeroHashes[i-1][:])
		hasher.Write(zeroHashes[i-1][:])
		hasher.Sum(zeroHashes[i][:0])
		hasher.Reset()
	}
}
