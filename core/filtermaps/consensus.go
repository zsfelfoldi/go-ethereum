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

func (p *Params) finalizedInMap(index treeIndex) uint32 {
	if !index.matchRoot(rtiEpochs) {
		return math.MaxUint32
	}
	epoch := uint32(index.splitRoot(p.logEpochHistory).Last())
	var mapSubIndex uint32
	switch {
	case index.matchRoot(rtiFilterMaps):
		index.splitRoot(p.logMapHeight)
		mapSubIndex = uint32(index.splitRoot(p.logMapsPerEpoch).Last())
	case index.matchRoot(rtiLogEntries):
		mapSubIndex = uint32(index.splitRoot(p.logMapsPerEpoch).Last())
	default:
		mapSubIndex = p.mapsPerEpoch - 1
	}
	return epoch*p.mapsPerEpoch + mapSubIndex
}

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
