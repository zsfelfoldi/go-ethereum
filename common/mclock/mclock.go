// Copyright 2016 The go-ethereum Authors
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

// package mclock is a wrapper for a monotonic clock source
package mclock

import (
	"time"

	"github.com/aristanetworks/goarista/monotime"
)

type AbsTime time.Duration // absolute monotonic time

func Now() AbsTime {
	return AbsTime(monotime.Now())
}

type Clock interface {
	Now() AbsTime
	Sleep(time.Duration)
	After(time.Duration) <-chan time.Time
}

type MonotonicClock struct{}

func (MonotonicClock) Now() AbsTime {
	return AbsTime(monotime.Now())
}

func (MonotonicClock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (MonotonicClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}
