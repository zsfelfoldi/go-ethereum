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
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/logindex"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

var ErrMatchAll = errors.New("match-all queries not allowed")

const (
	maxMatcherThreads        = 4 //TODO config
	splitAfter               = time.Millisecond * 5
	splitThreshold           = time.Millisecond * 20
	splitTarget              = time.Millisecond * 10
	updateEstimatesFrequency = time.Millisecond
	maxIncompleteResults     = 8
)

type FilterQuery struct {
	FirstBlock, LastBlock, MaxResults uint64
	Reverse                           bool
	Addresses                         []common.Address
	Topics                            [][]common.Hash
}

type logIndex interface {
	GetRangeReaders(common.Hash, common.Range[uint64]) []*logindex.TableReader
	RequestBlock(common.Hash, uint64, logindex.DeliverBlockFn)
}

type Matcher struct {
	lock                       sync.Mutex
	logIndex                   logIndex
	contractProver             contractProver
	threadCount, activeThreads int
	requestCounter             uint64
	sessions                   map[*matcherSession]struct{} // accessed under lock mutex
	processing                 []*matcherProcess            // status == mpRunning
	startProcessCh             []chan *matcherProcess       // control -> worker
	stoppedProcessCh           chan int                     // worker -> control (thread index)
	sessionCh                  chan *matcherSession         // GetMatches -> control
	suspended, updateSort      processQueue
	updateTicker               *time.Ticker
	stopCh                     chan struct{}
	wg                         sync.WaitGroup
}

func NewMatcher(logIndex logIndex, contractProverBackend contractProverBackend) *Matcher {
	mc := &Matcher{
		logIndex:         logIndex,
		contractProver:   contractProver{backend: contractProverBackend},
		threadCount:      maxMatcherThreads,
		sessions:         make(map[*matcherSession]struct{}),
		processing:       make([]*matcherProcess, maxMatcherThreads),
		updateSort:       make(processQueue, maxMatcherThreads+1),
		startProcessCh:   make([]chan *matcherProcess, maxMatcherThreads),
		stoppedProcessCh: make(chan int),
		sessionCh:        make(chan *matcherSession),
		stopCh:           make(chan struct{}),
		updateTicker:     time.NewTicker(updateEstimatesFrequency),
	}
	for i := range maxMatcherThreads {
		mc.startProcessCh[i] = make(chan *matcherProcess, 1)
	}
	mc.updateTicker.Stop()
	mc.wg.Add(maxMatcherThreads + 1)
	go mc.controlLoop()
	for i := range maxMatcherThreads {
		go mc.workerLoop(i)
	}
	return mc
}

func (mc *Matcher) GetMatches(ctx context.Context, query FilterQuery, prove bool, refHeader *types.Header) ([]*types.Log, common.Range[uint64], *QueryProof, error) {
	start := time.Now()
	headBlock, refBlockHash := refHeader.Number.Uint64(), refHeader.Hash()
	firstBlock := min(query.FirstBlock, headBlock)
	lastBlock := min(query.LastBlock, headBlock)
	blockRange := common.NewRange[uint64](firstBlock, lastBlock+1-firstBlock)
	readerRange := blockRange
	if prove && lastBlock < headBlock {
		readerRange.SetLast(lastBlock + 1)
	}
	readers := mc.logIndex.GetRangeReaders(refBlockHash, readerRange)
	sort.Slice(readers, func(i, j int) bool {
		if query.Reverse {
			return readers[i].Meta.LastBlockNumber > readers[j].Meta.LastBlockNumber
		} else {
			return readers[i].Meta.LastBlockNumber < readers[j].Meta.LastBlockNumber
		}
	})
	for i := 1; i < len(readers); i++ {
		// check if the available tables cover a continuous range
		var cont bool
		if query.Reverse {
			cont = readers[i].BlockRange().AfterLast() == readers[i-1].BlockRange().First()
		} else {
			cont = readers[i-1].BlockRange().AfterLast() == readers[i].BlockRange().First()
		}
		if !cont {
			readers = readers[:i] // drop tables after a gap/overlap
			break
		}
	}
	matcher, err := newQueryMatcher(&query)
	if err != nil {
		return nil, common.Range[uint64]{}, nil, err
	}
	maxResults := math.MaxInt
	if uint64(maxResults) > query.MaxResults {
		maxResults = int(query.MaxResults)
	}
	session := newSession(ctx, mc, refBlockHash, len(query.Topics), maxResults, query.Reverse)
	//fmt.Println("create session", firstBlock, lastBlock, query.Addresses, query.Topics)
	var lastBlockProver *tableProver
	for i, tr := range readers {
		br := tr.BlockRange().Intersection(blockRange) // can be empty in the last table because of readerRange extension
		//fmt.Println(" ", br)
		prover := newTableProver(tr)
		if br.IsEmpty() {
			lastBlockProver = prover
			break
		}
		logicBuilder := prover.optimizer.newBuilderInstance()
		firstPos := logindex.IndexPosition{BlockNumber: br.First()}
		lastPos := logindex.IndexPosition{BlockNumber: br.Last(), TxIndex: math.MaxUint32, LogIndex: math.MaxUint32}
		firstBlock := br.First()
		lastBlock := br.Last()
		if query.Reverse {
			firstPos, lastPos = lastPos, firstPos
			firstBlock, lastBlock = lastBlock, firstBlock
		}
		matcherInstance := matcher.newInstance(
			ctx,
			&directionalReader{reader: tr, reverse: query.Reverse},
			logicBuilder,
			firstPos,
			lastPos,
		)
		mp := newMatcherProcess(mc.logIndex, matcherInstance, session, tr, firstBlock, lastBlock)
		mp.setProver(prover, logicBuilder)
		if i == 0 {
			session.first = mp
			session.last = mp
		} else {
			mp.prev = session.last
			session.last.next = mp
			session.last = mp
		}
	}
	//session.print()
	mc.lock.Lock()
	mc.sessions[session] = struct{}{}
	mc.lock.Unlock()
	defer func() {
		mc.lock.Lock()
		delete(mc.sessions, session)
		mc.lock.Unlock()
	}()

	select {
	case mc.sessionCh <- session:
	case <-ctx.Done():
		return nil, common.Range[uint64]{}, nil, ctx.Err()
	}
	var results *matcherResults
	select {
	case results = <-session.resultsCh:
	case <-ctx.Done():
		return nil, common.Range[uint64]{}, nil, ctx.Err()
	}
	if results == nil {
		return nil, common.Range[uint64]{}, nil, errors.New("entire search range has been invalidated") //TODO
	}
	if query.Reverse {
		last := len(results.logs) - 1
		for i := 0; i < last-i; i++ {
			results.logs[i], results.logs[last-i] = results.logs[last-i], results.logs[i]
		}
	}
	//fmt.Println("+++ Runtime without proof generation:", time.Since(start))
	var proof *QueryProof
	if prove {
		if query.Reverse {
			last := len(results.provers) - 1
			for i := 0; i < last-i; i++ {
				results.provers[i], results.provers[last-i] = results.provers[last-i], results.provers[i]
			}
			results.firstBlock, results.lastBlock = results.lastBlock, results.firstBlock
		}
		if lastBlockProver != nil && len(results.provers) > 0 &&
			lastBlockProver.reader.BlockRange().First() == results.provers[len(results.provers)-1].reader.BlockRange().AfterLast() {
			results.provers = append(results.provers, lastBlockProver)
		}
		proof = makeQueryProof(refHeader, &query, results.firstBlock, results.lastBlock, mc.contractProver, results.provers)
	}
	//fmt.Println(" results", len(results.logs), "error", results.err)
	//fmt.Println("+++ Runtime with proof generation:", time.Since(start))
	return results.logs, common.NewRange[uint64](results.firstBlock, results.lastBlock+1-results.firstBlock), proof, results.err
}

type matcherResults struct {
	logs                  []*types.Log
	provers               []*tableProver
	firstBlock, lastBlock uint64 // firstBlock >= lastBlock in case of reverse search
	err                   error
}

func (mc *Matcher) Stop() {
	close(mc.stopCh)
	mc.wg.Wait()
}

func newSession(ctx context.Context, mc *Matcher, refBlockHash common.Hash, minTopicCount int, maxResults int, reverse bool) *matcherSession {
	ms := &matcherSession{
		ctx:           ctx,
		refBlockHash:  refBlockHash,
		maxResults:    maxResults,
		reverse:       reverse,
		minTopicCount: minTopicCount,
		resultsCh:     make(chan *matcherResults, 1),
	}
	if mc != nil {
		ms.requestCounter = atomic.AddUint64(&mc.requestCounter, 1)
	}
	return ms
}

func (mc *Matcher) workerLoop(threadIndex int) {
	defer mc.wg.Done()

	for {
		select {
		case <-mc.stopCh:
			return
		case mp := <-mc.startProcessCh[threadIndex]:
			mp.run()
			select {
			case <-mc.stopCh:
				return
			case mc.stoppedProcessCh <- threadIndex:
			}
		}
	}
}

func (mc *Matcher) controlLoop() {
	defer mc.wg.Done()

	for {
		for mc.activeThreads < mc.threadCount && mc.suspended.Len() != 0 {
			//fmt.Println("controlLoop  total", mc.threadCount, "active", mc.activeThreads, "suspended", mc.suspended.Len())
			mp := heap.Pop(&mc.suspended).(*matcherProcess)
			_, canSplit := mp.getEstimatedResults()
			if mc.activeThreads+1 < mc.threadCount && canSplit {
				if mp2, err := mp.split(); err == nil {
					mc.start(mp)
					if mp2 != nil {
						mc.start(mp2)
					}
				} else {
					if !mp.session.finished {
						mp.session.finished, mp.session.err = true, err
						mp.session.returnResults()
					}
				}
			} else {
				mc.start(mp)
			}
		}
		select {
		case <-mc.stopCh:
			//fmt.Println("  ... stop control loop")
			return
		case stopped := <-mc.stoppedProcessCh:
			//fmt.Println("  ... stopped worker", stopped)
			mc.stopped(stopped)
		case session := <-mc.sessionCh:
			//fmt.Println("  ... starting session", session.requestCounter)
			mc.updateSessions(session.first)
		case <-mc.updateTicker.C:
			mc.updateSessions(nil)
		}
	}
}

func (mc *Matcher) start(mp *matcherProcess) {
	for i, running := range mc.processing {
		if running == nil {
			mc.processing[i] = mp
			mp.status = mpRunning
			//fmt.Println("starting process", mp.blockRange, "on worker", i)
			mc.startProcessCh[i] <- mp
			if mc.activeThreads == 0 {
				mc.updateTicker.Reset(updateEstimatesFrequency)
			}
			mc.activeThreads++
			return
		}
	}
	panic("no free threads available")
}

func (mc *Matcher) stopped(threadIndex int) {
	mp := mc.processing[threadIndex]
	mc.processing[threadIndex] = nil
	mp.status = mpInactive
	mc.activeThreads--
	if mc.activeThreads == 0 {
		mc.updateTicker.Stop()
	}
	if mp.err != nil && !mp.session.finished {
		mp.session.finished, mp.session.err = true, mp.err
		mp.session.returnResults()
	}
	mc.updateSessions(mp)
}

func (mc *Matcher) updateSessions(notRunning *matcherProcess) {
	mc.updateSort[0] = notRunning
	copy(mc.updateSort[1:], mc.processing)
	sort.Sort(mc.updateSort)
	allowedSplits := mc.threadCount - mc.activeThreads
	for i, mp := range mc.updateSort {
		if mp == nil {
			return
		}
		if i > 0 && mc.updateSort[i-1].session == mp.session {
			continue
		}
		allowedSplits = mc.updateSessionFrom(mp, allowedSplits)
	}
}

func (mc *Matcher) updateSessionFrom(start *matcherProcess, allowedSplits int) int {
	if start.session.finished {
		return allowedSplits
	}
	cumulativeResults, _ := start.getCumulativeResults()
	// iterate session process chain from first running process
	for mp := start; mp != nil; mp = mp.next {
		estimatedResults, canSplit := mp.getEstimatedResults()
		suspendNow := mp.session.finished
		if !suspendNow && canSplit && allowedSplits > 0 && mp.status == mpRunning {
			suspendNow = true
			allowedSplits--
		}
		mp.setCumulativeResults(cumulativeResults, suspendNow)
		if mp.status != mpRunning {
			mc.updateIdleStatus(mp, cumulativeResults)
			if mp.next == nil && mp.status == mpFinishedAll && !mp.session.finished {
				mp.session.finished = true
				mp.session.returnResults()
			}
		}
		cumulativeResults += estimatedResults
	}
	return allowedSplits
}

func (mc *Matcher) updateIdleStatus(mp *matcherProcess, cumulativeResults uint64) {
	newStatus := mp.newStatus(cumulativeResults)
	if newStatus == mp.status {
		return
	}
	//fmt.Println("updateIdleStatus", mp.blockRange, "old", mp.status, "new", newStatus)
	if mp.status == mpSuspended {
		heap.Remove(&mc.suspended, mp.pqIndex)
	}
	mp.status = newStatus
	if mp.status == mpSuspended {
		heap.Push(&mc.suspended, mp)
	}
}

func (mp *matcherProcess) newStatus(cumulativeResults uint64) int {
	if mp.session.finished {
		return mpFinishedAll
	}
	if mp.finished {
		if mp.prev == nil || mp.prev.status == mpFinishedAll {
			return mpFinishedAll
		}
		return mpFinished
	}
	if cumulativeResults+uint64(mp.completeValid) >= uint64(mp.session.maxResults) {
		if mp.prev == nil || mp.prev.status == mpFinishedAll {
			return mpFinishedAll
		}
		return mpInactive
	}
	return mpSuspended
}

type processQueue []*matcherProcess

func (pq processQueue) Len() int { return len(pq) }

func (pq processQueue) Less(i, j int) bool {
	switch {
	case pq[j] == nil:
		return true
	case pq[i] == nil:
		return false
	}
	switch {
	case pq[i].session.requestCounter < pq[j].session.requestCounter:
		return true
	case pq[i].session.requestCounter > pq[j].session.requestCounter:
		return false
	}
	if pq[i].session.reverse {
		return pq[i].firstBlock > pq[j].firstBlock
	} else {
		return pq[i].firstBlock < pq[j].firstBlock
	}
}

func (pq processQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	if pq[i] != nil {
		pq[i].pqIndex = i
	}
	if pq[j] != nil {
		pq[j].pqIndex = j
	}
}

func (pq *processQueue) Push(x any) {
	n := len(*pq)
	item := x.(*matcherProcess)
	item.pqIndex = n
	*pq = append(*pq, item)
}

func (pq *processQueue) Pop() any {
	n := len(*pq)
	item := (*pq)[n-1]
	(*pq)[n-1] = nil
	item.pqIndex = -1
	*pq = (*pq)[:n-1]
	return item
}

func (fq *FilterQuery) matchSpecified(log *types.Log) bool {
	if len(fq.Addresses) > 0 && !slices.Contains(fq.Addresses, log.Address) {
		return false
	}
	for i, sub := range fq.Topics {
		if len(sub) == 0 {
			continue // empty rule set == wildcard
		}
		if len(log.Topics) <= i || !slices.Contains(sub, log.Topics[i]) {
			return false
		}
	}
	return true
}

func (fq *FilterQuery) matchLength(log *types.Log) bool {
	return len(fq.Topics) <= len(log.Topics)
}

type matcherSession struct {
	ctx            context.Context
	refBlockHash   common.Hash
	requestCounter uint64
	maxResults     int
	reverse        bool
	minTopicCount  int
	resultsCh      chan *matcherResults
	first, last    *matcherProcess // ordered according to search direction
	finished       bool
	err            error
}

func (ms *matcherSession) print() {
	for mp := ms.first; mp != nil; mp = mp.next {
		mp.blockDataLock.Lock()
		var prevFirst, prevLast, nextFirst, nextLast uint64
		if mp.prev != nil {
			prevFirst, prevLast = mp.prev.firstBlock, mp.prev.lastBlock
		}
		if mp.next != nil {
			nextFirst, nextLast = mp.next.firstBlock, mp.next.lastBlock
		}
		fmt.Println(" range", mp.firstBlock, mp.lastBlock, "len(pos)", len(mp.positions), "len(allMatches)", len(mp.allMatches), "validMatches", mp.validMatches,
			"completeUntil", mp.completeUntil, "completeValid", mp.completeValid, "matcherFinished", mp.matcherFinished, "finished", mp.finished, "status", mp.status,
			"prevRange", prevFirst, prevLast, "nextRange", nextFirst, nextLast)
		mp.blockDataLock.Unlock()
	}
}

func (ms *matcherSession) returnResults() {
	//fmt.Println("*** returnResults")
	//ms.print()
	if ms.err != nil {
		ms.resultsCh <- &matcherResults{err: ms.err}
		return
	}
	var resCount int
	for mp := ms.first; mp != nil; mp = mp.next {
		//fmt.Println(" mp", mp.blockRange)
		resCount += mp.validMatches
		//fmt.Println("  resCount", len(mp.positions), mp.droppedResults, resCount)
		if resCount >= ms.maxResults {
			resCount = ms.maxResults
			break
		}
	}
	if ms.first == nil {
		//fmt.Println("  ms.first == nil")
		ms.resultsCh <- nil
		return
	}
	res := &matcherResults{
		logs:       make([]*types.Log, 0, resCount),
		firstBlock: ms.first.firstBlock,
		lastBlock:  ms.last.lastBlock,
	}
	var (
		currentProver   *tableProver
		andNode         logicNodeID
		lastResultCount int
	)

	addProcessResults := func(mp *matcherProcess) bool {
		mp.blockDataLock.Lock()
		defer mp.blockDataLock.Unlock()

		//fmt.Println("addProcessResults", mp.tableReader.BlockRange(), "len(mp.allMatches)", len(mp.allMatches), "len(mp.sectionNodes)", len(mp.sectionNodes))
		if mp.tableProver != nil && mp.tableProver != currentProver {
			if currentProver != nil {
				res.provers = append(res.provers, currentProver)
			}
			currentProver = mp.tableProver
			andNode = mp.logicBuilder.addAndGateNode()
			lastResultCount = len(res.logs)
			mp.logicBuilder.connect(andNode, mp.logicBuilder.addOutputNode())
		}
		_, lastBlockProven := mp.blockProofs[mp.tableReader.BlockRange().Last()]
		for i, log := range mp.allMatches {
			if len(res.logs) == ms.maxResults {
				lastLog := res.logs[ms.maxResults-1]
				trimBlockProofs(mp.blockProofs, lastLog.BlockNumber, uint32(lastLog.TxIndex), mp.session.reverse)
				mp.tableProver.addBlockProofs(mp.blockProofs, ms.maxResults-lastResultCount)
				lastResultCount = ms.maxResults
				return lastBlockProven
			}
			mp.logicBuilder.connect(mp.sectionNodes[i], andNode)
			if log == nil {
				fmt.Println(mp.firstBlock, mp.lastBlock, "*** nil log at", i, "mp.finished", mp.finished, "len(mp.allMatches)", len(mp.allMatches), "mp.completeUntil", mp.completeUntil, "mp.completeValid", mp.completeValid)
			}
			if len(log.Topics) >= ms.minTopicCount {
				res.logs = append(res.logs, log)
			}
		}
		if len(mp.sectionNodes) > len(mp.allMatches) {
			mp.logicBuilder.connect(mp.sectionNodes[len(mp.allMatches)], andNode)
		}
		mp.tableProver.addBlockProofs(mp.blockProofs, len(res.logs)-lastResultCount)
		lastResultCount = len(res.logs)
		return lastBlockProven || len(res.logs) != ms.maxResults
	}

	for mp := ms.first; mp != nil && addProcessResults(mp); mp = mp.next {
	}
	if currentProver != nil {
		res.provers = append(res.provers, currentProver)
	}
	//fmt.Println("  ms.resultsCh <- res")
	ms.resultsCh <- res
	return
}

func trimBlockProofs(blockProofs map[uint64]*blockProof, lastBlock uint64, lastTx uint32, reverse bool) {
	for number, bp := range blockProofs {
		if (!reverse && number > lastBlock) || (reverse && number < lastBlock) {
			delete(blockProofs, number)
			continue
		}
		if number == lastBlock {
			for txi := range bp.matchingTxs {
				if (!reverse && txi > lastTx) || (reverse && txi < lastTx) {
					delete(bp.matchingTxs, txi)
				}
			}
		}
	}
}
