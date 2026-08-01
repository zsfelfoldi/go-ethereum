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
	"context"
	crand "crypto/rand"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/logindex"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

const (
	testMaxTxCount    = 2
	testMaxLogCount   = 2
	testMaxTopicCount = 1
)

func TestSingleMatcherProcess(t *testing.T) {
	for minTopicCount := range 2 {
		for _, reverse := range []bool{false, true} {
			for _, limited := range []bool{false, true} {
				testSingleMatcherProcess(t, reverse, limited, minTopicCount)
			}
		}
	}
}

func testSingleMatcherProcess(t *testing.T, reverse, limited bool, minTopicCount int) {
	blockCount := uint64(1024)
	desc := fmt.Sprintf("[reverse %v, limited %v, minTopicCount %d]", reverse, limited, minTopicCount)
	ts := newTestScheduler(t, desc, blockCount, blockCount, 64, minTopicCount, reverse)
	positions, logs := ts.getMatches(0, blockCount-1)
	matcher := &testMatcher{pos: positions}
	if len(matcher.pos) == 0 {
		limited = false
	}
	maxResults := math.MaxInt
	if limited {
		maxResults = len(matcher.pos) / 2
	}
	first := logindex.IndexPosition{}
	last := logindex.IndexPosition{
		BlockNumber: blockCount - 1,
		TxIndex:     math.MaxUint32,
		LogIndex:    math.MaxUint32,
	}
	if reverse {
		first, last = last, first
	}
	instance := matcher.newInstance(context.Background(), &directionalReader{reader: ts.readers[0], reverse: reverse}, nil, first, last)
	session := newSession(context.Background(), nil, common.Hash{}, minTopicCount, maxResults, reverse)
	mp := newMatcherProcess(ts, instance, session, ts.readers[0], 0, blockCount-1)
	mp.testRegisterHook = ts.registerHook
	go ts.run()
	mp.run()
	ts.stop()
	if len(mp.positions) > len(positions) {
		ts.t.Fatalf("%s: too many matching positions returned (expected max %d, got %d)", ts.desc, len(positions), len(mp.positions))
	}
	if limited && len(logs) > 0 && !ts.isValidMatch(logs[len(logs)-1]) {
		ts.t.Fatalf("%s: last position returned for limited search is an invalid match", ts.desc)
	}
	var validCount int
	for i, pos := range mp.positions {
		if pos != positions[i] {
			ts.t.Fatalf("%s: invalid position #%d (expected max %v, got %v)", ts.desc, i, positions[i], pos)
		}
		if !ts.isMatch(logs[i]) {
			ts.t.Fatalf("%s: non-matching log returned", ts.desc)
		}
		if ts.isValidMatch(logs[i]) {
			validCount++
		}
	}
	if limited && validCount != maxResults {
		ts.t.Fatalf("%s: invalid number of valid matches for limited search (expected %v, got %v)", ts.desc, maxResults, validCount)
	}
	if !limited && len(mp.positions) != len(positions) {
		ts.t.Fatalf("%s: invalid number of returned positions for unlimited search (expected %v, got %v)", ts.desc, len(positions), len(mp.positions))
	}
}

type testMatcher struct {
	pos []logindex.IndexPosition
}

func (tm *testMatcher) newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance {
	var start int
	for start < len(tm.pos) && reader.comparePosition(&tm.pos[start], &first) < 0 {
		start++
	}
	stop := start
	for stop < len(tm.pos) && reader.comparePosition(&tm.pos[stop], &last) <= 0 {
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

func (tmi *testMatcherInstance) next() (*logindex.IndexPosition, logicNodeID, error) {
	<-tmi.returnCh
	if len(tmi.pos) == 0 {
		return nil, 0, nil
	}
	return &tmi.pos[0], 0, nil
}

func (tmi *testMatcherInstance) advance(pos *logindex.IndexPosition) error {
	<-tmi.returnCh
	if pos == nil {
		if len(tmi.pos) != 0 {
			tmi.pos = tmi.pos[1:]
		}
	} else {
		for len(tmi.pos) != 0 && tmi.reader.comparePosition(&tmi.pos[0], pos) < 0 {
			tmi.pos = tmi.pos[1:]
		}
	}
	return nil
}

func (tmi *testMatcherInstance) split(lb *logicBuilder, blockNumber uint64) matcherInstance {
	splitPos := logindex.IndexPosition{BlockNumber: blockNumber}
	var splitAt int
	for splitAt < len(tmi.pos) && tmi.reader.comparePosition(&tmi.pos[splitAt], &splitPos) < 0 {
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
	t                           *testing.T
	desc                        string
	lock                        sync.Mutex
	matchDensity, minTopicCount int
	reverse                     bool
	processes                   map[*matcherProcess]*testProcessState
	blocks                      []*types.Block
	receipts                    []types.Receipts
	readers                     []*logindex.TableReader
	registerCh                  chan *matcherProcess
	stopCh                      chan struct{}
}

func newTestScheduler(t *testing.T, desc string, blockCount, tableSize uint64, matchDensity, minTopicCount int, reverse bool) *testScheduler {
	ts := &testScheduler{
		t:             t,
		desc:          desc,
		matchDensity:  matchDensity,
		minTopicCount: minTopicCount,
		reverse:       reverse,
		processes:     make(map[*matcherProcess]*testProcessState),
		registerCh:    make(chan *matcherProcess),
		stopCh:        make(chan struct{}),
	}
	gspec := &core.Genesis{
		Alloc:   types.GenesisAlloc{},
		BaseFee: big.NewInt(params.InitialBaseFee),
		Config:  params.TestChainConfig,
	}
	blockGen := func(i int, gen *core.BlockGen) {
		txCount := rand.Intn(testMaxTxCount + 1)
		for k := txCount; k > 0; k-- {
			receipt := types.NewReceipt(nil, false, 0)
			logCount := rand.Intn(testMaxLogCount + 1)
			receipt.Logs = make([]*types.Log, logCount)
			for i := range receipt.Logs {
				log := &types.Log{}
				receipt.Logs[i] = log
				crand.Read(log.Address[:])
				log.Topics = make([]common.Hash, rand.Intn(testMaxTopicCount+1))
			}
			gen.AddUncheckedReceipt(receipt)
			gen.AddUncheckedTx(types.NewTransaction(999, common.HexToAddress("0x999"), big.NewInt(999), 999, gen.BaseFee(), nil))
		}
	}
	_, blocks, receipts := core.GenerateChainWithGenesis(gspec, ethash.NewFaker(), int(blockCount-1), blockGen)
	ts.blocks = append([]*types.Block{gspec.ToBlock()}, blocks...)
	ts.receipts = append([]types.Receipts{types.Receipts{}}, receipts...)
	ts.readers = make([]*logindex.TableReader, blockCount/tableSize)
	for i := range ts.readers {
		ts.readers[i] = &logindex.TableReader{
			//IndexContract: testIndexContract,
			Meta: logindex.TableMeta{
				LastBlockNumber: uint64(i+1)*tableSize - 1,
				BlockCount:      tableSize,
			},
		}
	}
	return ts
}

func (ts *testScheduler) isMatch(log *types.Log) bool {
	return int(log.Address[0]) < ts.matchDensity
}

func (ts *testScheduler) isValidMatch(log *types.Log) bool {
	return len(log.Topics) >= ts.minTopicCount
}

func (ts *testScheduler) getMatches(firstBlock, lastBlock uint64) ([]logindex.IndexPosition, []*types.Log) {
	var (
		pos  []logindex.IndexPosition
		logs []*types.Log
	)
	for i := firstBlock; i <= lastBlock; i++ {
		receipts := ts.receipts[i]
		for txi, receipt := range receipts {
			for li, log := range receipt.Logs {
				if ts.isMatch(log) {
					pos = append(pos, logindex.IndexPosition{
						BlockNumber: uint64(i),
						TxIndex:     uint32(txi),
						LogIndex:    uint32(li),
					})
					logs = append(logs, log)
				}
			}
		}
	}
	if ts.reverse {
		last := len(pos) - 1
		for i := 0; i < last-i; i++ {
			pos[i], pos[last-i] = pos[last-i], pos[i]
		}
	}
	return pos, logs
}

func (ts *testScheduler) GetRangeReaders(refBlockHash common.Hash, readerRange common.Range[uint64]) []*logindex.TableReader {
	var res []*logindex.TableReader
	for _, reader := range ts.readers {
		if !reader.BlockRange().Intersection(readerRange).IsEmpty() {
			res = append(res, reader)
		}
	}
	return res
}

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
	deliverFn(logindex.BlockRequest{Number: blockNumber, NeedBody: true, NeedReceipts: true}, nil, nil, nil)
}

func (ts *testScheduler) registerHook(mp *matcherProcess) {
	mp.testHook = make(chan int)
	ts.registerCh <- mp
}

func (ts *testScheduler) returnMatcher(mp *matcherProcess) {
	tmi := mp.matcher.(*testMatcherInstance)
	select {
	case tmi.returnCh <- struct{}{}:
	default:
		ts.t.Fatalf("%s: test matcher to be returned was not waiting", ts.desc)
	}
}

func (ts *testScheduler) deliverBlockTo(mp *matcherProcess) {
	tmi := mp.matcher.(*testMatcherInstance)
	if len(tmi.blockRequests) == 0 {
		ts.t.Fatalf("%s: no block requests waiting for delivery", ts.desc)
	}
	req := tmi.blockRequests[0]
	tmi.blockRequests = tmi.blockRequests[1:]
	req.fn(logindex.BlockRequest{Number: req.number, NeedBody: true, NeedReceipts: true}, ts.blocks[req.number].Header(), ts.blocks[req.number].Body(), ts.receipts[req.number])
}

type testProcessState struct {
	waitingFor int
	priority   [2]int
}

func (ts *testScheduler) run() {
	for {
		ts.lock.Lock()
		select {
		case mp := <-ts.registerCh:
			waitingFor, ok := <-mp.testHook
			if ok {
				ts.processes[mp] = &testProcessState{waitingFor: waitingFor, priority: [2]int{rand.Intn(1000000), rand.Intn(1000000)}}
			}
		case <-ts.stopCh:
			return
		default:
		}
		var (
			bestMp    *matcherProcess
			bestState *testProcessState
		)
		bestPri := 1000000
		for mp, state := range ts.processes {
			if pri := state.priority[state.waitingFor]; pri < bestPri {
				bestMp, bestState, bestPri = mp, state, pri
			}
		}
		ts.lock.Unlock()
		if bestMp != nil {
			switch bestState.waitingFor {
			case testWaitMatcher:
				ts.returnMatcher(bestMp)
			case testWaitDeliver:
				ts.deliverBlockTo(bestMp)
			default:
				panic(nil)
			}
			waitingFor, ok := <-bestMp.testHook
			ts.lock.Lock()
			if ok {
				bestState.waitingFor = waitingFor
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
