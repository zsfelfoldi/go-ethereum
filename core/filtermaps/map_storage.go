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
	"fmt"
	"math"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type mapStorage struct {
	params             *Params
	mapDb              *mapDatabase
	triggerCh, closeCh chan struct{}
	closeWg            sync.WaitGroup

	mtForceWrite, mtOverrideSuspend uint32
	mtBusy1, mtBusy2                uint32

	lock                       sync.RWMutex
	initialized                bool
	knownEpochs                uint32           // epochs initialized with last map block pointer and corresponding reverse block lv pointer
	valid, dirty               rangeSet[uint32] // valid and dirty maps in database
	overlay                    rangeSet[uint32] // memory maps
	overlayCount               uint32
	validBlocks, overlayBlocks rangeSet[uint64]
	writeEpochs                rangeSet[uint32]
	maps                       map[uint32]*finishedMap
	suspend                    bool
}

func newMapStorage(params *Params, mapDb *mapDatabase) *mapStorage {
	m := &mapStorage{
		params:    params,
		mapDb:     mapDb,
		triggerCh: make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
		maps:      make(map[uint32]*finishedMap),

		mtForceWrite:      params.rowGroupSize[0] * 17 / 16,
		mtOverrideSuspend: params.rowGroupSize[0] * 18 / 16,
		mtBusy1:           params.rowGroupSize[0] * 17 / 16,
		mtBusy2:           params.rowGroupSize[0] * 19 / 16,
	}
	if valid, dirty, knownEpochs, ok := m.mapDb.loadMapRange(); ok {
		m.valid, m.dirty, m.knownEpochs, m.initialized = valid, dirty, knownEpochs, true
		if err := m.validUpdated(); err != nil {
			m.resetWithError(fmt.Sprintf("could not initialize valid block range: %v", err))
		}
	}
	m.closeWg.Add(1)
	go m.eventLoop()
	return m
}

func (m *mapStorage) stop() {
	close(m.closeCh)
	m.closeWg.Wait()
}

func (m *mapStorage) busyLevel() int {
	m.lock.Lock()
	defer m.lock.Unlock()

	switch {
	case m.overlayCount < m.mtBusy1:
		return 0
	case m.overlayCount < m.mtBusy2:
		return 1
	default:
		return 2
	}
}

func (m *mapStorage) lastBoundaryBefore(mapIndex uint32) uint32 {
	if mapIndex == 0 {
		return 0
	}
	var lastBoundary uint32
	if m, ok := m.valid.closestLte(mapIndex - 1); ok {
		lastBoundary = m + 1
	}
	if m, ok := m.overlay.closestLte(mapIndex - 1); ok {
		lastBoundary = max(lastBoundary, m+1)
	}
	return lastBoundary
}

func (m *mapStorage) canExtendKnownEpochs(cpList checkpointList) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return uint32(len(cpList)) > m.knownEpochs
}

func (m *mapStorage) matchKnownEpochs(cpList checkpointList) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	if m.knownEpochs == 0 || len(cpList) == 0 {
		return true
	}
	epoch := min(m.knownEpochs, uint32(len(cpList))) - 1
	number, hash, err := m.getLastBlockOfMap(m.params.lastEpochMap(epoch))
	if err != nil {
		m.resetWithError(fmt.Sprintf("could not read last known epoch boundary: %v", err))
		return true
	}
	return number == cpList[epoch].BlockNumber && hash == cpList[epoch].BlockId
}

func (m *mapStorage) addKnownEpochs(cpList checkpointList) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	if uint32(len(cpList)) <= m.knownEpochs {
		return errors.New("checkpoint init list has no new epochs")
	}
	if m.knownEpochs > 0 {
		lastNumber, lastHash, err := m.getLastBlockOfMap(m.params.lastEpochMap(m.knownEpochs - 1))
		if err != nil {
			return err //TODO fmt.Errorf
		}
		lvPointer, err := m.getBlockLvPointer(lastNumber)
		if err != nil {
			return err //TODO fmt.Errorf
		}
		if cp := cpList[m.knownEpochs-1]; cp.BlockNumber != lastNumber || cp.BlockId != lastHash || cp.FirstIndex != lvPointer {
			return errors.New("checkpoint init list does not match known epochs")
		}
	}

	for epoch := m.knownEpochs; epoch < uint32(len(cpList)); epoch++ {
		m.mapDb.storeLastBlockOfMap(m.params.lastEpochMap(epoch), cpList[epoch].BlockNumber, cpList[epoch].BlockId)
		m.mapDb.storeBlockLvPointer(cpList[epoch].BlockNumber, cpList[epoch].FirstIndex)
	}
	m.knownEpochs = uint32(len(cpList))
	m.mapDb.storeMapRange(m.valid, m.dirty, m.knownEpochs)
	return nil
}

func (m *mapStorage) addMap(mapIndex uint32, fm *finishedMap, forceCommit bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if m.valid.includes(mapIndex) || m.overlay.includes(mapIndex) {
		panic("addMap to non-empty map index")
	}
	epoch := m.params.mapEpoch(mapIndex)
	if (epoch > m.knownEpochs || mapIndex != m.params.firstEpochMap(epoch)) &&
		!m.valid.includes(mapIndex-1) && !m.overlay.includes(mapIndex-1) {
		panic("addMap to map index with no known boundary")
	}
	m.overlay = m.overlay.union(rangeSet[uint32]{common.NewRange[uint32](mapIndex, 1)})
	m.overlayUpdated()
	if epoch >= m.knownEpochs && mapIndex == m.params.lastEpochMap(epoch) {
		m.knownEpochs = epoch + 1
	}
	m.maps[mapIndex] = fm
	if forceCommit || (mapIndex+1)%m.params.rowGroupSize[0] == 0 {
		m.writeEpochs = m.writeEpochs.union(rangeSet[uint32]{common.NewRange[uint32](epoch, 1)})
	}
	m.trigger()
}

func (m *mapStorage) revert(mapIndex uint32) {
	m.lock.Lock()
	defer m.lock.Unlock()

	dr := rangeSet[uint32]{common.NewRange[uint32](mapIndex, math.MaxUint32-mapIndex)}
	for i := range dr.intersection(m.overlay).iter() {
		delete(m.maps, i)
	}
	m.overlay = m.overlay.exclude(dr)
	m.overlayUpdated()
	m.dirty = m.dirty.union(dr.intersection(m.valid))
	m.valid = m.valid.exclude(dr)
	m.knownEpochs = min(m.knownEpochs, m.params.mapEpoch(mapIndex))
	if err := m.validUpdated(); err != nil {
		m.resetWithError(fmt.Sprintf("could not revert valid block range: %v", err))
	}
	m.trigger()
}

func (m *mapStorage) suspendOrRelease(suspend bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.suspend = suspend
	if suspend {
		m.trigger()
	}
}

func (m *mapStorage) eventLoop() {
	defer m.closeWg.Done()

	var stopped bool

	blockingSelect := func() {
		select {
		case <-m.triggerCh:
		case <-m.closeCh:
			stopped = true
		}
	}

	nonBlockingSelect := func() {
		select {
		case <-m.triggerCh:
		case <-m.closeCh:
			stopped = true
		default:
		}
	}

	stopCallback := func() bool {
		m.lock.Lock()
		suspend := m.suspend && m.overlayCount < m.mtOverrideSuspend
		m.lock.Unlock()

		if suspend {
			blockingSelect()
		} else {
			nonBlockingSelect()
		}
		return stopped
	}

	for !stopped {
		if !m.initialized {
			if done, _ := m.mapDb.reset(stopCallback); !done {
				return // node stopped before cleaning old database
			}
			m.lock.Lock()
			m.initialized = true
			m.lock.Unlock()
		}
		done, err := m.doWriteCycle(stopCallback)
		if err != nil {
			m.resetWithError(fmt.Sprintf("could not read last known epoch boundary: %v", err))
			continue
		}
		if !done && !stopped { // wait for next event if no changes done
			blockingSelect()
		}
	}
}

func (m *mapStorage) trigger() {
	select {
	case m.triggerCh <- struct{}{}:
	default:
	}
}

func (m *mapStorage) getBlockLvPointer(blockNumber uint64) (uint64, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

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
	m.lock.RLock()
	defer m.lock.RUnlock()

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

func (m *mapStorage) getFilterMapRows(mapIndices []uint32, rowIndex, layers uint32) ([]FilterRow, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	rows := make([]FilterRow, len(mapIndices))
	dbMaps := make([]uint32, 0, len(mapIndices))
	for i, mapIndex := range mapIndices {
		if m.overlay.includes(mapIndex) {
			fm := m.maps[mapIndex]
			if fm == nil {
				return nil, errors.New("memory overlay map not found") //TODO fmt.Errorf...
			}
			rows[i] = fm.getRow(rowIndex, m.params.getMaxRowLength(layers))
		} else {
			dbMaps = append(dbMaps, mapIndex)
		}
	}
	dbRows, err := m.mapDb.getFilterMapRows(dbMaps, rowIndex, layers)
	if err != nil {
		return nil, err
	}
	var j int
	for i, row := range rows {
		if row == nil { // zero length row is represented as zero length slice
			rows[i] = dbRows[j]
			j++
		}
	}
	if j != len(mapIndices) {
		panic("rows length mismatch")
	}
	return rows, nil
}

// returns nil, nil if map is unknown
func (m *mapStorage) getFilterMap(mapIndex uint32) (*finishedMap, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	if m.overlay.includes(mapIndex) {
		fm := m.maps[mapIndex]
		if fm == nil {
			return nil, errors.New("memory overlay map not found") //TODO fmt.Errorf...
		}
		return fm, nil
	}
	if m.valid.includes(mapIndex) {
		return m.mapDb.getFilterMap(mapIndex)
	}
	return nil, nil
}

func (m *mapStorage) extendPointerRange(deleteRange common.Range[uint32]) common.Range[uint32] {
	epoch := deleteRange.First() >> m.params.logMapsPerEpoch
	if deleteRange.Last()>>m.params.logMapsPerEpoch != epoch {
		panic("deleted map range crosses epoch boundary")
	}
	first := min(m.knownEpochs, epoch) << m.params.logMapsPerEpoch
	if deleteRange.First() > 0 {
		if c, ok := m.valid.closestLte(deleteRange.First() - 1); ok {
			first = max(first, c+1)
		}
	}
	afterLast := uint32(math.MaxUint32)
	if epoch < m.knownEpochs {
		afterLast = (epoch + 1) << m.params.logMapsPerEpoch
	}
	if fa, ok := m.valid.closestGte(deleteRange.AfterLast()); ok {
		afterLast = min(afterLast, fa)
	}
	return common.NewRange[uint32](first, afterLast-first)
}

func (m *mapStorage) selectEpochTriggeredWrite() (uint32, bool) {
	if len(m.writeEpochs) == 0 {
		return 0, false
	}
	return m.writeEpochs[len(m.writeEpochs)-1].First(), true
}

func (m *mapStorage) selectEpochForcedWrite() (uint32, bool) {
	if m.overlayCount < m.mtForceWrite {
		return 0, false
	}
	var longest common.Range[uint32]
	for _, r := range m.overlay {
		if r.Count() > longest.Count() {
			longest = r
		}
	}
	return m.params.mapEpoch(longest.First()), true
}

func (m *mapStorage) mapToEpochRange(mapRange rangeSet[uint32]) rangeSet[uint32] {
	vb := make(rangeBoundaries[uint32], 0, len(mapRange)*2)
	for _, r := range mapRange {
		first := m.params.mapEpoch(r.First())
		last := m.params.mapEpoch(r.Last())
		vb.add(common.NewRange[uint32](first, last+1-first), 1)
	}
	return vb.makeSet(1)
}

func (m *mapStorage) selectEpochOnlyDirty() (uint32, bool) {
	epochs := m.mapToEpochRange(m.dirty).exclude(m.mapToEpochRange(m.overlay))
	if len(epochs) == 0 {
		return 0, false
	}
	return epochs[0].First(), true
}

func (m *mapStorage) doWriteCycle(stopCallback func() bool) (bool, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	// always operate on a single epoch
	epoch, ok := m.selectEpochTriggeredWrite()
	if !ok {
		epoch, ok = m.selectEpochForcedWrite()
		if !ok {
			epoch, ok = m.selectEpochOnlyDirty()
			if !ok {
				return true, nil
			}
		}
	}
	//TODO wait for group boundary unless last map is added
	epochRange := rangeSet[uint32]{common.NewRange[uint32](epoch<<m.params.logMapsPerEpoch, m.params.mapsPerEpoch)}
	writeMaps := epochRange.intersection(m.overlay).singleRange()
	validInEpoch := epochRange.intersection(m.valid).singleRange()
	dirtyInEpoch := epochRange.intersection(m.dirty).singleRange()
	// delete old pointers
	m.mapDb.deletePointers(m.extendPointerRange(dirtyInEpoch), stopCallback)
	if writeMaps.IsEmpty() && validInEpoch.IsEmpty() {
		// delete map rows of entire epoch if nothing to write or keep
		m.lock.Unlock()
		done, err := m.mapDb.deleteEpochRows(epoch, stopCallback)
		m.lock.Lock()
		if done {
			if err := m.updateRange(m.valid, m.dirty.exclude(epochRange), m.overlay); err != nil {
				return false, err
			}
		}
		return done, err
	}
	maps := make(map[uint32]*finishedMap)
	for i := range writeMaps.Iter() {
		maps[i] = m.maps[i]
	}
	// temporarily mark newly written maps as dirty (replaced/deleted maps are already dirty)
	if err := m.updateRange(m.valid, m.dirty.union(rangeSet[uint32]{writeMaps}), m.overlay); err != nil {
		return false, err
	}
	m.lock.Unlock()
	// write/overwrite map rows and delete dirty map data, write new pointers
	done, err := m.mapDb.writeMaps(writeMaps, validInEpoch, dirtyInEpoch, maps, stopCallback)
	m.lock.Lock()
	if !done {
		return false, err
	}
	// check if newly written maps are still valid according to the current memory
	// map overlay and shorten range if some maps have been invalidated
	for mapIndex := range writeMaps.Iter() {
		if m.maps[mapIndex] != maps[mapIndex] {
			writeMaps = common.NewRange[uint32](writeMaps.First(), mapIndex-writeMaps.First())
			break
		}
		delete(m.maps, mapIndex)
	}
	writeMapsRs := rangeSet[uint32]{writeMaps}
	if err := m.updateRange(m.valid.union(writeMapsRs), m.dirty.exclude(writeMapsRs), m.overlay.exclude(writeMapsRs)); err != nil {
		return false, err
	}
	return true, nil
}

func (m *mapStorage) updateRange(valid, dirty, overlay rangeSet[uint32]) error {
	if !valid.equal(m.valid) {
		m.valid = valid
		if err := m.validUpdated(); err != nil {
			return err
		}
	}
	m.dirty = dirty
	if !overlay.equal(m.overlay) {
		m.overlay = overlay
		m.overlayUpdated()
	}
	m.mapDb.storeMapRange(valid, dirty, m.knownEpochs)
	return nil
}

func (m *mapStorage) resetWithError(errStr string) {
	log.Error("Resetting invalid log index database", "error", errStr)
	m.uninitialize()
}

func (m *mapStorage) uninitialize() {
	m.valid, m.dirty, m.overlay, m.knownEpochs, m.initialized = nil, nil, nil, 0, false
	m.mapDb.deleteMapRange()
	m.trigger()
}

func (m *mapStorage) validUpdated() error {
	vb := make(rangeBoundaries[uint64], 0, len(m.valid)*2)
	for _, vr := range m.valid {
		var first uint64
		if vr.First() > 0 {
			lb, _, err := m.mapDb.getLastBlockOfMap(vr.First() - 1)
			if err != nil {
				return err
			}
			first = lb + 1
		}
		last, _, err := m.mapDb.getLastBlockOfMap(vr.Last() - 1)
		if err != nil {
			return err
		}
		vb.add(common.NewRange[uint64](first, last+1-first), 1)
	}
	m.validBlocks = vb.makeSet(1)
	return nil
}

func (m *mapStorage) overlayUpdated() {
	ob := make(rangeBoundaries[uint64], 0, len(m.overlay)*2)
	for _, or := range m.overlay {
		first := m.maps[or.First()].firstBlock()
		last := m.maps[or.Last()].lastBlock.number
		ob.add(common.NewRange[uint64](first, last+1-first), 1)
	}
	m.overlayBlocks = ob.makeSet(1)
	m.overlayCount = m.overlay.count()
}
