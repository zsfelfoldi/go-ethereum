// Copyright 2020 The go-ethereum Authors
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

package client

import (
	"sync"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

type ThresholdSet struct {
	lock                 sync.Mutex
	ns                   *nodestate.NodeStateMachine
	setFlags, clearFlags nodestate.Flags
	thresholdField       nodestate.Field
	queue                *prque.Prque
}

// NewFillSet creates a new FillSet
func NewThresholdSet(ns *nodestate.NodeStateMachine, thresholdField nodestate.Field, setFlags, clearFlags nodestate.Flags) *ThresholdSet {
	ts := &ThresholdSet{
		ns:             ns,
		setFlags:       setFlags,
		clearFlags:     clearFlags,
		thresholdField: thresholdField,
		queue:          prque.New(),
	}
	ns.SubscribeField(thresholdField, func(n *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		if threshold, ok := newValue.(int64); ok {
			ts.lock.Lock()
			ts.queue.Push(n, threshold)
			ts.lock.Unlock()
		}
	})
	return ts
}

func (ts *ThresholdSet) SetThreshold(threshold int64) {
	ts.lock.Lock()
	for ts.queue.Size() != 0 {
		item, pri := ts.queue.Pop()
		node := item.(*enode.Node)
		if pri == ts.ns.GetField(node, ts.thresholdField).(int64) {
			if pri > threshold {
				ts.queue.Push(item, pri)
				break
			}
			ts.ns.SetState(node, ts.setFlags, ts.clearFlags, 0)
			ts.ns.SetField(node, ts.thresholdField, nil)
		}
	}
	ts.lock.Unlock()
}
