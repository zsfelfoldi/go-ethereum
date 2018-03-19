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
	"runtime"
	"sync"
	"time"
)

type event struct {
	do func()
	at AbsTime
}

type SimulatedClock struct {
	now       AbsTime
	scheduled []event
	lock      sync.RWMutex
}

func NewSimulatedClock() *SimulatedClock {
	s := &SimulatedClock{}

	go func() {
		for {
			runtime.Gosched()
			s.lock.Lock()
			if len(s.scheduled) > 0 {
				ev := s.scheduled[0]
				s.scheduled = s.scheduled[1:]
				s.now = ev.at
				ev.do()
			}
			s.lock.Unlock()
		}
	}()

	return s
}

func (s *SimulatedClock) Now() AbsTime {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.now
}

func (s *SimulatedClock) Sleep(d time.Duration) {
	done := make(chan struct{})
	s.insert(d, func() {
		close(done)
	})
	<-done
}

func (s *SimulatedClock) After(d time.Duration) <-chan time.Time {
	after := make(chan time.Time, 1)
	s.insert(d, func() {
		after <- time.Unix(0, int64(s.now))
	})
	return after
}

func (s *SimulatedClock) insert(d time.Duration, do func()) {
	s.lock.Lock()
	defer s.lock.Unlock()

	at := s.now + AbsTime(d)
	l, h := 0, len(s.scheduled)
	ll := h
	for l != h {
		m := (l + h) / 2
		if at < s.scheduled[m].at {
			h = m
		} else {
			l = m + 1
		}
	}
	s.scheduled = append(s.scheduled, event{})
	copy(s.scheduled[l+1:], s.scheduled[l:ll])
	s.scheduled[l] = event{do: do, at: at}
}
