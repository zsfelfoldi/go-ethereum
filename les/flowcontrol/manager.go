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
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"gopkg.in/karalabe/cookiejar.v2/collections/prque"
)

type cmNodeFields struct {
	servingStarted      mclock.AbsTime
	servingMaxCost      uint64
	intValueWhenStarted int64
}

type ClientManager struct {
	child     *ClientManager
	lock      sync.RWMutex
	nodes     map[*ClientNode]struct{}
	enabledCh chan struct{}

	parallelReqs, maxParallelReqs int
	targetParallelReqs            float64
	queue                         *prque.Prque

	intLastUpdate                                      mclock.AbsTime
	intLog, intTimeConst, intLogMin, intLogMax, pConst float64
	intValue                                           int64
}

func NewClientManager(maxParallelReqs int, targetParallelReqs float64, child *ClientManager) *ClientManager {
	cm := &ClientManager{
		nodes: make(map[*ClientNode]struct{}),
		child: child,
		queue: prque.New(),

		maxParallelReqs:    maxParallelReqs,
		targetParallelReqs: targetParallelReqs,
		intTimeConst:       1 / float64(time.Millisecond*100),
		intLog:             math.Log(0.01),
		intLogMin:          math.Log(0.01),
		intLogMax:          0.5,
		pConst:             1,
	}
	return cm
}

func (cm *ClientManager) isEnabled() bool {
	return cm.enabledCh == nil
}

func (cm *ClientManager) setEnabled(en bool) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if cm.isEnabled() == en {
		return
	}
	if en {
		close(cm.enabledCh)
		cm.enabledCh = nil
	} else {
		cm.enabledCh = make(chan struct{})
	}
	if cm.child != nil && cm.parallelReqs == 0 {
		cm.child.setEnabled(en)
	}
}

func (cm *ClientManager) setParallelReqs(p int, time mclock.AbsTime) {
	if p == cm.parallelReqs {
		return
	}
	if cm.child != nil && cm.isEnabled() {
		if cm.parallelReqs == 0 {
			cm.child.setEnabled(false)
		}
		if p == 0 {
			cm.child.setEnabled(true)
		}
	}

	cm.updateIntegrator(time)
	cm.parallelReqs = p
}

func (cm *ClientManager) updateIntegrator(time mclock.AbsTime) {
	dt := float64(time - cm.intLastUpdate)
	a := float64(cm.parallelReqs)/cm.targetParallelReqs - 1
	iOld := cm.intLog
	iDiff := a * dt * cm.intTimeConst
	iNew := iOld + iDiff
	cm.intLog = iNew
	if cm.intLog < cm.intLogMin {
		cm.intLog = cm.intLogMin
	}
	if cm.intLog > cm.intLogMax {
		cm.intLog = cm.intLogMax
	}
	dtFlat := float64(0)
	if cm.intLog == iOld {
		dtFlat = dt
	} else if cm.intLog != iNew && (iDiff > 1e-30 || iDiff < -1e-30) {
		dtFlat = (iNew - cm.intLog) * dt / iDiff
	}

	pCorr := a * cm.pConst
	iOld += pCorr
	iNew = cm.intLog + pCorr
	// l(t) goes from iOld to iNew in (dt-dtFlat) time, then has a flat section with the length of dtFlat
	// we are looking for int(e^l(t))
	expInt := float64(0)
	expNew := math.Exp(iNew)
	iDiff = iNew - iOld
	if dtFlat != dt {
		if iDiff > 1e-3 || iDiff < -1e-3 {
			expInt = (expNew - math.Exp(iOld)) * (dt - dtFlat) / iDiff
		} else {
			expInt = (expNew + math.Exp(iOld)) * (dt - dtFlat) / 2
		}
	}
	if dtFlat > 0 {
		expInt += dtFlat * expNew
	}

	cm.intValue += int64(expInt)
	cm.intLastUpdate = time
}

func (cm *ClientManager) GetIntegratorValues() (float64, int64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.updateIntegrator(Clock.Now())
	return cm.intLog, cm.intValue
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
		cm.queue.Push(start, float32(node.bufValue)/float32(node.params.BufLimit))
		return ch
	}

	cm.setParallelReqs(cm.parallelReqs+1, time)
	node.servingStarted = time
	node.intValueWhenStarted = cm.intValue
	node.servingMaxCost = maxCost
	return nil
}

func (cm *ClientManager) started(node *ClientNode, maxCost uint64, time mclock.AbsTime) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.updateIntegrator(time)
	node.servingStarted = time
	node.intValueWhenStarted = cm.intValue
	node.servingMaxCost = maxCost
}

func (cm *ClientManager) processed(node *ClientNode, time mclock.AbsTime) (realCost uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.updateIntegrator(time)
	bvc := cm.intValue - node.intValueWhenStarted
	if bvc < 0 {
		bvc = 0
	}
	if bvc > int64(node.servingMaxCost) {
		bvc = int64(node.servingMaxCost)
	}
	node.bufValue += node.servingMaxCost - uint64(bvc)
	if node.bufValue > node.params.BufLimit {
		node.bufValue = node.params.BufLimit
	}

	realCost = uint64(time - node.servingStarted)
	for !cm.queue.Empty() {
		if cm.queue.PopItem().(func() bool)() {
			return
		}
	}
	cm.setParallelReqs(cm.parallelReqs-1, time)
	return
}
