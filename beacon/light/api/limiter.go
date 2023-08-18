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
// GNU Lesser General Public License for more detaiapi.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package api

import (
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

const (
	responseSizeOffset = 200 //TODO
	costBufferTarget   = time.Millisecond * 300
	costFactorTC       = time.Second
	queueTimeout       = time.Second
)

type servingQueue struct {
	processLimiter, sendLimiter *limiter
	client                      *clientQueue
	triggerCh                   chan struct{}
}

func (sq *servingQueue) run() {
	var now mclock.AbsTime
	for {
		for {
			now = mclock.Now()
			dt, dt2 := sq.processLimiter.getDelay(now), sq.sendLimiter.getDelay(now)
			if dt2 > dt {
				dt = dt2
			}
			if dt == 0 {
				break
			}
			time.Sleep(dt)
		}
		if task := sq.client.getTask(); task != nil {
			task.startCh <- true
		} else {
			if _, ok := <-sq.triggerCh; !ok {
				return
			}
		}
	}
}

func (sq *servingQueue) stop() {
	close(sq.triggerCh)
}

func (sq *servingQueue) newTask() *task {
	return &task{
		sq: sq,
		cq: sq.client,
	}
}

type task struct {
	sq *servingQueue
	cq *clientQueue

	startCh             chan bool
	queuedAt, startedAt mclock.AbsTime
	sizeCost            float64
}

func (t *task) start() bool {
	t.startCh = make(chan bool, 1)
	if !t.cq.addTask(t) {
		return false
	}
	if <-t.startCh {
		t.startedAt = mclock.Now()
		t.sq.processLimiter.enter(t.startedAt)
		return true
	}
	return false
}

func (t *task) processed(responseSize int) float64 {
	now := mclock.Now()
	pcost := t.sq.processLimiter.exit(now, float64(now-t.startedAt))
	t.sq.sendLimiter.enter(now)
	t.sizeCost = float64(responseSize + responseSizeOffset)
	scost := t.sq.sendLimiter.normalizedCost(now, t.sizeCost) //TODO vonjuk le mar itt a vegleges koltseget?
	if scost > pcost {
		return scost
	}
	return pcost
}

func (t *task) sent() {
	t.sq.sendLimiter.exit(mclock.Now(), t.sizeCost)
}

// called from multiple sq goroutines
type limiter struct {
	lock          sync.RWMutex
	maxActiveRate float64
	maxThreads    int
	idleThreshold time.Duration

	activeThreads int
	lastSwitch    mclock.AbsTime
	idleTime      time.Duration

	lastCostUpdate         mclock.AbsTime
	costBuffer, costFactor float64
}

//TODO max threads limit?
func (l *limiter) getDelay(now mclock.AbsTime) time.Duration {
	l.lock.RLock()
	delay := l.getIdleTime(now) - l.idleThreshold
	if delay < 0 {
		delay = 0
	}
	l.lock.RUnlock()
	return delay
}

func (l *limiter) getIdleTime(now mclock.AbsTime) time.Duration {
	if l.activeThreads == 0 {
		if dt := time.Duration(now - l.lastSwitch); dt < l.idleTime {
			return l.idleTime - dt
		} else {
			return 0
		}
	}
	return l.idleTime + time.Duration(float64(now-l.lastSwitch)*(1-l.maxActiveRate)/l.maxActiveRate)
}

//TODO max threads limit?
func (l *limiter) enter(now mclock.AbsTime) {
	l.lock.Lock()
	if l.activeThreads == 0 {
		l.idleTime = l.getIdleTime(now)
		l.lastSwitch = now
		l.lastCostUpdate = now // cost calculation only considers active sections
	}
	l.activeThreads++
	l.lock.Unlock()
}

// call after enter (lastCostUpdate)
func (l *limiter) normalizedCost(now mclock.AbsTime, cost float64) float64 {
	l.lock.RLock()
	dt := float64(now - l.lastCostUpdate)
	costBuffer := l.costBuffer + dt/l.maxActiveRate
	normCost := -costBuffer * math.Expm1(-cost*l.costFactor)
	l.lock.RUnlock()
	return normCost
}

func (l *limiter) exit(now mclock.AbsTime, cost float64) float64 {
	l.lock.Lock()
	l.activeThreads--
	if l.activeThreads == 0 {
		l.idleTime = l.getIdleTime(now)
		l.lastSwitch = now
	}
	dt := float64(now - l.lastCostUpdate)
	l.lastCostUpdate = now
	l.costBuffer += dt / l.maxActiveRate
	normCost := -l.costBuffer * math.Expm1(-cost*l.costFactor)
	l.costBuffer -= normCost
	l.costFactor *= math.Exp((l.costBuffer/float64(costBufferTarget) - 1) * dt / float64(costFactorTC))
	l.lock.Unlock()
	return normCost
}

// called from sq main thread
type clientQueue struct {
	sq        *servingQueue
	maxLength int
	queue     []*task
}

func (cq *clientQueue) isEmpty() bool {
	return len(cq.queue) == 0
}

func (cq *clientQueue) addTask(task *task) bool {
	if len(cq.queue) >= cq.maxLength {
		return false
	}
	cq.queue = append(cq.queue, task)
	if len(cq.queue) == 1 {
		select {
		case cq.sq.triggerCh <- struct{}{}:
		default:
		}
	}
	task.queuedAt = mclock.Now()
	return true
}

func (cq *clientQueue) getTask() *task {
	cq.checkTimeout()
	if len(cq.queue) == 0 {
		return nil
	}
	task := cq.queue[0]
	cq.queue = cq.queue[1:]
	return task
}

func (cq *clientQueue) checkTimeout() {
	limit := mclock.Now() - mclock.AbsTime(queueTimeout)
	for len(cq.queue) > 0 && cq.queue[0].queuedAt < limit {
		cq.queue[0].startCh <- false
		cq.queue = cq.queue[1:]
	}
}
