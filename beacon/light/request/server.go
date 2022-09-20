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

package request

import (
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

const (
	// request events
	EvResponse = iota // data: IdAndResponse
	EvFail            // data: ID
	EvTimeout         // data: ID
	// server events
	EvRegistered      // data: nil
	EvUnregistered    // data: nil
	EvCanRequestAgain // data: nil
	EvAppSpecific     // application specific events start at this index
)

const (
	softRequestTimeout = time.Second
	hardRequestTimeout = time.Second * 10
)

const (
	parallelAdjustUp   = 0.1
	parallelAdjustDown = 1
	minParallelLimit   = 1
)

type RequestServer interface {
	Subscribe(eventCallback func(event Event))
	SendRequest(request Request) ID
	Unsubscribe()
}

type Server interface {
	RequestServer
	CanRequestNow() (bool, float32)
	Fail(desc string)
}

func NewServer(rs RequestServer, clock mclock.Clock) Server {
	s := &serverWithDelay{}
	s.serverWithTimeout.RequestServer = rs
	s.serverWithTimeout.init(clock)
	s.init()
	return s
}

type serverSet map[Server]struct{}

type Event struct {
	Type int
	Data any
}

type IdAndResponse struct {
	ID       ID
	Response Response
}

type serverWithTimeout struct {
	RequestServer
	lock         sync.Mutex
	clock        mclock.Clock
	childEventCb func(event Event)
	timeouts     map[ID]mclock.Timer
}

func (s *serverWithTimeout) init(clock mclock.Clock) {
	s.clock = clock
	s.timeouts = make(map[ID]mclock.Timer)
}

func (s *serverWithTimeout) Subscribe(eventCallback func(event Event)) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.childEventCb = eventCallback
	s.RequestServer.Subscribe(s.eventCallback)
}

func (s *serverWithTimeout) eventCallback(event Event) {
	s.lock.Lock()
	defer s.lock.Unlock()

	switch event.Type {
	case EvResponse, EvFail:
		var id ID
		if event.Type == EvResponse {
			id = event.Data.(IdAndResponse).ID
		} else {
			id = event.Data.(ID)
		}
		if timer, ok := s.timeouts[id]; ok {
			// Note: if stopping the timer is unsuccessful then the resulting AfterFunc
			// call will just do nothing
			s.stopTimer(timer)
			delete(s.timeouts, id)
			s.childEventCb(event)
		}
	default:
		s.childEventCb(event)
	}
}

func (s *serverWithTimeout) SendRequest(request Request) (reqId ID) {
	s.lock.Lock()
	defer s.lock.Unlock()

	reqId = s.RequestServer.SendRequest(request)
	s.timeouts[reqId] = s.clock.AfterFunc(softRequestTimeout, func() {
		/*if s.testTimerResults != nil {
			s.testTimerResults = append(s.testTimerResults, true) // simulated timer finished
		}*/
		s.lock.Lock()
		defer s.lock.Unlock()

		if _, ok := s.timeouts[reqId]; !ok {
			return
		}
		s.timeouts[reqId] = s.clock.AfterFunc(hardRequestTimeout-softRequestTimeout, func() {
			/*if s.testTimerResults != nil {
				s.testTimerResults = append(s.testTimerResults, true) // simulated timer finished
			}*/
			s.lock.Lock()
			defer s.lock.Unlock()

			if _, ok := s.timeouts[reqId]; !ok {
				return
			}
			delete(s.timeouts, reqId)
			s.childEventCb(Event{Type: EvFail, Data: reqId})
		})
		s.childEventCb(Event{Type: EvTimeout, Data: reqId})
	})
	return reqId
}

// stop stops all goroutines associated with the server.
func (s *serverWithTimeout) Unsubscribe() {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, timer := range s.timeouts {
		if timer != nil {
			s.stopTimer(timer)
		}
	}
	s.childEventCb = nil
	s.RequestServer.Unsubscribe()
}

func (s *serverWithTimeout) stopTimer(timer mclock.Timer) {
	timer.Stop()
	/*if timer.Stop() && s.scheduler.testTimerResults != nil {
		s.scheduler.testTimerResults = append(s.scheduler.testTimerResults, false) // simulated timer stopped
	}*/
}

type serverWithDelay struct {
	serverWithTimeout
	lock                       sync.Mutex
	childEventCb               func(event Event)
	softTimeouts               map[ID]struct{}
	pendingCount, timeoutCount int
	parallelLimit              float32
	sendEvent                  bool
	delayTimer                 mclock.Timer
	delayCounter               int
}

func (s *serverWithDelay) init() {
	s.softTimeouts = make(map[ID]struct{})
}

func (s *serverWithDelay) Subscribe(eventCallback func(event Event)) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.childEventCb = eventCallback
	s.serverWithTimeout.Subscribe(s.eventCallback)
}

func (s *serverWithDelay) eventCallback(event Event) {
	s.lock.Lock()
	defer s.lock.Unlock()

	switch event.Type {
	case EvTimeout:
		s.softTimeouts[event.Data.(ID)] = struct{}{}
		s.timeoutCount++
		s.parallelLimit -= parallelAdjustDown
		if s.parallelLimit < minParallelLimit {
			s.parallelLimit = minParallelLimit
		}
	case EvResponse, EvFail:
		var id ID
		if event.Type == EvResponse {
			id = event.Data.(IdAndResponse).ID
		} else {
			id = event.Data.(ID)
		}
		if _, ok := s.softTimeouts[id]; ok {
			delete(s.softTimeouts, id)
			s.timeoutCount--
		}
		if event.Type == EvResponse && s.pendingCount >= int(s.parallelLimit) {
			s.parallelLimit -= parallelAdjustUp
		}
		s.pendingCount--
		s.canRequestNow() // send event if needed
	}
	s.childEventCb(event)
}

func (s *serverWithDelay) SendRequest(request Request) (reqId ID) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.pendingCount++
	return s.serverWithTimeout.SendRequest(request)
}

// stop stops all goroutines associated with the server.
func (s *serverWithDelay) Unsubscribe() {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.delayTimer != nil {
		s.stopTimer(s.delayTimer)
		s.delayTimer = nil
	}
	s.childEventCb = nil
	s.serverWithTimeout.Unsubscribe()
}

func (s *serverWithDelay) canRequestNow() (bool, float32) {
	if s.delayTimer != nil || s.pendingCount >= int(s.parallelLimit) {
		return false, 0
	}
	if s.sendEvent {
		s.childEventCb(Event{Type: EvCanRequestAgain})
		s.sendEvent = false
	}
	if s.parallelLimit < minParallelLimit {
		s.parallelLimit = minParallelLimit
	}
	return true, -(float32(s.pendingCount) + rand.Float32()) / s.parallelLimit
}

// EvCanRequestAgain guaranteed if it returns false
func (s *serverWithDelay) CanRequestNow() (bool, float32) {
	s.lock.Lock()
	defer s.lock.Unlock()

	canSend, priority := s.canRequestNow()
	if !canSend {
		s.sendEvent = true
	}
	return canSend, priority
}

func (s *serverWithDelay) Delay(delay time.Duration) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.delayTimer != nil {
		// Note: if stopping the timer is unsuccessful then the resulting AfterFunc
		// call will just do nothing
		s.stopTimer(s.delayTimer)
		s.delayTimer = nil
	}

	s.delayCounter++
	delayCounter := s.delayCounter
	s.delayTimer = s.clock.AfterFunc(delay, func() {
		/*if s.scheduler.testTimerResults != nil {
			s.scheduler.testTimerResults = append(s.scheduler.testTimerResults, true) // simulated timer finished
		}*/
		s.lock.Lock()
		if s.delayTimer != nil && s.delayCounter == delayCounter { // do nothing if there is a new timer now
			s.delayTimer = nil
			s.canRequestNow() // send event if necessary
		}
		s.lock.Unlock()
	})
}

func (s *serverWithDelay) Fail(desc string) {
	//TODO
}
