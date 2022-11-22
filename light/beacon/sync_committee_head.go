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
	"bytes"
	"math/bits"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

// SignedHead represents a beacon header signed by a sync committee
type SignedHead struct {
	BitMask   []byte
	Signature []byte
	Header    Header
}

// signerCount returns the number of individual signers in the signature aggregate
func (s *SignedHead) signerCount() int {
	if len(s.BitMask) != 64 {
		return 0 // signature check will filter it out later but we calculate this before sig check
	}
	var signerCount int
	for _, v := range s.BitMask {
		signerCount += bits.OnesCount8(v)
	}
	return signerCount
}

// Equal returns true if both the headers and the signer sets are the same
func (s *SignedHead) Equal(s2 *SignedHead) bool {
	return s.Header == s2.Header && bytes.Equal(s.BitMask, s2.BitMask) && bytes.Equal(s.Signature, s2.Signature)
}

// AddSignedHeads adds signed heads to the tracker if the syncing process has been finished;
// adds them to a deferred list otherwise that is processed when the syncing is finished.
func (s *SyncCommitteeTracker) AddSignedHeads(peer sctServer, heads []SignedHead) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if sp := s.connected[peer]; sp != nil && (sp.requesting || sp.queued) {
		sp.deferredHeads = append(sp.deferredHeads, heads...)
		return
	}
	s.addSignedHeads(peer, heads)
}

// addSignedHeads adds signed heads to the tracker after a successful verification
// (it is assumed that the local update chain has been synced with the given peer)
func (s *SyncCommitteeTracker) addSignedHeads(peer sctServer, heads []SignedHead) {
	var oldHeadHash common.Hash
	if len(s.acceptedList.list) > 0 {
		oldHeadHash = s.acceptedList.list[0].hash
	}
	for _, head := range heads {
		signerCount := head.signerCount()
		if signerCount < s.signerThreshold {
			continue
		}
		sigOk, age := s.verifySignature(head)
		if age < 0 {
			log.Warn("Future signed head received", "age", age)
		}
		if age > time.Minute*2 {
			log.Warn("Old signed head received", "age", age)
		}
		if !sigOk {
			peer.WrongReply("invalid header signature")
			continue
		}
		hash := head.Header.Hash()
		if h := s.acceptedList.getHead(hash); h != nil {
			h.receivedFrom[peer] = struct{}{}
			if signerCount > h.signerCount {
				h.head = head
				h.signerCount = signerCount
				h.sentTo = nil
				s.acceptedList.updateHead(h)
			}
		} else {
			h := &headInfo{
				head:         head,
				hash:         hash,
				sentTo:       make(map[sctClient]struct{}),
				receivedFrom: map[sctServer]struct{}{peer: struct{}{}},
			}
			s.acceptedList.updateHead(h)
		}
	}
	if len(s.acceptedList.list) > 0 && oldHeadHash != s.acceptedList.list[0].hash {
		head := s.acceptedList.list[0].head.Header
		for _, subFn := range s.headSubs {
			subFn(head)
		}
	}
}

// verifySignature returns true if the given signed head has a valid signature according to the local
// committee chain. The caller should ensure that the committees advertised by the same source where
// the signed head came from are synced before verifying the signature.
// The age of the header is also returned (the time elapsed since the beginning of the given slot,
// according to the local system clock). If enforceTime is true then negative age (future) headers
// are rejected.
func (s *SyncCommitteeTracker) verifySignature(head SignedHead) (bool, time.Duration) {
	slotTime := int64(time.Second) * int64(s.genesisTime+uint64(head.Header.Slot)*12)
	age := time.Duration(s.unixNano() - slotTime)
	if s.enforceTime && age < 0 {
		return false, age
	}
	committee := s.getSyncCommittee(uint64(head.Header.Slot+1) >> 13) // signed with the next slot's committee
	if committee == nil {
		return false, age
	}
	return s.sigVerifier.verifySignature(committee, s.forks.signingRoot(head.Header), head.BitMask, head.Signature), age
}

// SubscribeToNewHeads subscribes the given callback function to head beacon headers with a verified valid sync committee signature.
func (s *SyncCommitteeTracker) SubscribeToNewHeads(subFn func(Header)) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.headSubs = append(s.headSubs, subFn)
}

// headInfo contains the best signed header and the state of propagation belonging to a given block root
type headInfo struct {
	head         SignedHead
	hash         common.Hash
	signerCount  int
	receivedFrom map[sctServer]struct{}
	sentTo       map[sctClient]struct{}
	processed    bool
}

// headList is a list of best known heads for the few most recent slots
// Note: usually only the highest slot is interesting but in case of low signer participation or
// slow propagation/aggregation of signatures it might make sense to keep track of multiple heads
// as different clients might have different tradeoff preferences between delay and security.
type headList struct {
	list  []*headInfo // highest slot first
	limit int
}

// newHeadList creates a new headList
func newHeadList(limit int) headList {
	return headList{limit: limit}
}

// getHead returns the headInfo belonging to the given block root
func (h *headList) getHead(hash common.Hash) *headInfo {
	//return h.hashMap[hash]
	for _, headInfo := range h.list {
		if headInfo.hash == hash {
			return headInfo
		}
	}
	return nil
}

// updateHead adds or updates the given headInfo in the list
func (h *headList) updateHead(head *headInfo) {
	for i, hh := range h.list {
		if hh.head.Header.Slot <= head.head.Header.Slot {
			if hh.head.Header.Slot < head.head.Header.Slot {
				if len(h.list) < h.limit {
					h.list = append(h.list, nil)
				}
				copy(h.list[i+1:len(h.list)], h.list[i:len(h.list)-1])
			}
			h.list[i] = head
			return
		}
	}
	if len(h.list) < h.limit {
		h.list = append(h.list, head)
	}
}
