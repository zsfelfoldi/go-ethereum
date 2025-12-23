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
	"math"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
)

const (
	// relative to root
	rtiEpochs    = 2
	rtiNextEntry = 3
	// relative to epoch root
	rtiFilterMaps   = 2
	rtiIndexEntries = 3
	// relative to list root (progressive or legacy)
	rtiListTree  = 2
	rtiListCount = 3
	// relative to progressive list tree root
	rtiProgListSubtree  = 2
	rtiProgListNextTree = 3
	// relative to index entry root
	rtiLogEntry  = 2
	rtiEntryMeta = 3
	// relative to log entry root
	rtiLogAddress = 4
	rtiLogTopics  = 5 // list[4]
	rtiLogData    = 6 // prog list
	// relative to entry meta root
	// log entry meta
	rtiLogMetaBlockNumber = 4
	rtiLogMetaTxHash      = 5
	rtiLogMetaTxIndex     = 6
	rtiLogMetaLogIndex    = 7
	// transaction entry meta
	rtiTxMetaBlockNumber = 4
	rtiTxMetaTxHash      = 5
	rtiTxMetaTxIndex     = 6
	rtiTxMetaReceiptHash = 7
	// block entry meta
	rtiBlockMetaBlockNumber = 4
	rtiBlockMetaBlockHash   = 5
	rtiBlockMetaTimestamp   = 6
)

func (p *Params) mapRowRoot(mapIndex, rowIndex uint32) treeIndex {
	epochRoot := ti64(rtiEpochs).arraySub(uint64(mapIndex/p.mapsPerEpoch), p.logEpochHistory)
	rowRoot := epochRoot.gtSub(rtiFilterMaps).arraySub(uint64(rowIndex), p.logMapHeight)
	return rowRoot.arraySub(uint64(mapIndex%p.mapsPerEpoch), p.logMapsPerEpoch)
}

func (p *Params) indexEnrtyRoot(lvIndex uint64) treeIndex {
	epochRoot := ti64(rtiEpochs).arraySub(lvIndex/(uint64(p.mapsPerEpoch)*p.valuesPerMap), p.logEpochHistory)
	return epochRoot.gtSub(rtiIndexEntries).arraySub(lvIndex%(uint64(p.mapsPerEpoch)*p.valuesPerMap), p.logMapsPerEpoch+p.logValuesPerMap)
}

// relative to progressive list root
func (p *Params) progListSubIndex(leafIndex uint32) treeIndex {
	//fmt.Println("progListSubIndex", leafIndex)
	height := p.progListHeightFirst
	index := ti64(rtiListTree)
	for {
		//fmt.Println(" height", height, "leafIndex", leafIndex, "index", index)
		stLength := uint32(1) << height
		if leafIndex < stLength {
			return index.gtSub(rtiProgListSubtree).arraySub(uint64(leafIndex), height)
		}
		leafIndex -= stLength
		height += p.progListHeightStep
		index = index.gtSub(rtiProgListNextTree)
	}
}

type emptySubtree struct {
	value               merkle.Value
	parent, left, right *emptySubtree
}

var zeroLeaf = &emptySubtree{}

func (e *emptySubtree) getNode(gti treeIndex) merkle.Value {
	for gti != gtiRoot {
		if e == nil {
			panic("unknown empty subtree node")
		}
		switch {
		case gti.matchRoot(2):
			e = e.left
		case gti.matchRoot(3):
			e = e.right
		default:
			panic("invalid tree index")
		}
	}
	return e.value
}

func emptyTreeNode(left, right *emptySubtree) *emptySubtree {
	e := &emptySubtree{
		value: treeHash(left.value, right.value),
		left:  left,
		right: right,
	}
	left.parent = e
	right.parent = e
	return e
}

func emptyVector(height uint, leaves *emptySubtree) *emptySubtree {
	if height == 0 {
		return leaves
	}
	s := emptyVector(height-1, leaves)
	return emptyTreeNode(s, s)
}

func (e *emptySubtree) zeroDefault() *emptySubtree {
	e.value = merkle.Value{}
	return e
}

const maxProgListTreeLevel = 16

func (p *Params) emptyProgListTree(level uint) *emptySubtree {
	if level > maxProgListTreeLevel {
		return zeroLeaf
	}
	treeLevel := emptyVector(p.progListHeightFirst+level*p.progListHeightStep, zeroLeaf)
	return emptyTreeNode(treeLevel, p.emptyProgListTree(level+1)).zeroDefault()
}

func (p *Params) initEmptyTree() {
	progList := emptyTreeNode(p.emptyProgListTree(0), zeroLeaf).zeroDefault()
	filterMapsTree := emptyVector(p.logMapHeight+p.logMapsPerEpoch, progList)
	topicsList := emptyTreeNode(emptyVector(2, zeroLeaf), zeroLeaf)
	logEntry := emptyTreeNode(emptyTreeNode(zeroLeaf, topicsList), emptyTreeNode(progList, zeroLeaf)).zeroDefault()
	entryMeta := emptyVector(2, zeroLeaf)
	indexEntry := emptyTreeNode(logEntry, entryMeta)
	indexEntriesTree := emptyVector(p.logMapsPerEpoch+p.logValuesPerMap, indexEntry)
	epochTree := emptyTreeNode(filterMapsTree, indexEntriesTree)
	epochHistoryTree := emptyVector(p.logEpochHistory, epochTree)
	emptyIndexTree := emptyTreeNode(epochHistoryTree, zeroLeaf)
	p.treeRoot = mtNode{index: rootIndex, empty: emptyIndexTree, gti: gtiRoot}
}

func (p *Params) subtreeLvRange(gti treeIndex) common.Range[uint64] {
	if !gti.matchRoot(rtiEpochs) {
		return common.NewRange[uint64](0, math.MaxUint64)
	}
	epochRange := gti.splitRoot(p.logEpochHistory)
	if epochRange.Count() > 1 {
		return common.NewRange[uint64](epochRange.First()*uint64(p.mapsPerEpoch)*p.valuesPerMap, epochRange.Count()*uint64(p.mapsPerEpoch)*p.valuesPerMap)
	}
	epoch := epochRange.First()
	switch {
	case gti.matchRoot(rtiFilterMaps):
		gti.splitRoot(p.logMapHeight)
		mapSubRange := gti.splitRoot(p.logMapsPerEpoch)
		return common.NewRange[uint64]((epoch*uint64(p.mapsPerEpoch)+mapSubRange.First())*p.valuesPerMap, mapSubRange.Count()*p.valuesPerMap)
	case gti.matchRoot(rtiIndexEntries):
		valueSubRange := gti.splitRoot(p.logMapsPerEpoch + p.logValuesPerMap)
		return common.NewRange[uint64](epoch*uint64(p.mapsPerEpoch)*p.valuesPerMap+valueSubRange.First(), valueSubRange.Count())
	default:
		return common.NewRange[uint64](epoch*uint64(p.mapsPerEpoch)*p.valuesPerMap, uint64(p.mapsPerEpoch)*p.valuesPerMap)
	}
}

type filterRowReader func(mapIndex, rowIndex uint32) (FilterRow, error)

type mapRowIndex struct{ mapIndex, rowIndex uint32 }

type filterRowNodeReader struct {
	params          *Params
	getFilterMapRow filterRowReader
	cache           *lru.Cache[mapRowIndex, FilterRow]
}

func (p *Params) newFilterRowNodeReader(reader filterRowReader) *filterRowNodeReader {
	return &filterRowNodeReader{
		params:          p,
		getFilterMapRow: reader,
		cache:           lru.NewCache[mapRowIndex, FilterRow](1000),
	}
}

func (r *filterRowNodeReader) getNode(gti treeIndex) (nodeWithWeight, int, error) {
	if gti == gtiRoot {
		return nodeWithWeight{}, mtaInternal, nil
	}
	if !gti.matchRoot(rtiEpochs) {
		return nodeWithWeight{}, mtaUnknown, nil
	}
	epochRange := gti.splitRoot(r.params.logEpochHistory)
	if epochRange.Count() > 1 {
		return nodeWithWeight{}, mtaInternal, nil
	}
	epoch := uint32(epochRange.First())
	if !gti.matchRoot(rtiFilterMaps) {
		return nodeWithWeight{}, mtaUnknown, nil
	}
	rowRange := gti.splitRoot(r.params.logMapHeight)
	if rowRange.Count() > 1 {
		return nodeWithWeight{}, mtaInternal, nil
	}
	rowIndex := uint32(rowRange.First())
	mapSubRange := gti.splitRoot(r.params.logMapsPerEpoch)
	if mapSubRange.Count() > 1 {
		return nodeWithWeight{}, mtaInternal, nil
	}
	mapIndex := epoch*r.params.mapsPerEpoch + uint32(mapSubRange.First())
	row, err := r.getFilterMapRow(mapIndex, rowIndex)
	if err != nil {
		return nodeWithWeight{}, 0, err
	}
	switch {
	case gti == gtiRoot:
		return nodeWithWeight{}, mtaInternal, nil
	case gti.matchRoot(rtiListTree):
		return r.params.getProgListNode(row, 0, gti)
	case gti.matchRoot(rtiListCount):
		if gti != gtiRoot {
			return nodeWithWeight{}, mtaUnknown, nil
		}
		countBytes := uint32(1)
		if len(row) >= 256 {
			countBytes = 2
		}
		return nodeWithWeight{value: uint64ToValue(uint64(len(row))), weight: r.params.filterRowNodeWeight(countBytes)}, mtaKnown, nil
	default:
		panic("invalid tree index")
	}
}

func (p *Params) getProgListNode(row FilterRow, level uint, gti treeIndex) (nodeWithWeight, int, error) {
	subtreeHeight := p.progListHeightFirst + level*p.progListHeightStep
	subtreeLen := 8 << subtreeHeight
	switch {
	case gti == gtiRoot:
		return nodeWithWeight{}, mtaInternal, nil
	case gti.matchRoot(rtiProgListSubtree):
		chunkRange := gti.splitRoot(subtreeHeight)
		if chunkRange.Count() > 1 {
			return nodeWithWeight{}, mtaInternal, nil
		}
		if gti != gtiRoot {
			return nodeWithWeight{}, mtaUnknown, nil
		}
		chunk := int(chunkRange.First())
		first, afterLast := chunk*8, min(chunk*8+8, len(row))
		if first >= afterLast {
			return nodeWithWeight{}, mtaKnown, nil
		}
		count := afterLast - first
		nw := nodeWithWeight{weight: p.filterRowNodeWeight(uint32(count) * uint32(p.logMapWidth+7/8))}
		for i := range count {
			binary.LittleEndian.PutUint32(nw.value[i*4:i*4+4], row[first+i])
		}
		return nw, mtaKnown, nil
	case gti.matchRoot(rtiProgListNextTree):
		if len(row) > subtreeLen {
			return p.getProgListNode(row[subtreeLen:], level+1, gti)
		}
		if gti != gtiRoot {
			return nodeWithWeight{}, mtaUnknown, nil
		}
		return nodeWithWeight{}, mtaKnown, nil
	default:
		panic("invalid tree index")
	}
}

type logIndexBoundaryReader struct {
	params    *Params
	nextEntry uint64
}

func (p *Params) newLogIndexBoundaryReader(nextEntry uint64) logIndexBoundaryReader {
	return logIndexBoundaryReader{
		params:    p,
		nextEntry: nextEntry,
	}
}

func (l logIndexBoundaryReader) getNode(gti treeIndex) (nw nodeWithWeight, avail int, err error) {
	if gti.matchRoot(rtiNextEntry) {
		if gti == gtiRoot {
			return nodeWithWeight{value: uint64ToValue(l.nextEntry), weight: 1}, mtaKnown, nil
		}
		return
	}
	entryRange := l.params.subtreeLvRange(gti)
	if l.nextEntry <= entryRange.First() {
		return nodeWithWeight{value: l.params.treeRoot.empty.getNode(gti)}, mtaKnown, nil
	}
	return
}

func (l logIndexBoundaryReader) nodeStatus(gti treeIndex) int {
	entryRange := l.params.subtreeLvRange(gti)
	switch {
	case l.nextEntry >= entryRange.AfterLast():
		return mtsComplete
	case l.nextEntry <= entryRange.First():
		return mtsEmpty
	default:
		return mtsPartial
	}
}
