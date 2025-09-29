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
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/logindex"
	"github.com/ethereum/go-ethereum/core/types"
)

// PatternMatcher defines a general abstraction for any matcher configuration
// that can instantiate a matcherInstance.
type PatternMatcher interface {
	newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance
	matchLogs(logs []*types.Log, index int) bool
	proofMatches(tqp *tableQueryProof, begin, end logindex.IndexPosition) indexPositionSet
	json.Marshaler
}

// matcherInstance defines a general abstraction for a position matcher instance
// that operates on a single index table and iterates over matching index
// positions in the specified range.
type matcherInstance interface {
	next() (*logindex.IndexPosition, logicNodeID, error)
	advance(*logindex.IndexPosition) error
	split(*logicBuilder, uint64) matcherInstance
}

func NewPatternMatcherFromJSON(msg json.RawMessage) (PatternMatcher, error) {
	var p struct {
		Type   *string           `json:"field"`
		Value  *string           `json:"value"`
		Offset *json.RawMessage  `json:"offset"`
		Amount *int              `json:"amount"`
		AnyOf  []json.RawMessage `json:"anyOf"`
		AllOf  []json.RawMessage `json:"allOf"`
	}
	if err := json.Unmarshal(msg, &p); err != nil {
		return nil, err
	}
	if p.Type != nil || p.Value != nil {
		if p.Type == nil {
			return nil, errors.New("entry type missing")
		}
		if p.Value == nil {
			return nil, errors.New("entry value missing")
		}
		if len(p.AnyOf) != 0 {
			return nil, errors.New("both type/value and anyOf fields specified")
		}
		if len(p.AllOf) != 0 {
			return nil, errors.New("both type/value and allOf fields specified")
		}
		if p.Offset != nil {
			return nil, errors.New("both type/value and offset fields specified")
		}
		if p.Amount != nil {
			return nil, errors.New("both type/value and amount fields specified")
		}
		value, err := hexutil.Decode(*p.Value)
		if err != nil {
			return nil, errors.New("invalid entry value hex string")
		}
		if len(value) > 32 {
			return nil, errors.New("entry value is longer than 32 bytes")
		}
		sm := new(singleMatcher)
		copy(sm.value.Value[32-len(value):], value)
		switch {
		case *p.Type == "block":
			sm.value.EntryType = logindex.IeBlock
		case *p.Type == "transaction":
			sm.value.EntryType = logindex.IeTransaction
		case *p.Type == "address":
			sm.value.EntryType = logindex.IeAddress
		case len(*p.Type) > 8 && (*p.Type)[:7] == "topics[" && (*p.Type)[len(*p.Type)-1:] == "]":
			topicIndex, err := strconv.Atoi((*p.Type)[7 : len(*p.Type)-1])
			if err != nil || topicIndex < 0 || topicIndex >= logindex.MaxTopicCount {
				return nil, errors.New("invalid topic index")
			}
			sm.value.EntryType = logindex.IeTopic0 + uint32(topicIndex)
		default:
			return nil, errors.New("unknown entry type")
		}
		return sm, nil
	}
	if p.Offset != nil || p.Amount != nil {
		if p.Offset == nil {
			return nil, errors.New("offset type missing")
		}
		if p.Amount == nil {
			return nil, errors.New("amount value missing")
		}
		if len(p.AnyOf) != 0 {
			return nil, errors.New("both type/value and anyOf fields specified")
		}
		if len(p.AllOf) != 0 {
			return nil, errors.New("both type/value and allOf fields specified")
		}
		child, err := NewPatternMatcherFromJSON(*p.Offset)
		if err != nil {
			return nil, err
		}
		return &offsetMatcher{
			child:  child,
			offset: *p.Amount,
		}, nil
	}
	var err error
	if len(p.AnyOf) != 0 {
		if len(p.AllOf) != 0 {
			return nil, errors.New("both anyOf and allOf fields specified")
		}
		m := make(matchAny, len(p.AnyOf))
		for i, msg := range p.AnyOf {
			if m[i], err = NewPatternMatcherFromJSON(msg); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	if len(p.AllOf) != 0 {
		m := make(matchAll, len(p.AllOf))
		for i, msg := range p.AllOf {
			if m[i], err = NewPatternMatcherFromJSON(msg); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	return nil, errors.New("empty pattern matcher is not allowed")
}

func NewLegacyMatcher(addresses []common.Address, topics [][]common.Hash) (PatternMatcher, error) {
	// build matcher according to the given filter criteria
	matcher := make(matchAll, 0, len(topics)+1)
	// matchAddress signals a match when there is a match for any of the given
	// addresses.
	// If the list of addresses is empty then it creates a "wild card" matcher
	// that signals every index as a potential match.
	if len(addresses) > 0 {
		matchAddress := make(matchAny, len(addresses))
		for i, address := range addresses {
			var addr32 [32]byte
			copy(addr32[32-common.AddressLength:], address[:])
			matchAddress[i] = &singleMatcher{value: logindex.IndexValue{EntryType: logindex.IeAddress, Value: addr32}}
		}
		matcher = append(matcher, matchAddress)
	}
	for i, topicList := range topics {
		// matchTopic signals a match when there is a match for any of the topics
		// specified for the given position (topicList).
		// If topicList is empty then it creates a "wild card" matcher that signals
		// every index as a potential match.
		if len(topicList) > 0 {
			matchTopic := make(matchAny, len(topicList))
			for j, topic := range topicList {
				matchTopic[j] = &singleMatcher{value: logindex.IndexValue{EntryType: logindex.IeTopic0 + uint32(i), Value: ([32]byte)(topic)}}
			}
			matcher = append(matcher, matchTopic)
		}
	}
	if len(matcher) > 0 {
		return matcher, nil
	}
	return nil, ErrMatchAll
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
			IndexValue: m.value,
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

func (m *singleMatcher) matchLogs(logs []*types.Log, index int) bool {
	if index < 0 || index >= len(logs) {
		return false
	}
	log := logs[index]
	switch {
	case m.value.EntryType == logindex.IeAddress:
		return slices.Equal(log.Address[:], m.value.Value[len(m.value.Value)-common.AddressLength:])
	case m.value.EntryType >= logindex.IeTopic0 && m.value.EntryType < logindex.IeTopic0+logindex.MaxTopicCount:
		i := int(m.value.EntryType - logindex.IeTopic0)
		return len(log.Topics) > i && log.Topics[i] == m.value.Value
	default:
		panic("matchLogs only allowed for address/topic matchers")
	}
}

func (m *singleMatcher) proofMatches(tqp *tableQueryProof, begin, end logindex.IndexPosition) indexPositionSet {
	return tqp.getValueMatches(m.value, begin, end)
}

func (m *singleMatcher) MarshalJSON() ([]byte, error) {
	enc := struct {
		Type  string      `json:"field"`
		Value common.Hash `json:"value"`
	}{
		Value: m.value.Value,
	}
	switch {
	case m.value.EntryType == logindex.IeBlock:
		enc.Type = "block"
	case m.value.EntryType == logindex.IeTransaction:
		enc.Type = "transaction"
	case m.value.EntryType == logindex.IeAddress:
		enc.Type = "address"
	case m.value.EntryType >= logindex.IeTopic0 && m.value.EntryType < logindex.IeTopic0+logindex.MaxTopicCount:
		enc.Type = "topics[" + strconv.Itoa(int(m.value.EntryType-logindex.IeTopic0)) + "]"
	default:
		enc.Type = "unknown"
	}
	return json.Marshal(enc)
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
		if entry.IndexValue != m.value || m.reader.comparePosition(&entry.IndexPosition, &m.last) == 1 {
			m.isEmpty = true
			m.currentPos = nil
			return nil, m.lastSectionNode, nil
		}
		m.currentPos = &entry.IndexPosition
	}
	//TODO offset
	return m.currentPos, m.lastSectionNode, nil
}

// advance implements matcherInstance.
func (m *singleMatcherInstance) advance(findPos *logindex.IndexPosition) error {
	//TODO offset (findPos)
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
	m.compare.IndexPosition = *findPos
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

type offsetMatcher struct {
	child  PatternMatcher
	offset int
}

// offsetMatcherInstance is an instance of offsetMatcher.
type offsetMatcherInstance struct {
	childInstance   matcherInstance
	logic           *logicBuilder
	offset          int
	currentPos      *logindex.IndexPosition
	isEmpty         bool
	lastSectionNode logicNodeID
}

func addOffset(pos *logindex.IndexPosition, offset int) bool {
	logIndex := int64(pos.LogIndex) + int64(offset)
	if logIndex < 0 {
		pos.LogIndex = 0
		return true
	}
	if logIndex > math.MaxUint32 {
		pos.LogIndex = math.MaxUint32
		return true
	}
	pos.LogIndex = uint32(logIndex)
	return false
}

// newInstance creates a new instance of singleMatcher.
func (m *offsetMatcher) newInstance(ctx context.Context, reader *directionalReader, logic *logicBuilder, first, last logindex.IndexPosition) matcherInstance {
	addOffset(&first, m.offset)
	addOffset(&last, m.offset)
	return &offsetMatcherInstance{
		childInstance: m.child.newInstance(ctx, reader, logic, first, last),
		logic:         logic,
		offset:        m.offset,
	}
}

func (m *offsetMatcher) matchLogs(logs []*types.Log, index int) bool {
	index += m.offset
	if index < 0 || index >= len(logs) {
		return false
	}
	return m.child.matchLogs(logs, index)
}

func (m *offsetMatcher) proofMatches(tqp *tableQueryProof, begin, end logindex.IndexPosition) indexPositionSet {
	addOffset(&begin, m.offset)
	addOffset(&end, m.offset)
	ips := m.child.proofMatches(tqp, begin, end)
	for i := range ips {
		addOffset(&ips[i].p, -m.offset)
	}
	return ips.filter(1)
}

func (m *offsetMatcher) MarshalJSON() ([]byte, error) {
	enc := struct {
		Offset PatternMatcher `json:"offset"`
		Amount int            `json:"amount"`
	}{
		Offset: m.child,
		Amount: m.offset,
	}
	return json.Marshal(enc)
}

// next implements matcherInstance.
func (m *offsetMatcherInstance) next() (dpos *logindex.IndexPosition, dlsn logicNodeID, derr error) {
	if m.isEmpty {
		return nil, 0, nil
	}
	if m.currentPos == nil {
	loop:
		for {
			pos, lsn, err := m.childInstance.next()
			if err != nil {
				return nil, 0, err
			}
			if pos == nil {
				m.isEmpty = true
				return nil, 0, nil
			}
			m.currentPos = new(logindex.IndexPosition)
			*m.currentPos = *pos
			if m.lastSectionNode == 0 {
				m.lastSectionNode = lsn
			} else {
				andNode := m.logic.addAndGateNode()
				m.logic.connect(m.lastSectionNode, andNode)
				m.logic.connect(lsn, andNode)
				m.lastSectionNode = andNode
			}
			if !addOffset(m.currentPos, -m.offset) {
				break loop
			}
			if err := m.childInstance.advance(nil); err != nil {
				return nil, 0, err
			}
		}
	}
	return m.currentPos, m.lastSectionNode, nil
}

// advance implements matcherInstance.
func (m *offsetMatcherInstance) advance(findPos *logindex.IndexPosition) error {
	if m.isEmpty {
		return nil
	}
	m.currentPos, m.lastSectionNode = nil, 0
	if findPos == nil {
		return m.childInstance.advance(nil)
	}
	fpos := new(logindex.IndexPosition)
	*fpos = *findPos
	addOffset(fpos, m.offset)
	return m.childInstance.advance(fpos)
}

// split implements matcherInstance.
func (m *offsetMatcherInstance) split(logic *logicBuilder, splitBlock uint64) matcherInstance {
	return m.childInstance.split(logic, splitBlock)
}

// matchAny combinines a set of matchers and returns a match for every position
// where any of the underlying matchers signaled a match. A zero-length matchAny
// acts as a "wild card" that signals a potential match at every position.
type matchAny []PatternMatcher

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

func (m matchAny) matchLogs(logs []*types.Log, index int) bool {
	for _, mm := range m {
		if mm.matchLogs(logs, index) {
			return true
		}
	}
	return false
}

func (m matchAny) proofMatches(tqp *tableQueryProof, begin, end logindex.IndexPosition) indexPositionSet {
	childResults := make([]indexPositionSet, len(m))
	for i, mm := range m {
		childResults[i] = mm.proofMatches(tqp, begin, end)
	}
	return ipsUnion(childResults)
}

func (m matchAny) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AnyOf []PatternMatcher `json:"anyOf"`
	}{m})
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

type matchAll []PatternMatcher

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

func (m matchAll) matchLogs(logs []*types.Log, index int) bool {
	for _, mm := range m {
		if !mm.matchLogs(logs, index) {
			return false
		}
	}
	return true
}

func (m matchAll) proofMatches(tqp *tableQueryProof, begin, end logindex.IndexPosition) indexPositionSet {
	childResults := make([]indexPositionSet, len(m))
	for i, mm := range m {
		childResults[i] = mm.proofMatches(tqp, begin, end)
	}
	return ipsIntersection(childResults)
}

func (m matchAll) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AllOf []PatternMatcher `json:"allOf"`
	}{m})
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
	if dr.reader.EntryCount == 0 {
		return 0, false
	}
	if dr.reverse {
		return dr.reader.EntryCount - 1, true
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
	if entryIndex+1 >= dr.reader.EntryCount {
		return 0, false
	}
	return entryIndex + 1, true
}

func (dr *directionalReader) entryAtOrAfter(target *logindex.IndexEntry) (uint64, bool, error) {
	pos, found, err := dr.reader.SeekEntry(target)
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
	if pos >= dr.reader.EntryCount {
		return 0, false, nil
	}
	return pos, true, nil
}

func (dr *directionalReader) splitBoundaries(blockNumber uint64) (before, after logindex.IndexPosition) {
	before.BlockNumber = blockNumber
	after.BlockNumber = blockNumber
	if dr.reverse {
		after.Decrease()
	} else {
		before.Decrease()
	}
	return
}

func (dr *directionalReader) comparePosition(a, b *logindex.IndexPosition) int {
	cmp := a.Compare(b)
	if dr.reverse {
		return -cmp
	}
	return cmp
}

func (dr *directionalReader) getEntry(index uint64) (*logindex.IndexEntry, error) {
	return dr.reader.GetEntry(index)
}
