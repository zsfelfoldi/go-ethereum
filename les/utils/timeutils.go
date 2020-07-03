// Copyright 2020 The go-ethereum Authors
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

package utils

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

type ThresholdTimer struct {
	clock     mclock.Clock
	lock      sync.Mutex
	last      mclock.AbsTime
	threshold time.Duration
}

func NewThresholdTimer(clock mclock.Clock, threshold time.Duration) *ThresholdTimer {
	// Don't panic for lazy users
	if clock == nil {
		clock = mclock.System{}
	}
	return &ThresholdTimer{
		clock:     clock,
		last:      clock.Now(),
		threshold: threshold,
	}
}

func (t *ThresholdTimer) Update(callback func(diff mclock.AbsTime)) bool {
	t.lock.Lock()
	defer t.lock.Unlock()

	now := t.clock.Now()
	if now < t.last+mclock.AbsTime(t.threshold) {
		return false
	}
	diff := now - t.last
	t.last = now
	callback(diff)
	return true
}

type DiffTimer struct {
	lock  sync.Mutex
	clock mclock.Clock
	last  mclock.AbsTime
}

func NewDiffTimer(clock mclock.Clock) *DiffTimer {
	// Don't panic for lazy users
	if clock == nil {
		clock = mclock.System{}
	}
	return &DiffTimer{
		clock: clock,
		last:  clock.Now(),
	}
}

// Update calculates the time difference by local clock invokes the callback.
func (t *DiffTimer) Update(callback func(diff mclock.AbsTime)) {
	t.lock.Lock()
	defer t.lock.Unlock()

	now := t.clock.Now()
	diff := now - t.last
	if diff < 0 {
		diff = mclock.AbsTime(0)
	}
	callback(diff)
	t.last = now
}

// Diff calculates the time difference by the given future time or
// local clock and invokes the callback. The different with Update
// is the local last timestamp won't be updated.
func (t *DiffTimer) Diff(future mclock.AbsTime, callback func(diff mclock.AbsTime)) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if future == 0 {
		future = t.clock.Now()
	}
	diff := future - t.last
	if diff < 0 {
		diff = mclock.AbsTime(0)
	}
	callback(diff)
}
