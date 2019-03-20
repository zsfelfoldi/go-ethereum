// Copyright 2018 The go-ethereum Authors
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

package les

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/les/csvlogger"
)

// servingQueue allows running tasks in a limited number of threads and puts the
// waiting tasks in a priority queue
type servingQueue struct {
	queueAddCh, queueBestCh chan *servingTask
	stopThreadCh, quit      chan struct{}
	setThreadsCh            chan int

	wg          sync.WaitGroup
	threadCount int          // number of currently running threads
	queue       *prque.Prque // priority queue for waiting or suspended tasks
	best        *servingTask // the highest priority task (not included in the queue)
	suspendBias int64        // priority bias against suspending an already running task

	burstTime, burstLimit, burstDropLimit int64
	burstDecRate                          float64
	lastUpdate                            mclock.AbsTime

	logger       *csvlogger.Logger
	logBurstTime *csvlogger.Channel
}

// servingTask represents a request serving task. Tasks can be implemented to
// run in multiple steps, allowing the serving queue to suspend execution between
// steps if higher priority tasks are entered. The creator of the task should
// set the following fields:
//
// - priority: greater value means higher priority; values can wrap around the int64 range
// - run: execute a single step; return true if finished
// - after: executed after run finishes or returns an error, receives the total serving time
type servingTask struct {
	sq                              *servingQueue
	servingTime, maxTime, timeAdded uint64
	peer                            *peer
	priority                        int64
	biasAdded                       bool
	token                           runToken
	tokenCh                         chan runToken
}

// runToken received by servingTask.start allows the task to run. Closing the
// channel by servingTask.stop signals the thread controller to allow a new task
// to start running.
type runToken chan struct{}

// start blocks until the task can start and returns true if it is allowed to run.
// Returning false means that the task should be cancelled.
func (t *servingTask) start() bool {
	if t.peer.isFrozen() {
		return false
	}
	t.tokenCh = make(chan runToken, 1)
	select {
	case t.sq.queueAddCh <- t:
	case <-t.sq.quit:
		return false
	}
	select {
	case t.token = <-t.tokenCh:
	case <-t.sq.quit:
		return false
	}
	if t.token == nil {
		return false
	}
	t.servingTime -= uint64(mclock.Now())
	return true
}

// done signals the thread controller about the task being finished and returns
// the total serving time of the task in nanoseconds.
func (t *servingTask) done() uint64 {
	t.servingTime += uint64(mclock.Now())
	close(t.token)
	atomic.AddInt64(&t.sq.burstTime, int64(t.servingTime-t.timeAdded))
	t.timeAdded = t.servingTime
	return t.servingTime
}

// waitOrStop can be called during the execution of the task. It blocks if there
// is a higher priority task waiting (a bias is applied in favor of the currently
// running task). Returning true means that the execution can be resumed. False
// means the task should be cancelled.
func (t *servingTask) waitOrStop() bool {
	t.done()
	if !t.biasAdded {
		t.priority += t.sq.suspendBias
		t.biasAdded = true
	}
	return t.start()
}

// newServingQueue returns a new servingQueue
func newServingQueue(suspendBias int64, utilTarget float64, logger *csvlogger.Logger) *servingQueue {
	sq := &servingQueue{
		queue:          prque.New(nil),
		suspendBias:    suspendBias,
		queueAddCh:     make(chan *servingTask, 100),
		queueBestCh:    make(chan *servingTask),
		stopThreadCh:   make(chan struct{}),
		quit:           make(chan struct{}),
		setThreadsCh:   make(chan int, 10),
		burstLimit:     int64(utilTarget * bufLimitRatio * 1200000),
		burstDropLimit: int64(utilTarget * bufLimitRatio * 1000000),
		burstDecRate:   utilTarget,
		lastUpdate:     mclock.Now(),
		logger:         logger,
		logBurstTime:   logger.NewMinMaxChannel("burstTime", false),
	}
	sq.wg.Add(2)
	go sq.queueLoop()
	go sq.threadCountLoop()
	return sq
}

// newTask creates a new task with the given priority
func (sq *servingQueue) newTask(peer *peer, maxTime uint64, priority int64) *servingTask {
	return &servingTask{
		sq:       sq,
		peer:     peer,
		maxTime:  maxTime,
		priority: priority,
	}
}

// threadController is started in multiple goroutines and controls the execution
// of tasks. The number of active thread controllers equals the allowed number of
// concurrently running threads. It tries to fetch the highest priority queued
// task first. If there are no queued tasks waiting then it can directly catch
// run tokens from the token channel and allow the corresponding tasks to run
// without entering the priority queue.
func (sq *servingQueue) threadController() {
	for {
		token := make(runToken)
		select {
		case best := <-sq.queueBestCh:
			best.tokenCh <- token
		case <-sq.stopThreadCh:
			sq.wg.Done()
			return
		case <-sq.quit:
			sq.wg.Done()
			return
		}
		<-token
		select {
		case <-sq.stopThreadCh:
			sq.wg.Done()
			return
		case <-sq.quit:
			sq.wg.Done()
			return
		default:
		}
	}
}

type (
	peerTasks struct {
		peer                   *peer
		list                   []*servingTask
		sumTime, worstPriority int64
	}
	peerList []*peerTasks
)

func (l peerList) Len() int {
	return len(l)
}

func (l peerList) Less(i, j int) bool {
	return (l[i].worstPriority - l[j].worstPriority) < 0
}

func (l peerList) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

// freezePeers selects the peers with the worst priority queued tasks and freezes
// them until burstTime goes under burstDropLimit or all peers are frozen
func (sq *servingQueue) freezePeers() {
	peerMap := make(map[*peer]*peerTasks)
	var peerList peerList
	for sq.queue.Size() > 0 {
		task := sq.queue.PopItem().(*servingTask)
		tasks := peerMap[task.peer]
		if tasks == nil {
			tasks = &peerTasks{
				peer:          task.peer,
				worstPriority: task.priority,
			}
			peerMap[task.peer] = tasks
			peerList = append(peerList, tasks)
		} else {
			if tasks.worstPriority-task.priority > 0 {
				tasks.worstPriority = task.priority
			}
		}
		tasks.list = append(tasks.list, task)
		tasks.sumTime += int64(task.timeAdded - task.servingTime)
	}
	sort.Sort(peerList)
	drop := true
	for _, tasks := range peerList {
		if drop {
			tasks.peer.freezeClient()
			sq.logger.Event("freezing peer, " + tasks.peer.id)
			drop = atomic.AddInt64(&sq.burstTime, -tasks.sumTime) > sq.burstDropLimit
			for _, task := range tasks.list {
				task.tokenCh <- nil
			}
		} else {
			for _, task := range tasks.list {
				sq.queue.Push(task, task.priority)
			}
		}
	}
}

// addTask inserts a task into the priority queue
func (sq *servingQueue) addTask(task *servingTask) {
	now := mclock.Now()
	dt := now - sq.lastUpdate
	sq.lastUpdate = now
	subTime := int64(float64(dt) * sq.burstDecRate)
	maxTime := task.maxTime
	if task.servingTime > maxTime {
		maxTime = task.servingTime
	}
	addTime := int64(maxTime - task.timeAdded)
	for {
		oldValue := atomic.LoadInt64(&sq.burstTime)
		newValue := oldValue - subTime
		if newValue < 0 {
			newValue = 0
		}
		if addTime > 0 {
			newValue += addTime
			task.timeAdded = maxTime
		}
		if atomic.CompareAndSwapInt64(&sq.burstTime, oldValue, newValue) {
			sq.logBurstTime.Update(float64(newValue) / 1000)
			if newValue > sq.burstLimit {
				sq.freezePeers()
			}
			break
		}
	}
	if task.peer.isFrozen() {
		task.tokenCh <- nil
		return
	}
	if sq.best == nil {
		sq.best = task
	} else if task.priority > sq.best.priority {
		sq.queue.Push(sq.best, sq.best.priority)
		sq.best = task
		return
	} else {
		sq.queue.Push(task, task.priority)
	}
}

// queueLoop is an event loop running in a goroutine. It receives tasks from queueAddCh
// and always tries to send the highest priority task to queueBestCh. Successfully sent
// tasks are removed from the queue.
func (sq *servingQueue) queueLoop() {
	for {
		if sq.best != nil {
			select {
			case task := <-sq.queueAddCh:
				sq.addTask(task)
			case sq.queueBestCh <- sq.best:
				if sq.queue.Size() == 0 {
					sq.best = nil
				} else {
					sq.best, _ = sq.queue.PopItem().(*servingTask)
				}
			case <-sq.quit:
				sq.wg.Done()
				return
			}
		} else {
			select {
			case task := <-sq.queueAddCh:
				sq.addTask(task)
			case <-sq.quit:
				sq.wg.Done()
				return
			}
		}
	}
}

// threadCountLoop is an event loop running in a goroutine. It adjusts the number
// of active thread controller goroutines.
func (sq *servingQueue) threadCountLoop() {
	var threadCountTarget int
	for {
		for threadCountTarget > sq.threadCount {
			sq.wg.Add(1)
			go sq.threadController()
			sq.threadCount++
		}
		if threadCountTarget < sq.threadCount {
			select {
			case threadCountTarget = <-sq.setThreadsCh:
			case sq.stopThreadCh <- struct{}{}:
				sq.threadCount--
			case <-sq.quit:
				sq.wg.Done()
				return
			}
		} else {
			select {
			case threadCountTarget = <-sq.setThreadsCh:
			case <-sq.quit:
				sq.wg.Done()
				return
			}
		}
	}
}

// setThreads sets the allowed processing thread count, suspending tasks as soon as
// possible if necessary.
func (sq *servingQueue) setThreads(threadCount int) {
	select {
	case sq.setThreadsCh <- threadCount:
	case <-sq.quit:
		return
	}
}

// stop stops task processing as soon as possible and shuts down the serving queue.
func (sq *servingQueue) stop() {
	close(sq.quit)
	sq.wg.Wait()
}
