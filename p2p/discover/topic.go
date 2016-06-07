// Copyright 2015 The go-ethereum Authors
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

// Package discover implements the Node Discovery Protocol.
//
// The Node Discovery protocol provides a way to find RLPx nodes that
// can be connected to. It uses a Kademlia-like protocol to maintain a
// distributed database of the IDs and endpoints of all listening
// nodes.
package discover

import (
	"container/heap"
	"math"
	"math/rand"
	"time"
)

const (
	MaxEntries         = 10000
	MaxEntriesPerTopic = 50

	fallbackRegistrationExpiry = 1 * time.Hour
)

type Topic string

type topicEntry struct {
	topic   Topic
	fifoIdx uint64
	node    *Node
	expire  absTime
}

type topicInfo struct {
	entries            map[uint64]*topicEntry
	fifoHead, fifoTail uint64
	rqItem             *topicRequestQueueItem
	wcl                waitControlLoop
}

// removes tail element from the fifo
func (t *topicInfo) getFifoTail() *topicEntry {
	for t.entries[t.fifoTail] == nil {
		t.fifoTail++
	}
	tail := t.entries[t.fifoTail]
	t.fifoTail++
	return tail
}

type nodeInfo struct {
	entries                          map[Topic]*topicEntry
	lastIssuedTicket, lastUsedTicket uint32
	// you can't register a ticket newer than lastUsedTicket before noRegUntil (absolute time)
	noRegUntil absTime
}

type TopicTable struct {
	db                    *nodeDB
	nodes                 map[*Node]*nodeInfo
	topics                map[Topic]*topicInfo
	globalEntries         uint64
	requested             topicRequestQueue
	requestCnt            uint64
	lastGarbageCollection absTime
}

func NewTopicTable(db *nodeDB) *TopicTable {
	return &TopicTable{
		db:     db,
		nodes:  make(map[*Node]*nodeInfo),
		topics: make(map[Topic]*topicInfo),
	}
}

func (t *TopicTable) getOrNewTopic(topic Topic) *topicInfo {
	ti := t.topics[topic]
	if ti == nil {
		rqItem := &topicRequestQueueItem{
			topic:    topic,
			priority: t.requestCnt,
		}
		ti = &topicInfo{
			entries: make(map[uint64]*topicEntry),
			rqItem:  rqItem,
		}
		t.topics[topic] = ti
		heap.Push(&t.requested, rqItem)
	}
	return ti
}

// This function assumes that topic is in the topics table.
func (t *TopicTable) checkDeleteTopic(topic Topic) {
	ti := t.topics[topic]
	if len(ti.entries) == 0 && ti.wcl.hasMinimumWaitPeriod() {
		delete(t.topics, topic)
	}
}

func (t *TopicTable) getOrNewNode(node *Node) *nodeInfo {
	n := t.nodes[node]
	if n == nil {
		issued, used := t.db.fetchTopicRegTickets(node.ID)
		n = &nodeInfo{
			entries:          make(map[Topic]*topicEntry),
			lastIssuedTicket: issued,
			lastUsedTicket:   used,
		}
		t.nodes[node] = n
	}
	return n
}

// This function assumes that node is in the nodes table.
func (t *TopicTable) checkDeleteNode(node *Node) {
	n := t.nodes[node]
	if len(n.entries) == 0 && n.noRegUntil < monotonicTime() {
		delete(t.nodes, node)
	}
}

func (t *TopicTable) storeTicketCounters(node *Node) {
	n := t.getOrNewNode(node)
	t.db.updateTopicRegTickets(node.ID, n.lastIssuedTicket, n.lastUsedTicket)
}

func (t *TopicTable) GetEntries(topic Topic) []*Node {
	t.collectGarbage()

	te := t.topics[topic]
	if te == nil {
		return nil
	}
	nodes := make([]*Node, len(te.entries))
	i := 0
	for _, e := range te.entries {
		nodes[i] = e.node
		i++
	}
	t.requestCnt++
	t.requested.update(te.rqItem, t.requestCnt)
	return nodes
}

func (t *TopicTable) AddEntries(node *Node, topics []Topic) {
	n := t.getOrNewNode(node)
	// clear previous entries by the same node
	for _, e := range n.entries {
		t.deleteEntry(e)
	}

	tm := monotonicTime()
	for _, topic := range topics {
		te := t.getOrNewTopic(topic)

		if len(te.entries) == MaxEntriesPerTopic {
			t.deleteEntry(te.getFifoTail())
		}

		if t.globalEntries == MaxEntries {
			t.deleteEntry(t.leastRequested()) // not empty, no need to check for nil
		}

		fifoIdx := te.fifoHead
		te.fifoHead++
		entry := &topicEntry{
			topic:   topic,
			fifoIdx: fifoIdx,
			node:    node,
			expire:  tm + absTime(fallbackRegistrationExpiry),
		}
		te.entries[fifoIdx] = entry
		n.entries[topic] = entry
		t.globalEntries++
		te.wcl.registered(tm)
	}
}

// removes least requested element from the fifo
func (t *TopicTable) leastRequested() *topicEntry {
	for t.requested.Len() > 0 && t.topics[t.requested[0].topic] == nil {
		heap.Pop(&t.requested)
	}
	if t.requested.Len() == 0 {
		return nil
	}
	return t.topics[t.requested[0].topic].getFifoTail()
}

// entry should exist
func (t *TopicTable) deleteEntry(e *topicEntry) {
	ne := t.nodes[e.node].entries
	delete(ne, e.topic)
	if len(ne) == 0 {
		t.checkDeleteNode(e.node)
	}
	te := t.topics[e.topic]
	delete(te.entries, e.fifoIdx)
	if len(te.entries) == 0 {
		t.checkDeleteTopic(e.topic)
		heap.Remove(&t.requested, te.rqItem.index)
	}
	t.globalEntries--
}

// It is assumed that topics and waitPeriods have the same length.
func (t *TopicTable) useTicket(node *Node, serialNo uint32, topics []Topic, waitPeriods []uint32) (registered bool) {
	t.collectGarbage()

	n := t.getOrNewNode(node)
	if serialNo < n.lastUsedTicket {
		return false
	}

	tm := monotonicTime()
	if serialNo > n.lastUsedTicket && tm < n.noRegUntil {
		return false
	}
	if serialNo != n.lastUsedTicket {
		n.lastUsedTicket = serialNo
		n.noRegUntil = tm + absTime(noRegTimeout())
		t.storeTicketCounters(node)
	}

	currTime := uint32(tm / absTime(time.Second))
	var regTopics []Topic
	for i, w := range waitPeriods {
		relTime := int32(currTime - w)                    // make it safe even if time turns around
		if relTime >= -1 && relTime <= regTimeWindow+1 && // give clients a little security margin on both ends
			n.entries[topics[i]] == nil { // don't register again if there is an active entry
			regTopics = append(regTopics, topics[i])
		}
	}
	if regTopics != nil {
		t.AddEntries(node, regTopics)
		return true
	} else {
		return false
	}
}

func (topictab *TopicTable) getTicket(node *Node, topics []Topic) *ticket {
	topictab.collectGarbage()

	now := monotonicTime()
	n := topictab.getOrNewNode(node)
	n.lastIssuedTicket++
	topictab.storeTicketCounters(node)

	t := &ticket{
		issueTime: now,
		topics:    topics,
		serial:    n.lastIssuedTicket,
		regTime:   make([]absTime, len(topics)),
	}
	for i, topic := range topics {
		var waitPeriod time.Duration
		if topic := topictab.topics[topic]; topic != nil {
			waitPeriod = topic.wcl.waitPeriod
		} else {
			waitPeriod = minWaitPeriod
		}
		// If the node is within the no registration timeout,
		// we lengthen the wait periods in the ticket accordingly.
		if now+absTime(waitPeriod) < n.noRegUntil {
			waitPeriod = time.Duration(n.noRegUntil - now)
		}
		// Round up so the registrant doesn't come in to early.
		waitPeriod += time.Second

		t.regTime[i] = now + absTime(waitPeriod)
	}
	return t
}

const gcInterval = time.Minute

func (t *TopicTable) collectGarbage() {
	tm := monotonicTime()
	if time.Duration(tm-t.lastGarbageCollection) < gcInterval {
		return
	}
	t.lastGarbageCollection = tm

	for node, n := range t.nodes {
		for _, e := range n.entries {
			if e.expire <= tm {
				t.deleteEntry(e)
			}
		}

		t.checkDeleteNode(node)
	}

	for topic, _ := range t.topics {
		t.checkDeleteTopic(topic)
	}
}

const (
	minWaitPeriod   = time.Minute
	regTimeWindow   = 10 // seconds
	avgnoRegTimeout = time.Minute * 10
	// target average interval between two incoming ad requests
	wcTargetRegInterval = time.Minute * 10 / MaxEntriesPerTopic
	//
	wcTimeConst = time.Minute * 10
)

// initialization is not required, will set to minWaitPeriod at first registration
type waitControlLoop struct {
	lastIncoming absTime
	waitPeriod   time.Duration
}

func (w *waitControlLoop) registered(tm absTime) {
	w.waitPeriod = w.nextWaitPeriod(tm)
	w.lastIncoming = tm
}

func (w *waitControlLoop) nextWaitPeriod(tm absTime) time.Duration {
	period := tm - w.lastIncoming
	wp := time.Duration(float64(w.waitPeriod) * math.Exp((float64(wcTargetRegInterval)-float64(period))/float64(wcTimeConst)))
	if wp < minWaitPeriod {
		wp = minWaitPeriod
	}
	return wp
}

func (w *waitControlLoop) hasMinimumWaitPeriod() bool {
	return w.nextWaitPeriod(monotonicTime()) == minWaitPeriod
}

func noRegTimeout() time.Duration {
	e := rand.ExpFloat64()
	if e > 100 {
		e = 100
	}
	return time.Duration(float64(avgnoRegTimeout) * e)
}

type topicRequestQueueItem struct {
	topic    Topic
	priority uint64
	index    int
}

// A topicRequestQueue implements heap.Interface and holds topicRequestQueueItems.
type topicRequestQueue []*topicRequestQueueItem

func (tq topicRequestQueue) Len() int { return len(tq) }

func (tq topicRequestQueue) Less(i, j int) bool {
	return tq[i].priority < tq[j].priority
}

func (tq topicRequestQueue) Swap(i, j int) {
	tq[i], tq[j] = tq[j], tq[i]
	tq[i].index = i
	tq[j].index = j
}

func (tq *topicRequestQueue) Push(x interface{}) {
	n := len(*tq)
	item := x.(*topicRequestQueueItem)
	item.index = n
	*tq = append(*tq, item)
}

func (tq *topicRequestQueue) Pop() interface{} {
	old := *tq
	n := len(old)
	item := old[n-1]
	item.index = -1
	*tq = old[0 : n-1]
	return item
}

func (tq *topicRequestQueue) update(item *topicRequestQueueItem, priority uint64) {
	item.priority = priority
	heap.Fix(tq, item.index)
}
