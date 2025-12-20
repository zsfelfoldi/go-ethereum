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
	rtiNextIndex = 3
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
	height := p.progListHeightFirst
	index := ti64(rtiListTree)
	for {
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

func (e *emptySubtree) getNode(index treeIndex) merkle.Value {
	for index != rootIndex {
		if e == nil {
			panic("unknown empty subtree node")
		}
		switch {
		case index.matchRoot(2):
			e = e.left
		case index.matchRoot(3):
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
	p.treeRoot = mtNode{node: rootPtr, empty: emptyIndexTree, index: rootIndex}
}

func (p *Params) subtreeMapRange(index treeIndex) common.Range[uint32] {
	if !index.matchRoot(rtiEpochs) {
		return common.NewRange[uint32](0, math.MaxUint32)
	}
	epochRange := index.splitRoot(p.logEpochHistory)
	if epochRange.Count() > 1 {
		return common.NewRange[uint32](uint32(epochRange.First())*p.mapsPerEpoch, uint32(epochRange.Count())*p.mapsPerEpoch)
	}
	epoch := uint32(epochRange.First())
	switch {
	case index.matchRoot(rtiFilterMaps):
		index.splitRoot(p.logMapHeight)
		mapSubRange := index.splitRoot(p.logMapsPerEpoch)
		return common.NewRange[uint32](epoch*p.mapsPerEpoch+uint32(mapSubRange.First()), uint32(mapSubRange.Count()))
	case index.matchRoot(rtiIndexEntries):
		mapSubRange := index.splitRoot(p.logMapsPerEpoch)
		return common.NewRange[uint32](epoch*p.mapsPerEpoch+uint32(mapSubRange.First()), uint32(mapSubRange.Count()))
	default:
		return common.NewRange[uint32](epoch*p.mapsPerEpoch, p.mapsPerEpoch)
	}
}

func (p *Params) splitMapRowIndex(index treeIndex) (mapIndex, rowIndex uint32, subIndex treeIndex) {
	if !index.matchRoot(rtiEpochs) {
		return
	}
	epochRange := index.splitRoot(p.logEpochHistory)
	if epochRange.Count() > 1 {
		return
	}
	epoch := uint32(epochRange.First())
	if !index.matchRoot(rtiFilterMaps) {
		return
	}
	rowRange := index.splitRoot(p.logMapHeight)
	if rowRange.Count() > 1 {
		return
	}
	rowIndex = uint32(rowRange.First())
	mapSubRange := index.splitRoot(p.logMapsPerEpoch)
	if mapSubRange.Count() > 1 {
		return
	}
	mapIndex = epoch*p.mapsPerEpoch + uint32(mapSubRange.First())
	return mapIndex, rowIndex, index
}

type filterRowReader interface {
	getFilterMapRow(mapIndex, rowIndex uint32) (FilterRow, error)
}

type mapRowIndex struct{ mapIndex, rowIndex uint32 }

type filterRowNodeReader struct {
	params *Params
	reader filterRowReader
	cache  *lru.Cache[mapRowIndex, FilterRow]
}

func (r *filterRowNodeReader) getNode(index treeIndex) (nodeWithWeight, int, error) {
	mapIndex, rowIndex, subIndex := r.params.splitMapRowIndex(index)
	if (subIndex == treeIndex{}) {
		return nodeWithWeight{}, mtaInternal, nil
	}
	row, err := r.reader.getFilterMapRow(mapIndex, rowIndex)
	if err != nil {
		return nodeWithWeight{}, 0, err
	}
	switch {
	case subIndex.matchRoot(rtiListTree):
		return r.params.getProgListNode(row, 0, subIndex)
	case subIndex.matchRoot(rtiListCount):
		if subIndex != rootIndex {
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

func (p *Params) getProgListNode(row FilterRow, level uint, index treeIndex) (nodeWithWeight, int, error) {
	height := p.progListHeightFirst + level*p.progListHeightStep
	slen := 1 << height
	switch {
	case index.matchRoot(rtiProgListSubtree):
		chunkRange := index.splitRoot(height)
		if chunkRange.Count() > 1 {
			return nodeWithWeight{}, mtaInternal, nil
		}
		if index != rootIndex {
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
	case index.matchRoot(rtiProgListNextTree):
		if len(row) > slen {
			return p.getProgListNode(row[slen:], level+1, index)
		}
		if index != rootIndex {
			return nodeWithWeight{}, mtaUnknown, nil
		}
		return nodeWithWeight{}, mtaKnown, nil
	default:
		panic("invalid tree index")
	}
}
