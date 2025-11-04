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

func (t *treeIndex) matchRoot(relIndex uint64) bool {
	levels := 63 - bits.LeadingZeros64(relIndex)
	if levels > t.level() || ((*t)[1]>>(64-levels))+(uint64(1)<<levels) != relIndex {
		return false
	}
	*t = t.shiftLeft(levels)
	return true

}

func (t *treeIndex) splitRoot(levels uint) common.Range[uint64] {
	tl := t.level()
	if tl >= levels {
		subIndex := (*t)[1] >> (64 - levels)
		*t = t.shiftLeft(levels)
		return common.NewRange[uint64](subIndex, 1)
	}
	subIndex := (*t)[1] >> (64 - tl)
	*t = rootIndex
	return common.NewRange[uint64](subIndex<<(levels-tl), uint64(1)<<(levels-tl))
}

func (p *Params) finalizedInMap(index treeIndex) uint32 {
	if !index.matchRoot(rtiEpochs) {
		return math.MaxUint32
	}
	epoch := index.splitRoot(p.logEpochHistory).Last()
	var mapSubIndex uint32
	switch {
	case index.matchRoot(rtiFilterMaps):
		index.splitRoot(p.logMapHeight)
		mapSubIndex = index.splitRoot(p.logMapsPerEpoch).Last()
	case index.matchRoot(rtiLogEntries):
		mapSubIndex = index.splitRoot(p.logMapsPerEpoch).Last()
	default:
		mapSubIndex = p.mapsPerEpoch - 1
	}
	return epoch*p.mapsPerEpoch + mapSubIndex
}
