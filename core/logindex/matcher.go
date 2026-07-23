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
	"math/big"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
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
	ProveTableRoot(refHead *types.Header, contract common.Address, firstBlock, tableSize uint64, proofNodes, proofCodes map[common.Hash][]byte) (common.Hash, error)
}

type contractVerifier interface {
	GetProvenTableRoot(refHead *types.Header, contract common.Address, firstBlock, tableSize uint64, proofNodes, proofCodes map[common.Hash][]byte) (common.Hash, error)
}

func (ix *Indexer) GetMatches(ctx context.Context, query FilterQuery, prove bool, refHeader *types.Header, contractProver contractProver, contractVerifier contractVerifier) ([]*types.Log, common.Range[uint64], *QueryProof, error) {
	ix.lock.Lock()
	start := time.Now()
	headBlock := refHeader.Number.Uint64() //TODO check whether refHeader is part of the indexed chain (common ancestor if not?)
	firstBlock := min(query.FirstBlock, headBlock)
	lastBlock := min(query.LastBlock, headBlock)
	blockRange := common.NewRange[uint64](firstBlock, lastBlock+1-firstBlock)
	readerRange := blockRange
	if prove && lastBlock < headBlock {
		readerRange.SetLast(lastBlock + 1)
	}
	readers := ix.storage.getRangeReaders(readerRange)
	ix.lock.Unlock()
	sort.Slice(readers, func(i, j int) bool {
		return readers[i].meta.LastBlockNumber < readers[j].meta.LastBlockNumber
	})
	if query.Reverse {
		for i := len(readers) - 1; i > 0; i-- {
			if readers[i-1].blockRange().AfterLast() != readers[i].blockRange().First() {
				readers = readers[i:]
				break
			}
		}
	} else {
		for i := 1; i < len(readers); i++ {
			if readers[i-1].blockRange().AfterLast() != readers[i].blockRange().First() {
				readers = readers[:i]
				break
			}
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
	maxResults := math.MaxInt
	if uint64(maxResults) > query.MaxResults {
		maxResults = int(query.MaxResults)
	}
	session := &matcherSession{
		ctx:            ctx,
		requestCounter: atomic.AddUint64(&ix.matcherControl.requestCounter, 1),
		maxResults:     maxResults,
		validUntil:     blockRange.Last(),
		minTopicCount:  len(query.Topics),
		resultsCh:      make(chan matcherResults, 1),
	}
	//fmt.Println("create session", firstBlock, lastBlock, query.Addresses, query.Topics)
	var lastBlockProver *tableProver
	for i, reader := range readers {
		var tr matcherTableReader
		if query.Reverse {
			tr = reverseTableReader(reader)
		} else {
			tr = reader
		}
		br := tr.blockRange().Intersection(blockRange) // can be empty in the last table because of readerRange extension
		//fmt.Println(" ", br)
		prover := newTableProver(reader, query.Reverse)
		if br.IsEmpty() {
			lastBlockProver = prover
			break
		}
		logicBuilder := prover.optimizer.newBuilderInstance()
		mp := &matcherProcess{
			indexer: ix,
			matcher: matcher.newInstance(
				ctx,
				tr,
				logicBuilder,
				indexPosition{blockNumber: br.First()},
				indexPosition{blockNumber: br.Last(), txIndex: math.MaxUint32, logIndex: math.MaxUint32},
				direction),
			session:        session,
			tableReader:    tr,
			tableProver:    prover,
			logicBuilder:   logicBuilder,
			blockRange:     br,
			blockInResults: make(map[uint64]int),
			blockProofs:    make(map[uint64]*blockProof),
			deliverCh:      make(chan struct{}, 1),
		}
		if i == 0 {
			session.first = mp
			session.last = mp
		} else {
			if query.Reverse {
				mp.next = session.first
				session.first.prev = mp
				session.first = mp
			} else {
				mp.prev = session.last
				session.last.next = mp
				session.last = mp
			}
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
	if query.Reverse {
		results.blockRange = common.NewRange[uint64](math.MaxUint64-results.blockRange.Last(), results.blockRange.Count())
		last := len(results.logs) - 1
		for i := 0; i < last-i; i++ {
			results.logs[i], results.logs[last-1] = results.logs[last-1], results.logs[i]
		}
	}
	//fmt.Println("+++ Runtime without proof generation:", time.Since(start))
	var proof *QueryProof
	if prove {
		if query.Reverse {
			last := len(results.provers) - 1
			for i := 0; i < last-i; i++ {
				results.provers[i], results.provers[last-1] = results.provers[last-1], results.provers[i]
			}
		}
		if lastBlockProver != nil && len(results.provers) > 0 &&
			lastBlockProver.reader.blockRange().First() == results.provers[len(results.provers)-1].reader.blockRange().AfterLast() {
			results.provers = append(results.provers, lastBlockProver)
		}
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
		proofNodes := make(trieProofWriter)
		proofCodes := make(trieProofWriter)
		var proveParentBlock common.Hash
		for i, prover := range results.provers {
			if i != 0 && prover.reader.blockRange().First() != results.provers[i-1].reader.blockRange().AfterLast() {
				panic("prover block ranges are not continuous")
			}
			if prover == lastBlockProver && proveParentBlock == (common.Hash{}) {
				break
			}
			tproof, proveLastBlock, err := prover.finalize(proveParentBlock)
			if err != nil {
				return nil, common.Range[uint64]{}, nil, err
			}
			proveParentBlock = proveLastBlock
			tproof.IndexContract = proof.addOrGetIndexContract(prover.reader.indexContract)
			proof.TableQueryProofs[i] = tproof
			// generate state proof nodes for table root
			tableRoot, err := contractProver.ProveTableRoot(refHeader, prover.reader.indexContract, prover.reader.blockRange().First(), prover.reader.blockRange().Count(), proofNodes, proofCodes)
			//fmt.Println("GetTableRoot", prover.reader.blockRange(), tableRoot, err)
			if err != nil {
				return nil, common.Range[uint64]{}, nil, err
			}
			if tableRoot != common.Hash(prover.reader.tableRoot) {
				return nil, common.Range[uint64]{}, nil, errors.New("local table root does not match index contract")
			}
		}
		if proveParentBlock != (common.Hash{}) && proveParentBlock != refHeader.Hash() {
			return nil, common.Range[uint64]{}, nil, errors.New("could not prove last block of last table")
		}
		proof.ContractProofNodes = proofNodes.proofForStorage()
		proof.ContractProofCodes = proofCodes.proofForStorage()
		//proof.printStats()
		proofEnc, err := rlp.EncodeToBytes(proof)
		if err != nil {
			return nil, common.Range[uint64]{}, nil, err
		}
		proveTime := time.Since(start)
		start = time.Now()
		var proofDec QueryProof
		if err := rlp.DecodeBytes(proofEnc, &proofDec); err != nil {
			return nil, common.Range[uint64]{}, nil, err
		}
		//fmt.Println("decoded proof")
		//proofDec.printStats()
		if _, err := proof.Verify(contractVerifier); err != nil { //TODO only verify in dev mode
			//fmt.Println("verify error:", err)
			return nil, common.Range[uint64]{}, nil, err
		} /* else {
			fmt.Println("verified results:", len(res))
		}*/
		fmt.Println("[***] range length", blockRange.Count(), "result count", len(results.logs), "proof size", len(proofEnc), "prove time", proveTime, "verify time", time.Since(start))
	}
	//fmt.Println(" results", len(results.logs), "error", results.err)
	//fmt.Println("+++ Runtime with proof generation:", time.Since(start))
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

type matcherResults struct {
	logs       []*types.Log
	provers    []*tableProver
	blockRange common.Range[uint64]
	err        error
}

// matcher defines a general abstraction for any matcher configuration that
// can instantiate a matcherInstance.
type matcher interface {
	newInstance(ctx context.Context, reader matcherTableReader, prover *logicBuilder, first, last indexPosition) matcherInstance
}

type matcherTableReader interface {
	blockRange() common.Range[uint64]
	seekEntry(target *indexEntry) (uint64, bool, error)
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
	next() (*indexPosition, logicNodeID, error)
	advance(*indexPosition) error
	split(*logicBuilder, *indexPosition) matcherInstance
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
	reader                                           matcherTableReader
	logic                                            *logicBuilder
	entryPtr                                         uint64
	initialized, isEmpty                             bool
	first, last                                      indexPosition
	currentPos                                       *indexPosition
	lastEntryNode, currentEntryNode, lastSectionNode logicNodeID
}

// newInstance creates a new instance of singleMatcher.
func (m *singleMatcher) newInstance(ctx context.Context, reader matcherTableReader, logic *logicBuilder, first, last indexPosition) matcherInstance {
	mi := &singleMatcherInstance{
		singleMatcher: m,
		ctx:           ctx,
		compare: indexEntry{
			indexValue: m.value,
		},
		reader: reader,
		logic:  logic,
		first:  first,
		last:   last,
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
	//fmt.Println("init", m.value, "pos", *findPos, "entryPtr", m.entryPtr)
	return nil
}

// next implements matcherInstance.
func (m *singleMatcherInstance) next() (dpos *indexPosition, dlsn logicNodeID, derr error) {
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
			m.currentEntryNode = m.logic.addInputNode(m.entryPtr)
			//fmt.Println(" currentEntryNode ptr =", m.entryPtr)
		}
		if (m.direction == 1 && m.entryPtr != 0) || (m.direction == 1 && m.entryPtr < m.reader.entryCount-1) {
			if m.lastEntryNode == 0 {
				m.lastEntryNode = m.logic.addInputNode(uint64(int64(m.entryPtr) - int64(m.direction)))
				//fmt.Println(" lastEntryNode ptr =", uint64(int64(m.entryPtr)-int64(m.direction)))
			}
			m.lastSectionNode = m.logic.addAndGateNode()
			m.logic.connect(m.lastEntryNode, m.lastSectionNode)
			m.logic.connect(m.currentEntryNode, m.lastSectionNode)
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
			//fmt.Println("sm", m.value, "next (empty)")
			return nil, m.lastSectionNode, nil
		}
		m.currentPos = &entry.indexPosition
	}
	//fmt.Println("sm", m.value, "next", *m.currentPos)
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
		//fmt.Println("sm", m.value, "advance (next)")
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
	//fmt.Println("sm", m.value, "advance", *findPos)
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
	//fmt.Println("sm", m.value, "seek", *findPos, "pos", pos, "found", found)
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
func (m *singleMatcherInstance) split(logic *logicBuilder, splitPos *indexPosition) matcherInstance {
	if !m.initialized {
		panic("cannot split uninitialized single matcher")
	}
	m2 := &singleMatcherInstance{
		singleMatcher: m.singleMatcher,
		ctx:           m.ctx,
		compare:       m.compare,
		reader:        m.reader,
		logic:         logic,
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
	logic           *logicBuilder
	currentPos      *indexPosition
	lastSectionNode logicNodeID
	isEmpty         bool
}

// newInstance creates a new instance of matchAny.
func (m matchAny) newInstance(ctx context.Context, reader matcherTableReader, logic *logicBuilder, first, last indexPosition) matcherInstance {
	if len(m) == 0 {
		panic("zero length matchAny")
	}
	if len(m) == 1 {
		return m[0].newInstance(ctx, reader, logic, first, last, direction)
	}
	mi := &matchAnyInstance{
		children:  make([]matcherInstance, len(m)),
		logic:     logic,
		direction: direction,
	}
	for i, mm := range m {
		mi.children[i] = mm.newInstance(ctx, reader, logic, first, last, direction)
	}
	return mi
}

// next implements matcherInstance.
func (m *matchAnyInstance) next() (dpos *indexPosition, dlsn logicNodeID, derr error) {
	/*defer func() {
		fmt.Println("matchAnyInstance  pos", dpos, "lsn", dlsn, "err", derr)
	}()*/
	if m.isEmpty || m.currentPos != nil {
		return m.currentPos, m.lastSectionNode, nil
	}
	if m.logic != nil {
		m.lastSectionNode = m.logic.addAndGateNode()
	}
	for _, cm := range m.children {
		pos, node, err := cm.next()
		if err != nil {
			return nil, 0, err
		}
		if m.logic != nil {
			m.logic.connect(node, m.lastSectionNode)
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
func (m *matchAnyInstance) split(logic *logicBuilder, splitPos *indexPosition) matcherInstance {
	c := &matchAnyInstance{
		children:  make([]matcherInstance, len(m.children)),
		logic:     logic,
		direction: m.direction,
	}
	for i, cm := range m.children {
		c.children[i] = cm.split(logic, splitPos)
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
	logic           *logicBuilder
	currentPos      *indexPosition
	lastSectionNode logicNodeID
	isEmpty         bool
}

// newInstance creates a new instance of matchAll.
func (m matchAll) newInstance(ctx context.Context, reader matcherTableReader, logic *logicBuilder, first, last indexPosition) matcherInstance {
	if len(m) == 0 {
		panic("zero length matchAll")
	}
	if len(m) == 1 {
		return m[0].newInstance(ctx, reader, logic, first, last, direction)
	}
	mi := &matchAllInstance{
		children:  make([]matcherInstance, len(m)),
		logic:     logic,
		direction: direction,
	}
	for i, mm := range m {
		mi.children[i] = mm.newInstance(ctx, reader, logic, first, last, direction)
	}
	return mi
}

// next implements matcherInstance.
func (m *matchAllInstance) next() (dpos *indexPosition, dlsn logicNodeID, derr error) {
	/*defer func() {
		fmt.Println("matchAllInstance  pos", dpos, "lsn", dlsn, "err", derr)
	}()*/
	//fmt.Println("matchAllInstance.next()")
	if m.isEmpty || m.currentPos != nil {
		return m.currentPos, m.lastSectionNode, nil
	}
	var andNode logicNodeID
	if m.logic != nil {
		andNode = m.logic.addAndGateNode()
	}
	for {
		match := true
		var (
			next     *indexPosition
			nextNode logicNodeID
		)
		for i, cm := range m.children {
			pos, node, err := cm.next()
			if err != nil {
				return nil, 0, err
			}
			if pos == nil {
				m.isEmpty = true
				var orNode logicNodeID
				if m.logic != nil {
					orNode = m.logic.addOrGateNode()
					m.logic.connect(node, orNode)
					for j := i + 1; j < len(m.children); j++ {
						pos, node, err := cm.next()
						if err != nil {
							return nil, 0, err
						}
						if pos == nil {
							m.logic.connect(node, orNode)
						}
					}
					m.logic.connect(orNode, andNode)
				}
				m.currentPos, m.lastSectionNode = nil, andNode
				return nil, andNode, nil
			}
			//fmt.Println(" child", i, "next()", *pos)
			if i == 0 {
				next, nextNode = pos, node
			} else {
				switch next.compare(pos) {
				case m.direction:
					match = false
				case -m.direction:
					next, nextNode = pos, node
					match = false
				}
			}
		}
		if m.logic != nil {
			m.logic.connect(nextNode, andNode)
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
func (m *matchAllInstance) split(logic *logicBuilder, splitPos *indexPosition) matcherInstance {
	c := &matchAllInstance{
		children:  make([]matcherInstance, len(m.children)),
		logic:     logic,
		direction: m.direction,
	}
	for i, cm := range m.children {
		c.children[i] = cm.split(logic, splitPos)
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
	maxResults     int
	validUntil     uint64
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
		fmt.Println(" range", mp.blockRange, "len(pos)", len(mp.positions), "len(allMatches)", len(mp.allMatches), "validMatches", mp.validMatches,
			"completeUntil", mp.completeUntil, "completeValid", mp.completeValid, "matcherFinished", mp.matcherFinished, "finished", mp.finished, "status", mp.status,
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
	var resCount int
	for mp := ms.first; mp != nil; mp = mp.next { //TODO reverse ???
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
		resCount += mp.validMatches
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
		blockRange: common.NewRange[uint64](ms.first.blockRange.First(), ms.last.blockRange.AfterLast()-ms.first.blockRange.First()), //TODO reverse ???
	}
	var (
		currentProver   *tableProver
		andNode         logicNodeID
		lastResultCount int
	)

	addProcessResults := func(mp *matcherProcess) bool {
		mp.blockDataLock.Lock()
		defer mp.blockDataLock.Unlock()

		//fmt.Println("addProcessResults", mp.tableReader.blockRange(), "len(mp.allMatches)", len(mp.allMatches), "len(mp.sectionNodes)", len(mp.sectionNodes))
		if mp.tableProver != nil && mp.tableProver != currentProver {
			if currentProver != nil {
				res.provers = append(res.provers, currentProver)
			}
			currentProver = mp.tableProver
			andNode = mp.logicBuilder.addAndGateNode()
			lastResultCount = len(res.logs)
			mp.logicBuilder.connect(andNode, mp.logicBuilder.addOutputNode())
		}
		_, lastBlockProven := mp.blockProofs[mp.tableReader.blockRange().Last()]
		for i, log := range mp.allMatches {
			if len(res.logs) == ms.maxResults {
				lastLog := res.logs[ms.maxResults-1]
				trimBlockProofs(mp.blockProofs, lastLog.BlockNumber, uint32(lastLog.TxIndex), mp.session.direction)
				mp.tableProver.addBlockProofs(mp.blockProofs, ms.maxResults-lastResultCount)
				lastResultCount = ms.maxResults
				return lastBlockProven
			}
			mp.logicBuilder.connect(mp.sectionNodes[i], andNode)
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

func trimBlockProofs(blockProofs map[uint64]*blockProof, lastBlock uint64, lastTx uint32) {
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
	indexer      *Indexer
	matcher      matcherInstance
	session      *matcherSession
	tableReader  matcherTableReader
	tableProver  *tableProver
	logicBuilder *logicBuilder
	prev, next   *matcherProcess
	blockRange   common.Range[uint64]

	// accessed by control thread only
	status  int
	pqIndex int

	// accessed only by worker thread while status == mpRunning
	positions                    []indexPosition
	sectionNodes                 []logicNodeID
	completeUntil, completeValid int
	finished, matcherFinished    bool
	err                          error
	runTime                      time.Duration
	started                      mclock.AbsTime

	// accessed by block data delivery thread
	blockDataLock  sync.Mutex
	deliverCh      chan struct{}
	blockInResults map[uint64]int
	allMatches     []*types.Log // including invalid matches where len(log.Topics) < len(query.Topics)
	validMatches   int          // number of valid matches
	blockProofs    map[uint64]*blockProof
	deliveryErr    error

	// atomic flags accessed by both threads during processing
	estimatedResults  uint64 // set by worker thread; MSB is "can split" flag
	cumulativeResults uint64 // set by control thread; MSB is "suspend now" flag
}

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
		remainingResults := uint64(float64(mp.validMatches) * ratio)
		estimatedResults = uint64(mp.validMatches) + remainingResults
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
	return min(lastBlock+uint64(s)+1, mp.blockRange.AfterLast()), true
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
	done, remaining = lastBlock+1-mp.blockRange.First(), mp.blockRange.Last()-lastBlock
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
			for len(mp.positions) > len(mp.allMatches) {
				pos := mp.positions[len(mp.allMatches)]
				if _, ok := mp.blockInResults[pos.blockNumber]; !ok {
					mp.blockInResults[pos.blockNumber] = len(mp.allMatches)
					//fmt.Println("*** getBlockData", pos.blockNumber)
					//fmt.Println(" len(mp.positions)", len(mp.positions), "len(mp.logs)", len(mp.logs), "blockInResults[", pos.blockNumber, "] = ", len(mp.logs))
					requestBlocks = append(requestBlocks, pos.blockNumber)
				}
				mp.allMatches = append(mp.allMatches, nil)
			}
			for mp.completeUntil < len(mp.allMatches) && mp.allMatches[mp.completeUntil] != nil {
				if len(mp.allMatches[mp.completeUntil].Topics) >= mp.session.minTopicCount {
					mp.completeValid++
				}
				mp.completeUntil++
			}
			if mp.matcherFinished && mp.completeUntil == len(mp.allMatches) {
				mp.finished = true
			}
		}
		mp.blockDataLock.Unlock()
		for _, blockNumber := range requestBlocks {
			// start requests outside blockDataLock to avoid wrong locking order
			mp.indexer.getBlockDataLocked(blockNumber, true, true, 0 /*TODO*/, mp.deliverBlockData)
		}
		if suspendNow || cumulativeResults+uint64(mp.completeValid) >= uint64(mp.session.maxResults) {
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
			if req.number == mp.tableReader.blockRange().Last() { //TODO
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
	for ; firstInResults < len(mp.allMatches); firstInResults++ {
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
		if len(l.Topics) >= mp.session.minTopicCount {
			mp.validMatches++
		}
		mp.allMatches[firstInResults] = &types.Log{
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
		// Compute effective blob gas price.
		var blobGasPrice *big.Int
		if header.ExcessBlobGas != nil {
			blobGasPrice = eip4844.CalcBlobFee(params.MainnetChainConfig, header) //TODO chain config
		}
		if err := receipts.DeriveFields(params.MainnetChainConfig, header.Hash(), header.Number.Uint64(), header.Time, header.BaseFee, blobGasPrice, body.Transactions); err != nil { //TODO chain config
			mp.deliveryErr = err
			return
		}
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
	//fmt.Println("DeriveSha")
	types.DeriveSha(receipts, trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		if _, ok := proveHexKeys[string(path)]; ok {
			//fmt.Println(" path", path, "hash", hash, "node", blob)
			bp.receiptsProof[hash] = slices.Clone(blob)
			delete(proveHexKeys, string(path))
		}
	}))
	//fmt.Println("DeriveSha:", rh, "header receipts root:", bp.header.ReceiptHash)
}

func (mp *matcherProcess) split() (*matcherProcess, error) {
	splitAt, ok := mp.getSplitBlock()
	if !ok {
		return nil, nil
	}
	logicBuilder := mp.tableProver.optimizer.newBuilderInstance()
	mp2 := &matcherProcess{
		indexer:        mp.indexer,
		matcher:        mp.matcher.split(logicBuilder, &indexPosition{blockNumber: splitAt}),
		session:        mp.session,
		tableReader:    mp.tableReader,
		tableProver:    mp.tableProver,
		logicBuilder:   logicBuilder,
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
	mp.blockRange.SetAfterLast(splitAt)
	mp2.blockRange.SetFirst(splitAt)
	if mp.session.last == mp {
		mp.session.last = mp2
	}
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
	return pq[i].blockRange.First() < pq[j].blockRange.First()
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

type reverseTableReader *tableReader

func (rt reverseTableReader) blockRange() common.Range[uint64] {
	br := (*tableReader)(rt).blockRange()
	return common.NewRange[uint64](math.MaxUint64-br.Last(), br.Count())
}

func (rt reverseTableReader) seekEntry(target *indexEntry) (uint64, bool, error) {
	t := indexEntry{
		indexValue: target.indexValue,
		indexPosition: indexPosition{
			blockNumber: math.MaxUint64 - target.blockNumber,
			txIndex:     math.MaxUint32 - target.txIndex,
			logIndex:    math.MaxUint32 - target.logIndex,
		},
	}
	pos, found, err := (*tableReader)(rt).seekEntry(&t)
	if err != nil {
		return 0, false, err
	}
	pos = (*tableReader)(rt).entryCount - pos
	if found {
		pos--
	}
	return pos, found, nil
}
