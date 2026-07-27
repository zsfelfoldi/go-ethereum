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
	"math/big"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/logindex"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// matcher defines a general abstraction for any matcher configuration that
// can instantiate a matcherInstance.
type matcher interface {
	newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance
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
	next() (*logindex.IndexPosition, logicNodeID, error)
	advance(*logindex.IndexPosition) error
	split(*logicBuilder, uint64) matcherInstance
}

// singleMatcher implements matcher by returning matches for a single log value hash.
type singleMatcher struct {
	value logindex.IndexValue
	//stats runtimeStats
}

// singleMatcherInstance is an instance of singleMatcher.
type singleMatcherInstance struct {
	*singleMatcher
	ctx                                              context.Context
	compare                                          logindex.IndexEntry // value part is fixed, position part is used for comparisons
	reader                                           *directionalReader
	logic                                            *logicBuilder
	entryPtr                                         uint64
	direction                                        int
	initialized, isEmpty                             bool
	first, last                                      logindex.IndexPosition
	currentPos                                       *logindex.IndexPosition
	lastEntryNode, currentEntryNode, lastSectionNode logicNodeID
}

// newInstance creates a new instance of singleMatcher.
func (m *singleMatcher) newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance {
	mi := &singleMatcherInstance{
		singleMatcher: m,
		ctx:           ctx,
		compare: logindex.IndexEntry{
			logindex.IndexValue: m.value,
		},
		reader: reader,
		logic:  logic,
		first:  first,
		last:   last,
	}
	if firstEntry, firstExists := reader.firstEntry(); firstExists {
		mi.entryPtr = firstEntry
	} else {
		mi.isEmpty = true
	}
	return mi
}

func (m *singleMatcherInstance) init() error {
	m.initialized = true
	if err := m.advance(&m.first); err != nil {
		m.initialized = false
		return err
	}
	return nil
}

// next implements matcherInstance.
func (m *singleMatcherInstance) next() (dpos *logindex.IndexPosition, dlsn logicNodeID, derr error) {
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
		if prevEntry, ok := m.reader.prevNextEntry(m.entryPtr, false); ok {
			if m.lastEntryNode == 0 {
				m.lastEntryNode = m.logic.addInputNode(prevEntry)
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
		if entry.logindex.IndexValue != m.value || m.reader.comparePosition(&entry.logindex.IndexPosition, &m.last) == 1 {
			m.isEmpty = true
			m.currentPos = nil
			return nil, m.lastSectionNode, nil
		}
		m.currentPos = &entry.logindex.IndexPosition
	}
	return m.currentPos, m.lastSectionNode, nil
}

// advance implements matcherInstance.
func (m *singleMatcherInstance) advance(findPos *logindex.IndexPosition) error {
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
		if nextEntry, ok := m.reader.prevNextEntry(m.entryPtr, true); ok {
			m.entryPtr = nextEntry
		} else {
			m.isEmpty = true
		}
		m.lastEntryNode = m.currentEntryNode
		m.currentEntryNode, m.lastSectionNode = 0, 0
		return nil
	}
	// ensure that we are not moving beyond the position range limit
	if m.reader.comparePosition(findPos, &m.last) == 1 {
		findPos = &m.last
	}
	// move to the entry at or beyond findPos
	m.compare.logindex.IndexPosition = *findPos
	newEntryPtr, valid, err := m.reader.entryAtOrAfter(&m.compare)
	if err != nil {
		return err
	}
	oldEntryPtr := m.entryPtr
	nextEntryPtr, nextExists := m.reader.prevNextEntry(m.entryPtr, true)
	if valid {
		m.entryPtr = newEntryPtr
	} else {
		m.isEmpty = true
	}
	switch {
	case m.entryPtr == oldEntryPtr:
	case nextExists && m.entryPtr == nextEntryPtr:
		m.lastEntryNode = m.currentEntryNode
		m.currentEntryNode, m.lastSectionNode = 0, 0
	default:
		m.lastEntryNode, m.currentEntryNode, m.lastSectionNode = 0, 0, 0
	}
	return nil
}

// split implements matcherInstance.
func (m *singleMatcherInstance) split(logic *logicBuilder, splitBlock uint64) matcherInstance {
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
		first:         m.first,
		last:          m.last,
	}
	m.last, m2.first = m.reader.splitBoundaries(splitBlock)
	if m.currentPos != nil && m.reader.comparePosition(m.currentPos, &m.last) > 0 {
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
	reader          *directionalReader
	logic           *logicBuilder
	currentPos      *logindex.IndexPosition
	lastSectionNode logicNodeID
	isEmpty         bool
}

// newInstance creates a new instance of matchAny.
func (m matchAny) newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance {
	if len(m) == 0 {
		panic("zero length matchAny")
	}
	if len(m) == 1 {
		return m[0].newInstance(ctx, reader, logic, first, last)
	}
	mi := &matchAnyInstance{
		children: make([]matcherInstance, len(m)),
		reader:   reader,
		logic:    logic,
	}
	for i, mm := range m {
		mi.children[i] = mm.newInstance(ctx, reader, logic, first, last)
	}
	return mi
}

// next implements matcherInstance.
func (m *matchAnyInstance) next() (dpos *logindex.IndexPosition, dlsn logicNodeID, derr error) {
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
		if pos != nil && (m.currentPos == nil || m.reader.comparePosition(m.currentPos, pos) == 1) {
			m.currentPos = pos
		}
	}
	m.isEmpty = m.currentPos == nil
	return m.currentPos, m.lastSectionNode, nil
}

// advance implements matcherInstance.
func (m *matchAnyInstance) advance(findPos *logindex.IndexPosition) error {
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
func (m *matchAnyInstance) split(logic *logicBuilder, splitBlock uint64) matcherInstance {
	c := &matchAnyInstance{
		children: make([]matcherInstance, len(m.children)),
		reader:   m.reader,
		logic:    logic,
	}
	for i, cm := range m.children {
		c.children[i] = cm.split(logic, splitBlock)
	}
	lastPos, _ := m.reader.splitBoundaries(splitBlock)
	if m.currentPos != nil && m.reader.comparePosition(m.currentPos, &lastPos) > 0 {
		m.currentPos, m.isEmpty = nil, true
	}
	return c
}

type matchAll []matcher

// matchAllInstance is an instance of matchAll.
type matchAllInstance struct {
	children        []matcherInstance
	reader          *directionalReader
	logic           *logicBuilder
	direction       int
	currentPos      *logindex.IndexPosition
	lastSectionNode logicNodeID
	isEmpty         bool
}

// newInstance creates a new instance of matchAll.
func (m matchAll) newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance {
	if len(m) == 0 {
		panic("zero length matchAll")
	}
	if len(m) == 1 {
		return m[0].newInstance(ctx, reader, logic, first, last)
	}
	mi := &matchAllInstance{
		children: make([]matcherInstance, len(m)),
		reader:   reader,
		logic:    logic,
	}
	for i, mm := range m {
		mi.children[i] = mm.newInstance(ctx, reader, logic, first, last)
	}
	return mi
}

// next implements matcherInstance.
func (m *matchAllInstance) next() (dpos *logindex.IndexPosition, dlsn logicNodeID, derr error) {
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
			next     *logindex.IndexPosition
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
				switch m.reader.comparePosition(next, pos) {
				case 1:
					match = false
				case -1:
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
func (m *matchAllInstance) advance(findPos *logindex.IndexPosition) error {
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
func (m *matchAllInstance) split(logic *logicBuilder, splitBlock uint64) matcherInstance {
	c := &matchAllInstance{
		children: make([]matcherInstance, len(m.children)),
		reader:   m.reader,
		logic:    logic,
	}
	for i, cm := range m.children {
		c.children[i] = cm.split(logic, splitBlock)
	}
	lastPos, _ := m.reader.splitBoundaries(splitBlock)
	if m.currentPos != nil && m.reader.comparePosition(m.currentPos, &lastPos) > 0 {
		m.currentPos, m.isEmpty = nil, true
	}
	return c
}

type directionalReader struct {
	reader  *logindex.TableReader
	reverse bool
}

func (dr *directionalReader) firstEntry() (uint64, bool) {
	if dr.reader.entryCount == 0 {
		return 0, false
	}
	if dr.reverse {
		return dr.reader.entryCount - 1, true
	}
	return 0, true
}

func (dr *directionalReader) prevNextEntry(entryIndex uint64, next bool) (uint64, bool) {
	if dr.reverse == next {
		if entryIndex == 0 {
			return 0, false
		}
		return entryIndex - 1, true
	}
	if entryIndex+1 >= dr.reader.entryCount {
		return 0, false
	}
	return entryIndex + 1, true
}

func (dr *directionalReader) entryAtOrAfter(target *logindex.IndexEntry) (uint64, bool, error) {
	pos, found, err := dr.reader.seekEntry(target)
	if err != nil {
		return 0, false, err
	}
	if dr.reverse {
		if !found {
			if pos == 0 {
				return 0, false, nil
			}
			pos--
		}
		return pos, true, nil
	}
	if pos >= dr.reader.entryCount {
		return 0, false, nil
	}
	return pos, true, nil
}

func (dr *directionalReader) splitBoundaries(blockNumber uint64) (before, after logindex.IndexPosition) {
	before.blockNumber = blockNumber
	after.blockNumber = blockNumber
	if dr.reverse {
		after.decrease()
	} else {
		before.decrease()
	}
	return
}

func (dr *directionalReader) comparePosition(a, b *logindex.IndexPosition) int {
	cmp := a.compare(b)
	if dr.reverse {
		return -cmp
	}
	return cmp
}

func (dr *directionalReader) getEntry(index uint64) (*logindex.IndexEntry, error) {
	return dr.reader.getEntry(index)
}
