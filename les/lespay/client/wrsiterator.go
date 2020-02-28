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

package utils

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

type (
	WrsIterator struct {
		lock                                                 sync.Mutex
		ns                                                   *NodeStateMachine
		wrs                                                  *WeightedRandomSelect
		requireStates, disableStates, stSelected, stReturned NodeStateBitMask
		enrFieldID                                           int
		wakeup                                               chan struct{}
		nextID                                               enode.ID
		closed                                               bool
	}
)

func NewWrsIterator(ns *NodeStateMachine, requireStates, disableStates NodeStateBitMask, wfn func(interface{}) uint64, enrFieldID int) *WrsIterator {
	w := &WrsIterator{
		ns:            ns,
		wrs:           NewWeightedRandomSelect(wfn),
		requireStates: requireStates,
		disableStates: disableStates,
		stSelected:    ns.GetState("selected"),
		stReturned:    ns.GetState("returned"),
	}
	ns.AddStateSub(requireStates|disableStates, w.nodeEvent)
	return w
}

func (w *WrsIterator) nodeEvent(id enode.ID, oldState, newState NodeStateBitMask) {
	w.lock.Lock()
	defer w.lock.Unlock()

	ps := (oldState&w.disableStates) == 0 && (oldState&w.requireStates) == w.requireStates
	ns := (newState&w.disableStates) == 0 && (newState&w.requireStates) == w.requireStates
	if ps == ns {
		return
	}
	if ns {
		w.wrs.Update(id)
		if w.wakeup != nil && !w.wrs.IsEmpty() {
			close(w.wakeup)
			w.wakeup = nil
		}
	} else {
		w.wrs.Remove(id)
	}
}

func (w *WrsIterator) Next() bool {
	w.lock.Lock()
	defer w.lock.Unlock()

	for {
		if w.closed {
			return false
		}
		n := w.wrs.Choose()
		if n != nil {
			w.nextID = n.(enode.ID)
			w.ns.UpdateState(w.nextID, w.stSelected, 0, time.Second*5)
			return true
		}
		ch := make(chan struct{})
		w.wakeup = ch
		w.lock.Unlock()
		<-ch
		w.lock.Lock()
	}
}

func (w *WrsIterator) Close() {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.closed = true
	if w.wakeup != nil {
		close(w.wakeup)
		w.wakeup = nil
	}
}

func (w *WrsIterator) Node() *enode.Node {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.ns.UpdateState(w.nextID, w.stReturned, w.stSelected, time.Second*5)
	enr := w.ns.GetField(w.nextID, w.enrFieldID).(*enr.Record)
	node, err := enode.New(enode.V4ID{}, enr)
	if err != nil {
		panic(err) // ???
	}
	return node
}
