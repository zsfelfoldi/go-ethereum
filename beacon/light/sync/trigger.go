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
	"sync"
)

type Trigger struct {
	lock      sync.Mutex
	counter   uint64
	triggerCh chan struct{}
	stopped   bool

	running   int
	runningCh chan int
}

func NewTrigger() *Trigger {
	return &Trigger{
		triggerCh: make(chan struct{}),
	}
}

func (t *Trigger) Trigger() {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.stopped {
		return
	}
	close(t.triggerCh)
	t.counter++
	t.triggerCh = make(chan struct{})
}

func (t *Trigger) NewThread() *TriggerThread {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.running++
	if t.runningCh != nil {
		t.runningCh <- t.running
	}
	return &TriggerThread{trigger: t}
}

func (t *Trigger) Stop() {
	t.lock.Lock()
	defer t.lock.Unlock()

	close(t.triggerCh)
	t.stopped = true
}

type TriggerThread struct {
	trigger     *Trigger
	lastCounter uint64
}

func (th *TriggerThread) Wait() (stopped bool) {
	th.trigger.lock.Lock()
	defer th.trigger.lock.Unlock()

	if th.trigger.stopped {
		return true
	}
	if th.lastCounter == th.trigger.counter {
		th.trigger.running--
		if th.trigger.runningCh != nil {
			th.trigger.runningCh <- th.trigger.running
		}

		triggerCh := th.trigger.triggerCh
		th.trigger.lock.Unlock()
		<-triggerCh
		th.trigger.lock.Lock()

		th.trigger.running++
		if th.trigger.runningCh != nil {
			th.trigger.runningCh <- th.trigger.running
		}
	}
	th.lastCounter = th.trigger.counter
	return th.trigger.stopped
}

func (th *TriggerThread) Close() {
	th.trigger.lock.Lock()
	defer th.trigger.lock.Lock()

	th.trigger.running--
	if th.trigger.runningCh != nil {
		th.trigger.runningCh <- th.trigger.running
	}
}
