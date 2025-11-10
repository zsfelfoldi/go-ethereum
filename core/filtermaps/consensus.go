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
	"math/bits"

	"github.com/ethereum/go-ethereum/common"
	"lukechampine.com/uint128"
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

type treeIndex uint128.Uint128

var rootIndex = treeIndex(uint128.From64(1))

func (t *treeIndex) matchRoot(relIndex uint64) bool {
	rLevel := uint(63 - bits.LeadingZeros64(relIndex))
	tLevel := t.level()
	if rLevel > tLevel || t.rsh(tLevel-rLevel) != ti64(relIndex) {
		return false
	}
	*t = t.rsh(rLevel)
	return true

}

func (t *treeIndex) splitRoot(levels uint) common.Range[uint64] {
	tl := t.level()
	if tl >= levels {
		subIndex := t.rsh(tl-levels).Lo - (uint64(1) << levels)
		m := rootIndex.lsh(tl - levels)
		*t = treeIndex(uint128.Uint128(*t).And(uint128.Uint128(m).Sub(uint128.Uint128(rootIndex))).Add(uint128.Uint128(m)))
		return common.NewRange[uint64](subIndex, 1)
	}
	subIndex := uint128.Uint128(*t).And64((uint64(1) << tl) - 1).Lo
	*t = rootIndex
	return common.NewRange[uint64](subIndex<<(levels-tl), uint64(1)<<(levels-tl))
}

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

func (t treeIndex) level() uint {
	return 127 - uint(uint128.Uint128(t).LeadingZeros())
}

func (t treeIndex) gtSub(subIndex uint64) treeIndex {
	shift := uint(63 - bits.LeadingZeros64(subIndex))
	return treeIndex(uint128.Uint128(t.lsh(shift)).Add64(subIndex - (uint64(1) << shift)))
}

func (t treeIndex) arraySub(arrayIndex uint64, indexLen uint) treeIndex {
	return treeIndex(uint128.Uint128(t.lsh(indexLen)).Add64(arrayIndex))
}

func ti64(i uint64) treeIndex                { return treeIndex(uint128.From64(i)) }
func (t treeIndex) lsh(shift uint) treeIndex { return treeIndex(uint128.Uint128(t).Lsh(shift)) }
func (t treeIndex) rsh(shift uint) treeIndex { return treeIndex(uint128.Uint128(t).Rsh(shift)) }

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
func (p *Params) progListSubIndex(leafIndex uint64) treeIndex {
	height := p.progListHeightFirst
	index := ti64(rtiProgListTree)
	for {
		stLength := uint64(1) << height
		if leafIndex < stLength {
			return index.gtSub(rtiProgListSubtree).arraySub(leafIndex, height)
		}
		leafIndex -= stLength
		height += p.progListHeightStep
		index = index.gtSub(rtiProgListNextTree)
	}
}
