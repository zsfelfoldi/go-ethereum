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
	"errors"
	"hash/fnv"
	"math"
	"math/bits"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

var (
	errNotIndexed = errors.New("head is not indexed")
	errNotLeaf    = errors.New("not a leaf")
)

type treeRange interface {
	merkle.TreeHashable
	logValueRange() (first, afterLast uint64)
}

func (fm *FilterMaps) emptySubtree(tree treeRange) (bool, error) {
	if !fm.indexedRange.headIndexed {
		return false, errNotIndexed
	}
	first, _ := tree.logValueRange()
	return first >= fm.indexedRange.headDelimiter
}

// implements treeRange
type epochTreeRange struct {
	params    *Params
	fm        *FilterMaps
	treeIndex uint64
}

func (r epochTreeRange) Subtree(subIndex uint64) merkle.TreeShape {
	if subIndex == 1 {
		return r
	}
	newIndex := merkle.ChildIndex(r.treeIndex, subIndex)
	if newIndex < r.epochHistory*2 {
		return epochTreeRange{Params: r.Params, treeIndex: newIndex}
	}
	epoch, epochSub := merkle.SplitIndex(newIndex, r.logEpochHistory)
	treeType, treeSub := merkle.SplitIndex(epochSub, 1)
	switch treeType {
	case 2:
		return mapTreeRange{Params: r.Params, epoch: epoch, treeIndex: 1}.Subtree(treeSub)
	case 3:
		return logTreeRange{Params: r.Params, epoch: epoch, treeIndex: 1}.Subtree(treeSub)
	default:
		panic("invalid tree type")
	}
}

func (r epochTreeRange) logValueRange() (first, afterLast uint64) {
	lz := bits.LeadingZeros64(r.treeIndex)
	firstEpoch := r.treeIndex << (lz + 1) >> (64 - r.logEpochHistory)
	afterLastEpoch := (r.treeIndex + 1) << (lz + 1) >> (64 - r.logEpochHistory)
	return firstEpoch << (r.logMapsPerEpoch + r.logValuesPerMap), afterLastEpoch << (r.logMapsPerEpoch + r.logValuesPerMap)
}

func (r epochTreeRange) IsLeaf() bool { return false }

func (r epochTreeRange) IsSymmetrical() bool { return r.index < r.epochHistory }

func (r epochTreeRange) IsEmpty() (bool, error) { return r.fm.emptySubtree(r) }

func (r epochTreeRange) GetLeaf() (merkle.Value, error) { return merkle.Value{}, errNotLeaf }

// implements treeRange
type mapTreeRange struct {
	params    *Params
	data      *FilterMaps
	epoch     uint32
	treeIndex uint64
}

func (r mapTreeRange) Subtree(subIndex uint64) merkle.TreeShape {
	if subIndex == 1 {
		return r
	}
	newIndex := merkle.ChildIndex(r.treeIndex, subIndex)
	if newIndex < r.mapsPerEpoch*r.mapHeight {
		return mapTreeRange{Params: r.Params, epoch: r.epoch, treeIndex: newIndex}
	}
	rowIndex, rowSub := merkle.SplitIndex(newIndex, r.logMapHeight)
	mapSubIndex, treeSub := merkle.SplitIndex(rowSub, r.logMapsPerEpoch)
	mapIndex := r.epoch<<r.logMapsPerEpoch + mapSubIndex
	return mapRowRange{Params: r.Params, mapIndex: mapIndex, rowIndex: rowIndex, treeIndex: 1}.Subtree(treeSub)
}

func (r mapTreeRange) logValueRange() (first, afterLast uint64) {
	shift := bits.LeadingZeros64(t.treeIndex) + r.logMapsPerEpoch + r.logMapHeight - 63
	if shift >= r.logMapHeight { // we are at row split level, entire epoch is covered
		return uint64(r.epoch) << (r.logMapsPerEpoch + r.logValuesPerMap), uint64(r.epoch+1) << (r.logMapsPerEpoch + r.logValuesPerMap)
	}
	mapSubIndex := (r.treeIndex << shift) & (r.mapsPerEpoch - 1)
	first = (uint64(r.epoch)<<r.logMapsPerEpoch + mapSubIndex) << r.logValuesPerMap
	return first, first + uint64(1)<<(shift+r.logValuesPerMap)
}

func (r mapTreeRange) IsLeaf() bool { return false }

func (r mapTreeRange) IsSymmetrical() bool { return true }

func (r mapTreeRange) IsEmpty() (bool, error) { return r.fm.emptySubtree(r) }

func (r mapTreeRange) GetLeaf() (merkle.Value, error) { return merkle.Value{}, errNotLeaf }

// implements treeRange
type logTreeRange struct {
	params    *Params
	data      *FilterMaps
	epoch     uint32
	treeIndex uint64
}

func (r logTreeRange) Subtree(subIndex uint64) merkle.TreeShape {
	if subIndex == 1 {
		return r
	}
	newIndex := merkle.ChildIndex(r.treeIndex, subIndex)
	if newIndex < r.mapsPerEpoch*r.valuesPerMap {
		return logTreeRange{Params: r.Params, epoch: r.epoch, treeIndex: newIndex}
	}
	logSubIndex, treeSub := merkle.SplitIndex(newIndex, r.logMapHeight)
	logValueIndex := r.epoch<<(r.logMapsPerEpoch+r.logValuesPerMap) + logSubIndex
	return logRange{Params: r.Params, logValueIndex: logValueIndex, treeIndex: 1}.Subtree(treeSub)
}

func (r logTreeRange) logValueRange() (first, afterLast uint64) {
	shift := bits.LeadingZeros64(t.treeIndex) + r.logMapsPerEpoch + r.logValuesPerMap - 63
	logSubIndex := (r.treeIndex << shift) - r.mapsPerEpoch*r.valuesPerMap
	first = uint64(r.epoch)<<(r.logMapsPerEpoch+r.logValuesPerMap) + logSubIndex
	return first, first + uint64(1)<<shift
}

func (r logTreeRange) IsLeaf() bool { return false }

func (r logTreeRange) IsSymmetrical() bool { return true }

func (r logTreeRange) IsEmpty() (bool, error) { return r.fm.emptySubtree(r) }

func (r logTreeRange) GetLeaf() (merkle.Value, error) { return merkle.Value{}, errNotLeaf }

// implements treeRange
type mapRowRange struct {
	*Params
	progressiveTree
	mapIndex, rowIndex uint32
	rowDataTree        bool
	rowDataLevel       int
	treeIndex          uint64
}

func (r mapRowRange) logValueRange() (first, afterLast uint64) {
	return uint64(r.mapIndex) << r.logValuesPerMap, uint64(r.mapIndex+1) << r.logValuesPerMap
}

const (
	// log
	gmiLogAddress      = 8
	gmiLogTopicsLength = 19
	gmiLogTopics1      = 72
	gmiLogTopics2      = 73
	gmiLogTopics3      = 74
	gmiLogTopics4      = 75
	gmiLogDataLength   = 21
	gmiLogDataRoot     = 20
	logGmiLogDataRoot  = 4
	// log meta
	gmiLogMetaBlockNumber = 12
	gmiLogMetaTxHash      = 13
	gmiLogMetaTxIndex     = 14
	gmiLogMetaLogIndex    = 15
	// block delimiter meta
	gmiDelimiterMetaBlockNumber = 12
	gmiDelimiterMetaBlockHash   = 13
	gmiDelimiterMetaTimestamp   = 14
	gmiDelimiterMetaDummy       = 15
)

var logTreeLeafs = fillInternalNodes(map[uint64]bool{
	gmiLogAddress:         true,
	gmiLogTopicsLength:    true,
	gmiLogTopics1:         true,
	gmiLogTopics2:         true,
	gmiLogTopics3:         true,
	gmiLogTopics4:         true,
	gmiLogDataLength:      true,
	gmiLogDataRoot:        false,
	gmiLogMetaBlockNumber: true,
	gmiLogMetaTxHash:      true,
	gmiLogMetaTxIndex:     true,
	gmiLogMetaLogIndex:    true,
}, 1)

func fillInternalNodes(fields map[uint64]bool, treeIndex uint64) map[uint64]bool {
	if _, ok := fields[treeIndex]; !ok {
		fillInternalNodes(fields, treeIndex*2)
		fillInternalNodes(fields, treeIndex*2+1)
		fields[treeIndex] = false
	}
	return fields
}

type logRange struct {
	*Params
	logValueIndex, treeIndex uint64
}

func (r logRange) Subtree(subIndex uint64) merkle.TreeShape {
	if subIndex == 1 {
		return r
	}
	newIndex := merkle.ChildIndex(r.treeIndex, subIndex)
	if _, ok := logFieldIndices[newIndex]; ok {
		return logRange{Params: r.Params, logValueIndex: r.logValueIndex, index: newIndex}
	}
	if field, logDataSub := merkle.SplitIndex(newIndex, logGmiLogDataRoot); field == lfLogData && logDataSub > 0 {
		return logDataRange{Params: r.Params, logValueIndex: r.logValueIndex, treeIndex: 1}.Subtree(logDataSub)
	}
	return nil
}

func (r logRange) logValueRange() (first, afterLast uint64) {
	return r.logValueIndex, r.logValueIndex + 1
}

func (r logRange) IsLeaf() bool { return logTreeLeafs[r.treeIndex] }

func (r logRange) IsSymmetrical() bool { return false }

func (r logRange) IsEmpty() (bool, error) {
	log, header, err := r.source.getLogOrDelimiter(r.logValueIndex)
	if err != nil {
		return false, err
	}
	return log == nil && header == nil, nil
}

func (r logRange) GetLeaf() (merkle.Value, error) {
	log, header, err := r.source.getLogOrDelimiter(r.logValueIndex)
	var value merkle.Value
	if err != nil {
		return value, err
	}
	if log != nil {
		switch r.treeIndex {
		case gmiLogAddress:
			copy(value[:], log.Address[:])
		case gmiLogTopicsLength:
			value[0] = byte(len(log.Topics))
		case gmiLogTopics1:
			if len(log.Topics) >= 1 {
				copy(value[:], log.Topics[0])
			}
		case gmiLogTopics2:
			if len(log.Topics) >= 1 {
				copy(value[:], log.Topics[1])
			}
		case gmiLogTopics3:
			if len(log.Topics) >= 1 {
				copy(value[:], log.Topics[2])
			}
		case gmiLogTopics4:
			if len(log.Topics) >= 1 {
				copy(value[:], log.Topics[3])
			}
		case gmiLogDataLength:
			binary.LittleEndian.PutUint32(value[:4], uint32(len(log.Data)))
		case gmiLogMetaBlockNumber:
			binary.LittleEndian.PutUint64(value[:8], log.BlockNumber) //TODO derived log fields except BlockHash should be filled
		case gmiLogMetaTxHash:
			copy(value[:], log.TxHash[:])
		case gmiLogMetaTxIndex:
			binary.LittleEndian.PutUint32(value[:4], uint32(log.TxIndex))
		case gmiLogMetaLogIndex:
			binary.LittleEndian.PutUint32(value[:4], uint32(log.Index))
		}
	}
	if header != nil {
		switch r.treeIndex {
		case gmiDelimiterMetaBlockNumber:
			binary.LittleEndian.PutUint64(value[:8], header.Number.Uint64())
		case gmiDelimiterMetaBlockHash:
			copy(value[:], header.Hash()[:])
		case gmiDelimiterMetaTimestamp:
			binary.LittleEndian.PutUint64(value[:8], header.Time)
		case gmiDelimiterMetaDummy:
			binary.LittleEndian.PutUint64(value[:8], 0xffffffffffffffff)
		}
	}
	return value, nil
}

type logDataRange struct { //TODO use progressive depth hash tree here?
	*Params
	logValueIndex, treeIndex uint64
}

func (r logDataRange) Subtree(subIndex uint64) merkle.TreeShape {
	if subIndex == 1 {
		return r
	}
	newIndex := merkle.ChildIndex(r.treeIndex, subIndex)
	if newIndex >= uint64(1)<<(lfLogDataHeight+1) {
		return nil
	}
	return logDataRange{Params: r.Params, logValueIndex: r.logValueIndex, treeIndex: newIndex}
}

func (r logDataRange) logValueRange() (first, afterLast uint64) {
	return r.logValueIndex, r.logValueIndex + 1
}

func (r logDataRange) IsLeaf() bool { return r.treeIndex >= uint64(1)<<lfLogDataHeight }

func (r logDataRange) IsSymmetrical() bool { return !r.IsLeaf() }

func (r logDataRange) IsEmpty() (bool, error) {
	shift := bits.LeadingZeros64(r.treeIndex) + lfLogDataHeight - 63
	first := (r.treeIndex<<shift - uint64(1)<<lfLogDataHeight) * 32
	log, _, err := r.source.getLogOrDelimiter(r.logValueIndex)
	if err != nil {
		return false, err
	}
	if log == nil {
		return true, nil
	}
	return start >= len(log.Data), nil
}

func (r logDataRange) GetLeaf() (merkle.Value, error) {
	start := (r.treeIndex - uint64(1)<<lfLogDataHeight) * 32
	var value merkle.Value
	log, _, err := r.source.getLogOrDelimiter(r.logValueIndex)
	if err != nil {
		return value, err
	}
	if log != nil && start < len(log.Data) {
		copy(value[:], log.Data[start:])
	}
	return value, nil
}
