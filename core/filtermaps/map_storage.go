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
	"math/bits"
	"sort"

	"github.com/ethereum/go-ethereum/core/rawdb"
)

const (
	maxBatchSize = 10000
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

// immutable compact memory representation of a single filter map
// assumes that params.valuesPerMap <= 2**16 and params.mapWidth <= 2**32
type memoryMap struct {
	mapIndex  uint32
	rowPtrs   []uint16
	rowData   []uint32
	treeNodes []storedNode
}

type storedNode struct {
	storageIndex treeIndex
	node         TreeNode
}

func (mm *memoryMap) getRow(rowIndex uint32) FilterRow {
	var start uint16
	if rowIndex > 0 {
		start = mm.rowPtrs[rowIndex-1]
	}
	count := mm.rowPtrs[rowIndex] - start
	return FilterRow(mm.rowData[start : uint(start)+uint(count)]) // typecast before add is needed in case rowPtrs[rowIndex] wrapped around to 0
}

func (params *Params) mapRowRootIndex(mapIndex, rowIndex uint32) treeIndex {
	epoch := mapIndex >> params.logMapsPerEpoch
	mapSubIndex := mapIndex % params.mapsPerEpoch
	filterMapsRootIndex := params.gtiEpochRoot(epoch).child(gtiFilterMaps)
	epochRowRootIndex := filterMapsRootIndex.append(uint64(rowIndex), params.logMapHeight)
	return epochRowRootIndex.append(uint64(mapSubIndex), params.logMapsPerEpoch)
}

func (params *Params) getRowDataFromTree(tree logIndexReader, mapRowRootIndex treeIndex, start, maxLen uint32, target []uint32) uint32 {
	var pl progListIndex
	pl.init(params, mapRowRootIndex)
	count := uint32(nodeToUint64(tree.get(pl.countIndex)))
	if start >= count {
		return 0
	}
	listIndex, listSubIndex := start/8, start%8
	var leaf TreeNode
	newLeaf := true
	readCount := min(maxLen, count-start)
	for i := range readCount {
		if newLeaf {
			leafIndex, _, _, _ := pl.getLeaf(uint64(listIndex))
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
		mapIndex: mapIndex,
		rowPtrs:  make([]uint16, params.mapHeight),
		rowData:  make([]uint32, params.valuesPerMap),
	}
	var ptr uint32
	epoch := mapIndex >> params.logMapsPerEpoch
	mapSubIndex := mapIndex % params.mapsPerEpoch
	epochRootIndex := params.gtiEpochRoot(epoch)
	filterMapsRootIndex := epochRootIndex.child(gtiFilterMaps)
	logEntriesRootIndex := epochRootIndex.child(gtiLogEntries)
	storeNode := func(index treeIndex) {
		node := mv.get(index)
		mm.treeNodes = append(mm.treeNodes, storedNode{storageIndex: params.toStorageIndex(index), node: node})
	}
	if mapSubIndex == params.mapsPerEpoch-1 {
		for i := uint64(1); i < uint64(params.mapHeight)*2; i++ {
			storeNode(filterMapsRootIndex.child(treeIndex{lo: i}))
		}
		for levelBelow := range params.logEpochHistory + 1 {
			index := epochRootIndex.shiftRight(levelBelow)
			if index != epochRootIndex.addInt(1).shiftRight(levelBelow) {
				storeNode(index)
			}
		}
	}
	for _, levelBelow := range params.storeLogSubtrees {
		firstIndex := logEntriesRootIndex.shiftRight(levelBelow)
		nextIndex := logEntriesRootIndex.addInt(1).shiftRight(levelBelow)
		for index := firstIndex; index != nextIndex; index = index.addInt(1) {
			storeNode(index)
		}
	}
	for rowIndex := range params.mapHeight {
		mapRowRootIndex := params.mapRowRootIndex(mapIndex, rowIndex)
		rowLength := params.getRowDataFromTree(mv, mapRowRootIndex, 0, uint32(params.valuesPerMap)-ptr, mm.rowData[ptr:])
		ptr += rowLength
		mm.rowPtrs[rowIndex] = uint16(ptr)

		for i, levelBelow := range params.storeMapSubtrees {
			if rowLength < params.storeMapSubtreeMinLength[i] {
				continue
			}
			index := mapRowRootIndex.shiftRight(levelBelow)
			if index == mapRowRootIndex.addInt(1).shiftRight(levelBelow) {
				break
			}
			storeNode(index)
		}
		if rowLength == 0 {
			continue
		}
		// store progressive tree nodes
		var (
			leafCount    = (rowLength + 7) / 8
			plTreeRoot   = mapRowRootIndex.child(gtiProgListTree)
			subtreeLevel uint
		)
		for leafCount > 0 {
			if subtreeLevel >= params.storeProgListTreesFrom {
				storeNode(plTreeRoot)
			}
			subtreeHeight := params.progListHeightFirst + subtreeLevel*params.progListHeightStep
			plSubtreeRoot := plTreeRoot.child(gtiProgListSubtree)
			subtreeLeaves := min(leafCount, uint32(1)<<subtreeHeight)
			for levelBelow := params.storeProgListSubtreeFirst; levelBelow <= subtreeHeight; levelBelow += params.storeProgListSubtreeNext {
				for subIndex := range ((subtreeLeaves - 1) >> levelBelow) + 1 {
					storeNode(plSubtreeRoot.append(uint64(subIndex), subtreeHeight-levelBelow))
				}
			}
			leafCount -= subtreeLeaves
			plTreeRoot = plTreeRoot.child(gtiProgListNextTree)
			subtreeLevel++
		}
	}
	mm.rowData = mm.rowData[:ptr]
	return mm
}

func (params *Params) toStorageIndex(index treeIndex) treeIndex {
	var mainIndex, epochIndex, epochSubIndex,
		rowIndex, mapSubIndex, rowSubIndex,
		tempIndex, storageIndex treeIndex
	mainIndex, index = index.split(1)
	if mainIndex != gtiEpochs {
		panic("unexpected index")
	}
	epochIndex, index = index.split(params.logEpochHistory)
	if index == rootIndex {
		return epochIndex
	}
	shl := 127 - params.logEpochHistory - 1
	storageIndex = epochIndex.shiftLeft(shl)
	epochSubIndex, index = index.split(1)
	shl -= 2
	storageIndex = storageIndex.or(epochSubIndex.shiftLeft(shl))
	switch epochSubIndex {
	case gtiFilterMaps:
		rowIndex, index = index.split(params.logMapHeight)
		if index == rootIndex {
			return storageIndex.or(rowIndex)
		}
		shl -= params.logMapHeight + 1
		storageIndex = storageIndex.or(rowIndex.shiftLeft(shl))
		mapSubIndex, index = index.split(params.logMapsPerEpoch)
		if index == rootIndex {
			return storageIndex.or(mapSubIndex)
		}
		rowSubIndex = index
		tempIndex, index = index.split(1)
		if tempIndex == gtiProgListTree && index != rootIndex {
			tempIndex, index = index.split(1)
			for tempIndex == gtiProgListNextTree && index != rootIndex {
				tempIndex, index = index.split(1)
			}
			subtreeIndexBits := index.level()
			if subtreeIndexBits > params.progListHeightFirst+(params.maxRowListLevels-1)*params.progListHeightStep {
				panic("unexpected prog list subtree index")
			}
			rowSubIndex = rowSubIndex.shiftRight(subtreeIndexBits)
			mapSubIndex = mapSubIndex.shiftLeft(subtreeIndexBits).or(index.lowerBits(subtreeIndexBits))
		}
		if rowSubIndex.level() > params.maxRowListLevels+1 {
			panic("unexpected prog list index")
		}
		shl -= params.maxRowListLevels + 2
		return storageIndex.or(rowSubIndex.shiftLeft(shl)).or(mapSubIndex)
	case gtiLogEntries:
		return storageIndex.or(index)
	default:
		panic("unexpected index")
	}
}

func (f *FilterMaps) storeMemoryMaps(maps []*memoryMap) error {
	var (
		batch       = f.db.NewBatch()
		batchWrites int
	)
	// store map rows
	mapIndices := make([]uint32, len(maps))
	for i, mm := range maps {
		mapIndices[i] = mm.mapIndex
	}
	rows := make([]FilterRow, len(maps))
	for rowIndex := range f.mapHeight {
		for i, mm := range maps {
			rows[i] = mm.getRow(rowIndex)
		}
		if err := f.storeFilterMapRows(batch, mapIndices, rowIndex, rows); err != nil {
			return err
		}
		batchWrites++
		if batchWrites >= maxBatchSize {
			if err := batch.Write(); err != nil {
				return err
			}
			batch = f.db.NewBatch()
			batchWrites = 0
		}
	}
	// store tree nodes
	var nodeCount int
	for _, mm := range maps {
		nodeCount += len(mm.treeNodes)
	}
	sortIndex := make([]uint32, nodeCount)
	var ptr int
	for i, mm := range maps {
		for j := range mm.treeNodes {
			sortIndex[ptr] = uint32(i)<<24 + uint32(j)
			ptr++
		}
	}
	mask := uint32(1)<<24 - 1
	sort.Slice(sortIndex, func(i, j int) bool {
		si, sj := sortIndex[i], sortIndex[j]
		sti, stj := maps[si>>24].treeNodes[si&mask].storageIndex, maps[sj>>24].treeNodes[sj&mask].storageIndex
		return sti.lo < stj.lo || (sti.lo == stj.lo && sti.hi < stj.hi)
	})
	var (
		lastGroup        = make([]*[32]byte, f.treeNodeGroupLength)
		lastGroupIndex   treeIndex
		lastGroupUpdated uint32
	)
	storeLastGroup := func(mustCommit bool) error {
		if lastGroupUpdated == 0 {
			return nil
		}
		if lastGroupUpdated < f.treeNodeGroupLength {
			oldGroup, err := rawdb.ReadLogIndexTreeNodes(f.db, lastGroupIndex.hi, lastGroupIndex.lo, int(f.treeNodeGroupLength))
			if err != nil {
				return err
			}
			for i, node := range lastGroup {
				if node == nil {
					lastGroup[i] = oldGroup[i]
				}
			}
		}
		if err := rawdb.WriteLogIndexTreeNodes(batch, lastGroupIndex.hi, lastGroupIndex.lo, lastGroup); err != nil {
			return err
		}
		batchWrites++
		if mustCommit || batchWrites >= maxBatchSize {
			if err := batch.Write(); err != nil {
				return err
			}
			batch = f.db.NewBatch()
			batchWrites = 0
		}
		for i := range f.treeNodeGroupLength {
			lastGroup[i] = nil
		}
		return nil
	}

	for _, si := range sortIndex {
		node := &maps[si>>24].treeNodes[si&mask]
		groupIndex := node.storageIndex.shiftRight(f.logTreeNodeGroupLength)
		subIndex := node.storageIndex.lowerBits(f.logTreeNodeGroupLength).lo
		if lastGroupIndex != groupIndex {
			if err := storeLastGroup(false); err != nil {
				return err
			}
			lastGroupIndex = groupIndex
			lastGroupUpdated = 0
			if lastGroup[subIndex] != nil {
				panic("duplicate storage index in storeTreeNodes batch")
			}
			lastGroup[subIndex] = (*[32]byte)(&node.node)
		}
	}
	return storeLastGroup(true)
}

/*
*** index view
    - db: rendered map range, tail range, dirty range
        - rendered map-ekhez last block, illetve block ptr-ek, ennek megfelelo a block range is
    - memory map layer:
        - egy egyseges nezetet nyujt, transzparensen kezeli a db update-eket
        - csak komplett immutable memoryMap-ekkel operal, lehet hozzaadni, rollback-elni
        - egy folytonos szakaszt kezel (tail-re kulon kell)
        - search backend-et biztosit (finalizalt node-ok is elerhetok, de ezzel proof-ot generalni nem lehet)
    - memory tree layer:
        - memory map layer + memoryTree
        - tobb head block-ra is search + proofgen backend-et biztosit
    - indexer:
        - 2x memory tree layer (head, tail) + chain view
        - set target view, log index history, automatikusan konvergal mindig a cel fele
        - tobb head block-ra is search + proofgen backend-et biztosit
            - idovel egyes view-k elavulhatnak, ekkor az olvasas hibat eredmenyez
            ? indexeles kozben lokalis kereseshez inkabb egesz blokkos (memory map layer) view-t hasznaljunk?



* memoryMapLayer:
    - epoch window range, rendered map range, block range
    - inicializalas epoch hataron (window mindig epoch hatarokon van)
    - egy folytonos overlay map range-t rak a disk layer fole, lehet atfedes, de a memory range a vege utan mindent kitakar
    - get map rows, tree nodes, last block, block lvptr
    - add memoryMap
    - revert
    - sajat disk update goroutine
        - torles az elso; amig van a disken dirty map, vagy olyan, amit a map overlay felulir/kitakar, addig torlunk
        - majd sorban, nagyobb csoportokban kiirjuk a memory map-eket, ha a disk layer-ben update-eltuk a rendered range-et, a memory map-et eldobjuk
            - amig irjuk, a disken dirty-nek szamit
            - iras kozben is revertalhatok a map-ek, ekkor marad dirty a disken, megkezdodik a torles
        - iras megkezdese elott osszevarunk egy nagyobb memory map csoportot kerek map index hatarig vagy az mml ablak hataraig
            - head eleresekor indexer kuldhet fel vmi jelet, hogy ne varjunk tovabb, irjuk ki, ami van
            - tul sok memory map osszegyulese eseten viszont add map blokkol, amig ki nem irodik egy resze
    - merge tail - egy befejezett masik memoryMapLayer-t (ami lenyegeben mar csak transzparens disk layer ablak) hozzafuz a sajat tail-jehez

*/
