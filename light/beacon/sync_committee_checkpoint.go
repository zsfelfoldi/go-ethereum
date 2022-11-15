// Copyright 2022 The go-ethereum Authors
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

package beacon

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// CheckpointData contains known committee roots based on a weak subjectivity checkpoint
type CheckpointData struct {
	Checkpoint     common.Hash
	Period         uint64
	CommitteeRoots []common.Hash
}

// LightClientInitData contains light sync initialization data based on a weak subjectivity checkpoint
type LightClientInitData struct {
	GenesisData
	CheckpointData
}

// sctInitBackend retrieves light sync initialization data based on a weak subjectivity checkpoint hash
type sctInitBackend interface {
	GetInitData(ctx context.Context, checkpoint common.Hash) (Header, LightClientInitData, error)
}

// WeakSubjectivityCheckpoint implements SctConstraints in a way that it fixes the committee belonging to the checkpoint and allows
// forward extending the committee chain indefinitely. If a parent constraint is specified then it is applied for committee periods
// older than the checkpoint period, also allowing backward syncing the committees.
// Note that light clients typically do not need to backward sync, this feature is intended for nodes serving other clients that might
// have an earlier checkpoint.
type WeakSubjectivityCheckpoint struct {
	lock sync.RWMutex

	parent                              SctConstraints // constraints applied to pre-checkpoint history (no old committees synced if nil)
	db                                  ethdb.KeyValueStore
	initData                            LightClientInitData
	initialized                         bool
	initTriggerCh, parentInitCh, stopCh chan struct{}
	initCallback                        func(GenesisData)
	updateCallback                      func()
}

// NewWeakSubjectivityCheckpoint creates a WeakSubjectivityCheckpoint that either initializes itself from the specified
// sctInitBackend based on the given checkpoint or from the database if the same checkpoint has been fetched before.
func NewWeakSubjectivityCheckpoint(db ethdb.KeyValueStore, backend sctInitBackend, checkpoint common.Hash, parent SctConstraints) *WeakSubjectivityCheckpoint {
	wsc := &WeakSubjectivityCheckpoint{
		parent:        parent,
		db:            db,
		initTriggerCh: make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
	}
	if parent != nil {
		wsc.parentInitCh = make(chan struct{})
	}

	var haveInitData bool
	if enc, err := db.Get(initDataKey); err == nil {
		var initData LightClientInitData
		if err := rlp.DecodeBytes(enc, &initData); err == nil {
			if initData.Checkpoint == checkpoint || initData.Checkpoint == (common.Hash{}) {
				log.Info("Beacon chain initialized with stored checkpoint", "checkpoint", initData.Checkpoint)
				wsc.initData = initData
				haveInitData = true
			}
		} else {
			log.Error("Error decoding stored beacon checkpoint", "error", err)
		}
	}
	if !haveInitData && checkpoint == (common.Hash{}) {
		return nil
	}
	go func() {
		var initData LightClientInitData
		if !haveInitData {
		loop:
			for {
				select {
				case <-wsc.stopCh:
					return
				case <-wsc.initTriggerCh:
					ctx, _ := context.WithTimeout(context.Background(), time.Second*20)
					log.Info("Requesting beacon init data", "checkpoint", checkpoint)
					var (
						header Header
						err    error
					)
					if header, initData, err = backend.GetInitData(ctx, checkpoint); err == nil {
						log.Info("Successfully initialized beacon chain", "checkpoint", checkpoint, "slot", header.Slot)
						break loop
					} else {
						log.Warn("Failed to retrieve beacon init data", "error", err)
					}
				}
			}
		}
		if wsc.parentInitCh != nil {
			select {
			case <-wsc.stopCh:
				return
			case <-wsc.parentInitCh:
			}
		}
		wsc.lock.Lock()
		wsc.initData = initData
		wsc.initialized = true
		initCallback := wsc.initCallback
		wsc.initCallback = nil
		wsc.lock.Unlock()
		if initCallback != nil {
			initCallback(initData.GenesisData)
		}
	}()
	return wsc
}

// init initializes the checkpoint with the given init data
func (wsc *WeakSubjectivityCheckpoint) init(initData LightClientInitData) {
	wsc.lock.Lock()
	if enc, err := rlp.EncodeToBytes(&initData); err == nil {
		wsc.db.Put(initDataKey, enc)
	} else {
		log.Error("Error encoding initData", "error", err)
	}
	wsc.initData, wsc.initialized = initData, true
	updateCallback, initCallback := wsc.updateCallback, wsc.initCallback
	wsc.lock.Unlock()
	if initCallback != nil {
		initCallback(initData.GenesisData)
	}
	updateCallback()
}

// PeriodRange implements SctConstraints
func (wsc *WeakSubjectivityCheckpoint) PeriodRange() (first, afterFixed, afterLast uint64) {
	wsc.lock.RLock()
	defer wsc.lock.RUnlock()

	if !wsc.initialized {
		return
	}
	if wsc.parent != nil {
		first, afterFixed, afterLast = wsc.parent.PeriodRange()
	}
	if afterFixed < wsc.initData.Period {
		first = wsc.initData.Period
	}
	wscAfterLast := wsc.initData.Period + uint64(len(wsc.initData.CommitteeRoots))
	if afterFixed < wscAfterLast {
		afterFixed = wscAfterLast
	}
	afterLast = math.MaxUint64 // no constraints on valid committee updates after the checkpoint
	return
}

// CommitteeRoot implements SctConstraints
func (wsc *WeakSubjectivityCheckpoint) CommitteeRoot(period uint64) (root common.Hash, matchAll bool) {
	wsc.lock.RLock()
	defer wsc.lock.RUnlock()

	if !wsc.initialized {
		return common.Hash{}, false
	}
	switch {
	case period < wsc.initData.Period:
		if wsc.parent != nil {
			return wsc.parent.CommitteeRoot(period)
		}
		return common.Hash{}, false
	case period >= wsc.initData.Period && period < wsc.initData.Period+uint64(len(wsc.initData.CommitteeRoots)):
		return wsc.initData.CommitteeRoots[int(period-wsc.initData.Period)], false
	default:
		return common.Hash{}, true // match all, no constraints on valid committee updates after the checkpoint
	}
}

// SetCallbacks implements SctConstraints
func (wsc *WeakSubjectivityCheckpoint) SetCallbacks(initCallback func(GenesisData), updateCallback func()) {
	wsc.lock.Lock()
	if wsc.initialized {
		wsc.lock.Unlock()
		initCallback(wsc.initData.GenesisData)
	} else {
		wsc.initCallback = initCallback
		wsc.updateCallback = updateCallback
		wsc.lock.Unlock()
	}
	if wsc.parent != nil {
		wsc.parent.SetCallbacks(func(GenesisData) { close(wsc.parentInitCh) }, updateCallback)
	}
}

// TriggerFetch triggers fetching the init data from the backend
func (wsc *WeakSubjectivityCheckpoint) TriggerFetch() {
	select {
	case wsc.initTriggerCh <- struct{}{}:
	default:
	}
}

// Stop should be called after ODR backend shutdown to ensure that init request does not get stuck
func (wsc *WeakSubjectivityCheckpoint) Stop() {
	close(wsc.stopCh)
}
