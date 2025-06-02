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
	"encoding/binary"
)

// immutable compact memory representation of a single filter map
// assumes that params.valuesPerMap <= 2**16 and params.mapWidth <= 2**32
type memoryMap struct {
	rowPtrs   []uint16
	rowData   []uint32
	treeNodes []storedNode
}

type storedNode struct {
	index uint64 // 56 bits group index plus 8 bits subindex
	node  TreeNode
}

func (mm *memoryMap) getRow(rowIndex uint32) FilterRow {
	var start uint16
	if rowIndex > 0 {
		start = mm.rowPtrs[rowIndex-1]
	}
	count := mm.rowPtrs[rowIndex] - start
	return FilterRow(mm.rowData[start : uint(start)+uint(count)]) // typecast before add is needed in case rowPtrs[rowIndex] wrapped around to 0
}

func (params *Params) mapRowRootIndex(mapIndex, rowIndex uint32) uint64 {
	epoch := mapIndex >> params.logMapsPerEpoch
	mapSubIndex := mapIndex % params.mapsPerEpoch
	filterMapsRootIndex := childIndex(params.gtiEpochRoot(epoch), gtiFilterMaps)
	epochRowRootIndex := appendIndex(filterMapsRootIndex, uint64(rowIndex), params.logMapHeight)
	return appendIndex(epochRowRootIndex, uint64(mapSubIndex), params.logMapsPerEpoch)
}

func (params *Params) getRowDataFromTree(tree logIndexReader, mapRowRootIndex uint64, start, maxLen uint64, target []uint32) uint64 {
	var pl progListIndex
	pl.init(params, mapRowRootIndex)
	count := nodeToUint64(tree.get(pl.countIndex))
	if start >= count {
		return 0
	}
	listIndex, listSubIndex := start/8, start%8
	var leaf TreeNode
	newLeaf := true
	readCount := min(maxLen, count-start)
	for i := range readCount {
		if newLeaf {
			leafIndex, _, _, _ := pl.getLeaf(listIndex)
			leaf = tree.get(leafIndex)
			newLeaf = false
		}
		target[i] = binary.LittleEndian.Uint32(leaf[listSubIndex*4 : listSubIndex*4+4])
		listSubIndex++
		if listSubIndex == 8 {
			listIndex++
			newLeaf = true
			listSubIndex = 0
		}
	}
	return readCount
}

func (params *Params) makeMemoryMap(mv *memTreeView, mapIndex uint32) *memoryMap {
	if params.logMapHeight > 16 || params.logValuesPerMap > 32 {
		panic("invalid filter map parameters")
	}
	mm := &memoryMap{
		rowPtrs: make([]uint16, params.mapHeight),
		rowData: make([]uint32, params.valuesPerMap),
	}
	var ptr uint64
	epoch := mapIndex >> params.logMapsPerEpoch
	mapSubIndex := mapIndex % params.mapsPerEpoch
	filterMapsRootIndex := childIndex(params.gtiEpochRoot(epoch), gtiFilterMaps)
	storeNode := func(index uint64) {
		node := mv.get(index)
		mm.treeNodes = append(mm.treeNodes, storedNode{index: params.generalizedIndexToGroupTreeIndex(index), node: node})
	}
	if mapSubIndex == params.mapsPerEpoch-1 {
		for _, level := range params.storedRowTreeLevels {
			width := uint64(1) << level
			for i := range width {
				storeNode(childIndex(filterMapsRootIndex, width+i))
			}
		}
	}
	mapSubtreeIndices := params.storeMapSubtreeIndices(mapSubIndex)
	for rowIndex := range params.mapHeight {
		epochRowRootIndex := appendIndex(filterMapsRootIndex, uint64(rowIndex), params.logMapHeight)
		mapRowRootIndex := params.mapRowRootIndex(mapIndex, rowIndex)
		rowLength := params.getRowDataFromTree(mv, mapRowRootIndex, 0, params.valuesPerMap-ptr, mm.rowData[ptr:])
		ptr += rowLength
		mm.rowPtrs[rowIndex] = uint16(ptr)
		for _, msi := range mapSubtreeIndices {
			storeNode(childIndex(epochRowRootIndex, msi))
		}
		for _, rti := range params.storeRowTreeIndices(rowLength) {
			storeNode(childIndex(mapRowRootIndex, rti))
		}
	}
	mm.rowData = mm.rowData[:ptr]
	return mm
}

func (params *Params) storeMapSubtreeIndices(mapSubIndex uint64) []uint64 {
	panic(nil)
}

func (params *Params) storeRowTreeIndices(rowLength uint64) []uint64 {
	panic(nil)
}

func (params *Params) generalizedIndexToGroupTreeIndex(index uint64) uint64 {
	panic(nil)
}
