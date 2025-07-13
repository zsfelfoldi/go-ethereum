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
	"errors"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrInvalidView = errors.New("chain view already invalidated")
)

type renderState struct {
	params           *Params
	renderRange      common.Range[uint32]
	lvPointer        uint64
	mapIndex         uint32
	currentMap       *memoryMap
	finishedMaps     []*finishedMap
	nextBlock        uint64
	partialBlock     bool
	partialBlockHash common.Hash
}

func (rs *renderState) checkNextHash(hash common.Hash) bool {
	if rs.partialBlock && rs.partialBlockHash != hash {
		return false
	}
	rs.partialBlock = false
	return true
}

func (rs *renderState) addReceipts(receipts types.Receipts) {
	if rs.partialBlock {
		panic("only addReceiptsAndHeader is allowed when last block is partially rendered")
	}
	for _, receipt := range receipts {
		//TODO add tx delimiter
		for _, log := range receipt.Logs {
			mapRemaining := rs.params.valuesPerMap - rs.lvPointer%rs.params.valuesPerMap
			if mapRemaining <= uint64(len(log.Topics)) {
				rs.skipValues(mapRemaining)
			}
			if rs.currentMap != nil {
				rs.addValue(addressValue(log.Address))
				for _, topic := range log.Topics {
					rs.addValue(topicValue(topic))
				}
			} else {
				rs.skipValues(uint64(len(log.Topics) + 1))
			}
			if rs.finished() {
				return
			}
		}
	}
}

func (rs *renderState) addHeader(header *types.Header) {
	if rs.partialBlock {
		panic("only addReceiptsAndHeader is allowed when last block is partially rendered")
	}
	if rs.nextBlock != header.Number.Uint64() {
		panic("wrong block number")
	}
	if rs.finished() {
		return
	}
	rs.skipValues(1) //TODO blockValue
	rs.nextBlock++
}

func (rs *renderState) getFinishedMaps() (uint32, []*finishedMap) {
	firstMapIndex, finishedMaps := rs.mapIndex-uint32(len(rs.finishedMaps)), rs.finishedMaps
	rs.finishedMaps = nil
	return firstMapIndex, finishedMaps
}

// assumes currentMap != nil
func (rs *renderState) addValue(logValue common.Hash) {
	for layerIndex := uint32(0); ; layerIndex++ {
		rowIndex := rs.params.rowIndex(rs.mapIndex, layerIndex, logValue)
		if rs.currentMap.rowLength(rowIndex) < rs.params.maxRowLength[min(layerIndex, uint32(len(rs.params.maxRowLength)-1))] {
			rs.currentMap.addToRow(rowIndex, rs.params.columnIndex(rs.lvPointer, &logValue))
			break
		}
	}
	rs.skipValues(1)
}

func (rs *renderState) skipValues(count uint64) {
	rs.lvPointer += count
	if uint32(rs.lvPointer>>rs.params.logValuesPerMap) > rs.mapIndex {
		if rs.currentMap != nil {
			rs.finishedMaps = append(rs.finishedMaps, rs.currentMap.finished())
		}
		rs.mapIndex++
		if rs.renderRange.Includes(rs.mapIndex) {
			rs.currentMap = rs.params.newMemoryMap()
		} else {
			rs.currentMap = nil
		}
	}
}

func (rs *renderState) finished() bool {
	return rs.mapIndex >= rs.renderRange.AfterLast()
}

type indexView struct {
	// if invalid <= 0 then the view is considered invalid and will return error
	// on any read operation (storage maps before firstMapIndex might have changed
	// since its creation)
	refCount, invalid int32
	storage           *mapStorage

	firstMapIndex    uint32
	firstBlockNumber uint64
	headBlockNumber  uint64
	headBlockHash    common.Hash
	lvPointer        uint64
	headMapIndex     uint32
	headMap          *memoryMap
	finishedMaps     []*finishedMap
}

func (iv *indexView) Release() {
	iv.addRefCount(-1)
}

func (iv *indexView) addRefCount(add int32) bool {
	return atomic.AddInt32(&iv.refCount, add) <= 0
}

func (iv *indexView) checkReleased() bool {
	return atomic.LoadInt32(&iv.refCount) <= 0
}

func (iv *indexView) invalidate() {
	atomic.StoreInt32(&iv.invalid, 1)
}

func (iv *indexView) checkInvalid() bool {
	return atomic.LoadInt32(&iv.invalid) != 0
}

func (iv *indexView) GetBlockLvPointer(blockNumber uint64) (uint64, error) {
	if iv.checkInvalid() {
		return 0, ErrInvalidView
	}

	if blockNumber < iv.firstBlockNumber {
		lvPtr, err := iv.storage.getBlockLvPointer(blockNumber)
		if iv.checkInvalid() {
			return 0, ErrInvalidView
		}
		return lvPtr, err
	}
	if blockNumber > iv.headBlockNumber {
		return 0, errors.New("block number out of range")
	}
	for _, fm := range iv.finishedMaps {
		if blockNumber >= fm.firstBlock() && blockNumber <= fm.lastBlock.number {
			return fm.blockPtrs[blockNumber-fm.firstBlock()], nil
		}
	}
	if blockNumber >= iv.headMap.firstBlock() && blockNumber <= iv.headMap.lastBlock.number {
		return iv.headMap.blockPtrs[blockNumber-iv.headMap.firstBlock()], nil
	}
	panic("indexView.GetBlockLvPointer: gap in blockLvPtrs")
}

func (iv *indexView) GetLastBlockOfMap(mapIndex uint32) (uint64, common.Hash, error) {
	if iv.checkInvalid() {
		return 0, common.Hash{}, ErrInvalidView
	}

	if mapIndex < iv.firstMapIndex {
		lastNumber, lastHash, err := iv.storage.getLastBlockOfMap(mapIndex)
		if iv.checkInvalid() {
			return 0, common.Hash{}, ErrInvalidView
		}
		return lastNumber, lastHash, err
	}
	if mapIndex > iv.headMapIndex {
		return 0, common.Hash{}, errors.New("map index out of range")
	}
	if mapIndex == iv.headMapIndex {
		return iv.headMap.lastBlock.number, iv.headMap.lastBlock.id, nil
	}
	fm := iv.finishedMaps[mapIndex-iv.firstMapIndex]
	return fm.lastBlock.number, fm.lastBlock.id, nil
}

// assumes strictly ascending order of map indices
func (iv *indexView) GetFilterMapRows(mapIndices []uint32, rowIndex, layers uint32) (rows []FilterRow, err error) {
	if iv.checkInvalid() {
		return nil, ErrInvalidView
	}

	dbIndices := len(mapIndices)
	for dbIndices > 0 && mapIndices[dbIndices-1] >= iv.firstMapIndex {
		dbIndices--
	}
	if dbIndices > 0 {
		rows, err = iv.storage.getFilterMapRows(mapIndices[:dbIndices], rowIndex, layers)
		if iv.checkInvalid() {
			return nil, ErrInvalidView
		}
		if err != nil {
			return nil, err
		}
	}
	for i := dbIndices; i < len(mapIndices); i++ {
		mapIndex := mapIndices[i]
		var row FilterRow
		if mapIndex == iv.headMapIndex {
			row = iv.headMap.getRow(rowIndex, iv.storage.params.maxRowLength[min(layers, uint32(len(iv.storage.params.maxRowLength)))-1])
		}
		if mapIndex < iv.headMapIndex {
			row = iv.finishedMaps[mapIndex-iv.firstMapIndex].getRow(rowIndex, iv.storage.params.maxRowLength[min(layers, uint32(len(iv.storage.params.maxRowLength)))-1])
		}
		rows = append(rows, row)
	}
	return rows, nil
}
