// Copyright 2023 The go-ethereum Authors
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

package api

import (
	"sync"

	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/light/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const (
	maxHeadLength = 4
)

type SyncServer struct {
	api  *BeaconLightApi
	Stop func()
	lock sync.RWMutex

	triggerCallback     func()
	latestHeads         []types.SignedHead
	canRequestBootstrap bool
	firstUpdate         uint64 //TODO ...
}

func NewSyncServer(api *BeaconLightApi) *SyncServer {
	s := &SyncServer{
		api:                 api,
		canRequestBootstrap: true,
	}
	s.Stop = s.api.StartHeadListener(s.newHead, s.newSignedHead, func(err error) {
		log.Warn("Head event stream error", "err", err)
	})
	return s
}

func (s *SyncServer) SetTriggerCallback(cb func()) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.triggerCallback = cb
}

func (s *SyncServer) Fail(desc string) {
	log.Warn("API endpoint failure", "URL", s.api.url, "error", desc)
}

func (s *SyncServer) LatestHeads() []types.SignedHead {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.latestHeads
}

func (s *SyncServer) CanRequestBootstrap() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.canRequestBootstrap
}

func (s *SyncServer) RequestBootstrap(checkpointHash common.Hash, response func(*light.CheckpointData)) {
	go func() {
		if checkpoint, err := s.api.GetCheckpointData(checkpointHash); err == nil {
			response(checkpoint)
		} else {
			s.lock.Lock()
			s.canRequestBootstrap = false
			s.lock.Unlock()
			response(nil)
		}
	}()
}

func (s *SyncServer) UpdateRange() types.PeriodRange {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if len(s.latestHeads) == 0 {
		return types.PeriodRange{}
	}
	r := types.PeriodRange{First: s.firstUpdate, AfterLast: types.PeriodOfSlot(s.latestHeads[len(s.latestHeads)-1].Header.Slot + 256)}
	if !r.IsEmpty() {
		return r
	}
	return types.PeriodRange{}
}

func (s *SyncServer) RequestUpdates(first, count uint64, response func([]*types.LightClientUpdate, []*types.SerializedCommittee)) {
	go func() {
		if updates, committees, err := s.api.GetBestUpdatesAndCommittees(first, count); err == nil {
			response(updates, committees)
		} else {
			response(nil, nil)
		}
	}()
}

func (s *SyncServer) newHead(slot uint64, blockRoot common.Hash) {}

func (s *SyncServer) newSignedHead(signedHead types.SignedHead) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.latestHeads == nil {
		s.latestHeads = []types.SignedHead{signedHead}
		s.triggerCallback()
		return
	}
	if lastHead := s.latestHeads[len(s.latestHeads)-1]; signedHead.Header.Slot < lastHead.Header.Slot ||
		(signedHead.Header.Slot == lastHead.Header.Slot && signedHead.SignerCount() <= lastHead.SignerCount()) {
		return
	}
	if len(s.latestHeads) < maxHeadLength {
		s.latestHeads = append(s.latestHeads, signedHead)
		s.triggerCallback()
		return
	}
	copy(s.latestHeads[:len(s.latestHeads)-1], s.latestHeads[1:])
	s.latestHeads[len(s.latestHeads)-1] = signedHead
	s.triggerCallback()
}
