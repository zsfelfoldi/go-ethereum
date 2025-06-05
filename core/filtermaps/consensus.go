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

var (
	// relative to root
	gtiEpochs    = treeIndex{lo: 2}
	gtiNextIndex = treeIndex{lo: 3}
	// relative to epoch root
	gtiFilterMaps = treeIndex{lo: 2}
	gtiLogEntries = treeIndex{lo: 3}
	// relative to progressive list root
	gtiProgListTree  = treeIndex{lo: 2}
	gtiProgListCount = treeIndex{lo: 3}
	// relative to progressive list tree root
	gtiProgListSubtree  = treeIndex{lo: 2}
	gtiProgListNextTree = treeIndex{lo: 3}
)

const cachedRowMappings = 10000 // log value to row mappings cached during rendering

func (params *Params) gtiEpochRoot(epoch uint32) treeIndex {
	return gtiEpochs.append(uint64(epoch), params.logEpochHistory)
}

type progListIndex struct {
	params                         *Params
	listRoot, countIndex, treeRoot treeIndex
	subtreeHeight                  uint
	subtreeFirst                   uint64
}

func (pl *progListIndex) init(params *Params, root treeIndex) {
	pl.params = params
	pl.listRoot = root
	pl.countIndex = root.child(gtiProgListCount)
	pl.treeRoot = root.child(gtiProgListTree)
	pl.subtreeHeight = params.progListHeightFirst
	pl.subtreeFirst = 0
}

func (pl *progListIndex) getLeaf(listIndex uint64) (leafIndex, treeRoot treeIndex, subtreeIndex uint64, subtreeHeight uint) {
	if listIndex < pl.subtreeFirst {
		pl.init(pl.params, pl.listRoot)
	}
	subtreeSize := uint64(1) << pl.subtreeHeight
	for pl.subtreeFirst+subtreeSize <= listIndex {
		// move up to next proglist subtree
		pl.subtreeFirst += subtreeSize
		pl.subtreeHeight += pl.params.progListHeightStep
		pl.treeRoot = pl.treeRoot.child(gtiProgListNextTree)
	}
	subtreeIndex = listIndex - pl.subtreeFirst
	return pl.treeRoot.child(gtiProgListSubtree).append(subtreeIndex, pl.subtreeHeight),
		pl.treeRoot, subtreeIndex, pl.subtreeHeight
}

type lvPosition struct{ rowIndex, layerIndex uint32 }

type logIndexReader interface {
	get(treeIndex) TreeNode
}

type logIndexData interface {
	logIndexReader
	set(treeIndex, TreeNode)
	finalize(treeIndex)
}

type Hasher struct {
	tree            logIndexData
	params          *Params
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

func (h *Hasher) addNewEpoch(nextEpoch uint32) {
	epochRootIndex := h.expandVector(gtiEpochs, uint64(nextEpoch), h.params.logEpochHistory, true)
	h.tree.set(epochRootIndex.child(gtiFilterMaps), zeroHashes[h.params.logMapHeight+h.params.logMapsPerEpoch])
	h.tree.set(epochRootIndex.child(gtiLogEntries), zeroHashes[h.params.logMapsPerEpoch+h.params.logValuesPerMap])
}

func (h *Hasher) addNewMap(nextMap uint32) {
	epoch := nextMap >> h.params.logMapsPerEpoch
	mapSubIndex := nextMap % h.params.mapsPerEpoch
	if mapSubIndex == 0 {
		h.addNewEpoch(epoch)
	}
	filterMapsRootIndex := h.params.gtiEpochRoot(epoch).child(gtiFilterMaps)
	for rowIndex := range h.params.mapHeight {
		epochRowRootIndex := filterMapsRootIndex.append(uint64(rowIndex), h.params.logMapHeight)
		mapRowRootIndex := h.expandVector(epochRowRootIndex, uint64(mapSubIndex), h.params.logMapsPerEpoch, true)
		h.tree.set(mapRowRootIndex.child(gtiProgListTree), TreeNode{})
		h.tree.set(mapRowRootIndex.child(gtiProgListCount), TreeNode{})
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
	filterMapsRootIndex := h.params.gtiEpochRoot(epoch).child(gtiFilterMaps)
	epochRowRootIndex := filterMapsRootIndex.append(uint64(rowIndex), h.params.logMapHeight)
	mapRowRootIndex := h.expandVector(epochRowRootIndex, uint64(mapSubIndex), h.params.logMapsPerEpoch, true)
	var pl progListIndex
	pl.init(h.params, mapRowRootIndex)
	nextEntry := nodeToUint64(h.tree.get(pl.countIndex))
	if nextEntry >= uint64(maxLen) {
		return false
	}
	h.tree.set(pl.countIndex, uint64ToNode(nextEntry+1))
	listIndex, listSubIndex := nextEntry/8, nextEntry%8
	_, treeRoot, subtreeIndex, subtreeHeight := pl.getLeaf(listIndex)
	if subtreeIndex == 0 && listSubIndex == 0 { // expand next proglist subtree
		h.tree.set(treeRoot.child(gtiProgListNextTree), TreeNode{})
	}
	subtreeRoot := treeRoot.child(gtiProgListSubtree)
	leafIndex := h.expandVector(subtreeRoot, subtreeIndex, subtreeHeight, false)
	var leaf TreeNode
	if listSubIndex != 0 {
		leaf = h.tree.get(leafIndex)
	}
	binary.LittleEndian.PutUint32(leaf[listSubIndex*4:listSubIndex*4+4], entry)
	h.tree.set(leafIndex, leaf)
	return true
}

func (h *Hasher) expandVector(vectorRoot treeIndex, nextIndex uint64, height uint, finalize bool) treeIndex {
	tz := uint(bits.TrailingZeros64(nextIndex))
	if tz > height {
		tz = height
	}
	subtreeRoot := vectorRoot.append(nextIndex>>tz, height-tz)
	if finalize && tz > 0 && nextIndex > 0 {
		prevSubtree := subtreeRoot
		prevSubtree.lo--
		h.tree.finalize(prevSubtree)
	}
	for tz > 0 {
		tz--
		subtreeRoot = subtreeRoot.shiftLeft(1)
		rightSibling := subtreeRoot
		rightSibling.lo++
		// expanding left branch, add empty sibling node to the right
		h.tree.set(rightSibling, zeroHashes[tz])
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
