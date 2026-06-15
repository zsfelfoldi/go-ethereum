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

package logindex

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
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

var ErrMatchAll = errors.New("match-all queries not allowed")

const (
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

type contractProver interface {
	GetTableRoot(contract common.Address, firstBlock, tableSize uint64) (common.Hash, error)
}

func (ix *Indexer) GetMatches(ctx context.Context, query FilterQuery, prove bool, refHeader *types.Header, contractProver contractProver) ([]*types.Log, common.Range[uint64], *QueryProof, error) {
	ix.lock.Lock()
	firstBlock := min(query.FirstBlock, ix.headBlock) //TODO inditas utan, amig syncing megy, itt 0 a head
	lastBlock := min(query.LastBlock, ix.headBlock)
	blockRange := common.NewRange[uint64](firstBlock, lastBlock+1-firstBlock)
	direction := 1
	if query.Reverse {
		direction = -1
	}
	readers := ix.storage.getRangeReaders(blockRange)
	ix.lock.Unlock()
	sort.Slice(readers, func(i, j int) bool {
		switch direction {
		case 1:
			return readers[i].meta.LastBlockNumber < readers[j].meta.LastBlockNumber
		case -1:
			return readers[i].meta.LastBlockNumber > readers[j].meta.LastBlockNumber
		default:
			panic("invalid search direction")
		}
	})
	for i := 1; i < len(readers); i++ {
		var cont bool
		switch direction {
		case 1:
			cont = readers[i-1].blockRange().AfterLast() == readers[i].blockRange().First()
		case -1:
			cont = readers[i].blockRange().AfterLast() == readers[i-1].blockRange().First()
		}
		if !cont {
			readers = readers[:i]
			break
		}
	}
	// build matcher according to the given filter criteria
	matcher := make(matchAll, 0, len(query.Topics)+1)
	// matchAddress signals a match when there is a match for any of the given
	// addresses.
	// If the list of addresses is empty then it creates a "wild card" matcher
	// that signals every index as a potential match.
	if len(query.Addresses) > 0 {
		matchAddress := make(matchAny, len(query.Addresses))
		for i, address := range query.Addresses {
			var addr32 [32]byte
			copy(addr32[32-common.AddressLength:], address[:])
			matchAddress[i] = &singleMatcher{value: indexValue{entryType: ieAddress, value: addr32}}
		}
		matcher = append(matcher, matchAddress)
	}
	for i, topicList := range query.Topics {
		// matchTopic signals a match when there is a match for any of the topics
		// specified for the given position (topicList).
		// If topicList is empty then it creates a "wild card" matcher that signals
		// every index as a potential match.
		if len(topicList) > 0 {
			matchTopic := make(matchAny, len(topicList))
			for j, topic := range topicList {
				matchTopic[j] = &singleMatcher{value: indexValue{entryType: ieTopic0 + uint32(i), value: ([32]byte)(topic)}}
			}
			matcher = append(matcher, matchTopic)
		}
	}
	if len(matcher) == 0 {
		return nil, common.Range[uint64]{}, nil, ErrMatchAll
	}
	// create matcher session
	session := &matcherSession{
		ctx:            ctx,
		requestCounter: atomic.AddUint64(&ix.matcherControl.requestCounter, 1),
		maxResults:     query.MaxResults,
		validUntil:     blockRange.Last(),
		direction:      direction,
		minTopicCount:  len(query.Topics),
		resultsCh:      make(chan matcherResults, 1),
	}
	fmt.Println("create session", firstBlock, lastBlock, query.Addresses, query.Topics)
	for i, tr := range readers {
		br := tr.blockRange().Intersection(blockRange)
		//fmt.Println(" ", br)
		prover := newTableProver(tr)
		proverInstance := prover.newInstance()
		mp := &matcherProcess{
			indexer: ix,
			matcher: matcher.newInstance(
				ctx,
				tr,
				proverInstance,
				indexPosition{blockNumber: br.First()},
				indexPosition{blockNumber: br.Last(), txIndex: math.MaxUint32, logIndex: math.MaxUint32},
				direction),
			session:        session,
			tableReader:    tr,
			tableProver:    prover,
			proverInstance: proverInstance,
			blockRange:     br,
			blockInResults: make(map[uint64]int),
			blockProofs:    make(map[uint64]*blockProof),
			deliverCh:      make(chan struct{}, 1),
		}
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
	ix.lock.Lock()
	ix.matcherControl.sessions[session] = struct{}{}
	ix.lock.Unlock()
	defer func() {
		ix.lock.Lock()
		delete(ix.matcherControl.sessions, session)
		ix.lock.Unlock()
	}()

	select {
	case ix.matcherControl.sessionCh <- session:
	case <-ctx.Done():
		return nil, common.Range[uint64]{}, nil, ctx.Err()
	}
	var results matcherResults
	select {
	case results = <-session.resultsCh:
	case <-ctx.Done():
		return nil, common.Range[uint64]{}, nil, ctx.Err()
	}
	if results.blockRange.IsEmpty() {
		return nil, common.Range[uint64]{}, nil, errors.New("entire search range has been invalidated") //TODO
	}
	var proof *QueryProof
	if prove {
		proof = &QueryProof{
			Query:            query,
			RefHeader:        *refHeader,
			TableQueryProofs: make([]tableQueryProof, len(results.provers)),
		}
		proof.Query.FirstBlock = results.blockRange.First()
		proof.Query.LastBlock = results.blockRange.Last()
		/*		stateDb := state.Database()
				trie, err := stateDb.OpenTrie(refHeader.Root)
				if err != nil {
					return nil, common.Range[uint64]{}, nil, err
				}*/
		proofDb := make(trieProofWriter)
		for i, prover := range results.provers {
			tproof, err := prover.finalize()
			if err != nil {
				return nil, common.Range[uint64]{}, nil, err
			}
			tproof.IndexContract = proof.addOrGetIndexContract(prover.reader.indexContract)
			proof.TableQueryProofs[i] = tproof
			// generate state proof nodes for table root
			/*account, err := trie.GetAccount(prover.reader.indexContract)
			if err != nil {
				return nil, common.Range[uint64]{}, nil, err
			}
			strie, err := stateDb.OpenStorageTrie(refHeader.Root, prover.reader.indexContract, account.Root, trie)
			if err != nil {
				return nil, common.Range[uint64]{}, nil, err
			}
			tableRootKey, err := getTableRootKey(stateDb, refHeader.Root, prover.reader.indexContract, tproof.FirstBlock, tproof.TableSize)
			if err != nil {
				return nil, common.Range[uint64]{}, nil, err
			}
			if err := strie.Prove(tableRootKey, proofDb); err != nil {
				return nil, common.Range[uint64]{}, nil, err
			}*/
		}
		/*for _, address := range proof.IndexContracts {
			if err := trie.Prove(address.Bytes(), proofDb); err != nil { //TODO hashed address?
				return nil, common.Range[uint64]{}, nil, err
			}
		}*/
		proof.IndexTablesProof = proofDb.proofForStorage()
		proof.printStats()
		proofEnc, err := rlp.EncodeToBytes(proof)
		if err != nil {
			return nil, common.Range[uint64]{}, nil, err
		}
		fmt.Println("encoded proof", len(proofEnc))
		var proofDec QueryProof
		if err := rlp.DecodeBytes(proofEnc, &proofDec); err != nil {
			return nil, common.Range[uint64]{}, nil, err
		}
		proofDec.printStats()
		if err := proof.Verify(); err != nil {
			return nil, common.Range[uint64]{}, nil, err
		}
		root, err := contractProver.GetTableRoot(common.Address{}, 0, 0)
		fmt.Println("getTableRoot", root, err)
		fmt.Println("refHeader.Parent", refHeader.ParentHash)
	}
	//fmt.Println(" results", len(results.logs), "error", results.err)
	return results.logs, results.blockRange, proof, results.err

	/*start := time.Now()
	res, err := m.process()
	matchRequestTimer.Update(time.Since(start))

	if doRuntimeStats {
		log.Info("Log search finished", "elapsed", time.Since(start))
		for i, ma := range matchers {
			for j, m := range ma.(matchAny) {
				log.Info("Single matcher stats", "matchSequence", i, "matchAny", j)
				m.(*singleMatcher).stats.print()
			}
		}
		log.Info("Get log stats")
		m.getLogStats.print()
	}
	return res, err*/
}

func getTableRootKey(stateDb state.Database, stateRoot common.Hash, contract common.Address, firstBlock, tableSize uint64) ([]byte, error) {
	return nil, nil //TODO
}

type matcherResults struct {
	logs       []*types.Log
	provers    []*tableProver
	blockRange common.Range[uint64]
	err        error
}

// matcher defines a general abstraction for any matcher configuration that
// can instantiate a matcherInstance.
type matcher interface {
	newInstance(ctx context.Context, reader *tableReader, prover *proverInstance, first, last indexPosition, direction int) matcherInstance
}

// matcherInstance defines a general abstraction for a matcher configuration
// working on a specific set of map indices and eventually returning a list of
// potentially matching log value indices.
// Note that processing happens per mapping layer, each call returning a set
// of results for the maps where the processing has been finished at the given
// layer. Map indices can also be dropped before a result is returned for them
// in case the result is no longer interesting. Dropping indices twice or after
// a result has been returned has no effect. Exactly one matcherResult is
// returned per requested map index unless dropped.
type matcherInstance interface {
	next() (*indexPosition, uint32, error)
	advance(*indexPosition) error
	split(*proverInstance, *indexPosition) matcherInstance
}

// singleMatcher implements matcher by returning matches for a single log value hash.
type singleMatcher struct {
	value indexValue
	//stats runtimeStats
}

// singleMatcherInstance is an instance of singleMatcher.
type singleMatcherInstance struct {
	*singleMatcher
	ctx                                              context.Context
	compare                                          indexEntry // value part is fixed, position part is used for comparisons
	reader                                           *tableReader
	prover                                           *proverInstance
	entryPtr                                         uint64
	direction                                        int
	initialized, isEmpty                             bool
	first, last                                      indexPosition
	currentPos                                       *indexPosition
	lastEntryNode, currentEntryNode, lastSectionNode uint32
}

// newInstance creates a new instance of singleMatcher.
func (m *singleMatcher) newInstance(ctx context.Context, reader *tableReader, prover *proverInstance, first, last indexPosition, direction int) matcherInstance {
	mi := &singleMatcherInstance{
		singleMatcher: m,
		ctx:           ctx,
		compare: indexEntry{
			indexValue: m.value,
		},
		reader:    reader,
		prover:    prover,
		direction: direction,
		first:     first,
		last:      last,
	}
	if reader.entryCount == 0 {
		mi.isEmpty = true
	} else if direction == -1 {
		mi.entryPtr = reader.entryCount - 1
	}
	return mi
}

func (m *singleMatcherInstance) init() error {
	var findPos *indexPosition
	switch m.direction {
	case 1:
		findPos = &m.first
	case -1:
		findPos = &m.last
	}
	m.initialized = true
	if err := m.advance(findPos); err != nil {
		m.initialized = false
		return err
	}
	return nil
}

// next implements matcherInstance.
func (m *singleMatcherInstance) next() (dpos *indexPosition, dlsn uint32, derr error) {
	/*defer func() {
		fmt.Println("singleMatcherInstance  pos", dpos, "lsn", dlsn, "err", derr)
	}()*/
	if !m.initialized {
		if err := m.init(); err != nil {
			return nil, 0, err
		}
	}
	if m.lastSectionNode == 0 {
		if m.currentEntryNode == 0 {
			m.currentEntryNode = m.prover.addProvenEntryNode(m.entryPtr)
		}
		if (m.direction == 1 && m.entryPtr != 0) || (m.direction == 1 && m.entryPtr < m.reader.entryCount-1) {
			if m.lastEntryNode == 0 {
				m.lastEntryNode = m.prover.addProvenEntryNode(uint64(int64(m.entryPtr) - int64(m.direction)))
			}
			m.lastSectionNode = m.prover.addAndGateNode()
			m.prover.connect(m.lastEntryNode, m.lastSectionNode)
			m.prover.connect(m.currentEntryNode, m.lastSectionNode)
		} else {
			m.lastSectionNode = m.currentEntryNode
		}
	}
	if m.currentPos == nil {
		if m.isEmpty {
			return nil, m.lastSectionNode, nil
		}
		entry, err := m.reader.getEntry(m.entryPtr)
		if err != nil {
			return nil, 0, err
		}
		var comparePos *indexPosition
		switch m.direction {
		case 1:
			comparePos = &m.last
		case -1:
			comparePos = &m.first
		}
		if entry.indexValue != m.value || entry.indexPosition.compare(comparePos) == m.direction {
			m.isEmpty = true
			m.currentPos = nil
			return nil, m.lastSectionNode, nil
		}
		m.currentPos = &entry.indexPosition
	}
	return m.currentPos, m.lastSectionNode, nil
}

// advance implements matcherInstance.
func (m *singleMatcherInstance) advance(findPos *indexPosition) error {
	select {
	case <-m.ctx.Done():
		return m.ctx.Err()
	default:
	}
	if !m.initialized {
		if err := m.init(); err != nil {
			return err
		}
	}
	if m.isEmpty {
		return nil
	}
	m.currentPos = nil
	if findPos == nil {
		// move on to the next entry
		switch m.direction {
		case 1:
			if m.entryPtr+1 < m.reader.entryCount {
				m.entryPtr++
			} else {
				m.isEmpty = true
			}
		case -1:
			if m.entryPtr > 0 {
				m.entryPtr--
			} else {
				m.isEmpty = true
			}
		}
		m.lastEntryNode = m.currentEntryNode
		m.currentEntryNode, m.lastSectionNode = 0, 0
		return nil
	}
	//fmt.Println("singleMatcherInstance", m.value, "advance", *findPos)
	// ensure that we are not moving beyond the position range limit
	var limitPos *indexPosition
	switch m.direction {
	case 1:
		limitPos = &m.last
	case -1:
		limitPos = &m.first
	}
	if findPos.compare(limitPos) == m.direction {
		findPos = limitPos
	}
	// move to the entry at or beyond findPos
	m.compare.indexPosition = *findPos
	pos, found, err := m.reader.seekEntry(&m.compare)
	if err != nil {
		return err
	}
	/*fmt.Println(" seekEntry  pos", pos, "found", found)
	//----
	for p := max(pos, 1) - 1; p < min(pos+2, m.reader.entryCount); p++ {
		entry, _ := m.reader.getEntry(p)
		fmt.Println("  pos", p, entry.indexValue, entry.indexPosition)
	}*/
	//----
	oldEntryPtr := m.entryPtr
	switch m.direction {
	case 1:
		if pos < m.reader.entryCount {
			m.entryPtr = pos
		} else {
			m.isEmpty = true
		}
	case -1:
		if !found {
			if pos > 0 {
				pos--
			} else {
				m.isEmpty = true
			}
		}
		m.entryPtr = pos
	}
	switch m.entryPtr {
	case oldEntryPtr:
	case uint64(int64(oldEntryPtr) + int64(m.direction)):
		m.lastEntryNode = m.currentEntryNode
		m.currentEntryNode, m.lastSectionNode = 0, 0
	default:
		m.lastEntryNode, m.currentEntryNode, m.lastSectionNode = 0, 0, 0
	}
	return nil
}

// split implements matcherInstance.
func (m *singleMatcherInstance) split(prover *proverInstance, splitPos *indexPosition) matcherInstance {
	if !m.initialized {
		panic("cannot split uninitialized single matcher")
	}
	m2 := &singleMatcherInstance{
		singleMatcher: m.singleMatcher,
		ctx:           m.ctx,
		compare:       m.compare,
		reader:        m.reader,
		prover:        prover,
		entryPtr:      m.entryPtr,
		direction:     m.direction,
		first:         m.first,
		last:          m.last,
	}
	switch m.direction {
	case 1:
		m.last = *splitPos
		m.last.decrease()
		m2.first = *splitPos
	case -1:
		m.first = *splitPos
		m2.last = *splitPos
		m2.last.decrease()
	default:
		panic("invalid search direction")
	}
	if m.currentPos != nil && (splitPos.compare(m.currentPos) == 1) != (m.direction == 1) {
		m.currentPos, m.isEmpty = nil, true
	}
	return m2
}

// matchAny combinines a set of matchers and returns a match for every position
// where any of the underlying matchers signaled a match. A zero-length matchAny
// acts as a "wild card" that signals a potential match at every position.
type matchAny []matcher

// matchAnyInstance is an instance of matchAny.
type matchAnyInstance struct {
	children        []matcherInstance
	prover          *proverInstance
	direction       int
	currentPos      *indexPosition
	lastSectionNode uint32
	isEmpty         bool
}

// newInstance creates a new instance of matchAny.
func (m matchAny) newInstance(ctx context.Context, reader *tableReader, prover *proverInstance, first, last indexPosition, direction int) matcherInstance {
	if len(m) == 0 {
		panic("zero length matchAny")
	}
	if len(m) == 1 {
		return m[0].newInstance(ctx, reader, prover, first, last, direction)
	}
	mi := &matchAnyInstance{
		children:  make([]matcherInstance, len(m)),
		prover:    prover,
		direction: direction,
	}
	for i, mm := range m {
		mi.children[i] = mm.newInstance(ctx, reader, prover, first, last, direction)
	}
	return mi
}

// next implements matcherInstance.
func (m *matchAnyInstance) next() (dpos *indexPosition, dlsn uint32, derr error) {
	/*defer func() {
		fmt.Println("matchAnyInstance  pos", dpos, "lsn", dlsn, "err", derr)
	}()*/
	if m.isEmpty || m.currentPos != nil {
		return m.currentPos, m.lastSectionNode, nil
	}
	if m.prover != nil {
		m.lastSectionNode = m.prover.addAndGateNode()
	}
	for _, cm := range m.children {
		pos, node, err := cm.next()
		if err != nil {
			return nil, 0, err
		}
		if m.prover != nil {
			m.prover.connect(node, m.lastSectionNode)
		}
		if pos != nil && (m.currentPos == nil || m.currentPos.compare(pos) == m.direction) {
			m.currentPos = pos
		}
	}
	m.isEmpty = m.currentPos == nil
	return m.currentPos, m.lastSectionNode, nil
}

// advance implements matcherInstance.
func (m *matchAnyInstance) advance(findPos *indexPosition) error {
	if m.isEmpty {
		return nil
	}
	if findPos == nil {
		currentPos, _, err := m.next()
		if err != nil {
			return err
		}
		if currentPos == nil {
			return nil
		}
		m.currentPos = nil
		for _, cm := range m.children {
			pos, _, err := cm.next()
			if err != nil {
				return err
			}
			if pos != nil && *pos == *currentPos {
				if err := cm.advance(nil); err != nil {
					return err
				}
			}
		}
		return nil
	}
	m.currentPos = nil
	for _, cm := range m.children {
		if err := cm.advance(findPos); err != nil {
			return err
		}
	}
	return nil
}

// split implements matcherInstance.
func (m *matchAnyInstance) split(prover *proverInstance, splitPos *indexPosition) matcherInstance {
	c := &matchAnyInstance{
		children:  make([]matcherInstance, len(m.children)),
		prover:    prover,
		direction: m.direction,
	}
	for i, cm := range m.children {
		c.children[i] = cm.split(prover, splitPos)
	}
	if m.currentPos != nil && (splitPos.compare(m.currentPos) == 1) != (m.direction == 1) {
		m.currentPos, m.isEmpty = nil, true
	}
	return c
}

type matchAll []matcher

// matchAllInstance is an instance of matchAll.
type matchAllInstance struct {
	children        []matcherInstance
	prover          *proverInstance
	direction       int
	currentPos      *indexPosition
	lastSectionNode uint32
	isEmpty         bool
}

// newInstance creates a new instance of matchAll.
func (m matchAll) newInstance(ctx context.Context, reader *tableReader, prover *proverInstance, first, last indexPosition, direction int) matcherInstance {
	if len(m) == 0 {
		panic("zero length matchAll")
	}
	if len(m) == 1 {
		return m[0].newInstance(ctx, reader, prover, first, last, direction)
	}
	mi := &matchAllInstance{
		children:  make([]matcherInstance, len(m)),
		prover:    prover,
		direction: direction,
	}
	for i, mm := range m {
		mi.children[i] = mm.newInstance(ctx, reader, prover, first, last, direction)
	}
	return mi
}

// next implements matcherInstance.
func (m *matchAllInstance) next() (dpos *indexPosition, dlsn uint32, derr error) {
	/*defer func() {
		fmt.Println("matchAllInstance  pos", dpos, "lsn", dlsn, "err", derr)
	}()*/
	//fmt.Println("matchAllInstance.next()")
	if m.isEmpty || m.currentPos != nil {
		return m.currentPos, m.lastSectionNode, nil
	}
	for {
		match := true
		var (
			next              *indexPosition
			nextNode, andNode uint32
		)
		if m.prover != nil {
			andNode = m.prover.addAndGateNode()
		}
		for i, cm := range m.children {
			pos, node, err := cm.next()
			if err != nil {
				return nil, 0, err
			}
			if pos == nil {
				m.isEmpty = true
				var orNode uint32
				if m.prover != nil {
					orNode = m.prover.addOrGateNode()
					m.prover.connect(node, orNode)
					for j := i + 1; j < len(m.children); j++ {
						pos, node, err := cm.next()
						if err != nil {
							return nil, 0, err
						}
						if pos == nil {
							m.prover.connect(node, orNode)
						}
					}
				}
				m.currentPos, m.lastSectionNode = nil, orNode
				return nil, orNode, nil
			}
			//fmt.Println(" child", i, "next()", *pos)
			if i == 0 {
				next, nextNode = pos, node
			} else {
				switch next.compare(pos) {
				case m.direction:
					match = false
				case -m.direction:
					next = pos
					match = false
				}
			}
		}
		if m.prover != nil {
			m.prover.connect(nextNode, andNode)
		}
		//fmt.Println(" match", match)
		if match {
			m.currentPos, m.lastSectionNode = next, andNode
			return next, andNode, nil
		}
		for _, cm := range m.children {
			if pos, _, _ := cm.next(); *pos != *next {
				//fmt.Println(" child", i, "advance", *next)
				if err := cm.advance(next); err != nil {
					return nil, 0, err
				}
			}
		}
	}
}

// advance implements matcherInstance.
func (m *matchAllInstance) advance(findPos *indexPosition) error {
	if m.isEmpty {
		return nil
	}
	if findPos == nil {
		if _, _, err := m.next(); err != nil {
			return err
		}
	}
	m.currentPos = nil
	for _, cm := range m.children {
		if err := cm.advance(findPos); err != nil {
			return err
		}
	}
	return nil
}

// split implements matcherInstance.
func (m *matchAllInstance) split(prover *proverInstance, splitPos *indexPosition) matcherInstance {
	c := &matchAllInstance{
		children:  make([]matcherInstance, len(m.children)),
		prover:    prover,
		direction: m.direction,
	}
	for i, cm := range m.children {
		c.children[i] = cm.split(prover, splitPos)
	}
	if m.currentPos != nil && (splitPos.compare(m.currentPos) == 1) != (m.direction == 1) {
		m.currentPos, m.isEmpty = nil, true
	}
	return c
}

/*
var stNames = []string{"", "fetchFirst", "fetchMore", "process", "getLog", "other"}

// set sets the processing state to one of the pre-defined constants.
// Processing time spent in each state is measured separately.

	func (ts *runtimeStats) setState(state *int, newState int) {
		if !doRuntimeStats || newState == *state {
			return
		}
		now := int64(mclock.Now())
		atomic.AddInt64(&ts.dt[*state], now)
		atomic.AddInt64(&ts.dt[newState], -now)
		atomic.AddInt64(&ts.cnt[newState], 1)
		*state = newState
	}

	func (ts *runtimeStats) addAmount(state int, amount int64) {
		atomic.AddInt64(&ts.amount[state], amount)
	}

// print prints the collected statistics.

	func (ts *runtimeStats) print() {
		for i := 1; i < stCount; i++ {
			log.Info("Matcher stats", "name", stNames[i], "dt", time.Duration(ts.dt[i]), "count", ts.cnt[i], "amount", ts.amount[i])
		}
	}
*/

type matcherSession struct {
	ctx            context.Context
	requestCounter uint64
	maxResults     uint64
	validUntil     uint64
	direction      int
	minTopicCount  int
	resultsCh      chan matcherResults
	first, last    *matcherProcess // ordered according to search direction
	finished       bool
	err            error
}

func (ms *matcherSession) print() {
	for mp := ms.first; mp != nil; mp = mp.next {
		mp.blockDataLock.Lock()
		var prevRange, nextRange common.Range[uint64]
		if mp.prev != nil {
			prevRange = mp.prev.blockRange
		}
		if mp.next != nil {
			nextRange = mp.next.blockRange
		}
		fmt.Println(" range", mp.blockRange, "len(pos)", len(mp.positions), "len(logs)", len(mp.logs), "completeUntil", mp.completeUntil,
			"droppedResults", mp.droppedResults, "matcherFinished", mp.matcherFinished, "finished", mp.finished, "status", mp.status,
			"prevRange", prevRange, "nextRange", nextRange)
		mp.blockDataLock.Unlock()
	}
}

func (ms *matcherSession) returnResults() {
	//fmt.Println("*** returnResults")
	//ms.print()
	if ms.err != nil {
		ms.resultsCh <- matcherResults{err: ms.err}
		return
	}
	var resCount uint64
	for mp := ms.first; mp != nil; mp = mp.next {
		//fmt.Println(" mp", mp.blockRange)
		if mp.isRemoved() {
			//fmt.Println("  removed")
			if mp.prev == nil {
				ms.first = mp.next
				if mp.next != nil {
					mp.next.prev = nil
				} else {
					ms.last = nil
				}
				continue
			} else {
				mp.prev.next = nil
				ms.last = mp.prev
			}
		}
		resCount += uint64(len(mp.positions) - mp.droppedResults)
		//fmt.Println("  resCount", len(mp.positions), mp.droppedResults, resCount)
		if resCount >= ms.maxResults {
			resCount = ms.maxResults
			break
		}
	}
	if ms.first == nil {
		//fmt.Println("  ms.first == nil")
		ms.resultsCh <- matcherResults{}
		return
	}
	res := matcherResults{
		logs:       make([]*types.Log, 0, resCount),
		blockRange: common.NewRange[uint64](ms.first.blockRange.First(), ms.last.blockRange.AfterLast()-ms.first.blockRange.First()),
	}
	var (
		currentProver *tableProver
		andNode       uint32
	)

	addProcessResults := func(mp *matcherProcess) bool {
		mp.blockDataLock.Lock()
		defer mp.blockDataLock.Unlock()

		fmt.Println("addProcessResults", mp.tableReader.blockRange())
		if mp.tableProver != nil && mp.tableProver != currentProver {
			if currentProver != nil {
				res.provers = append(res.provers, currentProver)
			}
			currentProver = mp.tableProver
			andNode = mp.proverInstance.addAndGateNode()
			mp.proverInstance.connect(andNode, mp.proverInstance.addFinalResultNode())
		}
		for i, log := range mp.logs {
			if uint64(len(res.logs)) == resCount {
				lastLog := res.logs[resCount-1]
				trimBlockProofs(mp.blockProofs, lastLog.BlockNumber, uint32(lastLog.TxIndex), mp.session.direction)
				mp.tableProver.addBlockProofs(mp.blockProofs)
				return false
			}
			mp.proverInstance.connect(mp.sectionNodes[i], andNode)
			if log != droppedLogResult {
				res.logs = append(res.logs, log)
			}
		}
		if len(mp.sectionNodes) > len(mp.logs) {
			mp.proverInstance.connect(mp.sectionNodes[len(mp.logs)], andNode)
		}
		mp.tableProver.addBlockProofs(mp.blockProofs)
		return uint64(len(res.logs)) != resCount
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

func trimBlockProofs(blockProofs map[uint64]*blockProof, lastBlock uint64, lastTx uint32, direction int) {
	for number, bp := range blockProofs {
		if (number > lastBlock && direction == 1) || (number < lastBlock && direction == -1) {
			delete(blockProofs, number)
			continue
		}
		if number == lastBlock {
			for txi := range bp.matchingTxs {
				if (txi > lastTx && direction == 1) || (txi < lastTx && direction == -1) {
					delete(bp.matchingTxs, txi)
				}
			}
		}
	}
}

var uint64msb = uint64(1) << 63

const (
	mpInactive    = iota // not scheduled for running because no more results will be needed according to estimates
	mpSuspended          // scheduled for running
	mpRunning            // passed to a worker thread, included in matcherControl.processing
	mpFinished           // all results found or hit an error
	mpFinishedAll        // all previous processes have the same state; either found all results or enough to reach maxResults
)

type matcherProcess struct {
	indexer        *Indexer
	matcher        matcherInstance
	session        *matcherSession
	tableReader    *tableReader
	tableProver    *tableProver
	proverInstance *proverInstance
	prev, next     *matcherProcess
	blockRange     common.Range[uint64]

	// accessed by control thread only
	status  int
	pqIndex int

	// accessed only by worker thread while status == mpRunning
	positions                     []indexPosition
	sectionNodes                  []uint32
	completeUntil, droppedResults int
	finished, matcherFinished     bool
	err                           error
	runTime                       time.Duration
	started                       mclock.AbsTime

	// accessed by block data delivery thread
	blockDataLock  sync.Mutex
	deliverCh      chan struct{}
	blockInResults map[uint64]int
	logs           []*types.Log
	blockProofs    map[uint64]*blockProof
	deliveryErr    error

	// atomic flags accessed by both threads during processing
	estimatedResults  uint64 // set by worker thread; MSB is "can split" flag
	cumulativeResults uint64 // set by control thread; MSB is "suspend now" flag
}

var droppedLogResult = &types.Log{}

// called by worker thread
func (mp *matcherProcess) updateEstimatedResults(running bool) (uint64, bool) {
	var (
		estimatedResults uint64
		canSplit         bool
	)
	if len(mp.positions) > 0 {
		done, _, remaining := mp.getProgress()
		ratio := float64(remaining) / float64(done) // remaining to done ratio; done >= 1
		runTime := mp.runTime
		if running {
			runTime += time.Duration(max(0, mclock.Now()-mp.started))
		}
		remainingResults := uint64(float64(len(mp.positions)-mp.droppedResults) * ratio)
		estimatedResults = uint64(len(mp.positions)-mp.droppedResults) + remainingResults
		if runTime >= splitAfter {
			remainingTime := time.Duration(float64(runTime) * ratio)
			canSplit = remainingTime >= splitThreshold
		}
	}
	if canSplit {
		atomic.StoreUint64(&mp.estimatedResults, estimatedResults+uint64msb)
	} else {
		atomic.StoreUint64(&mp.estimatedResults, estimatedResults)
	}
	return estimatedResults, canSplit
}

// estimated result count, "can split" flag
func (mp *matcherProcess) getEstimatedResults() (uint64, bool) {
	v := atomic.LoadUint64(&mp.estimatedResults)
	return v & (uint64msb - 1), (v & uint64msb) != 0
}

// split after returned block number; always called while not running
func (mp *matcherProcess) getSplitBlock() (uint64, bool) {
	done, lastBlock, remaining := mp.getProgress()
	s := float64(done) * float64(splitTarget) / float64(max(mp.runTime, 1))
	if s >= float64(remaining)/2 {
		return 0, false
	}
	splitAfter := uint64(s)
	switch mp.session.direction {
	case 1:
		return min(lastBlock+splitAfter+1, mp.blockRange.AfterLast()), true
	case -1:
		return max(lastBlock, mp.blockRange.First()+splitAfter) - splitAfter, true
	default:
		panic("invalid search direction")
	}
}

func (mp *matcherProcess) isRemoved() bool {
	return mp.blockRange.Last() > atomic.LoadUint64(&mp.session.validUntil)
}

func (mp *matcherProcess) getProgress() (done, lastBlock, remaining uint64) {
	if len(mp.positions) > 0 {
		lastBlock = max(mp.blockRange.First(), mp.positions[len(mp.positions)-1].blockNumber)
	} else {
		lastBlock = mp.blockRange.First()
	}
	switch mp.session.direction {
	case 1:
		done, remaining = lastBlock+1-mp.blockRange.First(), mp.blockRange.Last()-lastBlock
	case -1:
		done, remaining = mp.blockRange.AfterLast()-lastBlock, lastBlock-mp.blockRange.First()
	default:
		panic("invalid search direction")
	}
	return
}

func (mp *matcherProcess) getCumulativeResults() (uint64, bool) {
	v := atomic.LoadUint64(&mp.estimatedResults)
	return v & (uint64msb - 1), (v & uint64msb) != 0
}

func (mp *matcherProcess) setCumulativeResults(cumulativeResults uint64, suspendNow bool) {
	if suspendNow {
		atomic.StoreUint64(&mp.cumulativeResults, cumulativeResults+uint64msb)
	} else {
		atomic.StoreUint64(&mp.cumulativeResults, cumulativeResults)
	}
}

func (mp *matcherProcess) run() {
	mp.started = mclock.Now()
	defer func() {
		mp.runTime += time.Duration(mclock.Now() - mp.started)
	}()

	//fmt.Println("matcherProcess", mp.blockRange, "started")
	//defer fmt.Println("matcherProcess", mp.blockRange, "stopped")

	for !mp.finished {
		if !mp.matcherFinished && len(mp.positions) < mp.completeUntil+maxIncompleteResults {
			//fmt.Println(" mp.matcher.next()")
			pos, node, err := mp.matcher.next()
			if err != nil {
				//fmt.Println("matcherProcess", mp.blockRange, "error (next)", err)
				mp.finished, mp.err = true, err
				return
			}
			mp.sectionNodes = append(mp.sectionNodes, node)
			if pos == nil {
				//fmt.Println("matcherProcess", mp.blockRange, "finished")
				mp.matcherFinished = true
			} else {
				//fmt.Println("matcherProcess", mp.blockRange, "found", *pos)
				mp.positions = append(mp.positions, *pos)
				//fmt.Println(" mp.matcher.advance(nil)")
				if err := mp.matcher.advance(nil); err != nil {
					//fmt.Println("matcherProcess", mp.blockRange, "error (advance)", err)
					mp.finished, mp.err = true, err
					return
				}
			}
		} else {
			//fmt.Println(" <-mp.deliverCh")
			select {
			case <-mp.deliverCh:
			case <-mp.session.ctx.Done():
				return
			}
		}
		mp.updateEstimatedResults(true)
		cumulativeResults, suspendNow := mp.getCumulativeResults()
		mp.blockDataLock.Lock()
		var requestBlocks []uint64
		if mp.deliveryErr != nil {
			mp.finished, mp.err = true, mp.deliveryErr
		} else {
			for len(mp.positions) > len(mp.logs) {
				pos := mp.positions[len(mp.logs)]
				if _, ok := mp.blockInResults[pos.blockNumber]; !ok {
					mp.blockInResults[pos.blockNumber] = len(mp.logs)
					//fmt.Println("*** getBlockData", pos.blockNumber)
					//fmt.Println(" len(mp.positions)", len(mp.positions), "len(mp.logs)", len(mp.logs), "blockInResults[", pos.blockNumber, "] = ", len(mp.logs))
					requestBlocks = append(requestBlocks, pos.blockNumber)
				}
				mp.logs = append(mp.logs, nil)
			}
			for mp.completeUntil < len(mp.logs) && mp.logs[mp.completeUntil] != nil {
				if mp.logs[mp.completeUntil] == droppedLogResult {
					mp.droppedResults++
				}
				mp.completeUntil++
			}
			if mp.matcherFinished && mp.completeUntil == len(mp.logs) {
				mp.finished = true
			}
		}
		mp.blockDataLock.Unlock()
		for _, blockNumber := range requestBlocks {
			// start requests outside blockDataLock to avoid wrong locking order
			mp.indexer.getBlockDataLocked(blockNumber, true, true, 0 /*TODO*/, mp.deliverBlockData)
		}
		if suspendNow || cumulativeResults+uint64(mp.completeUntil-mp.droppedResults) >= mp.session.maxResults {
			return
		}
	}
}

func (mp *matcherProcess) deliverBlockData(req blockRequest, header *types.Header, body *types.Body, receipts types.Receipts) {
	//fmt.Println("*** deliverBlockData", req.number)
	mp.blockDataLock.Lock()
	defer mp.blockDataLock.Unlock()

	firstInResults, ok := mp.blockInResults[req.number]
	if ok {
		delete(mp.blockInResults, req.number)
	} else {
		return
	}
	if header == nil || body == nil || receipts == nil {
		mp.deliveryErr = errors.New("block data not delivered")
		return
	}
	select {
	case mp.deliverCh <- struct{}{}:
	default:
	}

	var blockProof *blockProof
	if mp.tableProver != nil {
		blockProof = mp.blockProofs[req.number]
		if blockProof == nil {
			if req.number == mp.blockRange.Last() {
				blockProof = newBlockProof(header, math.MaxUint64)
			} else {
				blockEntry := indexEntry{
					indexValue: indexValue{
						entryType: ieBlock,
						value:     ([32]byte)(header.Hash()),
					},
					indexPosition: indexPosition{
						blockNumber: req.number,
					},
				}
				blockEntryIndex, found, err := mp.tableReader.seekEntry(&blockEntry)
				if err != nil {
					mp.deliveryErr = err
					return
				}
				if !found {
					mp.deliveryErr = errors.New("could not find block entry index")
					return
				}
				blockProof = newBlockProof(header, blockEntryIndex)
			}
			mp.blockProofs[req.number] = blockProof
		}
	}

loop:
	for ; firstInResults < len(mp.logs); firstInResults++ {
		pos := mp.positions[firstInResults]
		if pos.blockNumber != req.number || uint32(len(receipts)) <= pos.txIndex || uint32(len(receipts[pos.txIndex].Logs)) <= pos.logIndex {
			break loop
		}
		var logOffset uint
		for i := range pos.txIndex {
			logOffset += uint(len(receipts[i].Logs)) //TODO different position encoding?
		}
		txHash := body.Transactions[pos.txIndex].Hash()
		l := receipts[pos.txIndex].Logs[pos.logIndex]
		if len(l.Topics) < mp.session.minTopicCount && mp.tableProver == nil { // include results with too few topic in proving mode
			mp.logs[firstInResults] = droppedLogResult
		} else {
			mp.logs[firstInResults] = &types.Log{
				Address:        l.Address,
				Topics:         l.Topics,
				Data:           l.Data,
				BlockNumber:    pos.blockNumber,
				TxHash:         txHash,
				TxIndex:        uint(pos.txIndex),
				BlockHash:      header.Hash(),
				BlockTimestamp: header.Time,
				Index:          logOffset + uint(pos.logIndex),
			}
		}
		if blockProof != nil {
			txEntry := indexEntry{
				indexValue: indexValue{
					entryType: ieTransaction,
					value:     ([32]byte)(txHash),
				},
				indexPosition: indexPosition{
					blockNumber: pos.blockNumber,
					txIndex:     pos.txIndex,
					logIndex:    uint32(logOffset),
				},
			}
			txEntryIndex, found, err := mp.tableReader.seekEntry(&txEntry)
			if err != nil {
				mp.deliveryErr = err
				return
			}
			if !found {
				mp.deliveryErr = errors.New("could not find transaction entry index")
				return
			}
			blockProof.addMatchingTx(pos.txIndex, txEntryIndex)
		}
	}
	if blockProof != nil {
		blockProof.createProof(receipts)
	}
}

type blockProof struct {
	header          *types.Header
	blockEntryIndex uint64 // MaxUint64 if block is last in table
	matchingTxs     map[uint32]matchingTx
	receiptsProof   trieProofWriter
}

type matchingTx struct {
	txEntryIndex      uint64
	receiptProofAdded bool
}

func newBlockProof(header *types.Header, blockEntryIndex uint64) *blockProof {
	return &blockProof{
		header:          header,
		blockEntryIndex: blockEntryIndex,
		matchingTxs:     make(map[uint32]matchingTx),
		receiptsProof:   make(trieProofWriter),
	}
}

func (bp *blockProof) merge(bp2 *blockProof) {
	if bp.header.Hash() != bp2.header.Hash() || bp.blockEntryIndex != bp2.blockEntryIndex {
		panic("invalid block proof merge")
	}
	for txi, mtx2 := range bp2.matchingTxs {
		if mtx, ok := bp.matchingTxs[txi]; ok {
			if mtx.txEntryIndex != mtx2.txEntryIndex {
				panic("invalid matching tx proof merge")
			}
			if mtx2.receiptProofAdded && !mtx.receiptProofAdded {
				bp.matchingTxs[txi] = mtx2
			}
		} else {
			bp.matchingTxs[txi] = mtx2
		}
	}
	for hash, blob := range bp2.receiptsProof {
		bp.receiptsProof[hash] = blob
	}
}

func (bp *blockProof) addMatchingTx(txIndex uint32, entryIndex uint64) {
	if _, ok := bp.matchingTxs[txIndex]; !ok {
		bp.matchingTxs[txIndex] = matchingTx{txEntryIndex: entryIndex}
	}
}

func (bp *blockProof) createProof(receipts types.Receipts) {
	proveHexKeys := make(map[string]struct{})
	proveHexKeys[""] = struct{}{}
	var indexBuf, indexHex []byte
	for txi, mtx := range bp.matchingTxs {
		if mtx.receiptProofAdded {
			continue
		}
		indexBuf = rlp.AppendUint64(indexBuf[:0], uint64(txi))
		indexHex = indexHex[:0]
		for _, b := range indexBuf {
			indexHex = append(indexHex, b/16)
			proveHexKeys[string(indexHex)] = struct{}{}
			indexHex = append(indexHex, b%16)
			proveHexKeys[string(indexHex)] = struct{}{}
		}
		mtx.receiptProofAdded = true
		bp.matchingTxs[txi] = mtx
	}
	types.DeriveSha(receipts, trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		if _, ok := proveHexKeys[string(path)]; ok {
			bp.receiptsProof[hash] = slices.Clone(blob)
			delete(proveHexKeys, string(path))
		}
	}))
}

func (mp *matcherProcess) split() (*matcherProcess, error) {
	splitAt, ok := mp.getSplitBlock()
	if !ok {
		return nil, nil
	}
	proverInstance := mp.tableProver.newInstance()
	mp2 := &matcherProcess{
		indexer:        mp.indexer,
		matcher:        mp.matcher.split(proverInstance, &indexPosition{blockNumber: splitAt}),
		session:        mp.session,
		tableReader:    mp.tableReader,
		tableProver:    mp.tableProver,
		proverInstance: proverInstance,
		prev:           mp,
		next:           mp.next,
		blockRange:     mp.blockRange,
		blockInResults: make(map[uint64]int),
		blockProofs:    make(map[uint64]*blockProof),
		deliverCh:      make(chan struct{}, 1),
	}
	if mp.next != nil {
		mp.next.prev = mp2
	}
	mp.next = mp2
	switch mp.session.direction {
	case 1:
		mp.blockRange.SetAfterLast(splitAt)
		mp2.blockRange.SetFirst(splitAt)
		if mp.session.last == mp {
			mp.session.last = mp2
		}
	case -1:
		mp.blockRange.SetFirst(splitAt)
		mp2.blockRange.SetAfterLast(splitAt)
		if mp.session.first == mp {
			mp.session.first = mp2
		}
	default:
		panic("invalid search direction")
	}
	//fmt.Println("*** split", splitAt)
	//mp.session.print()
	return mp2, nil
}

type matcherControl struct {
	threadCount, activeThreads int
	requestCounter             uint64
	sessions                   map[*matcherSession]struct{}
	processing                 []*matcherProcess      // status == mpRunning
	startProcessCh             []chan *matcherProcess // control -> worker
	stoppedProcessCh           chan int               // worker -> control (thread index)
	sessionCh                  chan *matcherSession   // GetMatches -> control
	suspended, updateSort      processQueue
	updateTicker               *time.Ticker
	stopCh                     chan struct{}
	wg                         sync.WaitGroup
}

func (mc *matcherControl) init(threadCount int) {
	mc.threadCount = threadCount
	mc.sessions = make(map[*matcherSession]struct{})
	mc.processing = make([]*matcherProcess, threadCount)
	mc.updateSort = make(processQueue, threadCount+1)
	mc.startProcessCh = make([]chan *matcherProcess, threadCount)
	for i := range threadCount {
		mc.startProcessCh[i] = make(chan *matcherProcess, 1)
	}
	mc.stoppedProcessCh = make(chan int)
	mc.sessionCh = make(chan *matcherSession)
	mc.stopCh = make(chan struct{})
	mc.updateTicker = time.NewTicker(updateEstimatesFrequency)
	mc.updateTicker.Stop()
	mc.wg.Add(threadCount + 1)
	go mc.controlLoop()
	for i := range threadCount {
		go mc.workerLoop(i)
	}
}

func (mc *matcherControl) stop() {
	close(mc.stopCh)
	mc.wg.Wait()
}

// called under Indexer.lock
func (mc *matcherControl) revert(blockNumber uint64) {
	for session := range mc.sessions {
		if blockNumber < session.validUntil {
			atomic.StoreUint64(&session.validUntil, blockNumber)
		}
	}
}

func (mc *matcherControl) workerLoop(threadIndex int) {
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

func (mc *matcherControl) controlLoop() {
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
					if !mp.isRemoved() && !mp.session.finished {
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

func (mc *matcherControl) start(mp *matcherProcess) {
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

func (mc *matcherControl) stopped(threadIndex int) {
	mp := mc.processing[threadIndex]
	mc.processing[threadIndex] = nil
	mp.status = mpInactive
	mc.activeThreads--
	if mc.activeThreads == 0 {
		mc.updateTicker.Stop()
	}
	if mp.err != nil && !mp.isRemoved() && !mp.session.finished {
		mp.session.finished, mp.session.err = true, mp.err
		mp.session.returnResults()
	}
	mc.updateSessions(mp)
}

func (mc *matcherControl) updateSessions(notRunning *matcherProcess) {
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

func (mc *matcherControl) updateSessionFrom(start *matcherProcess, allowedSplits int) int {
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

func (mc *matcherControl) updateIdleStatus(mp *matcherProcess, cumulativeResults uint64) {
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
	if cumulativeResults+uint64(mp.completeUntil-mp.droppedResults) >= mp.session.maxResults {
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
	switch pq[i].session.direction {
	case 1:
		return pq[i].blockRange.First() < pq[j].blockRange.First()
	case -1:
		return pq[i].blockRange.First() > pq[j].blockRange.First()
	default:
		panic("invalid search direction")
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
