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
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type mapStorage struct {
	params             *Params
	mapDb              *mapDatabase
	triggerCh, closeCh chan struct{}
	pauseCh            chan bool //TODO
	closeWg            sync.WaitGroup

	lock                       sync.Mutex
	initialized                bool
	valid, dirty               rangeSet[uint32] // valid and dirty maps in database
	overlay                    rangeSet[uint32] // memory maps
	validBlocks, overlayBlocks rangeSet[uint64]
	maps                       map[uint32]*finishedMap
}

func newMapStorage(params *Params, mapDb *mapDatabase) *mapStorage {
	m := &mapStorage{
		params:    params,
		mapDb:     mapDb,
		triggerCh: make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
		pauseCh:   make(chan bool, 1),
		maps:      make(map[uint32]*finishedMap),
	}
	valid, dirty, err := m.mapDb.loadMapRange()
	if err == nil {
		m.valid, m.dirty, m.initialized = valid, dirty, true
	}
	//TODO validBlocks
	m.closeWg.Add(1)
	go m.eventLoop()
	return m
}

func (m *mapStorage) eventLoop() {
	defer m.closeWg.Done()

	var stopped bool
	stopCallback := func() bool {
		select {
		case <-m.triggerCh:
		case <-m.closeCh:
			stopped = true
		case paused := <-m.pauseCh:
			for paused && !stopped {
				select {
				case <-m.triggerCh:
				case <-m.closeCh:
					stopped = true
				case paused = <-m.pauseCh:
				}
			}
		default:
		}
		return stopped
	}

	if !m.initialized {
		if !m.mapDb.reset(stopCallback) {
			return // node stopped before cleaning old database
		}
		m.lock.Lock()
		m.initialized = true
		m.lock.Unlock()
	}

	for !stopped {
		done, err := m.doWriteCycle(stopCallback)
		if err != nil {
			panic("TODO")
		}
		if !done && !stopped { // wait for next event if no changes done
			select {
			case <-m.triggerCh:
			case <-m.closeCh:
				stopped = true
			case paused := <-m.pauseCh:
				for paused && !stopped {
					select {
					case <-m.triggerCh:
					case <-m.closeCh:
						stopped = true
					case paused = <-m.pauseCh:
					}
				}
			}
		}
	}
}

func (m *mapStorage) stop() {
	close(m.closeCh)
	m.closeWg.Wait()
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
		for _, fm := range m.maps { //TODO ??optimize with binary search?
			if fm.blocks().Includes(blockNumber) {
				return fm.blockPtrs[blockNumber-fm.firstBlock()], nil
			}
		}
		return 0, errors.New("memory overlay block pointer not found")
	}
	if m.validBlocks.includes(blockNumber) {
		return m.mapDb.getBlockLvPointer(blockNumber)
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
		return m.mapDb.getLastBlockOfMap(mapIndex)
	}
	return 0, common.Hash{}, errors.New("last block of map not found")
}

func (m *mapStorage) doWriteCycle(stopCallback func() bool) (bool, error) {
	m.lock.Lock()

	var epoch uint32
	if len(m.overlay) > 0 {
		epoch = m.overlay[len(m.overlay)-1].First() >> m.params.logMapsPerEpoch
	} else if len(m.dirty) > 0 {
		epoch = m.dirty[len(m.dirty)-1].First() >> m.params.logMapsPerEpoch
	} else {
		m.lock.Unlock()
		return true, nil
	}
	epochRange := rangeSet[uint32]{common.NewRange[uint32](epoch<<m.params.logMapsPerEpoch, m.params.mapsPerEpoch)}
	writeMaps := epochRange.intersection(m.overlay)
	deleteMaps := epochRange.intersection(m.dirty).exclude(writeMaps)
	validInEpoch := epochRange.intersection(m.valid)
	keepMaps := validInEpoch.exclude(writeMaps)
	if len(writeMaps) == 0 && len(keepMaps) == 0 {
		m.updateRange(m.valid.exclude(epochRange), m.dirty.union(validInEpoch), m.overlay)
		m.lock.Unlock()
		done, err := m.mapDb.deleteEpoch(epoch, stopCallback)
		if done {
			m.lock.Lock()
			m.updateRange(m.valid, m.dirty.exclude(epochRange), m.overlay)
			m.lock.Unlock()
		}
		return done, err
	}
	maps := make(map[uint32]*finishedMap)
	for i := range writeMaps.iter() {
		maps[i] = m.maps[i]
	}
	updateMaps := writeMaps.union(deleteMaps)
	m.updateRange(m.valid.exclude(updateMaps), m.dirty.union(updateMaps), m.overlay)
	m.lock.Unlock()
	done, err := m.mapDb.writeMaps(writeMaps, deleteMaps, keepMaps, maps, stopCallback)
	if done {
		m.lock.Lock()
		var validRb, dirtyRb rangeBoundaries[uint32]
		for mapIndex, fm := range maps {
			if m.maps[mapIndex] == fm {
				delete(m.maps, mapIndex)
				validRb.add(common.NewRange[uint32](mapIndex, 1), 1)
			} else {
				dirtyRb.add(common.NewRange[uint32](mapIndex, 1), 1)
			}
		}
		newValid := validRb.makeSet(1)
		newDirty := dirtyRb.makeSet(1)
		m.updateRange(m.valid.union(newValid), m.dirty.exclude(epochRange).union(newDirty), m.overlay.exclude(newValid))
		m.lock.Unlock()
	}
	return done, err
}

func (m *mapStorage) updateRange(valid, dirty, overlay rangeSet[uint32]) {
	m.valid, m.dirty, m.overlay = valid, dirty, overlay
	m.mapDb.storeMapRange(valid, dirty)
	//TODO validBlocks
}
