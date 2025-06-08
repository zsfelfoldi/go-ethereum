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
	"math"
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

	// log
	gtiLogAddress      = treeIndex{lo: 8}
	gtiLogTopicsRoot   = treeIndex{lo: 18}
	gtiLogTopicsLength = treeIndex{lo: 19}
	gtiLogData         = treeIndex{lo: 10} // prog list
	gtiLogZero         = treeIndex{lo: 11}
	// log meta
	gtiLogMetaBlockNumber = treeIndex{lo: 12}
	gtiLogMetaTxHash      = treeIndex{lo: 13}
	gtiLogMetaTxIndex     = treeIndex{lo: 14}
	gtiLogMetaLogIndex    = treeIndex{lo: 15}
	// block delimiter meta
	gtiDelimiterZero            = treeIndex{lo: 2}
	gtiDelimiterMetaBlockNumber = treeIndex{lo: 12}
	gtiDelimiterMetaBlockHash   = treeIndex{lo: 13}
	gtiDelimiterMetaTimestamp   = treeIndex{lo: 14}
	gtiDelimiterMetaDummy       = treeIndex{lo: 15}
)

type TreeNode [32]byte

type treeIndex struct{ lo, hi uint64 }

var rootIndex = treeIndex{lo: 1}

func (t treeIndex) leadingZeros() uint {
	if t.hi == 0 {
		return uint(bits.LeadingZeros64(t.lo)) + 64
	}
	return uint(bits.LeadingZeros64(t.hi))
}

func (t treeIndex) level() uint {
	return 127 - t.leadingZeros()
}

func (t treeIndex) shiftLeft(b uint) treeIndex {
	if b >= 64 {
		return treeIndex{hi: t.lo << (b - 64)}
	}
	return treeIndex{lo: t.lo << b, hi: t.hi<<b + t.lo>>(64-b)}
}

func (t treeIndex) shiftRight(b uint) treeIndex {
	if b >= 64 {
		return treeIndex{lo: t.hi >> (b - 64)}
	}
	return treeIndex{lo: t.lo>>b + t.hi<<(64-b), hi: t.hi >> b}
}

func (t treeIndex) addInt(add int64) treeIndex {
	r := t
	r.lo += uint64(add)
	if add > 0 && r.lo < t.lo {
		r.hi++
	}
	if add < 0 && r.lo > t.lo {
		r.hi--
	}
	return r
}

func (t treeIndex) bit(b uint) uint {
	if b < 64 {
		return uint((t.lo >> b) & 1)
	}
	return uint((t.hi >> (b - 64)) & 1)
}

func (t treeIndex) lowerBits(b uint) treeIndex {
	if b <= 64 {
		return treeIndex{lo: t.lo & (uint64(1)<<b - 1)}
	}
	return treeIndex{lo: t.lo, hi: t.hi & (uint64(1)<<(b-64) - 1)}
}

func (t treeIndex) split(splitLevel uint) (treeIndex, treeIndex) {
	level := t.level()
	if level <= splitLevel {
		return t, rootIndex
	}
	level -= splitLevel
	return t.shiftRight(level), t.lowerBits(level)
}

func (t treeIndex) or(s treeIndex) treeIndex {
	return treeIndex{lo: t.lo | s.lo, hi: t.hi | s.hi}
}

func (t treeIndex) xor(s treeIndex) treeIndex {
	return treeIndex{lo: t.lo ^ s.lo, hi: t.hi ^ s.hi}
}

func (t treeIndex) child(s treeIndex) treeIndex {
	l := s.level()
	return t.shiftLeft(l).or(s.lowerBits(l))
}

func (t treeIndex) append(index uint64, height uint) treeIndex {
	res := t.shiftLeft(height)
	res.lo |= index
	return res
}

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
	for pl.subtreeFirst+(uint64(1)<<pl.subtreeHeight) <= listIndex {
		// move up to next proglist subtree
		pl.subtreeFirst += uint64(1) << pl.subtreeHeight
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
	treeReader
	set(treeIndex, TreeNode)
	finalize(treeIndex)
}

type Hasher struct {
	tree            logIndexData
	params          *Params
	rowMappingCache *lru.Cache[common.Hash, lvPosition]
}

func (h *Hasher) AddLogEvent(log *types.Log) (uint64, uint64) {
	addCount := uint64(len(log.Topics) + 1)
	lvIndex := h.addValues(addCount)
	//fmt.Println(" add log root", lvIndex)
	h.RenderLog(lvIndex, log)
	for lvi := lvIndex + 1; lvi < lvIndex+addCount; lvi++ {
		//fmt.Println(" add zero log", lvi)
		h.tree.set(h.params.gtiLogEntryRoot(lvi), TreeNode{})
	}
	h.addToMap(lvIndex, addressValue(log.Address))
	for i, topic := range log.Topics {
		h.addToMap(lvIndex+uint64(i+1), topicValue(topic))
	}
	h.advance(lvIndex, addCount)
	return lvIndex, lvIndex + addCount
}

func (h *Hasher) AddBlockDelimiter(header *types.Header) (uint64, uint64) {
	lvIndex := h.addValues(1)
	//fmt.Println(" add block delimiter", lvIndex)
	h.RenderBlockDelimiter(lvIndex, header)
	h.advance(lvIndex, 1)
	return lvIndex, lvIndex + 1
}

// addValues; add actual row and log entries; advance
func (h *Hasher) addValues(addCount uint64) uint64 {
	nextIndex := nodeToUint64(h.tree.get(gtiNextIndex))
	leftFromMap := h.params.valuesPerMap - nextIndex%h.params.valuesPerMap
	if leftFromMap < addCount {
		nextIndex += leftFromMap
	}
	if nextIndex%h.params.valuesPerMap == 0 {
		h.addNewMap(uint32(nextIndex / h.params.valuesPerMap))
	}
	//fmt.Println(" addValues", nextIndex, addCount)
	return nextIndex
}

func (h *Hasher) advance(startIndex, addCount uint64) {
	valuesPerEpoch := uint64(h.params.mapsPerEpoch) * h.params.valuesPerMap
	logEntriesRoot := h.params.gtiEpochRoot(uint32(startIndex / valuesPerEpoch)).child(gtiLogEntries)
	firstSubIndex := startIndex % valuesPerEpoch
	lastSubIndex := (startIndex + addCount - 1) % valuesPerEpoch
	h.expandVector(logEntriesRoot, firstSubIndex, lastSubIndex, h.params.logMapsPerEpoch+h.params.logValuesPerMap)
	h.finalizeVector(logEntriesRoot, firstSubIndex, lastSubIndex, h.params.logMapsPerEpoch+h.params.logValuesPerMap)
	h.tree.set(gtiNextIndex, uint64ToNode(startIndex+addCount))
	//fmt.Println(" advance", startIndex, addCount)
}

func (h *Hasher) RenderBlockDelimiter(lvIndex uint64, header *types.Header) {
	logEntryRoot := h.params.gtiLogEntryRoot(lvIndex)
	h.tree.set(logEntryRoot.child(gtiDelimiterZero), TreeNode{})
	h.tree.set(logEntryRoot.child(gtiDelimiterMetaBlockNumber), uint64ToNode(header.Number.Uint64()))
	h.tree.set(logEntryRoot.child(gtiDelimiterMetaBlockHash), TreeNode(header.Hash()))
	h.tree.set(logEntryRoot.child(gtiDelimiterMetaTimestamp), uint64ToNode(header.Time))
	h.tree.set(logEntryRoot.child(gtiDelimiterMetaDummy), uint64ToNode(math.MaxUint64))
}

func (h *Hasher) RenderLog(lvIndex uint64, log *types.Log) {
	logEntryRoot := h.params.gtiLogEntryRoot(lvIndex)
	var addr TreeNode
	copy(addr[:len(log.Address)], log.Address[:])
	h.tree.set(logEntryRoot.child(gtiLogAddress), addr)
	h.tree.set(logEntryRoot.child(gtiLogTopicsLength), uint64ToNode(uint64(len(log.Topics))))
	for i := range 4 {
		var node TreeNode
		if i < len(log.Topics) {
			node = TreeNode(log.Topics[i])
		}
		h.tree.set(logEntryRoot.child(gtiLogTopicsRoot).append(uint64(i), 2), node)
	}
	dataLen := uint64(len(log.Data))
	var pl progListIndex
	pl.init(h.params, logEntryRoot.child(gtiLogData))
	h.tree.set(pl.countIndex, uint64ToNode(dataLen))
	chunkIndex := uint64(0)
	for ptr := uint64(0); ptr < dataLen; {
		leafIndex, _, _, _ := pl.getLeaf(chunkIndex)
		var node TreeNode
		end := min(ptr+32, dataLen)
		copy(node[:end-ptr], log.Data[ptr:end])
		ptr = end
		h.tree.set(leafIndex, node)
		chunkIndex++
	}
	if chunkIndex == 0 {
		h.tree.set(pl.treeRoot, TreeNode{})
	} else {
		_, treeRoot, subtreeIndex, subtreeHeight := pl.getLeaf(chunkIndex - 1)
		h.tree.set(treeRoot.child(gtiProgListNextTree), TreeNode{})
		subtreeRoot := treeRoot.child(gtiProgListSubtree)
		h.expandVector(subtreeRoot, 0, subtreeIndex, subtreeHeight)
	}
	h.tree.set(logEntryRoot.child(gtiLogZero), TreeNode{})
	h.tree.set(logEntryRoot.child(gtiLogMetaBlockNumber), uint64ToNode(log.BlockNumber))
	h.tree.set(logEntryRoot.child(gtiLogMetaTxHash), TreeNode(log.TxHash))
	h.tree.set(logEntryRoot.child(gtiLogMetaTxIndex), uint64ToNode(uint64(log.TxIndex)))
	h.tree.set(logEntryRoot.child(gtiLogMetaLogIndex), uint64ToNode(uint64(log.Index)))
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
	//fmt.Println("addNewEpoch", nextEpoch)
	if nextEpoch > 0 {
		h.finalizeVector(gtiEpochs, uint64(nextEpoch-1), uint64(nextEpoch-1), h.params.logEpochHistory)
	}
	epochRootIndex := h.params.gtiEpochRoot(nextEpoch)
	for rowIndex := range h.params.mapHeight {
		h.tree.set(epochRootIndex.child(gtiFilterMaps).append(uint64(rowIndex), h.params.logMapHeight), zeroHashes[h.params.logMapsPerEpoch])
	}
	//	h.tree.set(epochRootIndex.child(gtiFilterMaps), zeroHashes[h.params.logMapHeight+h.params.logMapsPerEpoch])
	h.tree.set(epochRootIndex.child(gtiLogEntries), zeroHashes[h.params.logMapsPerEpoch+h.params.logValuesPerMap])
	h.expandVector(gtiEpochs, uint64(nextEpoch), uint64(nextEpoch), h.params.logEpochHistory)
}

func (h *Hasher) addNewMap(nextMap uint32) {
	//fmt.Println("addNewMap", nextMap)
	epoch := nextMap >> h.params.logMapsPerEpoch
	mapSubIndex := nextMap % h.params.mapsPerEpoch
	if mapSubIndex == 0 {
		h.addNewEpoch(epoch)
	}
	filterMapsRootIndex := h.params.gtiEpochRoot(epoch).child(gtiFilterMaps)
	for rowIndex := range h.params.mapHeight {
		epochRowRootIndex := filterMapsRootIndex.append(uint64(rowIndex), h.params.logMapHeight)
		if mapSubIndex > 0 {
			h.finalizeVector(epochRowRootIndex, uint64(mapSubIndex-1), uint64(mapSubIndex-1), h.params.logMapsPerEpoch)
		}
		mapRowRootIndex := epochRowRootIndex.append(uint64(mapSubIndex), h.params.logMapsPerEpoch)
		h.tree.set(mapRowRootIndex.child(gtiProgListTree), TreeNode{})
		h.tree.set(mapRowRootIndex.child(gtiProgListCount), TreeNode{})
		h.expandVector(epochRowRootIndex, uint64(mapSubIndex), uint64(mapSubIndex), h.params.logMapsPerEpoch)
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
	mapRowRootIndex := epochRowRootIndex.append(uint64(mapSubIndex), h.params.logMapsPerEpoch)
	var pl progListIndex
	pl.init(h.params, mapRowRootIndex)
	nextEntry := nodeToUint64(h.tree.get(pl.countIndex))
	if nextEntry >= uint64(maxLen) {
		return false
	}
	h.tree.set(pl.countIndex, uint64ToNode(nextEntry+1))
	listIndex, listSubIndex := nextEntry/8, nextEntry%8
	leafIndex, treeRoot, subtreeIndex, subtreeHeight := pl.getLeaf(listIndex)
	//fmt.Println("* proglist", listIndex, "leaf", leafIndex, "sub", subtreeIndex, "subh", subtreeHeight)
	if subtreeIndex == 0 && listSubIndex == 0 { // expand next proglist subtree
		h.tree.set(treeRoot.child(gtiProgListNextTree), TreeNode{})
	}
	subtreeRoot := treeRoot.child(gtiProgListSubtree)
	var leaf TreeNode
	if listSubIndex == 0 {
		h.expandVector(subtreeRoot, subtreeIndex, subtreeIndex, subtreeHeight)
	} else {
		leaf = h.tree.get(leafIndex)
	}
	binary.LittleEndian.PutUint32(leaf[listSubIndex*4:listSubIndex*4+4], entry)
	h.tree.set(leafIndex, leaf)
	//fmt.Println("addToRow", mapIndex, rowIndex, entry, maxLen, nextEntry)
	//dumpSubtree(h.tree, mapRowRootIndex, rootIndex)
	return true
}

// finalizeVector finalizes all subtrees whose last item is in the firstIndex to
// lastIndex range.
func (h *Hasher) finalizeVector(vectorRoot treeIndex, firstIndex, lastIndex uint64, height uint) {
	for shift := range height + 1 {
		afterLastShift := (lastIndex + 1) >> shift
		if firstIndex>>shift == afterLastShift {
			break // no subtree's last item is included on this level or the levels below
		}
		if afterLastShift&1 == 1 { // left subtree's last item included, finalize
			//TODO h.tree.finalize(vectorRoot.append(afterLastShift-1, height-shift))
		}
	}
}

// expandVector sets the right siblings of each left side subtree whose first item
// is in the firstIndex to lastIndex range, unless the first item of the right
// side subtree is also covered by the range.
func (h *Hasher) expandVector(vectorRoot treeIndex, firstIndex, lastIndex uint64, height uint) {
	for shift := range height {
		lastShift := lastIndex >> shift
		if firstIndex > 0 && ((firstIndex-1)>>shift == lastShift) {
			break // no subtree's first item is included on this level or the levels below
		}
		if lastShift&1 == 0 { // left subtree's first item included, initialize right sibling
			h.tree.set(vectorRoot.append(lastShift+1, height-shift), zeroHashes[shift])
		}
	}
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
