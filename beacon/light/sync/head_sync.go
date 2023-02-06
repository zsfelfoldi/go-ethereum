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
)

type signedHeadServer interface {
	syncServer
	LatestHeads() []types.SignedHead
}

type latestHeads struct {
	heads      map[uint64]types.SignedHead
	oldestSlot uint64
}

type HeadSyncer struct {
	lock          sync.Mutex
	headTracker   *light.HeadTracker
	chain         *light.CommitteeChain
	added, queued latestHeads
}

func NewHeadSyncer(headTracker *light.HeadTracker, chain *light.CommitteeChain) *HeadSyncer {
	return &HeadSyncer{
		headTracker: headTracker,
		chain:       chain,
	}
}

func (s *HeadSyncer) process(servers []syncServer) (changed bool, reqId interface{}, reqDone chan struct{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	nextPeriod, ok := s.chain.NextSyncPeriod()
	if !ok {
		return
	}
	for slot, head := range s.queued.heads {
		if head.Header.SyncPeriod() <= nextPeriod {
			delete(s.queued.heads, slot)
			if s.added.add(head) && s.headTracker.Add(head) == nil {
				changed = true
			}
		}
	}
	for _, server := range servers {
		if server, ok := server.(signedHeadServer); ok {
			heads := server.LatestHeads()
			for _, head := range heads {
				if head.Header.SyncPeriod() > nextPeriod {
					s.queued.add(head)
				} else if s.added.add(head) {
					if s.headTracker.Add(head) == nil {
						changed = true
					} else {
						server.Fail("received invalid signed head")
						break
					}
				}
			}
		}
	}
	return
}

func (s *HeadSyncer) timeout(reqId interface{})       {}
func (s *HeadSyncer) removedServer(server syncServer) {}

func (l *latestHeads) add(head types.SignedHead) bool {
	if l.heads == nil {
		l.heads = make(map[uint64]types.SignedHead)
		l.oldestSlot = head.Header.Slot
	}
	if oldHead, ok := l.heads[head.Header.Slot]; ok {
		if head.SignerCount() <= oldHead.SignerCount() {
			return false
		}
	}
	l.heads[head.Header.Slot] = head
	for len(l.heads) > 4 {
		delete(l.heads, l.oldestSlot)
		l.oldestSlot++
	}
	return true
}
