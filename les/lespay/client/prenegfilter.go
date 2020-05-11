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
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

// PreNegFilter is a filter on an enode.Iterator that performs connection pre-negotiation
// using the provided callback and only returns nodes that gave a positive answer recently.
type PreNegFilter struct {
	lock                         sync.Mutex
	cond                         *sync.Cond
	ns                           *nodestate.NodeStateMachine
	sfQueried, sfCanDial         nodestate.Flags
	queryTimeout, canDialTimeout time.Duration
	input, canDialIter           enode.Iterator
	query                        PreNegQuery
	pending                      map[*enode.Node]struct{}
	waiting                      map[*enode.Node]struct{}
	needQueries                  int
	maxPendingQueries            int
	waitingForNext, closed       bool
}

// PreNegQuery callback performs connection pre-negotiation.
type PreNegQuery func(n *enode.Node, result func(canDial bool)) func()

// NewPreNegFilter creates a new PreNegFilter. sfQueried is set for each queried node, sfCanDial
// is set together with sfQueried being reset if the callback returned a positive answer. The output
// iterator returns nodes with an active sfCanDial flag but does not automatically reset the flag
// (the dialer can do that together with setting the dialed flag).
// The filter starts at most the specified number of simultaneous queries if there are no nodes
// with an active sfCanDial flag and the output iterator is already being read. Note that until
// sfCanDial is reset or times out the filter won't start more queries even if the dial candidate
// has been returned by the output iterator.
func NewPreNegFilter(ns *nodestate.NodeStateMachine, input enode.Iterator, query PreNegQuery, sfQueried, sfCanDial nodestate.Flags, maxPendingQueries int, queryTimeout, canDialTimeout time.Duration) *PreNegFilter {
	pf := &PreNegFilter{
		ns:                ns,
		input:             input,
		query:             query,
		sfQueried:         sfQueried,
		sfCanDial:         sfCanDial,
		queryTimeout:      queryTimeout,
		maxPendingQueries: maxPendingQueries,
		canDialIter:       NewQueueIterator(ns, sfCanDial, nodestate.Flags{}, false),
		pending:           make(map[*enode.Node]struct{}),
		waiting:           make(map[*enode.Node]struct{}),
	}
	pf.cond = sync.NewCond(&pf.lock)
	ns.SubscribeState(sfQueried.Or(sfCanDial), func(n *enode.Node, oldState, newState nodestate.Flags) {
		pf.lock.Lock()
		defer pf.lock.Unlock()

		if oldState.HasAll(sfCanDial) {
			delete(pf.waiting, n)
		}
		if newState.HasAll(sfCanDial) {
			pf.waiting[n] = struct{}{}
		}
		// Query timeout, remove it from the pending set and spin up one more query.
		if oldState.HasAll(sfQueried) && newState.HasNone(sfQueried.Or(sfCanDial)) {
			if _, exist := pf.pending[n]; exist {
				delete(pf.pending, n)
				pf.checkQuery()
			}
		}
	})
	go pf.readLoop()
	return pf
}

// checkQuery checks whether we need more queries and signals readLoop if necessary.
func (pf *PreNegFilter) checkQuery() {
	if pf.waitingForNext && len(pf.waiting) == 0 {
		pf.needQueries = pf.maxPendingQueries
	}
	if pf.needQueries > len(pf.pending) {
		diff := pf.needQueries - len(pf.pending)
		for i := 0; i < diff; i++ {
			pf.cond.Signal()
		}
	}
}

// readLoop reads nodes from the input iterator and starts new queries if necessary
func (pf *PreNegFilter) readLoop() {
	for {
		pf.lock.Lock()
		for pf.needQueries <= len(pf.pending) {
			// either no queries are needed or we have enough pending;
			// wait until more are needed
			pf.cond.Wait()
			if pf.closed {
				pf.lock.Unlock()
				return
			}
		}
		pf.lock.Unlock()

		// fetch a node from the input that is not pending at the moment
		var node *enode.Node
		for {
			if !pf.input.Next() {
				pf.canDialIter.Close()
				return
			}
			node = pf.input.Node()

			pf.lock.Lock()
			_, pending := pf.pending[node]
			pf.lock.Unlock()
			if !pending {
				break
			}
		}
		// set sfQueried and start the query
		pf.ns.SetState(node, pf.sfQueried, nodestate.Flags{}, pf.queryTimeout)
		start := pf.query(node, func(canDial bool) {
			if canDial {
				pf.lock.Lock()
				delete(pf.pending, node)
				pf.needQueries = 0
				pf.lock.Unlock()
				pf.ns.SetState(node, pf.sfCanDial, pf.sfQueried, pf.canDialTimeout)
			} else {
				pf.lock.Lock()
				delete(pf.pending, node)
				pf.checkQuery()
				pf.lock.Unlock()
				pf.ns.SetState(node, nodestate.Flags{}, pf.sfQueried, 0)
			}
		})
		// add pending entry before actually starting
		pf.lock.Lock()
		pf.pending[node] = struct{}{}
		pf.lock.Unlock()
		start()
	}
}

// Next moves to the next selectable node.
func (pf *PreNegFilter) Next() bool {
	pf.lock.Lock()
	pf.waitingForNext = true // start queries if we cannot give a result immediately
	pf.checkQuery()
	pf.lock.Unlock()

	next := pf.canDialIter.Next()
	pf.lock.Lock()
	pf.needQueries = 0
	pf.waitingForNext = false
	delete(pf.waiting, pf.Node())
	pf.lock.Unlock()
	return next
}

// Close ends the iterator.
func (pf *PreNegFilter) Close() {
	pf.lock.Lock()
	pf.closed = true
	pf.cond.Signal()
	pf.lock.Unlock()
	pf.input.Close()
	pf.canDialIter.Close()
}

// Node returns the current node.
func (pf *PreNegFilter) Node() *enode.Node {
	return pf.canDialIter.Node()
}
