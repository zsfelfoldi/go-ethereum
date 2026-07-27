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
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common/lru"
)

const (
	nodeTypeShift = 62
	nodeTypeMask  = uint64(3) << nodeTypeShift

	ntInput   = 0
	ntAndGate = 1
	ntOrGate  = 2
	ntOutput  = 3

	logicStateShift = 60
	logicStateMask  = uint64(3) << logicStateShift

	lsAssumedFalse = 0
	lsAssumedTrue  = 1
	lsDecidedFalse = 2
	lsDecidedTrue  = 3

	nodeValueMask = (uint64(1) << logicStateShift) - 1
)

type logicOptimizer struct {
	lock         sync.Mutex
	nodeChunks   []*logicNodeChunk
	finalizeHeap finalizeHeap
}

type logicNode struct {
	typeStateValue uint64
	output         [maxOutputCount]logicNodeID
}

type logicNodeID uint32

type logicNodeChunk struct {
	nodes        [nodeChunkSize]logicNode
	index, count uint32
}

type logicBuilder struct {
	optimizer    *logicOptimizer
	cache        *lru.Cache[uint32, *logicNodeChunk]
	currentChunk *logicNodeChunk
}

type finalizeHeap struct {
	getNode                       func(logicNodeID) *logicNode
	savedInputCost                func(a, b, c uint64) int
	logicNodes                    []logicNodeID
	prevNode, nextNode, heapOrder []uint32 // indices of logicNodes //TODO struct?
	heapIndex                     []uint32 // indices of heapOrder
	fixedCostEntry                uint32   //TODO is this necessary?
	fixedCost                     int
}

func (lo *logicOptimizer) newBuilderInstance() *logicBuilder {
	return &logicBuilder{
		optimizer: lo,
		cache:     lru.NewCache[uint32, *logicNodeChunk](10),
	}
}

func (lo *logicOptimizer) hasNodes() bool {
	return len(lo.nodeChunks) != 0
}

func (lo *logicOptimizer) newChunk() *logicNodeChunk {
	lo.lock.Lock()
	defer lo.lock.Unlock()

	chunkIndex := uint32(len(lo.nodeChunks))
	lo.nodeChunks = append(lo.nodeChunks, &logicNodeChunk{
		index: chunkIndex,
	})
	return lo.nodeChunks[chunkIndex]
}

func (lo *logicOptimizer) getChunk(index uint32) *logicNodeChunk {
	lo.lock.Lock()
	defer lo.lock.Unlock()

	return lo.nodeChunks[index]
}

func (lo *logicOptimizer) getNode(node logicNodeID) *logicNode {
	if node == 0 {
		return nil
	}
	return &lo.nodeChunks[(node-1)/nodeChunkSize].nodes[(node-1)%nodeChunkSize]
}

func (lo *logicOptimizer) optimize(savedInputCost func(a, b, c uint64) int) ([]uint64, error) {
	var (
		inputCount, nodeCount int
		outputNode            *logicNode
	)
	for _, chunk := range lo.nodeChunks {
		nodeCount += int(chunk.count)
		for i := range chunk.count {
			switch chunk.nodes[i].nodeType() {
			case ntInput:
				inputCount++
			case ntOutput:
				if outputNode != nil {
					return nil, errors.New("more than one output node found")
				}
				outputNode = &chunk.nodes[i]
			}
		}
	}
	if outputNode == nil {
		return nil, errors.New("no output node found")
	}
	logicNodes := make([]logicNodeID, inputCount)
	var nodePtr int
	for _, chunk := range lo.nodeChunks {
		for i := range chunk.count {
			if chunk.nodes[i].nodeType() == ntInput {
				logicNodes[nodePtr] = logicNodeID(chunk.index*nodeChunkSize) + 1 + logicNodeID(i)
				nodePtr++
			}
		}
	}
	sort.Slice(logicNodes, func(i, j int) bool {
		return lo.getNode(logicNodes[i]).nodeValue() < lo.getNode(logicNodes[j]).nodeValue()
	})
	lb := lo.newBuilderInstance()
	var j int
	for _, node := range logicNodes {
		if j > 0 && lo.getNode(logicNodes[j-1]).nodeValue() == lo.getNode(node).nodeValue() {
			lb.mergeLogicNodes(logicNodes[j-1], node)
		} else {
			logicNodes[j] = node
			j++
		}
	}
	lb = nil
	logicNodes = logicNodes[:j]
	inputCount = j
	lo.finalizeHeap = finalizeHeap{
		getNode:        lo.getNode,
		savedInputCost: savedInputCost,
		logicNodes:     logicNodes,                      // ntInput logic nodes sorted by input ID
		prevNode:       make([]uint32, len(logicNodes)), // indices of logicNodes (sortedIndex)
		nextNode:       make([]uint32, len(logicNodes)), // indices of logicNodes (sortedIndex)
		heapOrder:      make([]uint32, len(logicNodes)), // indices of logicNodes (sortedIndex)
		heapIndex:      make([]uint32, len(logicNodes)), // indices of heapOrder
	}
	for i := range logicNodes {
		if i > 0 {
			lo.finalizeHeap.prevNode[i] = uint32(i - 1)
		} else {
			lo.finalizeHeap.prevNode[i] = math.MaxUint32
		}
		if i < len(logicNodes)-1 {
			lo.finalizeHeap.nextNode[i] = uint32(i + 1)
		} else {
			lo.finalizeHeap.nextNode[i] = math.MaxUint32
		}
		lo.finalizeHeap.heapOrder[i] = uint32(i)
		lo.finalizeHeap.heapIndex[i] = uint32(i)
	}
	heap.Init(&lo.finalizeHeap)
	for lo.finalizeHeap.Len() != 0 {
		sortedIndex := heap.Pop(&lo.finalizeHeap).(uint32)
		//fmt.Println("heap.Pop", sortedIndex)
		//lo.finalizeHeap.print()
		entryNode := lo.finalizeHeap.logicNodes[sortedIndex]
		switch outputNode.logicState() {
		case lsDecidedTrue:
			lo.traverse(tfSetFalse, entryNode)
			lo.finalizeHeap.removedEntry(sortedIndex)
			inputCount--
		case lsAssumedTrue:
			lo.traverse(tfTrySetFalse, entryNode)
			switch outputNode.logicState() {
			case lsAssumedTrue:
				lo.traverse(tfConfirmSetFalse, entryNode)
				lo.finalizeHeap.removedEntry(sortedIndex)
				inputCount--
			case lsAssumedFalse:
				lo.traverse(tfRevertSetFalse, entryNode)
				lo.traverse(tfSetTrue, entryNode)
			default:
				panic("unexpected logic state for final result node after tfTrySetFalse")
			}
		default:
			panic("unexpected logic state for final result node")
		}
	}
	if outputNode.logicState() != lsDecidedTrue {
		panic("invalid final result logic state")
	}
	inputs := make([]uint64, 0, len(lo.finalizeHeap.logicNodes))
	for _, nodeID := range lo.finalizeHeap.logicNodes {
		if nodeID.isValid() {
			inputs = append(inputs, lo.getNode(nodeID).nodeValue())
		}
	}
	lo.finalizeHeap = finalizeHeap{}
	lo.nodeChunks = nil
	return inputs, nil
}

const (
	taNone = iota
	taPropagate
	taStop
)

func (lo *logicOptimizer) traverse(traverseFn func(*logicNode) int, node logicNodeID) bool {
	n := lo.getNode(node)
	switch traverseFn(n) {
	case taNone:
	case taPropagate:
		for i := range n.outputCount() {
			if lo.traverse(traverseFn, n.output[i]) {
				return true
			}
		}
	case taStop:
		return true
	default:
		panic("invalid traverse action")
	}
	return false
}

func tfSetFalse(n *logicNode) int {
	if n.logicState() != lsAssumedTrue {
		return taNone
	}
	switch n.nodeType() {
	case ntInput, ntAndGate:
		n.setLogicState(lsDecidedFalse)
		return taPropagate
	case ntOrGate:
		value := n.nodeValue()
		if value == 0 {
			panic("assumed true input count below zero")
		}
		value--
		n.setNodeValue(value)
		if value == 0 {
			n.setLogicState(lsDecidedFalse)
			return taPropagate
		}
		return taNone
	case ntOutput:
		panic("cannot set final result node to decided false state")
	default:
		panic("invalid node type")
	}
}

func tfTrySetFalse(n *logicNode) int {
	if n.logicState() != lsAssumedTrue {
		return taNone
	}
	switch n.nodeType() {
	case ntInput, ntAndGate:
		n.setLogicState(lsAssumedFalse)
		return taPropagate
	case ntOrGate:
		value := n.nodeValue()
		if value == 0 {
			panic("assumed true input count below zero")
		}
		value--
		n.setNodeValue(value)
		if value == 0 {
			n.setLogicState(lsAssumedFalse)
			return taPropagate
		}
		return taNone
	case ntOutput:
		n.setLogicState(lsAssumedFalse)
		return taStop
	default:
		panic("invalid node type")
	}
}

func tfConfirmSetFalse(n *logicNode) int {
	if n.logicState() != lsAssumedFalse {
		return taNone
	}
	switch n.nodeType() {
	case ntInput, ntAndGate, ntOrGate:
		n.setLogicState(lsDecidedFalse)
		return taPropagate
	case ntOutput:
		panic("cannot set final result node to decided false state")
	default:
		panic("invalid node type")
	}
}

func tfRevertSetFalse(n *logicNode) int {
	switch n.nodeType() {
	case ntInput, ntAndGate:
		if n.logicState() == lsAssumedFalse {
			n.setLogicState(lsAssumedTrue)
			return taPropagate
		}
		return taNone
	case ntOrGate:
		n.setNodeValue(n.nodeValue() + 1)
		if n.logicState() == lsAssumedFalse {
			n.setLogicState(lsAssumedTrue)
			return taPropagate
		}
		return taNone
	case ntOutput:
		if n.logicState() == lsAssumedFalse {
			n.setLogicState(lsAssumedTrue)
		}
		return taNone
	default:
		panic("invalid node type")
	}
}

func tfSetTrue(n *logicNode) int {
	if n.logicState() != lsAssumedTrue {
		return taNone
	}
	switch n.nodeType() {
	case ntInput, ntOrGate:
		n.setLogicState(lsDecidedTrue)
		return taPropagate
	case ntAndGate:
		value := n.nodeValue()
		if value == 0 {
			panic("assumed true input count below zero")
		}
		value--
		n.setNodeValue(value)
		if value == 0 {
			n.setLogicState(lsDecidedTrue)
			return taPropagate
		}
		return taNone
	case ntOutput:
		n.setLogicState(lsDecidedTrue)
		return taNone
	default:
		panic("invalid node type")
	}
}

func (lb *logicBuilder) mergeLogicNodes(node, node2 logicNodeID) {
	n := lb.getNode(node)
	n2 := lb.getNode(node2)
	count := n.outputCount()
	count2 := n2.outputCount()
	if count+count2 <= maxOutputCount {
		copy(n.output[count:count+count2], n2.output[:count2])
	} else {
		node3 := lb.addOrGateNode()
		n3 := lb.getNode(node3)
		n3.output = n.output
		node4 := lb.addOrGateNode()
		n4 := lb.getNode(node4)
		n4.output = n2.output
		for i := range n.output {
			n.output[i] = 0
		}
		lb.connect(node, node3)
		lb.connect(node, node4)
	}
}

func (lb *logicBuilder) getNode(node logicNodeID) *logicNode {
	if node == 0 {
		return nil
	}
	chunkIndex := uint32(node-1) / nodeChunkSize
	chunk, ok := lb.cache.Get(chunkIndex)
	if !ok {
		chunk = lb.optimizer.getChunk(chunkIndex)
		lb.cache.Add(chunkIndex, chunk)
	}
	return &chunk.nodes[(node-1)%nodeChunkSize]
}

func (lb *logicBuilder) addInputNode(inputID uint64) logicNodeID {
	if inputID >= nodeValueMask {
		panic("invalid entry index")
	}
	return lb.addNode(ntInput, inputID)
}

func (lb *logicBuilder) addAndGateNode() logicNodeID {
	return lb.addNode(ntAndGate, 0)
}

func (lb *logicBuilder) addOrGateNode() logicNodeID {
	return lb.addNode(ntOrGate, 0)
}

func (lb *logicBuilder) addOutputNode() logicNodeID {
	return lb.addNode(ntOutput, 0)
}

func (lb *logicBuilder) addNode(nodeType uint32, nodeValue uint64) logicNodeID {
	if nodeValue > nodeValueMask {
		panic("invalid node value")
	}
	if lb.currentChunk == nil {
		lb.currentChunk = lb.optimizer.newChunk()
		lb.cache.Add(lb.currentChunk.index, lb.currentChunk)
	}
	node := logicNodeID(lb.currentChunk.index*nodeChunkSize) + 1 + logicNodeID(lb.currentChunk.count)
	lb.currentChunk.nodes[lb.currentChunk.count].typeStateValue = uint64(nodeType)<<nodeTypeShift + uint64(lsAssumedTrue)<<logicStateShift + nodeValue
	lb.currentChunk.count++
	if lb.currentChunk.count == nodeChunkSize {
		lb.currentChunk = nil
	}
	//fmt.Println("addNode", node, "nt", nodeType, "nv", nodeValue)
	return node
}

func (lb *logicBuilder) connect(source, target logicNodeID) {
	s := lb.getNode(source)
	t := lb.getNode(target)
	//fmt.Println("connect source", source, "nt", s.nodeType(), "target", target, "nt", t.nodeType())
	if t.nodeType() == ntInput {
		panic("logic connection target is a proven entry node")
	}
	if oc := s.outputCount(); oc < maxOutputCount {
		s.output[oc] = target
		t.setNodeValue(t.nodeValue() + 1)
	} else {
		split := lb.addOrGateNode()
		ss := lb.getNode(split)
		ss.output = s.output
		for i := range s.output {
			s.output[i] = 0
		}
		lb.connect(source, split)
		lb.connect(source, target)
	}
}

func (id logicNodeID) isValid() bool {
	return id != 0
}

func (ln *logicNode) nodeType() uint32 {
	return uint32((ln.typeStateValue & nodeTypeMask) >> nodeTypeShift)
}

func (ln *logicNode) setNodeType(nt uint32) {
	if nt > ntOutput {
		panic("invalid node type")
	}
	ln.typeStateValue = ln.typeStateValue & ^nodeTypeMask + uint64(nt)<<nodeTypeShift
}

func (ln *logicNode) logicState() uint32 {
	return uint32((ln.typeStateValue & logicStateMask) >> logicStateShift)
}

func (ln *logicNode) setLogicState(ls uint32) {
	if ls > lsDecidedTrue {
		panic("invalid logic state")
	}
	ln.typeStateValue = ln.typeStateValue & ^logicStateMask + uint64(ls)<<logicStateShift
}

func (ln *logicNode) nodeValue() uint64 {
	return ln.typeStateValue & nodeValueMask
}

func (ln *logicNode) setNodeValue(value uint64) {
	if value > nodeValueMask {
		panic("invalid node value")
	}
	ln.typeStateValue = ln.typeStateValue & ^nodeValueMask + value
}

func (ln *logicNode) outputCount() int {
	for i, v := range ln.output {
		if v == 0 {
			return i
		}
	}
	return maxOutputCount
}

func (fh *finalizeHeap) savedCost(entry uint32) int {
	if entry == fh.fixedCostEntry {
		return fh.fixedCost
	}
	var a, c uint64
	if prev := fh.prevNode[entry]; prev != math.MaxUint32 {
		a = fh.getNode(fh.logicNodes[prev]).nodeValue()
	} else {
		a = math.MaxUint64
	}
	b := fh.getNode(fh.logicNodes[entry]).nodeValue()
	if next := fh.nextNode[entry]; next != math.MaxUint32 {
		c = fh.getNode(fh.logicNodes[next]).nodeValue()
	} else {
		c = math.MaxUint64
	}
	return fh.savedInputCost(a, b, c)
}

func (fh *finalizeHeap) removedEntry(sortedIndex uint32) {
	prev := fh.prevNode[sortedIndex]
	next := fh.nextNode[sortedIndex]
	//fmt.Println("removedEntry", sortedIndex, "prev", prev, "next", next)
	//fh.print()
	if next != math.MaxUint32 {
		// save cost of next node before doing changes that affect cost calculation
		fh.fixedCostEntry, fh.fixedCost = next, fh.savedCost(next)
		fh.prevNode[next] = prev
	}
	if prev != math.MaxUint32 {
		fh.nextNode[prev] = next
	}
	fh.logicNodes[sortedIndex] = 0
	if prev != math.MaxUint32 {
		if h := fh.heapIndex[prev]; h != math.MaxUint32 {
			//fmt.Println("heap.Fix", h)
			heap.Fix(fh, int(h))
		}
	}
	if next != math.MaxUint32 {
		fh.fixedCostEntry = 0 // now allow calculating the updated cost for next node
		if h := fh.heapIndex[next]; h != math.MaxUint32 {
			//fmt.Println("heap.Fix", h)
			heap.Fix(fh, int(h))
		}
	}
	//fmt.Println(" ... after")
	//fh.print()
}

func (fh *finalizeHeap) print() {
	return
	fmt.Println(" logicNodes:", fh.logicNodes)
	fmt.Println(" prevNode: ", fh.prevNode)
	fmt.Println(" nextNode: ", fh.nextNode)
	fmt.Println(" heapOrder: ", fh.heapOrder)
	fmt.Println(" heapIndex: ", fh.heapIndex)
}

func (fh *finalizeHeap) Len() int { return len(fh.heapOrder) }

func (fh *finalizeHeap) Less(i, j int) bool {
	//fmt.Println("heap: Less", i, j)
	return fh.savedCost(fh.heapOrder[i]) > fh.savedCost(fh.heapOrder[j])
}

func (fh *finalizeHeap) Swap(i, j int) {
	//fmt.Println("heap: Swap", i, j)
	fh.heapOrder[i], fh.heapOrder[j] = fh.heapOrder[j], fh.heapOrder[i]
	fh.heapIndex[fh.heapOrder[i]] = uint32(i)
	fh.heapIndex[fh.heapOrder[j]] = uint32(j)
}

func (fh *finalizeHeap) Push(x any) {
	item := x.(uint32)
	//fmt.Println("heap: Push", item, len(fh.heapOrder))
	fh.heapIndex[item] = uint32(len(fh.heapOrder))
	fh.heapOrder = append(fh.heapOrder, item)
}

func (fh *finalizeHeap) Pop() any {
	n := len(fh.heapOrder)
	item := fh.heapOrder[n-1]
	//fmt.Println("heap: Pop", item, n-1)
	fh.heapIndex[item] = math.MaxUint32
	fh.heapOrder = fh.heapOrder[:n-1]
	return item
}
