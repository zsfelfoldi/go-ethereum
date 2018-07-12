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

// Package flowcontrol implements a client side flow control mechanism
package flowcontrol

import (
	"sync"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
)

// cmNodeFields are ClientNode fields used by the client manager
// Note: these fields are locked by the client manager's mutex
type cmNodeFields struct {
	corrBufValue   int64
	rcLastIntValue int64
	rcFullIntValue int64
	queueIndex     int // -1 if not queued
}

const FixedPointMultiplier = 1000000

type PieceWiseLinear []struct{ X, Y uint64 }

func (pwl PieceWiseLinear) ValueAt(x uint64) float64 {
	l := 0
	h := len(pwl)
	if h == 0 {
		return 0
	}
	for h != l {
		m := (l + h) / 2
		if x > pwl[m].X {
			l = m + 1
		} else {
			h = m
		}
	}
	if l == 0 {
		return float64(pwl[0].Y)
	}
	l--
	if h == len(pwl) {
		return float64(pwl[l].Y)
	}
	dx := pwl[h].X - pwl[l].X
	if dx < 1 {
		return float64(pwl[l].Y)
	}
	return float64(pwl[l].Y) + float64(pwl[h].Y-pwl[l].Y)*float64(x-pwl[l].X)/float64(dx)
}

func (pwl PieceWiseLinear) Valid() bool {
	var lastX uint64
	for _, i := range pwl {
		if i.X < lastX {
			return false
		}
		lastX = i.X
	}
	return true
}

// ClientManager controls the bandwidth assigned to the clients of a server.
// Since ServerParams guarantee a safe lower estimate for processable requests
// even in case of all clients being active, ClientManager calculates a
// corrigated buffer value and usually allows a higher remaining buffer value
// to be returned with each reply.
type ClientManager struct {
	clock     mclock.Clock
	lock      sync.Mutex
	nodes     map[*ClientNode]struct{}
	enabledCh chan struct{}

	curve          PieceWiseLinear
	sumRecharge    uint64
	rcLastUpdate   mclock.AbsTime
	rcLastIntValue int64 // normalized to MRR=FixedPointMultiplier
	rcQueue        *prque.Prque
}

// NewClientManager returns a new client manager. Multiple client managers can
// be chained to realize priority levels. Each level has its own manager, the
// parent has the higher priority (while the parent is processing a request
// the child is disabled).
func NewClientManager(curve PieceWiseLinear, clock mclock.Clock) *ClientManager {
	cm := &ClientManager{
		clock:   clock,
		nodes:   make(map[*ClientNode]struct{}),
		rcQueue: prque.New(func(a interface{}, i int) { a.(*ClientNode).queueIndex = i }),
		curve:   curve,
	}
	return cm
}

func (cm *ClientManager) SetBandwidthCurve(curve PieceWiseLinear) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.updateRecharge(cm.clock.Now())
	cm.curve = curve
}

func (cm *ClientManager) updateRecharge(time mclock.AbsTime) {
	lastUpdate := cm.rcLastUpdate
	cm.rcLastUpdate = time
	for cm.sumRecharge > 0 {
		slope := cm.curve.ValueAt(cm.sumRecharge) / float64(cm.sumRecharge)
		if slope < 1 {
			slope = 1
		}
		dt := time - lastUpdate
		q := cm.rcQueue.PopItem()
		if q == nil {
			cm.rcLastIntValue += int64(slope * float64(dt))
			return
		}
		rcqNode := q.(*ClientNode)
		dtNext := mclock.AbsTime(float64(rcqNode.rcFullIntValue-cm.rcLastIntValue) / slope)
		if dt < dtNext {
			cm.rcQueue.Push(rcqNode, -rcqNode.rcFullIntValue)
			cm.rcLastIntValue += int64(slope * float64(dt))
			return
		}
		if rcqNode.corrBufValue < int64(rcqNode.params.BufLimit) {
			rcqNode.corrBufValue = int64(rcqNode.params.BufLimit)
			cm.sumRecharge -= rcqNode.params.MinRecharge
		}
		lastUpdate += dtNext
		cm.rcLastIntValue = rcqNode.rcFullIntValue
	}
}

func (cm *ClientManager) updateNodeRc(node *ClientNode, bvc int64, time mclock.AbsTime) {
	cm.updateRecharge(time)
	wasFull := true
	if node.corrBufValue != int64(node.params.BufLimit) {
		wasFull = false
		node.corrBufValue += (cm.rcLastIntValue - node.rcLastIntValue) * int64(node.params.MinRecharge) / FixedPointMultiplier
		if node.corrBufValue > int64(node.params.BufLimit) {
			node.corrBufValue = int64(node.params.BufLimit)
		}
		node.rcLastIntValue = cm.rcLastIntValue
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
	if wasFull && !isFull {
		cm.sumRecharge += node.params.MinRecharge
	}
	if !wasFull && isFull {
		cm.sumRecharge -= node.params.MinRecharge
	}
	if !isFull {
		if node.queueIndex != -1 {
			cm.rcQueue.Remove(node.queueIndex)
		}
		node.rcLastIntValue = cm.rcLastIntValue
		node.rcFullIntValue = cm.rcLastIntValue + (int64(node.params.BufLimit)-node.corrBufValue)*FixedPointMultiplier/int64(node.params.MinRecharge)
		cm.rcQueue.Push(node, -node.rcFullIntValue)
	}
}

func (cm *ClientManager) init(node *ClientNode) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	node.corrBufValue = int64(node.params.BufLimit)
	node.rcLastIntValue = cm.rcLastIntValue
	node.queueIndex = -1
}

func (cm *ClientManager) accepted(node *ClientNode, maxCost uint64, now mclock.AbsTime) (priority int64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.updateNodeRc(node, -int64(maxCost), now)
	rcTime := (node.params.BufLimit - uint64(node.corrBufValue)) * FixedPointMultiplier / node.params.MinRecharge
	return -int64(now) - int64(rcTime)
}

// Note: processed should always be called for all accepted requests
func (cm *ClientManager) processed(node *ClientNode, maxCost, servingTime uint64, now mclock.AbsTime) (realCost uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	realCost = servingTime
	if realCost > maxCost {
		realCost = maxCost
	}
	cm.updateNodeRc(node, int64(maxCost-realCost), now)
	if uint64(node.corrBufValue) > node.bufValue {
		node.bufValue = uint64(node.corrBufValue)
	}
	return
}
