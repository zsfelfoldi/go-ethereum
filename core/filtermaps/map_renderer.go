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
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	valuesPerCallback = 10000
	maxMapsPerBatch   = 16
)

var (
	errChainUpdate = errors.New("rendered section of chain updated")
)

type mapRenderer struct {
	f            *FilterMaps
	afterLastMap uint32
	currentMap   *renderedMap
	finishedMaps map[uint32]*renderedMap
	iterator     *logIterator
}

type renderedMap struct {
	filterMap     filterMap
	mapIndex      uint32
	lastBlock     uint64
	lastBlockHash common.Hash
	blockLvPtrs   []uint64 // start pointers of blocks starting in this map; last one is lastBlock
	finished      bool     // iterator finished; all values rendered
	headDelimiter uint64   // if finished then points to the future block delimiter of the head block
}

func (r *renderedMap) firstBlock() uint64 {
	return r.lastBlock + 1 - uint64(len(r.blockLvPtrs))
}

func (f *FilterMaps) renderMapsBefore(afterLastMap uint32) (*mapRenderer, error) {
	if f.indexedView == nil {
		panic("aaaaaaaaaaaaaaaa")
	}
	if f.targetView == nil {
		panic("bbbbbbbbbbbbbbbb")
	}
	snapshot := f.findLastSnapshotBefore(afterLastMap)
	nextMap, startBlock, startLvPtr, err := f.findLastMapBoundaryBefore(afterLastMap)
	if err != nil {
		fmt.Println(" flmbb err", err)
		return nil, err
	}
	if snapshot != nil && snapshot.mapIndex >= nextMap {
		return f.renderMapsFromSnapshot(snapshot)
	}
	if nextMap >= afterLastMap {
		return nil, nil
	}
	return f.renderMapsFromMapBoundary(nextMap, afterLastMap, startBlock, startLvPtr)
}

func (f *FilterMaps) findLastSnapshotBefore(afterLastMap uint32) *renderedMap {
	var best *renderedMap
	for _, blockNumber := range f.renderSnapshots.Keys() {
		if cp, _ := f.renderSnapshots.Get(blockNumber); cp != nil &&
			f.targetView.getBlockHash(blockNumber) == cp.lastBlockHash &&
			cp.mapIndex < afterLastMap && (best == nil || blockNumber > best.lastBlock) {
			best = cp
		}
	}
	return best
}

func (f *FilterMaps) findLastMapBoundaryBefore(afterLastMap uint32) (nextMap uint32, startBlock, startLvPtr uint64, err error) {
	if !f.initialized {
		return 0, 0, 0, nil
	}
	mapIndex := afterLastMap
	for {
		var ok bool
		if mapIndex, ok = f.lastMapBoundaryBefore(mapIndex); !ok {
			return 0, 0, 0, nil
		}
		lastBlock, err := f.getLastBlockOfMap(mapIndex)
		if err != nil {
			fmt.Println(" glbm err", err)
			return 0, 0, 0, err
		}
		if lastBlock >= f.indexedView.headNumber || f.targetView.getBlockHash(lastBlock) != f.indexedView.getBlockHash(lastBlock) {
			// map is not full or inconsistent with targetView; roll back
			continue
		}
		lvPtr, err := f.getBlockLvPointer(lastBlock)
		if err != nil {
			fmt.Println(" gblp err", lastBlock, err)
			return 0, 0, 0, err
		}
		return mapIndex + 1, lastBlock, lvPtr, nil
	}
}

func (f *FilterMaps) lastMapBoundaryBefore(mapIndex uint32) (uint32, bool) {
	if !f.initialized || f.afterLastRenderedMap == 0 {
		return 0, false
	}
	if mapIndex > f.afterLastRenderedMap {
		mapIndex = f.afterLastRenderedMap
	}
	if mapIndex > f.firstRenderedMap {
		return mapIndex - 1, true
	}
	if mapIndex+f.mapsPerEpoch > f.firstRenderedMap {
		if mapIndex > f.firstRenderedMap-f.mapsPerEpoch+f.tailPartialEpoch {
			mapIndex = f.firstRenderedMap - f.mapsPerEpoch + f.tailPartialEpoch
		}
	} else {
		mapIndex = (mapIndex >> f.logMapsPerEpoch) << f.logMapsPerEpoch
	}
	if mapIndex == 0 {
		return 0, false
	}
	return mapIndex - 1, true
}

func (f *FilterMaps) renderMapsFromSnapshot(cp *renderedMap) (*mapRenderer, error) {
	iter, err := f.newLogIteratorFromBlockDelimiter(cp.lastBlock)
	if err != nil {
		return nil, err
	}
	return &mapRenderer{
		f: f,
		currentMap: &renderedMap{
			filterMap:     cp.filterMap.copy(),
			mapIndex:      cp.mapIndex,
			lastBlock:     cp.lastBlock,
			lastBlockHash: cp.lastBlockHash,
			blockLvPtrs:   cp.blockLvPtrs,
		},
		finishedMaps: make(map[uint32]*renderedMap),
		afterLastMap: math.MaxUint32,
		iterator:     iter,
	}, nil
}

func (f *FilterMaps) renderMapsFromMapBoundary(firstMap, afterLastMap uint32, startBlock, startLvPtr uint64) (*mapRenderer, error) {
	iter, err := f.newLogIteratorFromMapBoundary(firstMap, startBlock, startLvPtr)
	if err != nil {
		return nil, err
	}
	return &mapRenderer{
		f: f,
		currentMap: &renderedMap{
			filterMap:     f.emptyFilterMap(),
			mapIndex:      firstMap,
			lastBlock:     iter.blockNumber,
			lastBlockHash: iter.blockHash,
		},
		finishedMaps: make(map[uint32]*renderedMap),
		afterLastMap: afterLastMap,
		iterator:     iter,
	}, nil
}

func (r *mapRenderer) makeCheckpoint() *renderedMap {
	return &renderedMap{
		filterMap:     r.currentMap.filterMap.copy(),
		mapIndex:      r.currentMap.mapIndex,
		lastBlock:     r.iterator.blockNumber,
		lastBlockHash: r.iterator.blockHash,
		blockLvPtrs:   r.currentMap.blockLvPtrs,
		finished:      true,
		headDelimiter: r.iterator.lvIndex,
	}
}

func (r *mapRenderer) renderMaps(stopFn func() bool) (bool, error) {
	for {
		if done, err := r.renderCurrentMap(stopFn); !done {
			fmt.Println("rm interrupt", done, err)
			return done, err // stopped or failed
		}
		// map finished
		r.finishedMaps[r.currentMap.mapIndex] = r.currentMap
		if len(r.finishedMaps) >= maxMapsPerBatch {
			if err := r.writeFinishedMaps(); err != nil {
				fmt.Println("rm wfm1 err", err)
				return false, err
			}
		}
		if r.currentMap.mapIndex+1 == r.afterLastMap || r.iterator.finished {
			if err := r.writeFinishedMaps(); err != nil {
				fmt.Println("rm wfm2 err", err)
				return false, err
			}
			return true, nil
		}
		r.currentMap = &renderedMap{
			filterMap: r.f.emptyFilterMap(),
			mapIndex:  r.currentMap.mapIndex + 1,
		}
	}
}

func (r *mapRenderer) renderCurrentMap(stopFn func() bool) (bool, error) {
	if !r.iterator.updateChainView(r.f.targetView) {
		return false, errChainUpdate
	}
	epoch := r.currentMap.mapIndex >> r.f.logMapsPerEpoch
	var waitCnt int
	for r.iterator.lvIndex < uint64(r.currentMap.mapIndex+1)<<r.f.logValuesPerMap && !r.iterator.finished {
		waitCnt++
		if waitCnt >= valuesPerCallback {
			if stopFn() {
				return false, nil
			}
			waitCnt = 0
		}
		r.currentMap.lastBlock = r.iterator.blockNumber
		r.currentMap.lastBlockHash = r.iterator.blockHash
		if r.iterator.blockStart {
			r.currentMap.blockLvPtrs = append(r.currentMap.blockLvPtrs, r.iterator.lvIndex)
		}
		if logValue := r.iterator.getValueHash(); logValue != (common.Hash{}) {
			var rowIndex uint32
			for alternativeIndex := uint32(0); ; alternativeIndex++ {
				rowIndex = r.f.rowIndex(epoch, alternativeIndex, logValue)
				if uint32(len(r.currentMap.filterMap[rowIndex])) < r.f.maxRowLength {
					break
				}
			}
			r.currentMap.filterMap[rowIndex] = append(r.currentMap.filterMap[rowIndex], r.f.columnIndex(r.iterator.lvIndex, logValue))
		}
		if err := r.iterator.next(); err != nil {
			return false, err
		}
		if r.iterator.finished {
			r.currentMap.finished = true
			r.currentMap.headDelimiter = r.iterator.lvIndex
		}
	}
	return true, nil
}

func (r *mapRenderer) writeFinishedMaps() error {
	if len(r.finishedMaps) == 0 {
		return nil
	}
	r.f.indexLock.Lock()
	defer r.f.indexLock.Unlock()

	firstMap, lastMap := uint32(math.MaxUint32), uint32(0)
	for m := range r.finishedMaps {
		if m < firstMap {
			firstMap = m
		}
		if m > lastMap {
			lastMap = m
		}
	}
	batch := r.f.db.NewBatch()
	// update filterMapsRange
	newRange := r.f.filterMapsRange
	if r.afterLastMap == math.MaxUint32 {
		// head update
		//TODO remove old data if necessary
		if r.f.targetView == nil {
			panic("xxxxxxxxxx")
		}
		r.f.indexedView = r.f.targetView
		if !newRange.initialized {
			newRange.initialized = true
			newRange.firstRenderedMap = firstMap
			fm := r.finishedMaps[firstMap]
			newRange.firstIndexedBlock = fm.firstBlock()
		}
		newRange.headBlockNumber = r.f.targetView.headNumber
		newRange.headBlockHash = r.f.targetView.getBlockHash(newRange.headBlockNumber)
		if firstMap < newRange.firstRenderedMap {
			newRange.firstRenderedMap = firstMap
			newRange.tailPartialEpoch = 0
			newRange.firstIndexedBlock = r.finishedMaps[firstMap].firstBlock()
		}
		newRange.afterLastRenderedMap = lastMap + 1
		lm := r.finishedMaps[lastMap]
		if lm.finished {
			newRange.afterLastIndexedBlock = newRange.headBlockNumber + 1
			if lm.lastBlock != newRange.headBlockNumber {
				fmt.Println("xxx", lastMap, lm.lastBlock, newRange.headBlockNumber)
				panic("map rendering finished but last block != head block")
			}
			newRange.headBlockDelimiter = lm.headDelimiter
		} else {
			newRange.afterLastIndexedBlock = lm.lastBlock
			newRange.headBlockDelimiter = 0
		}
	} else {
		// tail extension
		if !newRange.initialized {
			return errors.New("tail extension of uninitialized log index")
		}
		if lastBlock := r.finishedMaps[lastMap].lastBlock; r.f.targetView.getBlockHash(lastBlock) != r.f.indexedView.getBlockHash(lastBlock) {
			return errChainUpdate
		}
		if firstMap != newRange.firstRenderedMap-r.f.mapsPerEpoch+newRange.tailPartialEpoch {
			fmt.Println("aaa", firstMap, newRange.firstRenderedMap, r.f.mapsPerEpoch, newRange.tailPartialEpoch)
			return errors.New("tail extension: first map invalid")
		}
		newRange.tailPartialEpoch = lastMap + 1 + r.f.mapsPerEpoch - newRange.firstRenderedMap
		if newRange.tailPartialEpoch > r.f.mapsPerEpoch {
			return errors.New("tail extension: last map invalid")
		}
		if newRange.tailPartialEpoch == r.f.mapsPerEpoch { // tail epoch completed
			newRange.firstRenderedMap -= r.f.mapsPerEpoch
			if newRange.firstRenderedMap > 0 {
				lastBlock, err := r.f.getLastBlockOfMap(newRange.firstRenderedMap - 1)
				if err != nil {
					return err
				}
				newRange.firstIndexedBlock = lastBlock + 1
			} else {
				newRange.firstIndexedBlock = 0
			}
			newRange.tailPartialEpoch = 0
		}
	}
	r.f.setRange(batch, newRange)
	// add or update filter rows
	for rowIndex := uint32(0); rowIndex < r.f.mapHeight; rowIndex++ {
		for mapIndex := firstMap; mapIndex <= lastMap; mapIndex++ {
			row := r.finishedMaps[mapIndex].filterMap[rowIndex]
			if fm := r.f.filterMapCache[mapIndex]; fm != nil && row.Equal(fm[rowIndex]) {
				continue
			}
			r.f.storeFilterMapRow(batch, mapIndex, rowIndex, row)
		}
	}
	// add or update block pointers
	for mapIndex := firstMap; mapIndex <= lastMap; mapIndex++ {
		renderedMap := r.finishedMaps[mapIndex]
		r.f.storeLastBlockOfMap(batch, mapIndex, renderedMap.lastBlock)
		blockNumber := renderedMap.firstBlock()
		for _, lvPtr := range renderedMap.blockLvPtrs {
			r.f.storeBlockLvPointer(batch, blockNumber, lvPtr)
		}
	}

	r.finishedMaps = make(map[uint32]*renderedMap)
	return batch.Write()
}

func (f *FilterMaps) emptyFilterMap() filterMap {
	return make(filterMap, f.mapHeight)
}

type chainView struct {
	chain        blockchain
	nonCanonical []*types.Header
	headNumber   uint64
	headHash     common.Hash
}

func newChainView(chain blockchain, number uint64, hash common.Hash) *chainView {
	cv := &chainView{
		chain:      chain,
		headNumber: number,
		headHash:   hash,
	}
	cv.extendNonCanonical()
	return cv
}

func (cv *chainView) extendNonCanonical() bool {
	for cv.headHash != cv.chain.GetCanonicalHash(cv.headNumber) {
		header := cv.chain.GetHeader(cv.headHash, cv.headNumber)
		if header == nil {
			log.Error("Header not found", "number", cv.headNumber, "hash", cv.headHash)
			return false
		}
		cv.nonCanonical = append(cv.nonCanonical, header)
		cv.headNumber, cv.headHash = cv.headNumber-1, header.ParentHash
	}
	return true
}

func (cv *chainView) getBlockHash(number uint64) common.Hash {
	if number <= cv.headNumber {
		hash := cv.chain.GetCanonicalHash(number)
		if !cv.extendNonCanonical() {
			return common.Hash{}
		}
		if number <= cv.headNumber {
			return hash
		}
	}
	if number-cv.headNumber > uint64(len(cv.nonCanonical)) {
		return common.Hash{}
	}
	return cv.nonCanonical[len(cv.nonCanonical)+1-int(number-cv.headNumber)].Hash()
}

func (cv *chainView) getHeader(number uint64) *types.Header {
	if number <= cv.headNumber {
		hash := cv.chain.GetCanonicalHash(number)
		if !cv.extendNonCanonical() {
			return nil
		}
		if number <= cv.headNumber {
			return cv.chain.GetHeader(hash, number)
		}
	}
	if number-cv.headNumber > uint64(len(cv.nonCanonical)) {
		return nil
	}
	return cv.nonCanonical[len(cv.nonCanonical)+1-int(number-cv.headNumber)]
}

type logIterator struct {
	chainView                       *chainView
	getReceiptsByHash               func(common.Hash) types.Receipts
	blockNumber                     uint64
	blockHash                       common.Hash
	receipts                        types.Receipts
	blockStart, delimiter, finished bool
	txIndex, logIndex, topicIndex   int
	lvIndex                         uint64
}

var errUnindexedRange = errors.New("unindexed range")

func (f *FilterMaps) newLogIteratorFromBlockDelimiter(blockNumber uint64) (*logIterator, error) {
	if blockNumber > f.targetView.headNumber {
		return nil, errors.New("iterator entry point after target chain head")
	}
	if blockNumber < f.firstIndexedBlock || blockNumber >= f.afterLastIndexedBlock {
		return nil, errUnindexedRange
	}
	blockHash := f.targetView.getBlockHash(blockNumber)
	if f.indexedView.getBlockHash(blockNumber) != blockHash {
		return nil, errors.New("target and indexed views diverged at iterator entry point")
	}
	var lvIndex uint64
	if blockNumber == f.headBlockNumber {
		lvIndex = f.headBlockDelimiter
	} else {
		var err error
		lvIndex, err = f.getBlockLvPointer(blockNumber + 1)
		if err != nil {
			return nil, err
		}
		lvIndex--
	}
	finished := blockNumber == f.targetView.headNumber
	return &logIterator{
		chainView:         f.targetView,
		getReceiptsByHash: f.chain.GetReceiptsByHash,
		blockNumber:       blockNumber,
		blockHash:         blockHash,
		finished:          finished,
		delimiter:         !finished,
		lvIndex:           lvIndex,
	}, nil
}

func (f *FilterMaps) newLogIteratorFromMapBoundary(mapIndex uint32, startBlock, startLvPtr uint64) (*logIterator, error) {
	if startBlock > f.targetView.headNumber {
		return nil, errors.New("iterator entry point after target chain head")
	}
	blockHash := f.targetView.getBlockHash(startBlock)
	if f.indexedView.getBlockHash(startBlock) != blockHash {
		return nil, errors.New("target and indexed views diverged at iterator entry point")
	}
	// get block receipts
	receipts := f.chain.GetReceiptsByHash(blockHash)
	if receipts == nil {
		return nil, errors.New("receipts not found")
	}
	// initialize iterator at block start
	l := &logIterator{
		chainView:         f.targetView,
		getReceiptsByHash: f.chain.GetReceiptsByHash,
		blockNumber:       startBlock,
		blockHash:         blockHash,
		receipts:          receipts,
		blockStart:        true,
		lvIndex:           startLvPtr,
	}
	l.nextValid()
	targetIndex := uint64(mapIndex) << f.logValuesPerMap
	if l.lvIndex > targetIndex {
		panic("last map block's lvPointer > map boundary")
	}
	// iterate to map boundary
	for l.lvIndex < targetIndex {
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

func (l *logIterator) next() error {
	if l.finished {
		return nil
	}
	if l.delimiter {
		l.delimiter = false
		l.blockNumber++
		l.blockHash = l.chainView.getBlockHash(l.blockNumber)
		l.receipts = l.getReceiptsByHash(l.blockHash)
		if l.receipts == nil {
			return errors.New("receipts not found")
		}
		l.txIndex, l.logIndex, l.topicIndex, l.blockStart = 0, 0, 0, true
	} else {
		l.topicIndex++
		l.blockStart = false
	}
	l.lvIndex++
	l.nextValid()
	return nil
}

func (l *logIterator) nextValid() {
	for ; l.txIndex < len(l.receipts); l.txIndex++ {
		receipt := l.receipts[l.txIndex]
		for ; l.logIndex < len(receipt.Logs); l.logIndex++ {
			log := receipt.Logs[l.logIndex]
			if l.topicIndex <= len(log.Topics) {
				return
			}
			l.topicIndex = 0
		}
		l.logIndex = 0
	}
	if l.blockNumber == l.chainView.headNumber {
		l.finished = true
	} else {
		l.delimiter = true
	}
}

//TODO
/*func (l *logIterator) getLog() (*types.Log, *types.Header) {
	if l.finished {
		return nil, nil
	}
	if l.delimiter {
		return nil, l.chainView.getHeader(l.blockNumber)
	}
	if l.topicIndex != 0 {
		return nil, nil
	}
	return l.receipts[l.txindex].Logs[l.logIndex]
}*/

func (l *logIterator) getValueHash() common.Hash {
	if l.delimiter || l.finished {
		return common.Hash{}
	}
	log := l.receipts[l.txIndex].Logs[l.logIndex]
	if l.topicIndex == 0 {
		return addressValue(log.Address)
	}
	return topicValue(log.Topics[l.topicIndex-1])
}
