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
	"errors"

	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/light/request"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

// testNode implements checkpointInitServer, updateServer and request.RequestServer
type testNode struct {
	db *memorydb.Database

	checkpointStore *light.CheckpointStore
	committeeChain  *light.CommitteeChain

	headTracker   *request.HeadTracker
	headUpdater   *HeadUpdater
	headValidator *light.HeadValidator

	allowPartialUpdates bool

	scheduler        *request.Scheduler
	newHeadCallbacks []func(uint64, common.Hash)
}

func newTestNode(config *types.ChainConfig, clock *mclock.Simulated, checkpointHash common.Hash) *testNode {
	node := new(testNode)
	node.db = memorydb.New()
	node.committeeChain = light.NewTestCommitteeChain(node.db, config, 300, false, clock)
	node.checkpointStore = light.NewCheckpointStore(node.db, node.committeeChain)
	node.headValidator = light.NewHeadValidator(node.committeeChain)
	node.headUpdater = NewHeadUpdater(node.headValidator, node.committeeChain)
	node.headTracker = request.NewHeadTracker(node.headUpdater.NewSignedHead)
	node.headValidator.Subscribe(350, func(signedHead types.SignedHeader) {
		node.headTracker.SetValidatedHead(signedHead.Header)
	})
	node.scheduler = request.NewScheduler(node.headTracker, clock)
	if checkpointHash != (common.Hash{}) {
		node.scheduler.RegisterModule(NewCheckpointInit(node.committeeChain, node.checkpointStore, checkpointHash))
		node.scheduler.RegisterModule(NewForwardUpdateSync(node.committeeChain))
	}
	node.scheduler.RegisterModule(node.headUpdater)
	node.scheduler.Start()
	return node
}

func (s *testNode) waitOrFail() bool  { return true }
func (s *testNode) wrongAnswer() bool { return false }

//func (s *testNode) stop() bool        {}

func (s *testNode) setHead(head types.Header) {
	for _, cb := range s.newHeadCallbacks {
		cb(head.Slot, head.Hash())
	}
}

func (s *testNode) setSignedHead(head types.SignedHeader) {
	s.headValidator.Add(head)
}

func (s *testNode) SubscribeHeads(newHead func(uint64, common.Hash), newSignedHead func(types.SignedHeader)) {
	s.newHeadCallbacks = append(s.newHeadCallbacks, newHead)
	s.headValidator.Subscribe(350, newSignedHead)
}

func (s *testNode) UnsubscribeHeads()          { panic(nil) } // not implemented
func (s *testNode) DelayUntil() mclock.AbsTime { return 0 }
func (s *testNode) Fail(string)                {}

func (s *testNode) RequestBootstrap(checkpointHash common.Hash, response func(*light.CheckpointData, error)) {
	go func() {
		if s.waitOrFail() {
			response(nil, errors.New("serving failed"))
			return
		}
		if c := s.checkpointStore.Get(checkpointHash); c != nil {
			if s.wrongAnswer() {
				c.Committee[0]++
			}
			response(c, nil)
			return
		}
		response(nil, errors.New("bootstrap not available"))
	}()
}

func (s *testNode) RequestUpdates(first, count uint64, response func([]*types.LightClientUpdate, []*types.SerializedSyncCommittee, error)) {
	go func() {
		if s.waitOrFail() {
			response(nil, nil, errors.New("serving failed"))
			return
		}
		var (
			updates    []*types.LightClientUpdate
			committees []*types.SerializedSyncCommittee
		)
		for period := first; period < first+count; period++ {
			if update, committee := s.committeeChain.GetUpdate(period), s.committeeChain.GetCommittee(period+1); update != nil && committee != nil {
				if s.wrongAnswer() {
					update.NextSyncCommitteeBranch[0][0]++
				}
				updates = append(updates, update)
				committees = append(committees, committee)
			} else if !s.allowPartialUpdates {
				response(nil, nil, errors.New("updates not available"))
				return
			}
		}
		response(updates, committees, nil)
	}()
}
