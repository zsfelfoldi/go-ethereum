// Copyright 2024 The go-ethereum Authors
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

package filtermaps

import (
	"context"
	"errors"

	//"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// ErrMatchAll is returned when the specified filter matches everything.
// Handling this case in filtermaps would require an extra special case and
// would actually be slower than reverting to legacy filter.
var ErrMatchAll = errors.New("match all patterns not supported")

// MatcherBackend defines the functions required for searching in the log index
// data structure. It is currently implemented by FilterMapsMatcherBackend but
// once EIP-7745 is implemented and active, these functions can also be trustlessly
// served by a remote prover.
type MatcherBackend interface {
	GetParams() *Params
	GetBlockLvPointer(ctx context.Context, blockNumber uint64) (uint64, error)
	GetFilterMapRow(ctx context.Context, mapIndex, rowIndex uint32) (FilterRow, error)
	GetLogByLvIndex(ctx context.Context, lvIndex uint64) (*types.Log, error)
	SyncLogIndex(ctx context.Context) (SyncRange, error)
	Close()
}

// SyncRange is returned by MatcherBackend.SyncLogIndex. It contains the latest
// chain head, the indexed range that is currently consistent with the chain
// and the valid range that has not been changed and has been consistent with
// all states of the chain since the previous SyncLogIndex or the creation of
// the matcher backend.
type SyncRange struct {
	HeadNumber uint64
	// block range where the index has not changed since the last matcher sync
	// and therefore the set of matches found in this region is guaranteed to
	// be valid and complete.
	Valid                 bool
	FirstValid, LastValid uint64
	// block range indexed according to the given chain head.
	Indexed                   bool
	FirstIndexed, LastIndexed uint64
}

// GetPotentialMatches returns a list of logs that are potential matches for the
// given filter criteria. If parts of the log index in the searched range are
// missing or changed during the search process then the resulting logs belonging
// to that block range might be missing or incorrect.
// Also note that the returned list may contain false positives.
func GetPotentialMatches(ctx context.Context, backend MatcherBackend, firstBlock, lastBlock uint64, addresses []common.Address, topics [][]common.Hash) ([]*types.Log, error) {
	params := backend.GetParams()
	// find the log value index range to search
	firstIndex, err := backend.GetBlockLvPointer(ctx, firstBlock)
	if err != nil {
		return nil, err
	}
	lastIndex, err := backend.GetBlockLvPointer(ctx, lastBlock+1)
	if err != nil {
		return nil, err
	}
	if lastIndex > 0 {
		lastIndex--
	}
	firstMap, lastMap := uint32(firstIndex>>params.logValuesPerMap), uint32(lastIndex>>params.logValuesPerMap)
	firstEpoch, lastEpoch := firstMap>>params.logMapsPerEpoch, lastMap>>params.logMapsPerEpoch

	// build matcher according to the given filter criteria
	matchers := make([]matcher, len(topics)+1)
	// matchAddress signals a match when there is a match for any of the given
	// addresses.
	// If the list of addresses is empty then it creates a "wild card" matcher
	// that signals every index as a potential match.
	matchAddress := make(matchAny, len(addresses))
	for i, address := range addresses {
		matchAddress[i] = &singleMatcher{backend: backend, value: addressValue(address)}
	}
	matchers[0] = matchAddress
	for i, topicList := range topics {
		// matchTopic signals a match when there is a match for any of the topics
		// specified for the given position (topicList).
		// If topicList is empty then it creates a "wild card" matcher that signals
		// every index as a potential match.
		matchTopic := make(matchAny, len(topicList))
		for j, topic := range topicList {
			matchTopic[j] = &singleMatcher{backend: backend, value: topicValue(topic)}
		}
		matchers[i+1] = matchTopic
	}
	// matcher is the final sequence matcher that signals a match when all underlying
	// matchers signal a match for consecutive log value indices.
	matcher := newMatchSequence(params, matchers)

	// processEpoch returns the potentially matching logs from the given epoch.
	processEpoch := func(epochIndex uint32) ([]*types.Log, error) {
		var logs []*types.Log
		// create a list of map indices to process
		fm, lm := epochIndex<<params.logMapsPerEpoch, (epochIndex+1)<<params.logMapsPerEpoch-1
		if fm < firstMap {
			fm = firstMap
		}
		if lm > lastMap {
			lm = lastMap
		}
		//
		mapIndices := make([]uint32, lm+1-fm)
		for i := range mapIndices {
			mapIndices[i] = fm + uint32(i)
		}
		// find potential matches
		matches, err := getAllMatches(ctx, matcher, mapIndices)
		if err != nil {
			return logs, err
		}
		// get the actual logs located at the matching log value indices
		for _, m := range matches {
			if m == nil {
				return nil, ErrMatchAll
			}
			mlogs, err := getLogsFromMatches(ctx, backend, firstIndex, lastIndex, m)
			if err != nil {
				return logs, err
			}
			logs = append(logs, mlogs...)
		}
		return logs, nil
	}

	type task struct {
		epochIndex uint32
		logs       []*types.Log
		err        error
		done       chan struct{}
	}

	taskCh := make(chan *task)
	var wg sync.WaitGroup
	defer func() {
		close(taskCh)
		wg.Wait()
	}()

	worker := func() {
		for task := range taskCh {
			//fmt.Println("taskCh")
			if task == nil {
				break
			}
			//fmt.Println("processEpoch start")
			task.logs, task.err = processEpoch(task.epochIndex)
			//fmt.Println("processEpoch stop")
			close(task.done)
		}
		wg.Done()
	}

	start := time.Now()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go worker()
	}

	var logs []*types.Log
	// startEpoch is the next task to send whenever a worker can accept it.
	// waitEpoch is the next task we are waiting for to finish in order to append
	// results in the correct order.
	startEpoch, waitEpoch := firstEpoch, firstEpoch
	tasks := make(map[uint32]*task)
	tasks[startEpoch] = &task{epochIndex: startEpoch, done: make(chan struct{})}
	for waitEpoch <= lastEpoch {
		select {
		case taskCh <- tasks[startEpoch]:
			startEpoch++
			if startEpoch <= lastEpoch {
				if tasks[startEpoch] == nil {
					tasks[startEpoch] = &task{epochIndex: startEpoch, done: make(chan struct{})}
				}
			}
		case <-tasks[waitEpoch].done:
			logs = append(logs, tasks[waitEpoch].logs...)
			if err := tasks[waitEpoch].err; err != nil {
				return logs, err
			}
			delete(tasks, waitEpoch)
			waitEpoch++
			if waitEpoch <= lastEpoch {
				if tasks[waitEpoch] == nil {
					tasks[waitEpoch] = &task{epochIndex: waitEpoch, done: make(chan struct{})}
				}
			}
		}
	}
	log.Info("Log search finished", "elapsed", time.Since(start))
	for i, m := range matchers {
		log.Info("Single matcher stats", "index", i)
		m.(matchAny)[0].(*singleMatcher).stats.log()
	}
	return logs, nil
}

// getLogsFromMatches returns the list of potentially matching logs located at
// the given list of matching log indices. Matches outside the firstIndex to
// lastIndex range are not returned.
func getLogsFromMatches(ctx context.Context, backend MatcherBackend, firstIndex, lastIndex uint64, matches potentialMatches) ([]*types.Log, error) {
	var logs []*types.Log
	for _, match := range matches {
		if match < firstIndex || match > lastIndex {
			continue
		}
		log, err := backend.GetLogByLvIndex(ctx, match)
		if err != nil {
			return logs, err
		}
		if log != nil {
			logs = append(logs, log)
		}
	}
	return logs, nil
}

const (
	stNone = iota
	stRowCalc
	stFetchFirst
	stFetchMore
	stProcess
	stOther
	stCount
)

var stNames = []string{"", "rowCalc", "fetchFirst", "fetchMore", "process", "other"}

type matcherStats struct {
	dt, cnt [stCount]int64
}

func (ms *matcherStats) set(state *int, newState int) {
	if ms == nil || newState == *state {
		return
	}
	now := int64(mclock.Now())
	atomic.AddInt64(&ms.dt[*state], now)
	atomic.AddInt64(&ms.dt[newState], -now)
	atomic.AddInt64(&ms.cnt[newState], 1)
	*state = newState
}

func (ms *matcherStats) log() {
	for i := 1; i < stCount; i++ {
		log.Info("Matcher stats", "name", stNames[i], "dt", time.Duration(ms.dt[i]), "count", ms.cnt[i])
	}
}

type matcher interface {
	newInstance(mapIndices []uint32) matcherInstance
}

type matcherResult struct {
	mapIndex uint32
	matches  potentialMatches
}

type matcherInstance interface {
	getMoreMatches(ctx context.Context, alternativeIndex uint32) ([]matcherResult, error)
	dropIndices(mapIndices []uint32)
}

func getAllMatches(ctx context.Context, matcher matcher, mapIndices []uint32) ([]potentialMatches, error) {
	//fmt.Println("getAllMatches", mapIndices[0]>>16)
	instance := matcher.newInstance(mapIndices)
	resultsMap := make(map[uint32]potentialMatches)
	for alternativeIndex := uint32(0); len(resultsMap) < len(mapIndices); alternativeIndex++ {
		//fmt.Println(" alt", alternativeIndex, len(resultsMap), len(mapIndices))
		results, err := instance.getMoreMatches(ctx, alternativeIndex)

		/*var sum, nilc uint64 //***
		for _, r := range results {
			sum += uint64(len(r.matches))
			if r.matches == nil {
				nilc++
			}
		}
		fmt.Println(" res", len(results), "sum", sum, "nil", nilc, "err", err)*/

		if err != nil {
			return nil, err
		}
		for _, result := range results {
			resultsMap[result.mapIndex] = result.matches
		}
	}
	matches := make([]potentialMatches, len(mapIndices))
	for i, mapIndex := range mapIndices {
		matches[i] = resultsMap[mapIndex]
	}
	return matches, nil
}

// singleMatcher implements matcher by returning matches for a single log value hash.
type singleMatcher struct {
	backend MatcherBackend
	value   common.Hash
	stats   matcherStats
}

type singleMatcherInstance struct {
	*singleMatcher
	mapIndices []uint32
	filterRows map[uint32][]FilterRow
}

func (m *singleMatcher) newInstance(mapIndices []uint32) matcherInstance {
	filterRows := make(map[uint32][]FilterRow)
	for _, idx := range mapIndices {
		filterRows[idx] = []FilterRow{}
	}
	return &singleMatcherInstance{
		singleMatcher: m,
		mapIndices:    mapIndices,
		filterRows:    filterRows,
	}
}

func (m *singleMatcherInstance) getMoreMatches(ctx context.Context, alternativeIndex uint32) (results []matcherResult, err error) {
	var st int
	m.stats.set(&st, stOther)
	params := m.backend.GetParams()
	//fmt.Println(" sm alt", alternativeIndex, "mapIndices", len(m.mapIndices))
	lastEpoch, rowIndex := uint32(math.MaxUint32), uint32(0)
	for _, mapIndex := range m.mapIndices {
		filterRows, ok := m.filterRows[mapIndex]
		if !ok {
			continue
		}
		epoch := mapIndex >> params.logMapsPerEpoch
		if epoch != lastEpoch { // usually all map indices are in the same epoch, rarely in two epochs
			lastEpoch = epoch
			m.stats.set(&st, stRowCalc)
			rowIndex = params.rowIndex(lastEpoch, alternativeIndex, m.value)
		}
		if alternativeIndex == 0 {
			m.stats.set(&st, stFetchFirst)
		} else {
			m.stats.set(&st, stFetchMore)
		}
		filterRow, err := m.backend.GetFilterMapRow(ctx, mapIndex, rowIndex)
		m.stats.set(&st, stOther)
		if err != nil {
			m.stats.set(&st, stNone)
			return nil, err
		}
		filterRows = append(filterRows, filterRow)
		if uint32(len(filterRow)) < params.maxRowLength {
			m.stats.set(&st, stProcess)
			results = append(results, matcherResult{
				mapIndex: mapIndex,
				matches:  params.potentialMatches(filterRows, mapIndex, m.value),
			})
			m.stats.set(&st, stOther)
			delete(m.filterRows, mapIndex)
		} else {
			m.filterRows[mapIndex] = filterRows
		}
	}
	//fmt.Println(" getMoreMatches 2", m.value)
	//fmt.Println("getMoreMatches", m.value, "mapIndices", len(m.mapIndices), "results", len(results))
	m.cleanMapIndices()
	m.stats.set(&st, stNone)

	/*var sum, nilc uint64 //***
	for _, r := range results {
		sum += uint64(len(r.matches))
		if r.matches == nil {
			nilc++
		}
	}
	fmt.Println(" getMoreMatches 3", m.value, "sum", sum, "nil", nilc)*/
	return results, nil
}

func (m *singleMatcherInstance) dropIndices(dropIndices []uint32) {
	//fmt.Println("dropIndices", m.value, "dropIndices", len(dropIndices))
	for _, mapIndex := range dropIndices {
		delete(m.filterRows, mapIndex)
	}
	m.cleanMapIndices()
}

func (m *singleMatcherInstance) cleanMapIndices() {
	var j int
	for i, mapIndex := range m.mapIndices {
		if _, ok := m.filterRows[mapIndex]; ok {
			if i != j {
				m.mapIndices[j] = mapIndex
			}
			j++
		}
	}
	//fmt.Println("cleanMapIndices", m.value, "old", len(m.mapIndices), "new", j)
	m.mapIndices = m.mapIndices[:j]
}

// matchAny combinines a set of matchers and returns a match for every position
// where any of the underlying matchers signaled a match. A zero-length matchAny
// acts as a "wild card" that signals a potential match at every position.
type matchAny []matcher

type matchAnyInstance struct {
	matchAny
	childInstances []matcherInstance
	childResults   map[uint32]matchAnyResults
}

type matchAnyResults struct {
	matches  []potentialMatches
	done     []bool
	needMore int
}

func (m matchAny) newInstance(mapIndices []uint32) matcherInstance {
	if len(m) == 1 {
		return m[0].newInstance(mapIndices)
	}
	childResults := make(map[uint32]matchAnyResults)
	for _, idx := range mapIndices {
		childResults[idx] = matchAnyResults{
			matches:  make([]potentialMatches, len(m)),
			done:     make([]bool, len(m)),
			needMore: len(m),
		}
	}
	childInstances := make([]matcherInstance, len(m))
	for i, matcher := range m {
		childInstances[i] = matcher.newInstance(mapIndices)
	}
	return &matchAnyInstance{
		matchAny:       m,
		childInstances: childInstances,
		childResults:   childResults,
	}
}

func (m *matchAnyInstance) getMoreMatches(ctx context.Context, alternativeIndex uint32) (mergedResults []matcherResult, err error) {
	if len(m.matchAny) == 0 {
		// return "wild card" results (potentialMatches(nil) is interpreted as a
		// potential match at every log value index of the map).
		mergedResults = make([]matcherResult, len(m.childResults))
		var i int
		for mapIndex := range m.childResults {
			mergedResults[i] = matcherResult{mapIndex: mapIndex, matches: nil}
			i++
		}
		return mergedResults, nil
	}
	for i, childInstance := range m.childInstances {
		results, err := childInstance.getMoreMatches(ctx, alternativeIndex)
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			mr, ok := m.childResults[result.mapIndex]
			if !ok || mr.done[i] {
				continue
			}
			mr.done[i] = true
			mr.matches[i] = result.matches
			mr.needMore--
			if mr.needMore == 0 || result.matches == nil {
				mergedResults = append(mergedResults, matcherResult{
					mapIndex: result.mapIndex,
					matches:  mergeResults(mr.matches),
				})
				delete(m.childResults, result.mapIndex)
			} else {
				m.childResults[result.mapIndex] = mr
			}
		}
	}
	return mergedResults, nil
}

func (m *matchAnyInstance) dropIndices(dropIndices []uint32) {
	for _, childInstance := range m.childInstances {
		childInstance.dropIndices(dropIndices)
	}
	for _, mapIndex := range dropIndices {
		delete(m.childResults, mapIndex)
	}
}

// mergeResults merges multiple lists of matches into a single one, preserving
// ascending order and filtering out any duplicates.
func mergeResults(results []potentialMatches) potentialMatches {
	if len(results) == 0 {
		return nil
	}
	var sumLen int
	for _, res := range results {
		if res == nil {
			// nil is a wild card; all indices in map range are potential matches
			return nil
		}
		sumLen += len(res)
	}
	merged := make(potentialMatches, 0, sumLen)
	for {
		best := -1
		for i, res := range results {
			if len(res) == 0 {
				continue
			}
			if best < 0 || res[0] < results[best][0] {
				best = i
			}
		}
		if best < 0 {
			return merged
		}
		if len(merged) == 0 || results[best][0] > merged[len(merged)-1] {
			merged = append(merged, results[best][0])
		}
		results[best] = results[best][1:]
	}
}

// matchSequence combines two matchers, a "base" and a "next" matcher with a
// positive integer offset so that the resulting matcher signals a match at log
// value index X when the base matcher returns a match at X and the next matcher
// gives a match at X+offset. Note that matchSequence can be used recursively to
// detect any log value sequence.
type matchSequence struct {
	params     *Params
	base, next matcher
	offset     uint64

	statsLock            sync.Mutex
	baseStats, nextStats matchSeqStats
}

type matchSeqStats struct {
	totalCount, nonEmptyCount, totalCost uint64
}

func (ms *matchSeqStats) add(nonEmpty bool, alternativeIndex uint32) {
	ms.totalCount++
	if nonEmpty {
		ms.nonEmptyCount++
	}
	ms.totalCost += uint64(alternativeIndex + 1)
}

func (ms *matchSeqStats) addStats(add matchSeqStats) {
	ms.totalCount += add.totalCount
	ms.nonEmptyCount += add.nonEmptyCount
	ms.totalCost += add.totalCost
}

func (m *matchSequence) baseFirst() bool {
	m.statsLock.Lock()
	bf := float64(m.baseStats.totalCost)*float64(m.nextStats.totalCount)+
		float64(m.baseStats.nonEmptyCount)*float64(m.nextStats.totalCost) <
		float64(m.baseStats.totalCost)*float64(m.nextStats.nonEmptyCount)+
			float64(m.nextStats.totalCost)*float64(m.baseStats.totalCount)
	m.statsLock.Unlock()
	return bf
}

// newMatchSequence creates a recursive sequence matcher from a list of underlying
// matchers. The resulting matcher signals a match at log value index X when each
// underlying matcher matchers[i] returns a match at X+i.
func newMatchSequence(params *Params, matchers []matcher) matcher {
	if len(matchers) == 0 {
		panic("zero length sequence matchers are not allowed")
	}
	if len(matchers) == 1 {
		return matchers[0]
	}
	return &matchSequence{
		params: params,
		base:   newMatchSequence(params, matchers[:len(matchers)-1]),
		next:   matchers[len(matchers)-1],
		offset: uint64(len(matchers) - 1),
	}
}

type matchSequenceInstance struct {
	*matchSequence
	baseInstance, nextInstance                matcherInstance
	baseRequested, nextRequested, needMatched map[uint32]struct{}
	baseResults, nextResults                  map[uint32]potentialMatches
}

func (m *matchSequence) newInstance(mapIndices []uint32) matcherInstance {
	// determine set of indices to request from next matcher
	nextIndices := make([]uint32, 0, len(mapIndices)*3/2)
	needMatched := make(map[uint32]struct{})
	baseRequested := make(map[uint32]struct{})
	nextRequested := make(map[uint32]struct{})
	for _, mapIndex := range mapIndices {
		needMatched[mapIndex] = struct{}{}
		baseRequested[mapIndex] = struct{}{}
		if _, ok := nextRequested[mapIndex]; !ok {
			nextIndices = append(nextIndices, mapIndex)
			nextRequested[mapIndex] = struct{}{}
		}
		nextIndices = append(nextIndices, mapIndex+1)
		nextRequested[mapIndex+1] = struct{}{}
	}
	return &matchSequenceInstance{
		matchSequence: m,
		baseInstance:  m.base.newInstance(mapIndices),
		nextInstance:  m.next.newInstance(nextIndices),
		needMatched:   needMatched,
		baseRequested: baseRequested,
		nextRequested: nextRequested,
		baseResults:   make(map[uint32]potentialMatches),
		nextResults:   make(map[uint32]potentialMatches),
	}
}

func (m *matchSequenceInstance) getMoreMatches(ctx context.Context, alternativeIndex uint32) (matchedResults []matcherResult, err error) {
	// decide whether to evaluate base or next matcher first
	baseFirst := m.baseFirst()
	//fmt.Println("*** matchSeq ofs", m.offset, "baseFirst", baseFirst, "alt", alternativeIndex, "bt be nt ne", baseTotal, baseEmpty, nextTotal, nextEmpty)
	if baseFirst {
		if err := m.evalBase(ctx, alternativeIndex); err != nil {
			return nil, err
		}
	}
	if err := m.evalNext(ctx, alternativeIndex); err != nil {
		return nil, err
	}
	if !baseFirst {
		if err := m.evalBase(ctx, alternativeIndex); err != nil {
			return nil, err
		}
	}
	// evaluate and return matched results where possible
	for mapIndex := range m.needMatched {
		if _, ok := m.baseRequested[mapIndex]; ok {
			continue
		}
		if _, ok := m.nextRequested[mapIndex]; ok {
			continue
		}
		if _, ok := m.nextRequested[mapIndex+1]; ok {
			continue
		}
		matchedResults = append(matchedResults, matcherResult{
			mapIndex: mapIndex,
			matches:  m.params.matchResults(mapIndex, m.offset, m.baseResults[mapIndex], m.nextResults[mapIndex], m.nextResults[mapIndex+1]),
		})
		delete(m.needMatched, mapIndex)
	}
	return matchedResults, nil
}

func (m *matchSequenceInstance) evalBase(ctx context.Context, alternativeIndex uint32) error {
	results, err := m.baseInstance.getMoreMatches(ctx, alternativeIndex)
	if err != nil {
		return err
	}
	var (
		dropIndices []uint32
		stats       matchSeqStats
	)
	for _, r := range results {
		m.baseResults[r.mapIndex] = r.matches
		delete(m.baseRequested, r.mapIndex)
		stats.add(r.matches == nil || len(r.matches) != 0, alternativeIndex)
	}
	m.statsLock.Lock()
	m.baseStats.addStats(stats)
	m.statsLock.Unlock()
	for _, r := range results {
		if m.dropNext(r.mapIndex) {
			dropIndices = append(dropIndices, r.mapIndex)
		}
		if m.dropNext(r.mapIndex + 1) {
			dropIndices = append(dropIndices, r.mapIndex+1)
		}
	}
	if len(dropIndices) > 0 {
		m.nextInstance.dropIndices(dropIndices)
	}
	return nil
}

func (m *matchSequenceInstance) evalNext(ctx context.Context, alternativeIndex uint32) error {
	results, err := m.nextInstance.getMoreMatches(ctx, alternativeIndex)
	if err != nil {
		return err
	}
	var (
		dropIndices []uint32
		stats       matchSeqStats
	)
	for _, r := range results {
		m.nextResults[r.mapIndex] = r.matches
		delete(m.nextRequested, r.mapIndex)
		stats.add(r.matches == nil || len(r.matches) != 0, alternativeIndex)
	}
	m.statsLock.Lock()
	m.nextStats.addStats(stats)
	m.statsLock.Unlock()
	for _, r := range results {
		if r.mapIndex > 0 && m.dropBase(r.mapIndex-1) {
			dropIndices = append(dropIndices, r.mapIndex-1)
		}
		if m.dropBase(r.mapIndex) {
			dropIndices = append(dropIndices, r.mapIndex)
		}
	}
	if len(dropIndices) > 0 {
		m.baseInstance.dropIndices(dropIndices)
	}
	return nil
}

func (m *matchSequenceInstance) dropBase(mapIndex uint32) bool {
	if _, ok := m.baseRequested[mapIndex]; !ok {
		return false
	}
	if _, ok := m.needMatched[mapIndex]; ok {
		if next := m.nextResults[mapIndex]; next == nil ||
			(len(next) > 0 && next[len(next)-1] >= (uint64(mapIndex)<<m.params.logValuesPerMap)+m.offset) {
			return false
		}
		if nextNext := m.nextResults[mapIndex]; nextNext == nil ||
			(len(nextNext) > 0 && nextNext[0] < (uint64(mapIndex+1)<<m.params.logValuesPerMap)+m.offset) {
			return false
		}
	}
	delete(m.baseRequested, mapIndex)
	return true
}

func (m *matchSequenceInstance) dropNext(mapIndex uint32) bool {
	if _, ok := m.nextRequested[mapIndex]; !ok {
		return false
	}
	if _, ok := m.needMatched[mapIndex-1]; ok {
		if prevBase := m.baseResults[mapIndex-1]; prevBase == nil ||
			(len(prevBase) > 0 && prevBase[len(prevBase)-1]+m.offset >= (uint64(mapIndex)<<m.params.logValuesPerMap)) {
			return false
		}
	}
	if _, ok := m.needMatched[mapIndex]; ok {
		if base := m.baseResults[mapIndex]; base == nil ||
			(len(base) > 0 && base[0]+m.offset < (uint64(mapIndex+1)<<m.params.logValuesPerMap)) {

			return false
		}
	}
	delete(m.nextRequested, mapIndex)
	return true
}

func (m *matchSequenceInstance) dropIndices(dropIndices []uint32) {
	//fmt.Println("ms", m.offset, "drop", len(dropIndices))
	for _, mapIndex := range dropIndices {
		delete(m.needMatched, mapIndex)
	}
	var dropBase, dropNext []uint32
	for _, mapIndex := range dropIndices {
		if m.dropBase(mapIndex) {
			dropBase = append(dropBase, mapIndex)
		}
	}
	m.baseInstance.dropIndices(dropBase)
	for _, mapIndex := range dropIndices {
		if m.dropNext(mapIndex) {
			dropNext = append(dropNext, mapIndex)
		}
		if m.dropNext(mapIndex + 1) {
			dropNext = append(dropNext, mapIndex+1)
		}
	}
	m.nextInstance.dropIndices(dropNext)
}

// matchResults returns a list of sequence matches for the given mapIndex and
// offset based on the base matcher's results at mapIndex and the next matcher's
// results at mapIndex and mapIndex+1. Note that acquiring nextNextRes may be
// skipped and it can be substituted with an empty list if baseRes has no potential
// matches that could be sequence matched with anything that could be in nextNextRes.
func (params *Params) matchResults(mapIndex uint32, offset uint64, baseRes, nextRes, nextNextRes potentialMatches) potentialMatches {
	//fmt.Println("matchResults", mapIndex, baseRes != nil, len(baseRes), nextRes != nil, len(nextRes), nextNextRes != nil, len(nextNextRes))
	if nextRes == nil || (baseRes != nil && len(baseRes) == 0) {
		// if nextRes is a wild card or baseRes is empty then the sequence matcher
		// result equals baseRes.
		return baseRes
	}
	if len(nextRes) > 0 {
		// discard items from nextRes whose corresponding base matcher results
		// with the negative offset applied would be located at mapIndex-1.
		start := 0
		for start < len(nextRes) && nextRes[start] < uint64(mapIndex)<<params.logValuesPerMap+offset {
			start++
		}
		nextRes = nextRes[start:]
	}
	if len(nextNextRes) > 0 {
		// discard items from nextNextRes whose corresponding base matcher results
		// with the negative offset applied would still be located at mapIndex+1.
		stop := 0
		for stop < len(nextNextRes) && nextNextRes[stop] < uint64(mapIndex+1)<<params.logValuesPerMap+offset {
			stop++
		}
		nextNextRes = nextNextRes[:stop]
	}
	maxLen := len(nextRes) + len(nextNextRes)
	if maxLen == 0 {
		return nextRes
	}
	if len(baseRes) < maxLen {
		maxLen = len(baseRes)
	}
	// iterate through baseRes, nextRes and nextNextRes and collect matching results.
	matchedRes := make(potentialMatches, 0, maxLen)
	for _, nextRes := range []potentialMatches{nextRes, nextNextRes} {
		if baseRes != nil {
			for len(nextRes) > 0 && len(baseRes) > 0 {
				if nextRes[0] > baseRes[0]+offset {
					baseRes = baseRes[1:]
				} else if nextRes[0] < baseRes[0]+offset {
					nextRes = nextRes[1:]
				} else {
					matchedRes = append(matchedRes, baseRes[0])
					baseRes = baseRes[1:]
					nextRes = nextRes[1:]
				}
			}
		} else {
			// baseRes is a wild card so just return next matcher results with
			// negative offset.
			for len(nextRes) > 0 {
				matchedRes = append(matchedRes, nextRes[0]-offset)
				nextRes = nextRes[1:]
			}
		}
	}
	return matchedRes
}
