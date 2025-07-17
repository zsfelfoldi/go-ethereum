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
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

const maxWritesPerBatch = 100000

type mapStorage struct {
	params             *Params
	db                 ethdb.KeyValueStore
	triggerCh, closeCh chan struct{}
	closeWg            sync.WaitGroup

	lock                       sync.Mutex
	valid, dirty               rangeSet[uint32] // valid and dirty maps in database
	overlay                    rangeSet[uint32] // memory maps
	validBlocks, overlayBlocks rangeSet[uint64]
	maps                       map[uint32]*finishedMap

	lastBlockCache *lru.Cache[uint32, lastBlockOfMap]
	lvPointerCache *lru.Cache[uint64, uint64]

	// current write cycle
	epoch        uint32
	deleteAll    bool
	writeMaps    map[uint32]*finishedMap
	writePattern []writePatterItem
}

type writePatterItem struct {
	mapIndex, dbLayer uint32
	keepRows          []uint32 // keep existing rows of layer group
}

func newMapStorage(params *Params, db ethdb.KeyValueStore) *mapStorage {
	m := &mapStorage{
		params:         params,
		db:             db,
		triggerCh:      make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
		maps:           make(map[uint32]*finishedMap),
		lastBlockCache: lru.NewCache[uint32, lastBlockOfMap](cachedLastBlocks),
		lvPointerCache: lru.NewCache[uint64, uint64](cachedLvPointers),
	}
	m.loadMapRange()
	m.closeWg.Add(1)
	go m.eventLoop()
	return m
}

func (m *mapStorage) addMap(mapIndex uint32, fm *finishedMap, forceCommit bool) {
	//TODO block if too many memory maps
	m.lock.Lock()
	defer m.lock.Unlock()

	mm := rangeSet[uint32]{common.NewRange[uint32](mapIndex, 1)}
	if m.valid.includes(mapIndex) {
		m.valid = m.valid.exclude(mm)
		m.dirty = m.dirty.union(mm)
	}
	m.overlay = m.overlay.union(mm)
	m.maps[mapIndex] = fm
	select {
	case m.triggerCh <- struct{}{}:
	default:
	}
}

func (m *mapStorage) deleteMaps(delRange common.Range[uint32]) {
	m.lock.Lock()
	defer m.lock.Unlock()

	dr := rangeSet[uint32]{delRange}
	for i := range dr.intersection(m.overlay).iter() {
		delete(m.maps, i)
	}
	m.overlay = m.overlay.exclude(dr)
	m.dirty = m.dirty.union(dr.intersection(m.valid))
	m.valid = m.valid.exclude(dr)
	select {
	case m.triggerCh <- struct{}{}:
	default:
	}
}

func (m *mapStorage) getBlockLvPointer(blockNumber uint64) (uint64, error) {
	if m.overlayBlocks.includes(blockNumber) {
		for mapIndex, fm := range m.maps { //TODO ??optimize with binary search?
			if mapIndex <= fm.lastBlock.number && mapIndex >= fm.firstBlock() {
				return fm.blockPtrs[mapIndex-fm.firstBlock()], nil
			}
		}
		return 0, rrors.New("memory overlay block pointer not found")
	}
	if m.validBlocks.includes(blockNumber) {
		return m.getBlockLvPointerFromDb(blockNumber)
	}
	return 0, errors.New("block log value pointer not found")
}

func (m *mapStorage) getLastBlockOfMap(mapIndex uint32) (uint64, common.Hash, error) {
	if m.overlay.includes(mapIndex) {
		fm := m.maps[mapIndex]
		if fm == nil {
			return 0, common.Hash{}, errors.New("memory overlay map not found")
		}
		return fm.lastBlock.number, fm.lastBlock.id, nil
	}
	if m.valid.includes(mapIndex) || m.valid.includes(mapIndex+1) {
		return m.getLastBlockOfMapFromDb(mapIndex)
	}
	return 0, common.Hash{}, errors.New("last block of map not found")
}

func (m *mapStorage) loadMapRange() {
	panic("TODO")
}

func (m *mapStorage) storeMapRange(valid, dirty rangeSet[uint32]) {
	panic("TODO")
}

func (m *mapStorage) eventLoop() {
	for {
		select {
		case <-m.triggerCh:
			for m.startWriteCycle() {
				done, err := m.processWriteCycle(func() bool {
					select {
					case <-m.triggerCh:
					case <-m.closeCh:
						return true
					default:
					}
					return false

				})
				if done {
					m.finishWriteCycle()
				} else {
					if err != nil {
						log.Error("Error processing log index write cycle", "error", err)
					}
					m.resetWriteCycle()
					break
				}
			}
		case <-m.closeCh:
			m.closeWg.Done()
			return
		}
	}
}

func (m *mapStorage) stop() {
	close(m.closeCh)
	m.closeWg.Wait()
}

func (m *mapStorage) startWriteCycle() bool {
	m.lock.Lock()
	var storeRange bool
	defer func() {
		valid, dirty := m.valid, m.dirty
		m.lock.Unlock()
		if storeRange {
			m.storeMapRange(valid, dirty)
		}
	}()

	if len(m.overlay) > 0 {
		m.epoch = m.overlay[len(m.overlay)-1].First() >> m.params.logMapsPerEpoch
	} else if len(m.dirty) > 0 {
		m.epoch = m.dirty[len(m.dirty)-1].First() >> m.params.logMapsPerEpoch
	} else {
		return false
	}
	epochRange := rangeSet[uint32]{common.NewRange[uint32](m.epoch<<m.params.logMapsPerEpoch, m.params.mapsPerEpoch)}
	writeMaps := epochRange.intersection(m.overlay)
	deleteMaps := epochRange.intersection(m.dirty).exclude(writeMaps)
	keepMaps := epochRange.intersection(m.valid).exclude(writeMaps)
	m.deleteAll = len(writeMaps) == 0 && len(keepMaps) == 0
	if m.deleteAll {
		return true
	}
	m.writeMaps = make(map[uint32]*finishedMap)
	for i := range writeMaps.iter() {
		m.writeMaps[i] = m.maps[i]
	}
	m.writePattern = nil
	updateMaps := writeMaps.union(deleteMaps)
	for dbLayer, groupSize := range m.params.rowGroupSize {
		updateGroups := updateMaps
		if groupSize > 1 {
			updateRb := make(rangeBoundaries[uint32], 0, len(updateMaps)*2)
			for _, r := range updateMaps {
				first, last := r.First()/groupSize, r.Last()/groupSize
				updateRb.add(common.NewRange[uint32](first, last+1-first), 1)
			}
			updateGroups = updateRb.makeSet(1)
		}
		for i := range updateGroups.iter() {
			var keepRows []uint32
			if groupSize > 1 {
				for j := range groupSize {
					if keepMaps.includes(i*groupSize + j) {
						keepRows = append(keepRows, j)
					}
				}
			}
			m.writePattern = append(m.writePattern, writePatterItem{
				mapIndex: i * groupSize,
				dbLayer:  uint32(dbLayer),
				keepRows: keepRows,
			})
		}
	}
	sort.Slice(m.writePattern, func(i, j int) bool {
		return m.writePattern[i].mapIndex < m.writePattern[j].mapIndex ||
			(m.writePattern[i].mapIndex == m.writePattern[j].mapIndex && m.writePattern[i].dbLayer < m.writePattern[j].dbLayer)
	})
	m.valid = m.valid.exclude(writeMaps)
	m.dirty = m.dirty.union(writeMaps)
	storeRange = true
	return true
}

func (m *mapStorage) processWriteCycle(stopCallback func() bool) (bool, error) {
	if m.deleteAll {
		return m.deleteEpoch(m.epoch, stopCallback)
	}
	batch := m.db.NewBatch()
	rowsPerBatch := uint32(max(maxWritesPerBatch/len(m.writePattern), 1))
	for rowIndex := range m.params.mapHeight {
		if err := m.writeRowUpdates(batch, rowIndex); err != nil {
			return false, err
		}
		if rowIndex%rowsPerBatch == rowsPerBatch-1 {
			if err := batch.Write(); err != nil {
				return false, err
			}
			if stopCallback() {
				return false, nil
			}
			batch = m.db.NewBatch()
		}
	}
	if err := batch.Write(); err != nil {
		return false, err
	}
	return true, nil
}

func (m *mapStorage) finishWriteCycle() {
	m.lock.Lock()
	defer func() {
		valid, dirty := m.valid, m.dirty
		m.lock.Unlock()
		m.storeMapRange(valid, dirty)
	}()

	var validRb, dirtyRb rangeBoundaries[uint32]
	for mapIndex, fm := range m.writeMaps {
		if m.maps[mapIndex] == fm {
			delete(m.maps, mapIndex)
			validRb.add(common.NewRange[uint32](mapIndex, 1), 1)
		} else {
			dirtyRb.add(common.NewRange[uint32](mapIndex, 1), 1)
		}
	}
	valid := validRb.makeSet(1)
	dirty := dirtyRb.makeSet(1)
	epochRange := rangeSet[uint32]{common.NewRange[uint32](m.epoch<<m.params.logMapsPerEpoch, m.params.mapsPerEpoch)}
	m.valid = m.valid.union(valid)
	m.overlay = m.overlay.exclude(valid)
	m.dirty = m.dirty.exclude(epochRange).union(dirty)
	m.writeMaps, m.writePattern = nil, nil
}

func (m *mapStorage) resetWriteCycle() {
	m.lock.Lock()
	m.writeMaps, m.writePattern = nil, nil
	m.lock.Unlock()
}

func (m *mapStorage) deleteEpoch(epoch uint32, stopCallback func() bool) (bool, error) {
	panic("TODO")
}

func (m *mapStorage) writeRowUpdates(batch ethdb.Batch, rowIndex uint32) error {
	for _, w := range m.writePattern {
		if groupSize := m.params.rowGroupSize[w.dbLayer]; groupSize == 1 {
			var row FilterRow
			if fm := m.writeMaps[w.mapIndex]; fm != nil {
				row = fm.getRow(rowIndex, m.params.maxRowLength[w.dbLayer])
			}
			var from uint32
			if w.dbLayer > 0 {
				from = m.params.maxRowLength[w.dbLayer-1]
			}
			if uint32(len(row)) > from {
				row = row[from:]
			} else {
				row = nil
			}
			rawdb.WriteFilterMapSingleRow(batch, m.params.mapRowIndex(w.mapIndex, rowIndex), w.dbLayer, row, m.params.logMapWidth)
		} else {
			rows := make([][]uint32, groupSize)
			if w.keepRows != nil {
				oldRows, err := rawdb.ReadFilterMapRowGroup(m.db, m.params.mapRowIndex(w.mapIndex, rowIndex), w.dbLayer, groupSize, m.params.logMapWidth)
				if err != nil {
					return err
				}
				for _, i := range w.keepRows {
					rows[i] = oldRows[i]
				}
			}
			var from uint32
			if w.dbLayer > 0 {
				from = m.params.maxRowLength[w.dbLayer-1]
			}
			to := m.params.maxRowLength[w.dbLayer]
			for i := range groupSize {
				if fm := m.writeMaps[w.mapIndex+i]; fm != nil {
					if row := fm.getRow(rowIndex, to); uint32(len(row)) > from {
						rows[i] = row[from:]
					}
				}
			}
			rawdb.WriteFilterMapRowGroup(batch, m.params.mapRowIndex(w.mapIndex, rowIndex), w.dbLayer, rows, m.params.logMapWidth)
		}
	}
	return nil
}

// getBlockLvPointer returns the starting log value index where the log values
// generated by the given block are located.
//
// Note that this function assumes that the indexer read lock is being held when
// called from outside the indexerLoop goroutine.
func (m *mapStorage) getBlockLvPointerFromDb(blockNumber uint64) (uint64, error) {
	if lvPointer, ok := f.lvPointerCache.Get(blockNumber); ok {
		return lvPointer, nil
	}
	lvPointer, err := rawdb.ReadBlockLvPointer(f.db, blockNumber)
	if err != nil {
		return 0, fmt.Errorf("failed to retrieve log value pointer of block %d: %v", blockNumber, err)
	}
	f.lvPointerCache.Add(blockNumber, lvPointer)
	return lvPointer, nil
}

// storeBlockLvPointer stores the starting log value index where the log values
// generated by the given block are located.
func (m *mapStorage) storeBlockLvPointerInDb(batch ethdb.Batch, blockNumber, lvPointer uint64) {
	f.lvPointerCache.Add(blockNumber, lvPointer)
	rawdb.WriteBlockLvPointer(batch, blockNumber, lvPointer)
}

// deleteBlockLvPointer deletes the starting log value index where the log values
// generated by the given block are located.
func (m *mapStorage) deleteBlockLvPointerFromDb(batch ethdb.Batch, blockNumber uint64) {
	f.lvPointerCache.Remove(blockNumber)
	rawdb.DeleteBlockLvPointer(batch, blockNumber)
}

// getLastBlockOfMap returns the number and id of the block that generated the
// last log value entry of the given map.
func (m *mapStorage) getLastBlockOfMapFromDb(mapIndex uint32) (uint64, common.Hash, error) {
	if lastBlock, ok := f.lastBlockCache.Get(mapIndex); ok {
		return lastBlock.number, lastBlock.id, nil
	}
	number, id, err := rawdb.ReadFilterMapLastBlock(f.db, mapIndex)
	if err != nil {
		return 0, common.Hash{}, fmt.Errorf("failed to retrieve last block of map %d: %v", mapIndex, err)
	}
	f.lastBlockCache.Add(mapIndex, lastBlockOfMap{number: number, id: id})
	return number, id, nil
}

// storeLastBlockOfMap stores the number of the block that generated the last
// log value entry of the given map.
func (m *mapStorage) storeLastBlockOfMapInDb(batch ethdb.Batch, mapIndex uint32, number uint64, id common.Hash) {
	f.lastBlockCache.Add(mapIndex, lastBlockOfMap{number: number, id: id})
	rawdb.WriteFilterMapLastBlock(batch, mapIndex, number, id)
}

// deleteLastBlockOfMap deletes the number of the block that generated the last
// log value entry of the given map.
func (m *mapStorage) deleteLastBlockOfMapFromDb(batch ethdb.Batch, mapIndex uint32) {
	f.lastBlockCache.Remove(mapIndex)
	rawdb.DeleteFilterMapLastBlock(batch, mapIndex)
}
