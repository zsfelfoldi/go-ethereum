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
	"math/rand"
	"sync"
	"time"
)

type syncModule interface {
	process(servers []syncServer) (changed bool, reqId interface{}, reqDone chan struct{})
	timeout(reqId interface{})
	removedServer(server syncServer)
}

type syncServer interface {
	SetTriggerCallback(func())
	Fail(string)
}

type Scheduler struct {
	lock      sync.Mutex
	modules   []syncModule // first has highest priority
	servers   []syncServer
	stopCh    chan chan struct{}
	triggerCh chan struct{}
}

func NewScheduler() *Scheduler {
	return &Scheduler{
		stopCh:    make(chan chan struct{}),
		triggerCh: make(chan struct{}, 1),
	}
}

// call before starting the scheduler
func (s *Scheduler) RegisterModule(m syncModule) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.modules = append(s.modules, m)
}

func (s *Scheduler) RegisterServer(server syncServer) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.servers = append(s.servers, server)
	s.trigger()
	server.SetTriggerCallback(s.trigger) //TODO trigger individual servers with callback
}

func (s *Scheduler) UnregisterServer(server syncServer) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, m := range s.modules {
		m.removedServer(server)
	}
	for i, srv := range s.servers {
		if srv == server {
			s.servers[i] = s.servers[len(s.servers)-1]
			s.servers = s.servers[:len(s.servers)-1]
			return
		}
	}
}

// call before registering servers
func (s *Scheduler) Start() {
	go s.syncLoop()
}

func (s *Scheduler) Stop() {
	stop := make(chan struct{})
	s.stopCh <- stop
	<-stop
}

func (s *Scheduler) syncLoop() {
	s.lock.Lock()
	for {
		wait := true
		for _, m := range s.modules {
			for {
				changed, reqId, reqDone := m.process(s.servers)
				if !changed && reqDone == nil {
					break
				}
				if changed {
					wait = false
				}
				if reqDone != nil {
					go func() {
						timer := time.NewTimer(time.Millisecond * 500)
						select {
						case <-reqDone:
							timer.Stop()
						case <-timer.C:
							m.timeout(reqId)
						}
						s.trigger()
					}()
				}
			}
		}
		if wait {
			s.lock.Unlock()
			select {
			case stop := <-s.stopCh:
				close(stop)
				return
			case <-s.triggerCh:
			}
			s.lock.Lock()
		}
	}
}

func (s *Scheduler) trigger() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}

func selectServer(servers []syncServer, priority func(server syncServer) uint64) syncServer {
	var (
		maxPriority uint64
		mpCount     int
		bestServer  syncServer
	)
	for _, server := range servers {
		pri := priority(server)
		if pri == 0 || pri < maxPriority { // 0 means it cannot serve the request at all
			continue
		}
		if pri > maxPriority {
			maxPriority = pri
			mpCount = 1
			bestServer = server
		} else {
			mpCount++
			if rand.Intn(mpCount) == 0 {
				bestServer = server
			}
		}
	}
	return bestServer
}
