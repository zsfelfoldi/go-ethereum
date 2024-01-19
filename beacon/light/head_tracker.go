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

package light

import (
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/log"
)

// HeadTracker keeps track of the latest validated head and the "prefetch" head
// which is the (not necessarily validated) head announced by the majority of
// servers.
type HeadTracker struct {
	lock            sync.RWMutex
	committeeChain  *CommitteeChain
	minSignerCount  int
	validatedHead   types.FinalityUpdate
	headSignerCount int
	prefetchHead    types.HeadInfo
	changeCounter   uint64
}

// NewHeadTracker creates a new HeadTracker.
func NewHeadTracker(committeeChain *CommitteeChain, minSignerCount int) *HeadTracker {
	return &HeadTracker{
		committeeChain: committeeChain,
		minSignerCount: minSignerCount,
	}
}

// ValidatedHead returns the latest validated head.
func (h *HeadTracker) ValidatedHead() (types.FinalityUpdate, bool) {
	h.lock.RLock()
	defer h.lock.RUnlock()

	return h.validatedHead, h.validatedHead.Attested.Header != (types.Header{})
}

// Validate validates the given signed head. If the head is successfully validated
// and it is better than the old validated head (higher slot or same slot and more
// signers) then ValidatedHead is updated. The boolean return flag signals if
// ValidatedHead has been changed.
func (h *HeadTracker) Validate(head types.FinalityUpdate) (bool, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	if err := head.Validate(); err != nil {
		return false, err
	}
	signerCount := head.Signature.SignerCount()
	if signerCount < h.minSignerCount {
		return false, errors.New("low signer count")
	}
	if head.Attested.Header.Slot < h.validatedHead.Attested.Header.Slot ||
		(head.Attested.Header.Slot == h.validatedHead.Attested.Header.Slot && signerCount <= h.headSignerCount) {
		return false, nil
	}
	sigOk, age, err := h.committeeChain.VerifySignedHeader(head.SignedHeader())
	if err != nil {
		return false, err
	}
	if age < 0 {
		log.Warn("Future signed head received", "age", age)
	}
	if age > time.Minute*2 {
		log.Warn("Old signed head received", "age", age)
	}
	if !sigOk {
		return false, errors.New("invalid header signature")
	}
	h.validatedHead, h.headSignerCount = head, signerCount
	h.changeCounter++
	return true, nil
}

// PrefetchHead returns the latest known prefetch head's head info.
// This head can be used to start fetching related data hoping that it will be
// validated soon.
// Note that the prefetch head cannot be validated cryptographically so it should
// only be used as a performance optimization hint.
func (h *HeadTracker) PrefetchHead() types.HeadInfo {
	h.lock.RLock()
	defer h.lock.RUnlock()

	return h.prefetchHead
}

// SetPrefetchHead sets the prefetch head info.
// Note that HeadTracker does not verify the prefetch head, just acts as a thread
// safe bulletin board.
func (h *HeadTracker) SetPrefetchHead(head types.HeadInfo) {
	h.lock.Lock()
	defer h.lock.Unlock()

	if head == h.prefetchHead {
		return
	}
	h.prefetchHead = head
	h.changeCounter++
}

func (h *HeadTracker) ChangeCounter() uint64 {
	h.lock.RLock()
	defer h.lock.RUnlock()

	return h.changeCounter
}
