// Copyright 2015 The go-ethereum Authors
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

// Package discover implements the Node Discovery Protocol.
//
// The Node Discovery protocol provides a way to find RLPx nodes that
// can be connected to. It uses a Kademlia-like protocol to maintain a
// distributed database of the IDs and endpoints of all listening
// nodes.
package discover

import (
	"time"

	"github.com/aristanetworks/goarista/atime"
	fbclock "github.com/facebookgo/clock"
)

type absTime time.Duration // absolute monotonic time

func monotonicTime() absTime {
	return absTime(atime.NanoTime())
}

type clock interface {
	fbclock.Clock
	MonotonicTime() uint64
}

type realClock struct {
	fbclock.Clock
}

func newRealClock() *realClock {
	return &realClock{fbclock.New()}
}

func (r *realClock) MonotonicTime() uint64 {
	return atime.NanoTime()
}

type fakeClock struct {
	fbclock.Clock
}

func newFakeClock() *fakeClock {
	return &fakeClock{fbclock.NewMock()}
}

func (r *fakeClock) MonotonicTime() uint64 {
	return uint64(r.Now().UnixNano())
}
