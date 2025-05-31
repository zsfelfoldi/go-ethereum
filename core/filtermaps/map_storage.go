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

// immutable compact memory representation of a single filter map
// assumes that params.valuesPerMap <= 2**16 and params.mapWidth <= 2**32
type memoryMap struct {
	rowPtrs   []uint16
	rowData   []uint32
	hashNodes map[uint64][]TreeNode
}

func (mm *memoryMap) getRow(rowIndex uint32) FilterRow {
	var start uint16
	if rowIndex > 0 {
		start = mm.rowPtrs[rowIndex-1]
	}
	count := mm.rowPtrs[rowIndex] - start
	return FilterRow(mm.rowData[start : uint(start)+uint(count)]) // typecast before add is needed in case rowPtrs[rowIndex] wrapped around to 0
}

func (params *Params) getRowDataFromTree(tree logIndexReader, mapIndex, rowIndex uint32, start, maxLen uint64, target []uint32) uint64 {
	epoch := mapIndex >> params.logMapsPerEpoch
	mapSubIndex := mapIndex % params.mapsPerEpoch
	filterMapsRootIndex := childIndex(params.gtiEpochRoot(epoch), gtiFilterMaps)
	epochRowRootIndex := appendIndex(filterMapsRootIndex, uint64(rowIndex), params.logMapHeight)
	mapRowRootIndex := appendIndex(epochRowRootIndex, uint64(mapSubIndex), params.logMapsPerEpoch)
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
	for i := 0; i < readCount; i++ {
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

func (params *Params) makeMemoryMap(mv *memTreeView, mapIndex uint32, mapSubIndices []uint64) *memoryMap {
	if params.logMapHeight > 16 || params.logValuesPerMap > 32 {
		panic("invalid filter map parameters")
	}
	mm := &memoryMap{
		rowPtrs:   make([]uint16, params.mapHeight),
		rowData:   make([]uint32, params.valuesPerMap),
		hashNodes: make(map[uint64][]TreeNode),
	}
	for _, msi := range mapSubIndices {
		mm.hashNodes[msi] = make([]TreeNode, params.valuesPerMap)
	}
	var ptr uint64
	epoch := mapIndex >> params.logMapsPerEpoch
	mapSubIndex := mapIndex % params.mapsPerEpoch
	filterMapsRootIndex := childIndex(params.gtiEpochRoot(epoch), gtiFilterMaps)
	for rowIndex := range params.mapHeight {
		epochRowRootIndex := appendIndex(filterMapsRootIndex, uint64(rowIndex), params.logMapHeight)
		for _, msi := range mapSubIndices {
			mm.hashNodes[msi][rowIndex] = mv.get(childIndex(epochRowRootIndex, msi))
		}
		count := params.getRowDataFromTree(mv, mapIndex, rowIndex, 0, params.valuesPerMap-ptr, mm.rowData[ptr:])
		ptr += count
		mm.rowPtrs[rowIndex] = uint16(ptr)
	}
	mm.rowData = mm.rowData[:ptr]
}
