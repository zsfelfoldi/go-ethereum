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
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

const (
	valuesPerCallback = 10000
	maxMapsPerBatch   = 16
)

var (
	errChainUpdate = errors.New("rendered section of chain updated")
)

type mapRenderer struct {
	f                                *FilterMaps
	afterLastMap                     uint32
	currentMap                       *renderedMap
	finishedMaps                     map[uint32]*renderedMap
	firstFinished, afterLastFinished uint32
	iterator                         *logIterator
}

type renderedMap struct {
	filterMap     filterMap
	mapIndex      uint32
	lastBlock     uint64
	lastBlockId   common.Hash
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
	//fmt.Println(" flmbb start")
	nextMap, startBlock, startLvPtr, err := f.findLastMapBoundaryBefore(afterLastMap)
	fmt.Println(" flmbb", nextMap, startBlock, startLvPtr)
	if err != nil {
		fmt.Println(" flmbb err", err)
		return nil, err
	}
	if snapshot := f.findLastSnapshotBefore(afterLastMap); snapshot != nil && snapshot.mapIndex >= nextMap {
		return f.renderMapsFromSnapshot(snapshot)
	}
	if nextMap >= afterLastMap {
		return nil, nil
	}
	return f.renderMapsFromMapBoundary(nextMap, afterLastMap, startBlock, startLvPtr)
}

func (f *FilterMaps) findLastSnapshotBefore(afterLastMap uint32) *renderedMap {
	var best *renderedMap
	fmt.Println("*** findLastSnapshotBefore", afterLastMap)
	for _, blockNumber := range f.renderSnapshots.Keys() {
		fmt.Println(" key", blockNumber)
		if cp, _ := f.renderSnapshots.Get(blockNumber); cp != nil {
			fmt.Println("  cp", blockNumber <= f.targetView.headNumber() && f.targetView.getBlockId(blockNumber) == cp.lastBlockId, f.targetView.headNumber(), cp)
		}
		if cp, _ := f.renderSnapshots.Get(blockNumber); cp != nil && blockNumber < f.afterLastIndexedBlock &&
			blockNumber <= f.targetView.headNumber() && f.targetView.getBlockId(blockNumber) == cp.lastBlockId &&
			cp.mapIndex < afterLastMap && (best == nil || blockNumber > best.lastBlock) {
			best = cp
		}
	}
	if best != nil {
		fmt.Println("*** findLastSnapshotBefore", afterLastMap, best.lastBlock)
	} else {
		fmt.Println("*** findLastSnapshotBefore", afterLastMap, "none")
	}
	return best
}

func (f *FilterMaps) findLastMapBoundaryBefore(afterLastMap uint32) (nextMap uint32, startBlock, startLvPtr uint64, err error) {
	//fmt.Println("flmbb", afterLastMap)
	//fmt.Println(" indexed", f.firstRenderedMap, f.afterLastRenderedMap, f.firstIndexedBlock, f.afterLastIndexedBlock)
	if !f.initialized {
		return 0, 0, 0, nil
	}
	mapIndex := afterLastMap
	for {
		var ok bool
		if mapIndex, ok = f.lastMapBoundaryBefore(mapIndex); !ok {
			return 0, 0, 0, nil
		}
		//fmt.Println(" lmbb", mapIndex)
		lastBlock, _, err := f.getLastBlockOfMap(mapIndex)
		if err != nil {
			fmt.Println(" glbm err", err)
			return 0, 0, 0, err
		}
		if lastBlock >= f.indexedView.headNumber() || lastBlock >= f.targetView.headNumber() ||
			!matchViews(f.indexedView, f.targetView, lastBlock) {
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
	fmt.Println("renderMapsFromSnapshot", cp.lastBlock)
	f.testSnapshotUsed = true
	iter, err := f.newLogIteratorFromBlockDelimiter(cp.lastBlock)
	if err != nil {
		fmt.Println(" rmfs err", err)
		return nil, err
	}
	return &mapRenderer{
		f: f,
		currentMap: &renderedMap{
			filterMap:   cp.filterMap.copy(),
			mapIndex:    cp.mapIndex,
			lastBlock:   cp.lastBlock,
			blockLvPtrs: cp.blockLvPtrs,
		},
		finishedMaps:      make(map[uint32]*renderedMap),
		firstFinished:     cp.mapIndex,
		afterLastFinished: cp.mapIndex,
		afterLastMap:      math.MaxUint32,
		iterator:          iter,
	}, nil
}

func (f *FilterMaps) renderMapsFromMapBoundary(firstMap, afterLastMap uint32, startBlock, startLvPtr uint64) (*mapRenderer, error) {
	//fmt.Println(" newLogIteratorFromMapBoundary start")
	iter, err := f.newLogIteratorFromMapBoundary(firstMap, startBlock, startLvPtr)
	//fmt.Println(" newLogIteratorFromMapBoundary done")
	if err != nil {
		return nil, err
	}
	return &mapRenderer{
		f: f,
		currentMap: &renderedMap{
			filterMap: f.emptyFilterMap(),
			mapIndex:  firstMap,
			lastBlock: iter.blockNumber,
		},
		finishedMaps:      make(map[uint32]*renderedMap),
		firstFinished:     firstMap,
		afterLastFinished: firstMap,
		afterLastMap:      afterLastMap,
		iterator:          iter,
	}, nil
}

func (r *mapRenderer) makeSnapshot() {
	r.f.renderSnapshots.Add(r.iterator.blockNumber, &renderedMap{
		filterMap:     r.currentMap.filterMap.copy(),
		mapIndex:      r.currentMap.mapIndex,
		lastBlock:     r.iterator.blockNumber,
		lastBlockId:   r.f.targetView.getBlockId(r.currentMap.lastBlock),
		blockLvPtrs:   r.currentMap.blockLvPtrs,
		finished:      true,
		headDelimiter: r.iterator.lvIndex,
	})
	fmt.Println("*** added snapshot", r.iterator.blockNumber)
}

func (f *FilterMaps) loadHeadSnapshot() error {
	fm, err := f.getFilterMap(f.afterLastRenderedMap - 1)
	if err != nil {
		return err
	}
	lastBlock, _, err := f.getLastBlockOfMap(f.afterLastRenderedMap - 1)
	if err != nil {
		return err
	}
	var firstBlock uint64
	if f.afterLastRenderedMap > 1 {
		prevLastBlock, _, err := f.getLastBlockOfMap(f.afterLastRenderedMap - 2)
		if err != nil {
			return err
		}
		firstBlock = prevLastBlock + 1
	}
	lvPtrs := make([]uint64, lastBlock+1-firstBlock)
	for i := range lvPtrs {
		lvPtrs[i], err = f.getBlockLvPointer(firstBlock + uint64(i))
		if err != nil {
			return err
		}
	}
	f.renderSnapshots.Add(f.targetBlockNumber, &renderedMap{
		filterMap:     fm,
		mapIndex:      f.afterLastRenderedMap - 1,
		lastBlock:     f.targetBlockNumber,
		lastBlockId:   f.targetBlockId,
		blockLvPtrs:   lvPtrs,
		finished:      true,
		headDelimiter: f.headBlockDelimiter,
	})
	fmt.Println("*** loaded head snapshot", f.targetBlockNumber)
	return nil
}

func (r *mapRenderer) renderMaps(stopFn func() bool) (bool, error) {
	for {
		if done, err := r.renderCurrentMap(stopFn); !done {
			fmt.Println("rm interrupt", done, err)
			return done, err // stopped or failed
		}
		// map finished
		r.finishedMaps[r.currentMap.mapIndex] = r.currentMap
		r.afterLastFinished++
		if len(r.finishedMaps) >= maxMapsPerBatch {
			if err := r.writeFinishedMaps(); err != nil {
				fmt.Println("rm wfm1 err", err)
				return false, err
			}
		}
		if r.afterLastFinished == r.afterLastMap || r.iterator.finished {
			if err := r.writeFinishedMaps(); err != nil {
				fmt.Println("rm wfm2 err", err)
				return false, err
			}
			return true, nil
		}
		r.currentMap = &renderedMap{
			filterMap: r.f.emptyFilterMap(),
			mapIndex:  r.afterLastFinished,
		}
	}
}

func (r *mapRenderer) renderCurrentMap(stopFn func() bool) (bool, error) {
	if !r.iterator.updateChainView(r.f.targetView) {
		return false, errChainUpdate
	}
	epoch := r.currentMap.mapIndex >> r.f.logMapsPerEpoch
	var waitCnt int

	fmt.Println("renderCurrentMap", r.currentMap.mapIndex)
	if r.iterator.lvIndex == 0 {
		r.currentMap.blockLvPtrs = []uint64{0}
	}
	for r.iterator.lvIndex < uint64(r.currentMap.mapIndex+1)<<r.f.logValuesPerMap && !r.iterator.finished {
		waitCnt++
		if waitCnt >= valuesPerCallback {
			if stopFn() {
				return false, nil
			}
			waitCnt = 0
		}
		r.currentMap.lastBlock = r.iterator.blockNumber
		//fmt.Println(" lastBlock", r.currentMap.lastBlock)
		if r.iterator.delimiter {
			r.currentMap.lastBlock++
			//fmt.Println(" blockLvPtr", r.iterator.lvIndex)
			r.currentMap.blockLvPtrs = append(r.currentMap.blockLvPtrs, r.iterator.lvIndex+1)
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
		if !r.f.testDisableSnapshots && r.afterLastMap >= r.f.afterLastRenderedMap &&
			(r.iterator.delimiter || r.iterator.finished) {
			r.makeSnapshot()
		}
	}
	if r.iterator.finished {
		r.currentMap.finished = true
		r.currentMap.headDelimiter = r.iterator.lvIndex
	}
	r.currentMap.lastBlockId = r.f.targetView.getBlockId(r.currentMap.lastBlock)
	fmt.Println("renderCurrentMap done", r.currentMap.mapIndex, r.currentMap.finished, r.currentMap.headDelimiter)
	return true, nil
}

func (r *mapRenderer) writeFinishedMaps() error {
	if len(r.finishedMaps) == 0 {
		return nil
	}
	r.f.indexLock.Lock()
	defer r.f.indexLock.Unlock()

	batch := r.f.db.NewBatch()
	oldRange := r.f.filterMapsRange
	if err := r.updateRange(batch); err != nil {
		fmt.Println("updateRange err", err)
		return err
	}
	// add or update filter rows
	for rowIndex := uint32(0); rowIndex < r.f.mapHeight; rowIndex++ {
		for mapIndex := r.firstFinished; mapIndex < r.afterLastFinished; mapIndex++ {
			row := r.finishedMaps[mapIndex].filterMap[rowIndex]
			if fm, _ := r.f.filterMapCache.Get(mapIndex); fm != nil && row.Equal(fm[rowIndex]) {
				continue
			}
			r.f.storeFilterMapRow(batch, mapIndex, rowIndex, row)
		}
		if r.f.afterLastRenderedMap == r.afterLastFinished { // head updated; remove future entries
			for mapIndex := r.afterLastFinished; mapIndex < oldRange.afterLastRenderedMap; mapIndex++ {
				if fm, _ := r.f.filterMapCache.Get(mapIndex); fm != nil && len(fm[rowIndex]) == 0 {
					continue
				}
				r.f.storeFilterMapRow(batch, mapIndex, rowIndex, nil)
			}
		}
	}
	// update filter map cache
	if r.f.afterLastRenderedMap == r.afterLastFinished {
		for mapIndex := r.firstFinished; mapIndex < r.afterLastFinished; mapIndex++ {
			r.f.filterMapCache.Add(mapIndex, r.finishedMaps[mapIndex].filterMap)
		}
		for mapIndex := r.afterLastFinished; mapIndex < oldRange.afterLastRenderedMap; mapIndex++ {
			r.f.filterMapCache.Remove(mapIndex)
		}
	} else {
		for mapIndex := r.firstFinished; mapIndex < r.afterLastFinished; mapIndex++ {
			r.f.filterMapCache.Remove(mapIndex)
		}
	}
	// add or update block pointers
	blockNumber := r.finishedMaps[r.firstFinished].firstBlock()
	for mapIndex := r.firstFinished; mapIndex < r.afterLastFinished; mapIndex++ {
		renderedMap := r.finishedMaps[mapIndex]
		r.f.storeLastBlockOfMap(batch, mapIndex, renderedMap.lastBlock, renderedMap.lastBlockId)
		//fmt.Println("storeLastBlockOfMap", mapIndex, renderedMap.lastBlock)
		if blockNumber != renderedMap.firstBlock() {
			panic("non-continuous block numbers")
		}
		for _, lvPtr := range renderedMap.blockLvPtrs {
			r.f.storeBlockLvPointer(batch, blockNumber, lvPtr)
			//fmt.Println("storeBlockLvPointer", blockNumber, lvPtr)
			blockNumber++
		}
	}
	if r.f.afterLastRenderedMap == r.afterLastFinished { // head updated; remove future entries
		for mapIndex := r.afterLastFinished; mapIndex < oldRange.afterLastRenderedMap; mapIndex++ {
			r.f.deleteLastBlockOfMap(batch, mapIndex)
		}
		for ; blockNumber < oldRange.afterLastIndexedBlock; blockNumber++ {
			r.f.deleteBlockLvPointer(batch, blockNumber)
		}
	}
	r.finishedMaps = make(map[uint32]*renderedMap)
	r.firstFinished = r.afterLastFinished
	//return batch.Write()  //TODO
	batch.Write()
	//fmt.Println("write range", r.f.filterMapsRange)
	if r.f.afterLastRenderedMap == r.f.firstRenderedMap {
		return nil
	}
	if _, err := r.f.getBlockLvPointer(r.f.firstIndexedBlock); err != nil { //TODO remove, add test
		panic(err)
	}
	if _, err := r.f.getBlockLvPointer(r.f.afterLastIndexedBlock - 1); err != nil {
		panic(err)
	}
	if _, _, err := r.f.getLastBlockOfMap(r.f.firstRenderedMap); err != nil {
		panic(err)
	}
	if r.f.firstRenderedMap > 0 {
		if _, _, err := r.f.getLastBlockOfMap(r.f.firstRenderedMap - 1); err != nil {
			panic(err)
		}
	}
	if _, _, err := r.f.getLastBlockOfMap(r.f.afterLastRenderedMap - 1); err != nil {
		panic(err)
	}
	return nil
}

func (r *mapRenderer) updateRange(batch ethdb.Batch) error {
	// update filterMapsRange
	newRange := r.f.filterMapsRange
	fmt.Println("addRenderedRange", r.firstFinished, r.afterLastFinished, r.afterLastMap)
	fmt.Println(" before", newRange)
	if err := r.addRenderedRange(&newRange); err != nil {
		return err
	}
	fmt.Println(" after", newRange)
	if newRange.firstRenderedMap != r.f.firstRenderedMap {
		// first rendered map changed; update first indexed block
		if newRange.firstRenderedMap > 0 {
			lastBlock, _, err := r.f.getLastBlockOfMap(newRange.firstRenderedMap - 1)
			if err != nil {
				fmt.Println(" lastBlock err", err)
				return err
			}
			newRange.firstIndexedBlock = lastBlock + 1
			fmt.Println(" firstIndexedBlock", newRange.firstIndexedBlock)
		} else {
			newRange.firstIndexedBlock = 0
		}
	}
	if newRange.afterLastRenderedMap == r.afterLastFinished {
		// last rendered map replaced; update last indexed block and head pointers
		if r.f.targetView == nil {
			panic("xxxxxxxxxx")
		}
		r.f.indexedView = r.f.targetView
		newRange.targetBlockNumber = r.f.targetView.headNumber()
		newRange.targetBlockId = r.f.targetView.getBlockId(newRange.targetBlockNumber)
		newRange.afterLastRenderedMap = r.afterLastFinished
		lm := r.finishedMaps[r.afterLastFinished-1]
		//fmt.Println("writeFinishedMaps afterLastFinished finished", r.afterLastFinished, lm.finished)
		if lm.finished {
			newRange.afterLastIndexedBlock = newRange.targetBlockNumber + 1
			if lm.lastBlock != newRange.targetBlockNumber {
				//fmt.Println("xxx", r.afterLastFinished, lm.lastBlock, newRange.targetBlockNumber)
				panic("map rendering finished but last block != head block")
			}
			newRange.headBlockDelimiter = lm.headDelimiter
		} else {
			newRange.afterLastIndexedBlock = lm.lastBlock
			newRange.headBlockDelimiter = 0
		}

	} else {
		// last rendered map not replaced; ensure that target chain view matches
		// indexed chain view on the rendered section
		if lastBlock := r.finishedMaps[r.afterLastFinished-1].lastBlock; !matchViews(r.f.indexedView, r.f.targetView, lastBlock) {
			fmt.Println(" errChainUpdate")
			return errChainUpdate
		}
	}
	fmt.Println(" setRange", newRange)
	r.f.setRange(batch, newRange)
	return nil
}

func (r *mapRenderer) addRenderedRange(fmr *filterMapsRange) error {
	if !fmr.initialized {
		return errors.New("log index not initialized")
	}
	type endpoint struct {
		m uint32
		d int
	}
	endpoints := []endpoint{{fmr.firstRenderedMap, 1}, {fmr.afterLastRenderedMap, -1}, {r.firstFinished, 1}, {r.afterLastFinished, -101}, {r.afterLastMap, 100}}
	if fmr.tailPartialEpoch > 0 {
		endpoints = append(endpoints, []endpoint{{fmr.firstRenderedMap - r.f.mapsPerEpoch, 1}, {fmr.firstRenderedMap - r.f.mapsPerEpoch + fmr.tailPartialEpoch, -1}}...)
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].m < endpoints[j].m })
	var (
		sum    int
		merged []uint32
		last   bool
	)
	for i, e := range endpoints {
		sum += e.d
		if i < len(endpoints)-1 && endpoints[i+1].m == e.m {
			continue
		}
		if (sum > 0) != last {
			merged = append(merged, e.m)
			last = !last
		}
	}
	if len(merged) == 2 {
		fmr.tailPartialEpoch = 0
		fmr.firstRenderedMap = merged[0]
		fmr.afterLastRenderedMap = merged[1]
		return nil
	}
	if len(merged) == 4 {
		if merged[2] != merged[0]+r.f.mapsPerEpoch {
			return errors.New("invalid tail partial epoch")
		}
		fmr.tailPartialEpoch = merged[1] - merged[0]
		fmr.firstRenderedMap = merged[2]
		fmr.afterLastRenderedMap = merged[3]
		return nil
	}
	return errors.New("invalid number of rendered sections")
}

func (f *FilterMaps) emptyFilterMap() filterMap {
	return make(filterMap, f.mapHeight)
}

type logIterator struct {
	chainView                       chainView
	blockNumber                     uint64
	receipts                        types.Receipts
	blockStart, delimiter, finished bool
	txIndex, logIndex, topicIndex   int
	lvIndex                         uint64
}

var errUnindexedRange = errors.New("unindexed range")

func (f *FilterMaps) newLogIteratorFromBlockDelimiter(blockNumber uint64) (*logIterator, error) {
	if blockNumber > f.targetView.headNumber() {
		return nil, errors.New("iterator entry point after target chain head")
	}
	if blockNumber < f.firstIndexedBlock || blockNumber >= f.afterLastIndexedBlock {
		fmt.Println(" range err", blockNumber, f.firstIndexedBlock, f.afterLastIndexedBlock)
		return nil, errUnindexedRange
	}
	if !matchViews(f.indexedView, f.targetView, blockNumber) {
		return nil, errors.New("target and indexed views diverged at iterator entry point")
	}
	var lvIndex uint64
	if blockNumber == f.targetBlockNumber {
		lvIndex = f.headBlockDelimiter
	} else {
		var err error
		lvIndex, err = f.getBlockLvPointer(blockNumber + 1)
		if err != nil {
			return nil, err
		}
		lvIndex--
	}
	finished := blockNumber == f.targetView.headNumber()
	return &logIterator{
		chainView:   f.targetView,
		blockNumber: blockNumber,
		finished:    finished,
		delimiter:   !finished,
		lvIndex:     lvIndex,
	}, nil
}

func (f *FilterMaps) newLogIteratorFromMapBoundary(mapIndex uint32, startBlock, startLvPtr uint64) (*logIterator, error) {
	if startBlock > f.targetView.headNumber() {
		return nil, errors.New("iterator entry point after target chain head")
	}
	if !matchViews(f.indexedView, f.targetView, startBlock) {
		return nil, errors.New("target and indexed views diverged at iterator entry point")
	}
	// get block receipts
	receipts := f.targetView.getReceipts(startBlock)
	if receipts == nil {
		return nil, errors.New("receipts not found")
	}
	// initialize iterator at block start
	l := &logIterator{
		chainView:   f.targetView,
		blockNumber: startBlock,
		receipts:    receipts,
		blockStart:  true,
		lvIndex:     startLvPtr,
	}
	l.nextValid()
	targetIndex := uint64(mapIndex) << f.logValuesPerMap
	if l.lvIndex > targetIndex {
		panic("last map block's lvPointer > map boundary")
	}
	// iterate to map boundary
	for l.lvIndex < targetIndex {
		//fmt.Println(l, f.indexedView.headNumber(), f.targetView.headNumber())
		if l.finished {
			panic("iterator finished") //TODO log error
		}
		if err := l.next(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *logIterator) updateChainView(cv chainView) bool {
	if !matchViews(cv, l.chainView, l.blockNumber) {
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
		l.receipts = l.chainView.getReceipts(l.blockNumber)
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
	if l.blockNumber == l.chainView.headNumber() {
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
