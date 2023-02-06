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

package sync

import (
	"sync"

	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/light/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const maxUpdateRequest = 8

type singleRequestLock struct {
	lock        sync.Mutex
	requestLock map[syncServer]bool // true if locked for all servers
	globalCount int                 // number of true items in requesting map
}

func (s *singleRequestLock) canRequest(server syncServer) bool {
	if s.globalCount != 0 {
		return false
	}
	_, ok := s.requestLock[server]
	return !ok
}

// assumes that canRequest returned true
func (s *singleRequestLock) requesting(server syncServer) {
	if s.requestLock == nil {
		s.requestLock = make(map[syncServer]bool)
	}
	s.requestLock[server] = true
	s.globalCount++
}

func (s *singleRequestLock) requestDone(server syncServer) {
	if s.requestLock[server] {
		s.globalCount--
	}
	delete(s.requestLock, server)
}

// implements syncModule
func (s *singleRequestLock) timeout(reqId interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	server, ok := reqId.(syncServer)
	if !ok {
		log.Error("singleRequestLock: wrong request id type")
		return
	}
	if s.requestLock[server] { // do not add lock back if it has already been removed
		s.requestLock[server] = false // after soft timeout allow other servers to retry
		s.globalCount--
	}
}

// implements syncModule
func (s *singleRequestLock) removedServer(server syncServer) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.requestDone(server)
}

type checkpointInitServer interface {
	syncServer
	CanRequestBootstrap() bool
	RequestBootstrap(checkpointHash common.Hash, response func(*light.CheckpointData))
}

type CheckpointInit struct {
	singleRequestLock
	chain          *light.CommitteeChain
	cs             *light.CheckpointStore
	checkpointHash common.Hash
	initialized    bool
}

func NewCheckpointInit(chain *light.CommitteeChain, cs *light.CheckpointStore, checkpointHash common.Hash) *CheckpointInit {
	return &CheckpointInit{
		chain:          chain,
		cs:             cs,
		checkpointHash: checkpointHash,
	}
}

func (s *CheckpointInit) process(servers []syncServer) (changed bool, reqId interface{}, reqDone chan struct{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.initialized {
		return
	}
	if checkpoint := s.cs.Get(s.checkpointHash); checkpoint != nil {
		checkpoint.InitChain(s.chain)
		s.initialized = true
		return true, nil, nil
	}
	srv := selectServer(servers, func(server syncServer) uint64 {
		if cserver, ok := server.(checkpointInitServer); ok && cserver.CanRequestBootstrap() && s.canRequest(server) {
			return 1
		}
		return 0
	})
	if srv == nil {
		return
	}
	server := srv.(checkpointInitServer)
	reqDone = make(chan struct{})
	s.requesting(server)
	server.RequestBootstrap(s.checkpointHash, func(checkpoint *light.CheckpointData) {
		s.lock.Lock()
		defer s.lock.Unlock()

		s.requestDone(server)
		close(reqDone)
		if checkpoint == nil || !checkpoint.Validate() {
			server.Fail("error retrieving checkpoint data")
			return
		}
		checkpoint.InitChain(s.chain)
		s.cs.Store(checkpoint)
		s.initialized = true
	})
	return false, server, reqDone
}

type forwardUpdateServer interface {
	syncServer
	UpdateRange() types.PeriodRange
	RequestUpdates(first, count uint64, response func([]*types.LightClientUpdate, []*types.SerializedCommittee))
}

type ForwardUpdateSyncer struct {
	singleRequestLock
	chain *light.CommitteeChain
}

func NewForwardUpdateSyncer(chain *light.CommitteeChain) *ForwardUpdateSyncer {
	return &ForwardUpdateSyncer{chain: chain}
}

func (s *ForwardUpdateSyncer) process(servers []syncServer) (changed bool, reqId interface{}, reqDone chan struct{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	first, ok := s.chain.NextSyncPeriod()
	if !ok {
		return
	}
	srv := selectServer(servers, func(server syncServer) uint64 {
		if server, ok := server.(forwardUpdateServer); ok && s.canRequest(server) {
			updateRange := server.UpdateRange()
			if first < updateRange.First {
				return 0
			}
			return updateRange.AfterLast
		}
		return 0
	})
	if srv == nil {
		return
	}
	server := srv.(forwardUpdateServer)
	updateRange := server.UpdateRange()
	if updateRange.AfterLast <= first {
		return
	}
	reqDone = make(chan struct{})
	s.requesting(server)
	count := updateRange.AfterLast - first
	if count > maxUpdateRequest { //TODO const
		count = maxUpdateRequest
	}
	server.RequestUpdates(first, count, func(updates []*types.LightClientUpdate, committees []*types.SerializedCommittee) {
		s.lock.Lock()
		defer s.lock.Unlock()

		s.requestDone(server)
		close(reqDone)

		if len(updates) != int(count) || len(committees) != int(count) {
			server.Fail("wrong number of updates received")
			return
		}
		for i, update := range updates {
			if update.Header.SyncPeriod() != first+uint64(i) {
				server.Fail("update with wrong sync period received")
				return
			}
			if err := s.chain.InsertUpdate(update, committees[i]); err != nil {
				if err == light.ErrInvalidUpdate || err == light.ErrWrongCommitteeRoot || err == light.ErrCannotReorg {
					server.Fail("invalid update received")
				} else {
					log.Error("Unexpected InsertUpdate error", "error", err)
				}
				return
			}
		}
	})
	return false, server, reqDone
}
