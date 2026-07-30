// Copyright 2026 The go-ethereum Authors
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

package logquery

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/logindex"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

func TestSingleMatcherProcess(t *testing.T) {
	ts := newTestScheduler()
}

type testMatcher struct {
	pos []logindex.IndexPosition
}

func (tm *testMatcher) newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance {
	var start int
	for start < len(tm.pos) && reader.comparePosition(tm.pos[start], first) < 0 {
		start++
	}
	stop := start
	for stop < len(tm.pos) && reader.comparePosition(tm.pos[stop], last) <= 0 {
		stop++
	}
	return &testMatcherInstance{
		reader:   reader,
		pos:      tm.pos[start:stop],
		returnCh: make(chan struct{}),
	}
}

type testMatcherInstance struct {
	reader        *directionalReader
	pos           []logindex.IndexPosition
	blockRequests []testBlockRequest
	returnCh      chan struct{}
}

type testBlockRequest struct {
	number uint64
	fn     logindex.DeliverBlockFn
}

type testBlockData struct {
	header   *types.Header
	body     *types.Body
	receipts types.Receipts
}

func (tmi *testMatcherInstance) next() (*logindex.IndexPosition, logicNodeID, error) {
	<-tmi.returnCh
	if len(tmi.pos) == 0 {
		return nil, logicNodeID{}, nil
	}
	return &tmi.pos[0], logicNodeID{}, nil
}

func (tmi *testMatcherInstance) advance(pos *logindex.IndexPosition) error {
	<-tmi.returnCh
	if pos == nil {
		if len(tmi.pos) != 0 {
			tmi.pos = tmi.pos[1:]
		}
	} else {
		for len(tmi.pos) != 0 && tmi.reader.comparePosition(tmi.pos[0], pos) < 0 {
			tmi.pos = tmi.pos[1:]
		}
	}
	return nil
}

func (tmi *testMatcherInstance) split(lb *logicBuilder, blockNumber uint64) matcherInstance {
	splitPos := logindex.IndexPosition{BlockNumber: blockNumber}
	var splitAt int
	for splitAt < len(tmi.pos) && tmi.reader.comparePosition(tmi.pos[splitAt], splitPos) < 0 {
		splitAt++
	}
	newPos := tmi.pos[splitAt:]
	tmi.pos = tmi.pos[:splitAt]
	return &testMatcherInstance{
		reader:   tmi.reader,
		pos:      newPos,
		returnCh: make(chan struct{}),
	}
}

type testScheduler struct {
	lock       sync.Mutex
	processes  map[*matcherProcess]*testProcessState
	blockData  map[uint64]testBlockData
	registerCh chan *matcherProcess
	stopCh     chan struct{}
}

func newTestScheduler() {
	return &testScheduler{
		processes:  make(map[*matcherProcess]*testProcessState),
		blockData:  make(map[uint64]testBlockData),
		registerCh: make(chan *matcherProcess),
		stopCh:     make(chan struct{}),
	}
}

type testProcessState struct {
	waitOnMatcher bool // waits on block delivery if false
	priority      [bool]int
}

func (ts *testScheduler) GetRangeReaders(common.Hash, common.Range[uint64]) []*logindex.TableReader {}

func (ts *testScheduler) RequestBlock(refBlockHash common.Hash, blockNumber uint64, deliverFn logindex.DeliverBlockFn) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	for mp := range ts.processes {
		if blockNumber >= min(mp.firstBlock, mp.lastBlock) && blockNumber <= max(mp.firstBlock, mp.lastBlock) {
			tmi := mp.matcher.(*testMatcherInstance)
			tmi.blockRequests = append(tmi.blockRequests, testBlockRequest{number: blockNumber, fn: deliverFn})
			return
		}
	}
	deliverFn(logindex.BlockRequest{BlockNumber: blockNumber, NeedBody: true, NeedReceipts: true}, nil, nil, nil)
}

func (ts *testScheduler) registerHook(mp *matcherProcess) chan bool {
	mp.testHook = make(chan bool)
	ts.registerCh <- mp
}

func (ts *testScheduler) returnMatcher(mp *matcherProcess) {
	tmi := mp.matcher.(*testMatcherInstance)
	select {
	case tmi.returnCh <- struct{}{}:
	default:
		ts.t.Fatalf("Test matcher to be returned was not waiting")
	}
}

func (ts *testScheduler) deliverBlockTo(mp *matcherProcess) {
	tmi := mp.matcher.(*testMatcherInstance)
	if len(tmi.blockRequests) == 0 {
		ts.t.Fatalf("No block requests waiting for delivery")
	}
	req := tmi.blockRequests[0]
	tmi.blockRequests = tmi.blockRequests[1:]
	bd, ok := ts.blockData[req.number]
	if !ok {
		ts.t.Fatalf("Requested block data not available", "number", req.number)
	}
	req.fn(logindex.BlockRequest{BlockNumber: req.number, NeedBody: true, NeedReceipts: true}, bd.header, bd.body, bd.receipts)
}

func (ts *testScheduler) run() {
	for {
		ts.lock.Lock()
		select {
		case mp := <-ts.registerCh:
			waitOnMatcher, ok := <-mp.testHook
			if ok {
				ts.processes[mp] = testProcessState{waitOnMatcher: waitOnMatcher, priority: [bool]int{rand.Intn(1000000), rand.Intn(1000000)}}
			}
		case <-ts.stopCh:
			return
		default:
		}
		var (
			bestMp    *matcherProcess
			bestState testProcessState
		)
		bestPri := 1000000
		for mp, state := range ts.processes {
			if pri := state.priority[state.waitOnMatcher]; pri < bestPri {
				bestMp, bestState, bestPri = mp, state, pri
			}
		}
		ts.lock.Unlock()
		if bestMp != nil {
			if bestState.waitOnMatcher {
				ts.returnMatcher(bestMp)
			} else {
				ts.deliverBlockTo(bestMp)
			}
			waitOnMatcher, ok := <-bestMp.testHook
			ts.lock.Lock()
			if ok {
				bestState.waitOnMatcher = waitOnMatcher
			} else {
				delete(ts.processes, bestMp)
			}
			ts.lock.Unlock()
		} else {
			time.Sleep(time.Millisecond)
		}
	}
}

func (ts *testScheduler) stop() {
	close(ts.stopCh)
}
