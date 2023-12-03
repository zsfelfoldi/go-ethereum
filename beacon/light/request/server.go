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

	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
)

var (
	RespInvalid     = &struct{}{}
	RespSoftTimeout = &struct{}{}
	RespHardTimeout = &struct{}{}
)

type RequestServer interface {
	Subscribe(eventCallback func(evType int, data interface{}))
	SendRequest(request interface{}) (id interface{})
	Unsubscribe()
}

type HeadInfo struct {
	Slot      uint64
	BlockRoot common.Hash
}

type Response struct {
	Id, RespData interface{}
}

const (
	EvNewHead         = iota // data: HeadInfo
	EvNewSignedHead          // data: types.SignedHeader
	EvValidResponse          // data: response
	EvInvalidResponse        // data: id
	EvSoftTimeout            // data: id
	EvHardTimeout            // data: id
	EvCanRequestAgain        // data: nil
)

type ServerWithTimeout struct {
	RequestServer
	lock         sync.Mutex
	clock        mclock.Clock
	childEventCb func(evType int, data interface{})
	timeouts     map[interface{}]mclock.Timer // stopped when request has returned; nil when timed out
}

func (s *ServerWithTimeout) Subscribe(eventCallback func(event)) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.childEventCb = eventCallback
	s.RequestServer.Subscribe(s.eventCallback)
}

func (s *ServerWithTimeout) eventCallback(evType int, data interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	switch evType {
	case EvNewHead, EvNewSignedHead:
		s.childEventCb(evType, data)
		return
	case EvValidResponse, EvInvalidResponse, EvHardTimeout:
	default:
		log.Error("Unexpected server event", "type", evType)
		return
	}

	if timer, ok := s.timeouts[reqId]; ok {
		if timer != nil {
			s.stopTimer(timer)
		}
		delete(s.timeouts, reqId)
	}
	s.childEventCb(evType, data)
}

func (s *ServerWithTimeout) SendRequest(request interface{}) (id interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	reqId := s.RequestServer.SendRequest(request)
	s.timeouts[reqId] = s.clock.AfterFunc(softRequestTimeout, func() {
		/*if s.testTimerResults != nil {
			s.testTimerResults = append(s.testTimerResults, true) // simulated timer finished
		}*/
		s.lock.Lock()
		if s.timeouts[reqId] != nil {
			// do not delete entry yet; nil means timed out request
			s.timeouts[reqId] = nil
			s.childEventCb(EvSoftTimeout, reqId)
		}
		s.lock.Unlock()
	})
	return reqId
}

// stop stops all goroutines associated with the server.
func (s *ServerWithTimeout) Unsubscribe() {
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

func (s *ServerWithTimeout) stopTimer(timer mclock.Timer) {
	timer.Stop()
	/*if timer.Stop() && s.scheduler.testTimerResults != nil {
		s.scheduler.testTimerResults = append(s.scheduler.testTimerResults, false) // simulated timer stopped
	}*/
}

type ServerWithDelay struct {
	ServerWithTimeout
	lock                    sync.Mutex
	clock                   mclock.Clock
	childEventCb            func(evType int, data interface{})
	timeouts                map[interface{}]struct{}
	waitCount, timeoutCount int
}

func (s *ServerWithDelay) Subscribe(eventCallback func(event)) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.childEventCb = eventCallback
	s.ServerWithTimeout.Subscribe(s.eventCallback)
}

func (s *ServerWithDelay) eventCallback(evType int, data interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	switch evType {
	case EvNewHead, EvNewSignedHead:
		s.childEventCb(evType, data)
		return
	case EvSoftTimeout:
		s.timeouts[reqId] = struct{}{}
		s.timeoutCount++
		if s.score > scoreMin+scoreDown {
			s.score -= scoreDown
		} else {
			s.score = scoreMin
		}
	case EvValidResponse, EvInvalidResponse, EvHardTimeout:
		if _, ok := s.timeouts[reqId]; ok {
			delete(s.timeouts, reqId)
			s.timeoutCount--
		}
		s.waitCount--
		if evType == EvValidResponse && s.waitCount >= s.score/scoreDiv {
			s.score += scoreUp
		}
	default:
		log.Error("Unexpected server event", "type", evType)
		return
	}
	s.childEventCb(evType, data)
}

func (s *ServerWithDelay) SendRequest(request interface{}) (id interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	reqId := s.ServerWithTimeout.SendRequest(request)
	s.waitCount++
	return reqId
}

// stop stops all goroutines associated with the server.
func (s *ServerWithDelay) Unsubscribe() {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.delayTimer != nil {
		s.stopTimer(s.delayTimer)
	}
	s.childEventCb = nil
	s.ServerWithTimeout.Unsubscribe()
}

func (s *ServerWithDelay) CanSendNow() bool {
	return 1, 0 //TODO
}

func (s *ServerWithDelay) Fail(desc string) {
	/*s.failureDelay *= 2
	now := s.clock.Now()
	if now > s.failureDelayUntil {
		s.failureDelay *= math.Pow(2, -float64(now-s.failureDelayUntil)/float64(maxFailureDelay))
	}
	if s.failureDelay < float64(minFailureDelay) {
		s.failureDelay = float64(minFailureDelay)
	}
	s.failureDelayUntil = now + mclock.AbsTime(s.failureDelay)
	log.Warn("API endpoint failure", "URL", s.api.url, "error", desc)
	*/
}

type Server struct {
	ServerWithDelay

	headLock       sync.RWMutex
	latestHeadSlot uint64
	latestHeadHash common.Hash
	unregistered   bool // accessed under HeadTracker.prefetchLock
}

// newServer creates a new Server.
func (s *Scheduler) newServer(server RequestServer) *Server {
	return &Server{
		RequestServer: server,
		scheduler:     s,
		timeouts:      make(map[uint64]mclock.Timer),
		stopCh:        make(chan struct{}),
	}
}

// setHead is called by the head event subscription.
func (s *Server) setHead(slot uint64, blockRoot common.Hash) {
	s.headLock.Lock()
	defer s.headLock.Unlock()

	s.latestHeadSlot, s.latestHeadHash = slot, blockRoot
}

// LatestHead returns the server's latest reported head (slot and block root).
// Note: the reason we can't return the full header here is that the standard
// beacon API head event only contains the slot and block root.
func (s *Server) LatestHead() (uint64, common.Hash) {
	s.headLock.RLock()
	defer s.headLock.RUnlock()

	return s.latestHeadSlot, s.latestHeadHash
}

// canRequestNow returns true if a request can be sent to the server immediately
// (has no timed out requests and underlying RequestServer does not require new
// requests to be delayed). It also returns a priority value that is taken into
// account when otherwise equally good servers are available.
// Note: if canRequestNow ever returns false then it is guaranteed that a server
// trigger will be emitted as soon as it becomes true again.
func (s *Server) canRequestNow() (bool, uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.isDelayed() || s.timeoutCount != 0 {
		s.needTrigger = true
		return false, 0
	}
	//TODO use priority based on in-flight requests (less is better)
	return true, uint64(rand.Uint32() + 1)
}

// isDelayed returns true if the underlying RequestServer requires requests to be
// delayed. In this case it also starts a timer to ensure that a server trigger
// can be emitted when the server becomes available again.
func (s *Server) isDelayed() bool {
	delayUntil := s.RequestServer.DelayUntil()
	if delayUntil == s.delayUntil {
		return s.delayTimer != nil
	}
	if s.delayTimer != nil {
		// Note: is stopping the timer is unsuccessful then the resulting AfterFunc
		// call will just do nothing
		s.stopTimer(s.delayTimer)
		s.delayTimer = nil
	}
	s.delayUntil = delayUntil
	delay := time.Duration(delayUntil - s.scheduler.clock.Now())
	if delay <= 0 {
		return false
	}
	s.delayTimer = s.scheduler.clock.AfterFunc(delay, func() {
		if s.scheduler.testTimerResults != nil {
			s.scheduler.testTimerResults = append(s.scheduler.testTimerResults, true) // simulated timer finished
		}
		s.lock.Lock()
		if s.delayTimer != nil && s.delayUntil == delayUntil { // do nothing if there is a new timer now
			s.delayTimer = nil
			if s.needTrigger && s.timeoutCount == 0 {
				s.needTrigger = false
				s.scheduler.Trigger()
			}
		}
		s.lock.Unlock()
	})
	return true
}
