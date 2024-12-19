// Copyright 2024 The go-ethereum Authors
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
	"math"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	valuesPerCallback = 10000
	rowsPerCallback   = 4097 //TODO
)

type mapRenderer struct {
	f                      *FilterMaps
	mapIndex, lastMapIndex uint32
	currentMap             filterMap // nil = unchanged
	iterator               *logIterator
}

func (f *FilterMaps) renderMapsFromHead() (*mapRenderer, error) {
	iter, err := f.newLogIteratorFromHead()
	if err != nil {
		return nil, err
	}
	return &mapRenderer{
		f:            f,
		mapIndex:     uint32(iter.lvIndex >> f.logValuesPerMap),
		lastMapIndex: math.MaxUint32,
		currentMap:   f.transparentFilterMap(),
		lvIndex:      iter.lvIndex,
		iterator:     iter,
	}, nil
}

// revert and re-render blockNumber and above; assumes that blockNumber is already indexed
func (f *FilterMaps) renderMapsFromBlock(blockNumber uint64) (*mapRenderer, error) {
	rp := f.revertPoints[blockNumber-1]
	if rp == nil {
		// cannot revert map using revert point; re-render entire map affected by the change
		lvIndex, err := f.getBlockLvPointer(blockNumber)
		if err != nil {
			return nil, err
		}
		revertMap := uint32(lvIndex >> f.logValuesPerMap)
		revertFromBlock, err := f.getMapBlockPtr(revertMap)
		if err != nil {
			return nil, err
		}
		f.revertFromBlock(revertFromBlock)
		return f.renderMapsInRange(revertMap, math.MaxUint32)
	}
	f.revertFromBlock(blockNumber)
	iter, err := f.newLogIteratorFromBlock(blockNumber)
	if err != nil {
		return nil, err
	}
	return &mapRenderer{
		f:            f,
		mapIndex:     uint32(iter.lvIndex >> f.logValuesPerMap),
		lastMapIndex: math.MaxUint32,
		currentMap:   f.revertFilterMap(rp),
		lvIndex:      iter.lvIndex,
		iterator:     iter,
	}, nil
}

func (f *FilterMaps) renderMapsInRange(firstMap, lastMap uint32) (*mapRenderer, error) {
	iter, err := f.newLogIteratorFromMap(firstMap)
	if err != nil {
		return nil, err
	}
	return &mapRenderer{
		f:            f,
		mapIndex:     firstMap,
		lastMapIndex: lastMap,
		currentMap:   f.emptyFilterMap(),
		lvIndex:      iter.lvIndex,
		iterator:     iter,
	}, nil
}

func (r *mapRenderer) process(stopFn func() bool) (bool, error) {
	if !r.iterator.updateChainView(r.f.chainView) {
		// chain changed at current iterator position, current map needs to be discarded
		r.currentMap = r.f.emptyFilterMap()
		var err error
		if iterator, err = r.f.newLogIteratorFromMap(r.mapIndex); err != nil {
			return false, err
		}
	}
	newRange := r.f.filterMapsRange
	var waitCnt, rowCnt int
	for ; r.mapIndex <= r.lastMapIndex; r.mapIndex++ {
		// render map into currentMap
		for r.iterator.lvIndex < uint64(r.mapIndex+1)<<r.logValuesPerMap {
			waitCnt++
			if waitCnt >= valuesPerCallback {
				if stopFn() {
					return false, nil
				}
				waitCnt = 0
			}
			if r.iterator.lvIndex == uint64(r.mapIndex)<<r.logValuesPerMap {
				r.f.storeMapBlockPtr(r.mapIndex, r.iterator.blockNumber)
			}
			if r.iterator.blockStart {
				r.f.storeBlockLvPointer(r.iterator.blockNumber, r.iterator.lvIndex)
			}
			if logValue := r.iterator.getValueHash(); logValue != (common.Hash{}) {
				var rowIndex uint32
				for alternativeIndex := uint32(0); ; alternativeIndex++ {
					rowIndex = r.f.rowIndex(mapIndex>>r.f.logMapsPerEpoch, alternativeIndex, logValue)
					if r.currentMap[rowIndex] == nil {
						var err error
						r.currentMap[rowIndex], err = r.f.getFilterMapRow(r.mapIndex, rowIndex)
						if err != nil {
							return false, err
						}
					}
					if len(r.currentMap[rowIndex]) < r.f.maxRowLength {
						break
					}
				}
				r.currentMap[rowIndex] = append(r.currentMap[rowIndex], r.f.columnIndex(r.iterator.lvIndex, logValue))
			}
			if err := r.iterator.next(); err != nil {
				return false, err
			}
			if r.iterator.delimiter {
				// update head block pointer if newer than current one
				if r.iterator.blockNumber > newRange.headBlockNumber {
					newRange.headBlockNumber = r.iterator.blockNumber
					newRange.headBlockHash = r.iterator.blockHash
					newRange.headLvPointer = r.iterator.lvIndex
					rp, err := r.makeRevertPoint()
					if err != nil {
						return false, err
					}
					r.f.addRevertPoint(r.iterator.blockNumber, rp)
				}
				if r.iterator.blockNumber == r.f.chainView.headNumber() {
					break // end of chain, last delimiter is not added
				}
			}
		}
		batch := r.f.db.NewBatch()
		for rowIndex, row := range r.currentMap {
			if row == nil {
				continue
			}
			r.f.storeFilterMapRow(batch, r.mapIndex, rowIndex, row)
			rowCnt++
			if rowCnt >= rowsPerCallback {
				if err := batch.Write(); err != nil {
					return false, err
				}
				batch = r.f.db.NewBatch()
				if stopFn() {
					return false, nil
				}
				rowCnt = 0
			}
		}
		if r.mapIndex > newRange.headMapIndex {
			newRange.headMapIndex = r.mapIndex
		}
		r.f.setRange(batch, newRange)
		if err := batch.Write(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// revertPoint can be used to revert the log index to a certain head block.
type revertPoint struct {
	lvPointer uint64
	rowLength []uint
}

// makeRevertPoint creates a new revertPoint.
func (r *mapRenderer) makeRevertPoint() (*revertPoint, error) {
	rp := &revertPoint{
		lvPointer: r.iterator.lvIndex,
		rowLength: make([]uint, r.f.mapHeight),
	}
	if uint32((rp.lvPointer-1)>>r.f.logValuesPerMap) != r.mapIndex {
		panic("lvPointer does not point to currentMap")
	}
	for i := range rp.rowLength {
		row := r.currentMap[i]
		if row == nil {
			var err error
			row, err = r.f.getFilterMapRow(r.mapIndex, uint32(i))
			if err != nil {
				return nil, err
			}
		}
		rp.rowLength[i] = uint(len(row))
	}
	return rp, nil
}

func (f *FilterMaps) revertFilterMap(rp *revertPoint) (filterMap, error) {
	fm := make(filterMap, f.mapHeight)
	mapIndex := uint32((rp.lvPointer - 1) >> r.f.logValuesPerMap)
	for rowIndex, rowLen := range rp.rowLength {
		row, err := f.getFilterMapRow(mapIndex, uint32(rowIndex))
		if err != nil {
			return nil, err
		}
		if uint(len(row)) < rowLen {
			return nil, errors.New("cannot revert (row too short)")
		}
		fm[rowIndex] = make(FilterRow, rowLen)
		copy(fm[rowIndex], row[:rowLen])
	}
	return fm, nil
}

func (f *FilterMaps) emptyFilterMap() filterMap {
	fm := make(filterMap, f.mapHeight)
	for i := range fm {
		fm[i] = make(FilterRow, 0, f.valuesPerMap/f.mapHeight)
	}
	return fm
}

func (f *FilterMaps) transparentFilterMap() filterMap {
	return make(filterMap, f.mapHeight)
}

type chainView struct {
	chain        blockchain
	nonCanonical []*types.Header
	number       uint64
	hash         common.Hash
}

func newChainView(chain blockchain, number uint64, hash common.Hash) *chainView {
	cv := &chainView{
		chain:  chain,
		number: number,
		hash:   hash,
	}
	cv.extendNonCanonical()
	return cv
}

func (cv *chainView) extendNonCanonical() bool {
	for cv.hash != cv.chain.GetCanonicalHash(cv.number) {
		header := cv.chain.GetHeader(cv.hash, cv.number)
		if header == nil {
			log.Error("Header not found", "number", cv.number, "hash", cv.hash)
			return false
		}
		cv.nonCanonical = append(cv.nonCanonical, header)
		cv.number, cv.hash = cv.number-1, header.ParentHash
	}
	return true
}

func (cv *chainView) getBlockHash(number uint64) common.Hash {
	if number <= cv.number {
		hash := cv.chain.GetCanonicalHash(number)
		if !cv.extendNonCanonical() {
			return common.Hash{}
		}
		if number <= cv.number {
			return hash
		}
	}
	if number-cv.number > uint64(len(cv.nonCanonical)) {
		return common.Hash{}
	}
	return cv.nonCanonical[len(cv.nonCanonical)+1-int(number-cv.number)].Hash()
}

func (cv *chainView) getHeader(number uint64) *types.Header {
	if number <= cv.number {
		hash := cv.chain.GetCanonicalHash(number)
		if !cv.extendNonCanonical() {
			return nil
		}
		if number <= cv.number {
			return cv.chain.GetHeader(hash, number)
		}
	}
	if number-cv.number > uint64(len(cv.nonCanonical)) {
		return nil
	}
	return cv.nonCanonical[len(cv.nonCanonical)+1-int(number-cv.number)]
}

type logIterator struct {
	chainView                     *chainView
	blockNumber                   uint64
	blockHash                     common.Hash
	receipts                      types.Receipts
	blockStart, delimiter         bool
	txIndex, logIndex, topicIndex int
	lvIndex                       uint64
}

var errUnindexedRange = errors.New("unindexed range")

// initializes at headLvPointer which points to the yet to be added block delimiter
// of the head block.
func (f *FilterMaps) newLogIteratorFromHead() (*logIterator, error) {
	return &logIterator{
		chainView:   f.chainView,
		blockNumber: f.headBlockNumber,
		blockHash:   f.headBlockHash,
		delimiter:   true,
		lvIndex:     f.headLvPointer,
	}, nil
}

func (f *FilterMaps) newLogIteratorFromBlock(blockNumber uint64) (*logIterator, error) {
	// get block receipts
	blockHash := f.chainView.getHash(blockNumber)
	receipts := f.chain.GetReceiptsByHash(blockHash)
	if receipts == nil {
		return nil, errors.New("receipts not found")
	}
	lvIndex, err := f.getBlockLvPointer(blockNumber)
	if err != nil {
		return nil, err
	}
	l := &logIterator{
		chainView:   f.chainView,
		blockNumber: blockNumber,
		blockHash:   blockHash,
		receipts:    receipts,
		blockStart:  true,
		lvIndex:     lvIndex,
	}
	l.nextValid()
	return l, nil
}

func (f *FilterMaps) newLogIteratorFromMap(mapIndex uint32) (*logIterator, error) {
	blockNumber, err := f.getMapBlockPtr(mapIndex)
	if err != nil {
		return nil, err
	}
	l, err := f.newLogIteratorFromBlock(blockNumber)
	if err != nl {
		return nil, err
	}
	targetIndex := uint64(mapIndex) << f.logValuesPerMap
	if l.lvIndex > targetIndex {
		panic("mapBlockPtr block's lvPointer > map boundary")
	}
	for l.lvIndex < targetIndex {
		if err := l.next(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (f *FilterMaps) newLogIteratorFromIndex(lvIndex uint64) (*logIterator, error) {
	if lvIndex < f.tailBlockLvPointer || lvIndex >= f.headLvPointer {
		return nil, errUnindexedRange
	}
	// find possible block range based on map to block pointers
	mapIndex := uint32(lvIndex >> f.logValuesPerMap)
	firstBlockNumber, err := f.getMapBlockPtr(mapIndex)
	if err != nil {
		return nil, err
	}
	if firstBlockNumber < f.tailBlockNumber {
		firstBlockNumber = f.tailBlockNumber
	}
	var lastBlockNumber uint64
	if mapIndex+1 < uint32((f.headLvPointer+f.valuesPerMap-1)>>f.logValuesPerMap) {
		lastBlockNumber, err = f.getMapBlockPtr(mapIndex + 1)
		if err != nil {
			return nil, err
		}
	} else {
		lastBlockNumber = f.chainView.headNumber()
	}
	// find block with binary search based on block to log value index pointers
	for firstBlockNumber < lastBlockNumber {
		midBlockNumber := (firstBlockNumber + lastBlockNumber + 1) / 2
		midLvPointer, err := f.getBlockLvPointer(midBlockNumber)
		if err != nil {
			return nil, err
		}
		if lvIndex < midLvPointer {
			lastBlockNumber = midBlockNumber - 1
		} else {
			firstBlockNumber = midBlockNumber
		}
	}
	l, err := f.newLogIteratorFromBlock(firstBlockNumber)
	if err != nl {
		return nil, err
	}
	if l.lvIndex > lvIndex {
		panic("block's lvPointer > target lvIndex")
	}
	for l.lvIndex < lvIndex {
		if err := l.next(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *logIterator) updateChainView(cv *chainView) bool {
	if cv.getBlockHash(l.blockNumber) != l.blockHash {
		return false
	}
	l.chainView = cv
	return true
}

func (l *logIterator) next() error { //TODO end of indexed range, no delimiter at end
	if l.delimiter {
		l.delimiter = false
		l.blockNumber++
		l.receipts = f.chain.GetReceiptsByHash(f.chainView.getHash(l.blockNumber))
		if l.receipts == nil {
			return errors.New("receipts not found")
		}
		l.txIndex, l.logIndex, l.topicIndex, l.blockStart = 0, 0, 0, true
	} else {
		l.topicIndex++
		l.blockStart = false
	}
	l.nextValid()
	return nil
}

func (l *logIterator) nextValid() {
	for ; l.txIndex < len(receipts); l.txIndex++ {
		receipt := l.receipts[l.txindex]
		for ; l.logIndex < len(receipt.Logs); l.logIndex++ {
			log := receipt.Logs[l.logIndex]
			if l.topicIndex <= len(log.Topics) {
				return nil
			}
			l.topicIndex = 0
		}
		l.logIndex = 0
	}
	l.delimiter = true
}

func (l *logIterator) getLog() (*types.Log, *types.Header) {
	if l.delimiter {
		return nil, l.chainView.getHeader(l.blockNumber)
	}
	if l.topicIndex != 0 {
		return nil, nil
	}
	return l.receipts[l.txindex].Logs[l.logIndex]
}

func (l *logIterator) getValueHash() common.Hash {
	if l.delimiter {
		return common.Hash{}
	}
	log := l.receipts[l.txindex].Logs[l.logIndex]
	if l.topicIndex == 0 {
		return addressValue(log.Address)
	}
	return topicValue(log.Topics[l.topicIndex-1])
}
