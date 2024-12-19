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
	currentMap             filterMap
	lvIndex                uint64
	iterator               *logIterator
}

func (f *FilterMaps) renderMapsFromBlock(blockNumber uint64) *mapRenderer {

}

func (f *FilterMaps) renderMapsInRange(firstMap, lastMap uint32) *mapRenderer {

}

func (r *mapRenderer) process(stopFn func() bool) (bool, error) {
	if !r.iterator.updateChainView(r.f.chainView) {
		// chain changed at current iterator position, current map needs to be discarded
		r.currentMap = r.f.emptyFilterMap()
		r.lvIndex = uint64(r.mapIndex) << r.f.logValuesPerMap
		r.iterator = r.f.newLogIterator(r.lvIndex)
	}
	newRange := r.f.filterMapsRange
	var waitCnt, rowCnt int
	for ; r.mapIndex <= r.lastMapIndex; r.mapIndex++ {
		// render map into currentMap
		for r.lvIndex < uint64(r.mapIndex+1)<<r.logValuesPerMap {
			waitCnt++
			if waitCnt >= valuesPerCallback {
				if stopFn() {
					return false, nil
				}
				waitCnt = 0
			}
			if r.lvIndex == uint64(r.mapIndex)<<r.logValuesPerMap {
				r.f.storeMapBlockPtr(r.mapIndex, r.iterator.blockNumber)
			}
			if r.iterator.blockStart {
				r.f.storeBlockLvPointer(r.iterator.blockNumber, r.lvIndex)
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
				r.currentMap[rowIndex] = append(r.currentMap[rowIndex], r.f.columnIndex(r.lvIndex, logValue))
			}
			if err := r.iterator.next(); err != nil {
				return false, err
			}
			r.lvIndex++
			if r.iterator.delimiter {
				//TODO make revert point
				// update head block pointer if newer than current one
				if r.iterator.blockNumber > newRange.headBlockNumber {
					newRange.headBlockNumber = r.iterator.blockNumber
					newRange.headBlockHash = r.iterator.blockHash
					newRange.headLvPointer = r.lvIndex
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

func (f *FilterMaps) newLogIteratorAtBlock(blockNumber uint64) (*logIterator, error) {
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

func (f *FilterMaps) newLogIteratorAtMap(mapIndex uint32) (*logIterator, error) {
	blockNumber, err := f.getMapBlockPtr(mapIndex)
	if err != nil {
		return nil, err
	}
	l, err := f.newLogIteratorAtBlock(blockNumber)
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

func (f *FilterMaps) newLogIteratorAtIndex(lvIndex uint64) (*logIterator, error) {
	l := &logIterator{chainView: f.chainView}
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
