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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

const softRequestTimeout = time.Second

// Module represents an update mechanism which is typically responsible for a
// passive data structure or a certain aspect of it. When registered to a Scheduler,
// it can be triggered either by server events, other modules or itself.
type Module interface {
	// Process is a non-blocking function that is called whenever the module is
	// triggered. It can start network requests through the received Environment
	// and/or do other data processing tasks. If triggers are set up correctly,
	// Process is eventually called whenever it might have something new to do
	// either because the data structures have been changed or because new servers
	// became available or new requests became available at existing ones.
	//
	// Note: Process functions of different modules are never called concurrently;
	// they are called by Scheduler in the same order of priority as they were
	// registered in.
	Process(env *Environment)
}

// Scheduler is a modular network data retrieval framework that coordinates multiple
// servers and retrieval mechanisms (modules). It implements a trigger mechanism
// that calls the Process function of registered modules whenever either the state
// of existing data structures or connected servers could allow new operations.
type Scheduler struct {
	headTracker *HeadTracker

	lock    sync.Mutex
	clock   mclock.Clock
	modules []Module // first has highest priority
	servers []*Server
	stopCh  chan chan struct{}

	triggerCh        chan struct{} // restarts waiting sync loop
	testWaitCh       chan struct{} // accepts sends when sync loop is waiting
	testTimerResults []bool        // true is appended when simulated timer is processed; false when stopped
}

// NewScheduler creates a new Scheduler.
func NewScheduler(headTracker *HeadTracker, clock mclock.Clock) *Scheduler {
	s := &Scheduler{
		headTracker: headTracker,
		clock:       clock,
		stopCh:      make(chan chan struct{}),
		// Note: testWaitCh should not have capacity in order to ensure
		// that after a trigger happens testWaitCh will block until the resulting
		// processing round has been finished
		triggerCh:  make(chan struct{}, 1),
		testWaitCh: make(chan struct{}),
	}
	headTracker.trigger = s.Trigger
	return s
}

// RegisterModule registers a module. Should be called before starting the scheduler.
// In each processing round the order of module processing depends on the order of
// registration.
func (s *Scheduler) RegisterModule(m Module) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.modules = append(s.modules, m)
}

// RegisterServer registers a new server.
func (s *Scheduler) RegisterServer(requestServer RequestServer) {
	s.lock.Lock()
	defer s.lock.Unlock()

	server := s.newServer(requestServer)
	s.servers = append(s.servers, server)
	s.headTracker.registerServer(server)
	s.Trigger()
}

// UnregisterServer removes a registered server.
func (s *Scheduler) UnregisterServer(RequestServer RequestServer) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for i, server := range s.servers {
		if server.RequestServer == RequestServer {
			s.servers[i] = s.servers[len(s.servers)-1]
			s.servers = s.servers[:len(s.servers)-1]
			server.stop()
			s.headTracker.unregisterServer(server)
			type moduleWithDisconnect interface {
				Module
				Disconnect(*Server)
			}

			for _, module := range s.modules {
				if m, ok := module.(moduleWithDisconnect); ok {
					m.Disconnect(server)
				}
			}
			return
		}
	}
}

// Start starts the scheduler. It should be called after registering all modules
// and before registering any servers.
func (s *Scheduler) Start() {
	go s.syncLoop()
}

// Stop stops the scheduler.
func (s *Scheduler) Stop() {
	s.lock.Lock()
	for _, server := range s.servers {
		server.stop()
	}
	s.servers = nil
	s.lock.Unlock()
	stop := make(chan struct{})
	s.stopCh <- stop
	<-stop
}

// syncLoop calls all processable modules in the order of their registration.
// A round of processing starts whenever there is at least one processable module.
// Triggers triggered during a processing round do not affect the current round
// but ensure that there is going to be a next round.
func (s *Scheduler) syncLoop() {
	for {
		s.lock.Lock()
		s.processModules()
		s.lock.Unlock()

	loop:
		for {
			select {
			case stop := <-s.stopCh:
				close(stop)
				return
			case <-s.triggerCh:
				break loop
			case <-s.testWaitCh:
			}
		}
	}
}

// processModules runs an entire processing round, calling processable modules
// with the appropriate Environment.
func (s *Scheduler) processModules() {
	env := Environment{ // enables all servers for triggered modules
		HeadTracker:   s.headTracker,
		scheduler:     s,
		allServers:    s.servers,
		canRequestNow: make(map[*Server]struct{}),
	}
	for _, server := range s.servers {
		if canRequest, _ := server.canRequestNow(); canRequest {
			env.canRequestNow[server] = struct{}{}
		}
	}
	for _, module := range s.modules {
		env.module = module
		module.Process(&env)
	}
}

func (s *Scheduler) Trigger() {
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}
