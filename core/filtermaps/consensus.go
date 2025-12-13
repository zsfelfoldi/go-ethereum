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
	"math"

	"github.com/ethereum/go-ethereum/beacon/merkle"
)

const (
	// relative to root
	rtiEpochs    = 2
	rtiNextIndex = 3
	// relative to epoch root
	rtiFilterMaps = 2
	rtiLogEntries = 3
	// relative to progressive list root
	rtiProgListTree  = 2
	rtiProgListCount = 3
	// relative to progressive list tree root
	rtiProgListSubtree  = 2
	rtiProgListNextTree = 3

	// log
	rtiLogAddress      = 8
	rtiLogTopicsRoot   = 18
	rtiLogTopicsLength = 19
	rtiLogData         = 10 // prog list
	rtiLogZero         = 11
	// log meta
	rtiLogMetaBlockNumber = 12
	rtiLogMetaTxHash      = 13
	rtiLogMetaTxIndex     = 14
	rtiLogMetaLogIndex    = 15
	// block delimiter meta
	rtiDelimiterZero            = 2
	rtiDelimiterMetaBlockNumber = 12
	rtiDelimiterMetaBlockHash   = 13
	rtiDelimiterMetaTimestamp   = 14
	rtiDelimiterMetaDummy       = 15
)

func (p *Params) mapRowRoot(mapIndex, rowIndex uint32) treeIndex {
	epochRoot := ti64(rtiEpochs).arraySub(uint64(mapIndex/p.mapsPerEpoch), p.logEpochHistory)
	rowRoot := epochRoot.gtSub(rtiFilterMaps).arraySub(uint64(rowIndex), p.logMapHeight)
	return rowRoot.arraySub(uint64(mapIndex%p.mapsPerEpoch), p.logMapsPerEpoch)
}

func (p *Params) logEnrtyRoot(lvIndex uint64) treeIndex {
	epochRoot := ti64(rtiEpochs).arraySub(lvIndex/(uint64(p.mapsPerEpoch)*p.valuesPerMap), p.logEpochHistory)
	return epochRoot.gtSub(rtiLogEntries).arraySub(lvIndex%(uint64(p.mapsPerEpoch)*p.valuesPerMap), p.logMapsPerEpoch+p.logValuesPerMap)
}

// relative to progressive list root
func (p *Params) progListSubIndex(leafIndex uint32) treeIndex {
	height := p.progListHeightFirst
	index := ti64(rtiProgListTree)
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
	node        merkle.Value
	left, right *emptySubtree
}

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
}

func (p *Params) initEmptyTree() {
	emptyVector := make([]emptySubtree, maxVectorHeight)
	for i := 1; i < maxVectorHeight; i++ {
		emptyVector[i] = emptySubtree{
			node:  treeHash(emptyVector[i].node, emptyVector[i].node),
			left:  &emptyVector[i-1],
			right: &emptyVector[i-1],
		}
	}

	e := &emptyTree{
		children: make(map[merkle.Value]emptyTreeChildren),
	}
	progListRoot := e.addMapping(merkle.Value{}, merkle.Value{})
	filterMapsRoot := progListRoot
	for range p.logMapHeight + p.logMapsPerEpoch {
		filterMapsRoot = e.addMapping(filterMapsRoot, filterMapsRoot)
	}
	logEntriesRoot := merkle.Value{}
	for range p.logValuesPerMap + p.logMapsPerEpoch {
		logEntriesRoot = e.addMapping(logEntriesRoot, logEntriesRoot)
	}
	epochRoot := e.addMapping(filterMapsRoot, logEntriesRoot)
	epochTreeRoot := epochRoot
	for range p.logEpochHistory {
		epochTreeRoot = e.addMapping(epochTreeRoot, epochTreeRoot)
	}
	e.root = e.addMapping(epochTreeRoot, merkle.Value{})
	return e
}

func (p *Params) subtreeMapRange(index treeIndex) common.Range[uint32] {
	if !index.matchRoot(rtiEpochs) {
		return math.MaxUint32
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
	case index.matchRoot(rtiLogEntries):
		mapSubRange := index.splitRoot(p.logMapsPerEpoch)
		return common.NewRange[uint32](epoch*p.mapsPerEpoch+uint32(mapSubRange.First()), uint32(mapSubRange.Count()))
	default:
		return common.NewRange[uint32](epoch*p.mapsPerEpoch, p.mapsPerEpoch)
	}
}
