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
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

type mapStorage struct {
	params       *Params
	db           ethdb.KeyValueStore
	lock         sync.Mutex
	valid, dirty rangeSet[uint32] // valid and dirty maps in database
	overlay      rangeSet[uint32] // memory maps
	maps         map[uint32]*finishedMap
	// current write cycle
	epoch        uint32
	deleteAll    bool
	writeMaps    map[uint32]*finishedMap
	writePattern []writePatterItem
}

type writePatterItem struct {
	mapIndex, dbLayer uint32
	keepRows          []int // keep existing rows of layer group
}

func newMapStorage(params *Params, db ethdb.KeyValueStore) *mapStorage {
	m := &mapStorage{
		params:    params,
		db:        db,
		triggerCh: make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
		maps:      make(map[uint32]*finishedMap),
	}
	m.loadMapRange()
	m.closeWg.Add(1)
	go m.eventLoop()
	return m
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
	for i := range writeMaps.Iter() {
		m.writeMaps[i] = m.maps[i]
	}
	m.writePattern = nil
	updateMaps := writeMaps.union(deleteMaps)
	for dbLayer, groupSize := range m.params.rowGroupSize {
		updateGroups := updateMaps
		if groupSize > 1 {
			updateGroups = make(rangeSet[uint32], len(updateMaps))
			for i, r := range updateMaps {
				first, last := r.First()/groupSize, r.Last()/groupSize
				updateGroups[i] = common.NewRange[uint32](first, last+1-first)
			}
			updateGroups.normalize()
		}
		for i := range updateGroups.Iter() {
			var keepRows []int
			if groupSize > 1 {
				for j := range groupSize {
					if keepMaps.includes(i*groupSize + j) {
						keepRows = append(keepRows, j)
					}
				}
			}
			m.writePattern = append(m.writePattern, writePatterItem{
				mapIndex: i * groupSize,
				dbLayer:  dbLayer,
				keepRows: keepRows,
			})
		}
	}
	sort.Slice(m.writePattern, func(i, j int) bool {
		return m.writePattern[i].mapIndex < m.writePattern[j].mapIndex ||
			(m.writePattern[i].mapIndex == m.writePattern[j].mapIndex && m.writePattern[i].dbLayer < m.writePattern[j].dbLayer)
	})
	m.valid = m.valid.intersection(invWriteMaps)
	m.dirty = m.dirty.union(writeMaps)
	storeRange = true
	return true
}

func (m *mapStorage) processWriteCycle(stopCallback func() bool) (bool, error) {
	if m.deleteAll {
		return m.deleteEpoch(m.epoch, stopCallback)
	}
	batch := m.db.NewBatch()
	rowsPerBatch := max(maxWritesPerBatch/len(m.writePattern), 1)
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

	var valid, dirty rangeSet[uint32]
	for mapIndex, fm := range m.writeMaps {
		if m.maps[mapIndex] == fm {
			delete(m.maps, mapIndex)
			valid = append(valid, common.NewRange[uint32](mapIndex, 1))
		} else {
			dirty = append(dirty, common.NewRange[uint32](mapIndex, 1))
		}
	}
	valid.normalize()
	dirty.normalize()
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

func (m *mapStorage) writeRowUpdates(batch ethdb.Batch, rowIndex uint32) error {
	for _, w := range m.writePattern {
		if groupSize := m.params.rowGroupSize[w.dbLayer]; groupSize == 1 {
			var row FilterRow
			if fm := m.writeMaps[w.mapIndex]; fm != nil {
				row = fm.getRow(rowIndex, m.params.maxRowLength(w.dbLayer))
			}
			var from uint32
			if w.dbLayer > 0 {
				from = m.params.maxRowLength(w.dbLayer - 1)
			}
			if len(row) > from {
				row = row[from:]
			} else {
				row = nil
			}
			rawdb.WriteFilterMapSingleRow(batch, m.params.mapRowIndex(w.mapIndex, rowIndex), w.dbLayer, row, m.params.logMapWidth)
		} else {
			rows := make([]FilterRow, groupSize)
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
				from = m.params.maxRowLength(w.dbLayer - 1)
			}
			to := m.params.maxRowLength(w.dbLayer)
			for i := range groupSize {
				if fm := m.writeMaps[w.mapIndex+i]; fm != nil {
					if row := fm.getRow(rowIndex, to); len(row) > from {
						rows[i] = row[from:]
					}
				}
			}
			rawdb.WriteFilterMapRowGroup(batch, m.params.mapRowIndex(w.mapIndex, rowIndex), w.dbLayer, rows, m.params.logMapWidth)
		}
	}
	return nil
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
	for i := range dr.intersection(m.overlay).Iter() {
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
