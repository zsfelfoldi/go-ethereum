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

// Package flowcontrol implements a client side flow control mechanism
package flowcontrol

import (
	//	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/flowcontrol/prque"
)

const (
	cmDisabled = iota
	cmNormal
	cmBlockProcessing
)

type cmNodeFields struct {
	servingStarted mclock.AbsTime
	servingMaxCost uint64

	cmLock         sync.Mutex
	corrBufValue   int64
	rcLastUpdate   mclock.AbsTime
	rcLastIntValue int64
}

type ClientManager struct {
	child     *ClientManager
	lock      sync.RWMutex
	nodes     map[*ClientNode]struct{}
	enabledCh chan struct{}

	parallelReqs, maxParallelReqs int
	targetParallelReqs            float64
	servingQueue                  *prque.Prque

	mode                             int
	totalRecharge                    float64
	forceMinRecharge, bufCorrEnabled bool

	sumRecharge    uint64
	rcLastUpdate   mclock.AbsTime
	rcLastIntValue int64 // normalized to MRR=1000000
	rcQueue        *prque.Prque
}

func NewClientManager(maxParallelReqs int, targetParallelReqs float64, child *ClientManager) *ClientManager {
	cm := &ClientManager{
		nodes:        make(map[*ClientNode]struct{}),
		child:        child,
		servingQueue: prque.New(),
		rcQueue:      prque.New(),

		maxParallelReqs:    maxParallelReqs,
		targetParallelReqs: targetParallelReqs,
	}
	cm.SetMode(cmNormal)
	return cm
}

func (cm *ClientManager) SetMode(newMode int) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if newMode == cm.mode {
		return
	}
	cm.updateRecharge(Clock.Now())

	enabled := cm.mode != cmDisabled
	newEnabled := cm.mode != cmDisabled
	if !enabled && newEnabled && cm.enabledCh != nil {
		close(cm.enabledCh)
		cm.enabledCh = nil
	}
	if enabled && !newEnabled {
		cm.enabledCh = make(chan struct{})
	}

	switch newMode {
	case cmDisabled:
		cm.totalRecharge = 0
		cm.bufCorrEnabled = false
		cm.forceMinRecharge = false
	case cmNormal:
		cm.totalRecharge = cm.targetParallelReqs * 1000000
		cm.bufCorrEnabled = true
		cm.forceMinRecharge = false
	case cmBlockProcessing:
		cm.totalRecharge = 0
		cm.bufCorrEnabled = false
		cm.forceMinRecharge = true
	}

	cm.mode = newMode

	if cm.child != nil {
		if cm.parallelReqs == 0 {
			cm.child.SetMode(newMode)
		} else {
			cm.child.SetMode(cmDisabled)
		}
	}
}

func (cm *ClientManager) setParallelReqs(p int, time mclock.AbsTime) {
	if p == cm.parallelReqs {
		return
	}
	if cm.child != nil && cm.mode != cmDisabled {
		if cm.parallelReqs == 0 {
			cm.child.SetMode(cmDisabled)
		}
		if p == 0 {
			cm.child.SetMode(cm.mode)
		}
	}
	cm.parallelReqs = p
}

func (cm *ClientManager) updateRecharge(time mclock.AbsTime) {
	//fmt.Println("update", cm.sumRecharge, "int", cm.rcLastIntValue)
	lastUpdate := cm.rcLastUpdate
	cm.rcLastUpdate = time
	if cm.totalRecharge == 0 {
		return
	}
	for cm.sumRecharge > 0 {
		var slope float64
		if cm.forceMinRecharge {
			slope = 1
		} else {
			slope = cm.totalRecharge / float64(cm.sumRecharge)
		}
		dt := time - lastUpdate
		//fmt.Println("time", time, "dt", dt, "slope", slope)
		n, ni := cm.rcQueue.Pop()
		nextIntValue := -ni
		dtNext := mclock.AbsTime(float64(nextIntValue-cm.rcLastIntValue) / slope)
		if n == nil || dt < dtNext {
			if n != nil {
				cm.rcQueue.Push(n, -nextIntValue)
			}
			cm.rcLastIntValue += int64(slope * float64(dt))
			return
		}
		node := n.(*ClientNode)
		node.cmLock.Lock()
		i := node.rcLastIntValue + (int64(node.params.BufLimit)-node.corrBufValue)*1000000/int64(node.params.MinRecharge)
		//fmt.Println(nextIntValue, i)
		if i != nextIntValue {
			cm.rcQueue.Push(n, -i)
			node.cmLock.Unlock()
			continue
		}
		if node.corrBufValue < int64(node.params.BufLimit) {
			node.corrBufValue = int64(node.params.BufLimit)
			cm.sumRecharge -= node.params.MinRecharge
		}
		lastUpdate += dtNext
		cm.rcLastIntValue = nextIntValue
		node.cmLock.Unlock()
	}
}

func (cm *ClientManager) updateNodeRc(node *ClientNode, bvc int64, time mclock.AbsTime) {
	cm.updateRecharge(time)

	node.cmLock.Lock()
	defer node.cmLock.Unlock()

	//fmt.Println("time", time, "bv", node.corrBufValue)
	wasFull := true
	if node.corrBufValue != int64(node.params.BufLimit) {
		wasFull = false
		node.corrBufValue += (cm.rcLastIntValue - node.rcLastIntValue) * int64(node.params.MinRecharge) / 1000000
		if node.corrBufValue > int64(node.params.BufLimit) {
			node.corrBufValue = int64(node.params.BufLimit)
		}
		node.rcLastIntValue = cm.rcLastIntValue
		//fmt.Println("rc", node.corrBufValue)
	}
	node.corrBufValue += bvc
	if node.corrBufValue < 0 {
		node.corrBufValue = 0
	}
	isFull := false
	if node.corrBufValue >= int64(node.params.BufLimit) {
		node.corrBufValue = int64(node.params.BufLimit)
		isFull = true
	}
	//fmt.Println("bvc", bvc, node.corrBufValue)
	if wasFull && !isFull {
		cm.sumRecharge += node.params.MinRecharge
		node.rcLastIntValue = cm.rcLastIntValue
		nextIntValue := cm.rcLastIntValue + (int64(node.params.BufLimit)-node.corrBufValue)*1000000/int64(node.params.MinRecharge)
		cm.rcQueue.Push(node, -nextIntValue)
	}
	if !wasFull && isFull {
		cm.sumRecharge -= node.params.MinRecharge
	}
}

func (cm *ClientManager) waitOrStop(node *ClientNode) bool {
	cm.lock.RLock()
	_, ok := cm.nodes[node]
	stop := !ok
	ch := cm.enabledCh
	cm.lock.RUnlock()

	if stop == false && ch != nil {
		<-ch
		cm.lock.RLock()
		_, ok = cm.nodes[node]
		stop = !ok
		cm.lock.RUnlock()
	}

	return stop
}

func (cm *ClientManager) Stop() {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.nodes = nil
}

func (cm *ClientManager) addNode(node *ClientNode) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	node.cmLock.Lock()
	node.corrBufValue = int64(node.params.BufLimit)
	node.rcLastIntValue = cm.rcLastIntValue
	node.cmLock.Unlock()

	if cm.nodes != nil {
		cm.nodes[node] = struct{}{}
	}
}

func (cm *ClientManager) removeNode(node *ClientNode) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if cm.nodes != nil {
		delete(cm.nodes, node)
	}
}

func (cm *ClientManager) accept(node *ClientNode, maxCost uint64, time mclock.AbsTime) chan bool {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if cm.parallelReqs == cm.maxParallelReqs {
		ch := make(chan bool, 1)
		start := func() bool {
			// always called while client manager lock is held
			_, started := cm.nodes[node]
			ch <- started
			return started
		}
		cm.servingQueue.Push(start, int64(1000000000*float64(node.bufValue)/float64(node.params.BufLimit)))
		return ch
	}

	cm.setParallelReqs(cm.parallelReqs+1, time)
	node.servingStarted = time
	node.servingMaxCost = maxCost
	cm.updateNodeRc(node, -int64(maxCost), time)
	return nil
}

func (cm *ClientManager) started(node *ClientNode, maxCost uint64, time mclock.AbsTime) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	node.servingStarted = time
	node.servingMaxCost = maxCost
	cm.updateNodeRc(node, -int64(maxCost), time)
}

func (cm *ClientManager) processed(node *ClientNode, time mclock.AbsTime) (realCost uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	realCost = uint64(time - node.servingStarted)
	if realCost > node.servingMaxCost {
		realCost = node.servingMaxCost
	}
	if !cm.forceMinRecharge {
		cm.updateNodeRc(node, int64(node.servingMaxCost-realCost), time)
	}
	if cm.bufCorrEnabled {
		node.cmLock.Lock()
		if uint64(node.corrBufValue) > node.bufValue {
			node.bufValue = uint64(node.corrBufValue)
		}
		node.cmLock.Unlock()
	}

	for !cm.servingQueue.Empty() {
		if cm.servingQueue.PopItem().(func() bool)() {
			return
		}
	}
	cm.setParallelReqs(cm.parallelReqs-1, time)
	return
}
