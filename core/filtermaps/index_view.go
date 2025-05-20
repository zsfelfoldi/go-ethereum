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
	"sync"
)

const (
	maxMapsPerBatch   = 32    // maximum number of maps rendered in memory
	valuesPerCallback = 1024  // log values processed per event process callback
	cachedRowMappings = 10000 // log value to row mappings cached during rendering

	// Number of rows written to db in a single batch.
	// The map renderer splits up writes like this to ensure that regular
	// block processing latency is not affected by large batch writes.
	rowsPerBatch = 1024
)

// always rendered until block boundary
type IndexView struct {
	f              *FilterMaps
	chainView      *ChainView
	lock           sync.RWMutex
	maps           common.Range[uint32]
	blocks         common.Range[uint64] // all blocks fully rendered
	lastDelimiter  uint64               // belongs to blocks.Last()
	dbMapsBefore   uint32
	dbBlocksBefore uint64
	overlay        []*memoryMap
}

type renderedView struct {
	IndexView              // last block might be partially rendered
	nextLogValue    uint64 // equal to lastDelimiter if last block is fully rendered
	rowMappingCache *lru.Cache[common.Hash, lvPosition]
}

type lvPosition struct{ rowIndex, layerIndex uint32 }

type memoryMap struct {
	filterMap   filterMap
	lastBlock   uint64
	lastBlockId common.Hash
	blockLvPtrs []uint64 // start pointers of blocks starting in this map; last one is lastBlock
}

func (m *memoryMap) fastCopy() *memoryMap {
	return &memoryMap{
		filterMap:   m.filterMap.fastCopy(),
		lastBlock:   m.lastBlock,
		lastBlockId: m.lastBlockId,
		blockLvPtrs: m.blockLvPtrs,
	}
}

func (m *memoryMap) fullCopy() *memoryMap {
	return &memoryMap{
		filterMap:   m.filterMap.fullCopy(),
		lastBlock:   m.lastBlock,
		lastBlockId: m.lastBlockId,
		blockLvPtrs: slices.Clone(m.blockLvPtrs),
	}
}

func (f *FilterMaps) newRenderedView(databaseMaps common.Range[uint32], chainView *ChainView) (*renderedView, error) {
	rv := &renderedView{
		IndexView: IndexView{
			f:         f,
			chainView: chainView,
			maps:      databaseMaps,
		},
	}
	sharedMaps, err := iv.sharedMapRange(chainView)
	if err != nil {
		return nil, err
	}
	rv.maps = sharedMaps
	if err := rv.setBlockRange(); err != nil {
		return nil, err
	}
	rv.dbMapsBefore = iv.maps.AfterLast()
	rv.dbBlocksBefore = iv.blocks.AfterLast()
	rv.nextLogValue = uint64(rv.dbMapsBefore) << f.logValuesPerMap
	return rv, nil
}

func (rv *renderedView) needWriteMaps() (bool, common.Range[uint32]) {}

func (rv *renderedView) writeMapRows() (bool, error) {}

func (rv *renderedView) makeImmutableView() *IndexView {
	return rv.IndexView.copy(false)
}

func (iv *IndexView) makeRenderedView() *renderedView {
	return &renderedView{
		IndexView: *iv.copy(true),
		nextLogValue: iv.headDelimiter,
	}
}

func (iv *IndexView) copy(fullMapCopy bool) *IndexView {
	c := &IndexView{
		f:              iv.f,
		chainView:      iv.chainView,
		maps:           iv.maps,
		blocks:         iv.blocks,
		lastDelimiter:  iv.lastDelimiter,
		dbMapsBefore:   iv.dbMapsBefore,
		dbBlocksBefore: iv.dbBlocksBefore,
		overlay:        slices.Clone(iv.overlay),
	}
	if len(c.overlay) > 0 {
		if fullMapCopy {
			c.overlay[len(c.overlay)-1] = c.overlay[len(c.overlay)-1].fullCopy()
		} else {
			c.overlay[len(c.overlay)-1] = c.overlay[len(c.overlay)-1].fastCopy()
		}
	}
	return c
}

func (iv *IndexView) setChainView(newView *ChainView) error {
	maps, err := iv.sharedMapRange(newView)
	if err != nil {
		return err
	}
	iv.maps = maps
	if err := iv.setBlockRange(); err != nil {
		return err
	}
	return nil
}

func (iv *IndexView) headRendered() bool {
	return iv.blocks.AfterLast() == iv.chainView.HeadNumber()+1 && iv.nextLogValue == iv.lastDelimiter
}

func (iv *IndexView) renderNextBlock(limit uint64) error {
	for blockNumber := iv.blocks.AfterLast(); blockNumber <= iv.chainView.HeadNumber(); blockNumber++ {
		if iv.nextLogValue >= limit {
			break
		}
		receipts := iv.chainView.RawReceipts(blockNumber)
		if receipts == nil {
			return fmt.Errorf("receipts not found for block %d", blockNumber)
		}
		iv.addBlock(receipts, blockNumber == 0, iv.nextLogValue-iv.lastDelimiter, limit)
	}
	return nil
}

func (iv *IndexView) addLogValue(logValue common.Hash) {
	mapIndex := uint32(iv.nextLogValue >> iv.f.logValuesPerMap)
	var rm *memoryMap
	if mapIndex < iv.maps.AfterLast() {
		rm = iv.overlay[mapIndex-iv.dbMapsBefore]
	} else {
		rm = &memoryMap{
			filterMap: iv.f.emptyFilterMap(),
			mapIndex:  mapIndex,
		}
		iv.maps.SetLast(mapIndex)
		iv.overlay = append(iv.overlay, rm)
		if iv.rowMappingCache == nil {
			iv.rowMappingCache = lru.NewCache[common.Hash, lvPosition](cachedRowMappings)
		} else {
			iv.rowMappingCache.Purge()
		}
	}
	if logValue != (common.Hash{}) {
		lvp, cached := iv.rowMappingCache.Get(logValue)
		if !cached {
			lvp = lvPosition{rowIndex: iv.f.rowIndex(mapIndex, 0, logValue)}
		}
		for uint32(len(rm.filterMap[lvp.rowIndex])) >= iv.f.maxRowLength(lvp.layerIndex) {
			lvp.layerIndex++
			lvp.rowIndex = iv.f.rowIndex(mapIndex, lvp.layerIndex, logValue)
			cached = false
		}
		rm.filterMap[lvp.rowIndex] = append(rm.filterMap[lvp.rowIndex], iv.f.columnIndex(iv.nextLogValue, &logValue))
		if !cached {
			iv.rowMappingCache.Add(logValue, lvp)
		}
	}
	iv.nextLogValue++
}

func (iv *IndexView) addBlock(receipts types.Receipts, isGenesis bool, skipFirst, limit uint64) {
	if iv.nextLogValue >= limit {
		return
	}
	if !isGenesis {
		if skipFirst == 0 {
			iv.addLogValue(common.Hash{}) // parent block delimiter
		} else {
			skipFirst--
		}
	}
	for _, l := range logIterator(receipts, iv.nextLogValue, iv.f.valuesPerMap) {
		if iv.nextLogValue >= limit {
			return
		}
		if skipFirst == 0 {
			iv.addLogValue(l.valueHash())
		} else {
			skipFirst--
		}
	}
	iv.blocks.SetLast(iv.blocks.AfterLast())
}

type logAndTopicIndex struct {
	log        *types.Log
	topicIndex int
}

func (l logAndIndex) log() *types.Log {
	if l.topicIndex == 0 {
		return l.log
	}
	return nil
}

func (l logAndIndex) valueHash() common.Hash {
	if l.topicIndex == 0 {
		return addressValue(l.log.Address)
	}
	return topicValue(l.log.Topics[l.topicIndex-1])
}

func logIterator(receipts types.Receipts, lvIndex, valuesPerMap uint64) iter.Seq2[uint64, logAndTopicIndex] {
	return func(yield func(uint64, logAndTopicIndex) bool) {
		for _, receipt := range receipts {
			for _, log := range receipt.Logs {
				valueCount := len(log.Topics) + 1
				if (lvIndex&(valuesPerMap-1))+uint64(valueCount) > valuesPerMap {
					for lvIndex&(valuesPerMap-1) != 0 {
						if !yield(lvIndex, logAndTopicIndex{}) {
							return
						}
						lvIndex++
					}
				}
				for topicIndex := range valueCount {
					if !yield(lvIndex, logAndTopicIndex{log: log, topicIndex: topicIndex}) {
						return
					}
					lvIndex++
				}
			}
		}
	}
}

// GetParams returns the filtermaps parameters.
func (iv *IndexView) GetParams() *Params {
	return &iv.f.Params
}

func (rv *renderedView) setBlockRange() error {
	var first, afterLast uint64
	if iv.maps.First() > 0 {
		lastBlock, _, _, err := iv.getLastBlockOfMap(sharedMaps.First() - 1)
		if err != nil {
			return err
		}
		first = lastBlock + 1
	}
	if iv.maps.AfterLast() > 0 {
		lastBlock, _, finished, err := iv.getLastBlockOfMap(sharedMaps.AfterLast() - 1)
		if err != nil {
			return err
		}
		afterLast = lastBlock
		if finished {
			afterLast++
		}
	}
	if afterLast > first {
		iv.blocks = common.NewRange[uint64](first, afterLast-first)
	} else {
		iv.blocks = common.Range[uint64]{}
	}
	return nil
}

// zero length range means there are no shared maps but there is still a suitable initialization point
func (iv *IndexView) sharedMapRange(chainView *ChainView) (common.Range[uint32], error) {
	sharedMaps := iv.maps
	for sharedMaps.AfterLast() > 0 {
		lastBlock, lastBlockId, err := iv.getLastBlockOfMap(sharedMaps.AfterLast() - 1) // not sharedMaps.Last() because it should work when sharedMaps.Count() == 0
		if err != nil {
			return common.Range[uint32]{}, err
		}
		if chainView.BlockId(lastBlock) == lastBlockId {
			return sharedMaps, nil
		}
		if sharedMaps.IsEmpty() {
			break
		}
		sharedMaps.SetAfterLast(sharedMaps.Last())
	}
	return common.Range[uint32]{}, nil
}

func (iv *IndexView) SharedBlockRange(chainView *ChainView) common.Range[uint64] {
	return iv.chainView.SharedRange(chainView).Intersection(iv.blocks)
}

// GetFilterMapRows fetches a set of filter map rows at the corresponding map
// indices and a shared row index. If baseLayerOnly is true then only the first
// baseRowLength entries are returned.
func (iv *IndexView) GetFilterMapRows(mapIndices []uint32, rowIndex uint32, baseLayerOnly bool) ([]FilterRow, error) {
	iv.lock.RLock()
	defer iv.lock.RUnlock()

	dbMaps := len(mapIndices)
	for dbMaps > 0 && mapIndices[dbMaps-1] >= iv.dbMapsBefore {
		dbMaps--
	}
	res, err := iv.f.getFilterMapRows(mapIndices[:dbMaps], rowIndex, baseLayerOnly)
	if err != nil {
		return nil, err
	}
	for i := dbMaps; i < len(mapIndices); i++ {
		var row FilterRow
		if j := mapIndices[i] - iv.dbMapsBefore; j < uint32(len(iv.overlay)) {
			row = iv.overlay[j].fm[rowIndex]
		}
		res = append(res, row)
	}
	return res, nil
}

// GetBlockLvPointer returns the starting log value index where the log values
// generated by the given block are located.
func (iv *IndexView) GetBlockLvPointer(blockNumber uint64) (uint64, error) {
	iv.lock.RLock()
	defer iv.lock.RUnlock()

	if blockNumber < iv.dbBlocksBefore {
		return iv.f.getBlockLvPointer(blockNumber)
	}
	for _, mm := range iv.overlay {
		if blockNumber <= mm.lastBlock {
			return mm.blockLvPtrs[blockNumber-mm.firstBlock()], nil
		}
	}
	return 0, errUnindexedRange
}

// getLastBlockOfMap returns the number and id of the block that generated the
// last log value entry of the given map. It also returns a boolean flag that
// signals if the last block is fully rendered.
func (iv *IndexView) getLastBlockOfMap(mapIndex uint32) (uint64, common.Hash, bool, error) {
	iv.lock.RLock()
	defer iv.lock.RUnlock()

	if mapIndex < iv.dbMapsBefore {
		lastBlock, lastBlockId, err := iv.f.getLastBlockOfMap(mapIndex)
		return lastBlock, lastBlockId, false, err
	}
	if i := mapIndex - iv.dbMapsBefore; i < uint32(len(iv.overlay)) {
		mm := iv.overlay[i]
		return mm.lastBlock, mm.lastBlockId, mm.finished, nil
	}
	return 0, nil, errUnindexedRange
}

// GetLogByLvIndex returns the log at the given log value index. If the index does
// not point to the first log value entry of a log then no log and no error are
// returned as this can happen when the log value index was a false positive.
func (iv *IndexView) GetLogByLvIndex(lvIndex uint64) (*types.Log, error) {
	mapIndex := uint32(lvIndex >> f.logValuesPerMap)
	if !iv.maps.Includes(mapIndex) {
		return nil, nil
	}
	// find possible block range based on map to block pointers
	lastBlockNumber, _, err := iv.getLastBlockOfMap(mapIndex)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve last block of map %d containing searched log value index %d: %v", mapIndex, lvIndex, err)
	}
	var firstBlockNumber uint64
	if mapIndex > 0 {
		firstBlockNumber, _, err = iv.getLastBlockOfMap(mapIndex - 1)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve last block of map %d before searched log value index %d: %v", mapIndex, lvIndex, err)
		}
	}
	if firstBlockNumber < iv.blocks.First() {
		firstBlockNumber = iv.blocks.First()
	}
	// find block with binary search based on block to log value index pointers
	for firstBlockNumber < lastBlockNumber {
		midBlockNumber := (firstBlockNumber + lastBlockNumber + 1) / 2
		midLvPointer, err := iv.GetBlockLvPointer(midBlockNumber)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve log value pointer of block %d while binary searching log value index %d: %v", midBlockNumber, lvIndex, err)
		}
		if lvIndex < midLvPointer {
			lastBlockNumber = midBlockNumber - 1
		} else {
			firstBlockNumber = midBlockNumber
		}
	}
	// get block receipts
	receipts := iv.chainView.Receipts(firstBlockNumber)
	if receipts == nil {
		return nil, fmt.Errorf("failed to retrieve receipts for block %d containing searched log value index %d: %v", firstBlockNumber, lvIndex, err)
	}
	lvPointer, err := iv.GetBlockLvPointer(firstBlockNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve log value pointer of block %d containing searched log value index %d: %v", firstBlockNumber, lvIndex, err)
	}
	// iterate through receipts to find the exact log starting at lvIndex
	for lvi, l := range logIterator(receipts, lvPointer, iv.f.valuesPerMap) {
		if lvi == lvIndex {
			return l.log(), nil
		}
	}
	return nil, nil
}
