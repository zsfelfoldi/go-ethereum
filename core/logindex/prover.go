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
	"math"
	"math/bits"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common/lru"
)

const (
	nodeChunkSize      = 1024
	maxOutputCount     = 4
	logicAndGate       = uint64(0xffffffff00000000)
	logicOrGate        = uint64(0xfffffffe00000000)
	logicGateMask      = uint64(0xffffffff00000000)
	logicInputMask     = uint64(0x00000000ffffffff)
	logicGateThreshold = uint64(0xfffffffe00000000)
)

type tableProver struct {
	lock         sync.Mutex
	reader       *tableReader
	treeHeight   int
	nodeChunks   []*logicNodeChunk
	finalizeHeap finalizeHeap
}

func newTableProver(reader *tableReader) *tableProver {
	return &tableProver{
		reader:     reader,
		treeHeight: 64 - bits.LeadingZeros64(max(reader.entryCount, 1)-1),
	}
}

func (tp *tableProver) newInstance() *proverInstance {
	return &proverInstance{
		prover: tp,
		cache:  lru.NewCache[uint32, *logicNodeChunk](10),
	}
}

func (tp *tableProver) newChunk() *logicNodeChunk {
	tp.lock.Lock()
	defer tp.lock.Unlock()

	chunkIndex := uint32(len(tp.nodeChunks))
	tp.nodeChunks = append(tp.nodeChunks, &logicNodeChunk{
		index: chunkIndex,
	})
	return tp.nodeChunks[chunkIndex]
}

func (tp *tableProver) getChunk(index uint32) *logicNodeChunk {
	tp.lock.Lock()
	defer tp.lock.Unlock()

	return tp.nodeChunks[index]
}

func (tp *tableProver) getNode(node uint32) *logicNode {
	if node == 0 {
		return nil
	}
	return &tp.nodeChunks[(node-1)/nodeChunkSize].nodes[(node-1)%nodeChunkSize]
}

func (tp *tableProver) finalize() {
	var (
		entryCount  int
		finalResult *logicNode
	)
	for _, chunk := range tp.nodeChunks {
		for i := range chunk.count {
			switch chunk.nodes[i].nodeType() {
			case ntProvenEntry:
				entryCount++
			case ntFinalResult:
				if finalResult != nil {
					panic("more than one final result node found")
				}
				finalResult = &chunk.nodes[i]
			}
		}
	}
	if finalResult == nil {
		panic("no final result node found")
	}
	entryNodes := make([]uint32, entryCount)
	var entryPtr int
	for _, chunk := range tp.nodeChunks {
		for i := range chunk.count {
			if chunk.nodes[i].nodeType() == ntProvenEntry {
				entryNodes[entryPtr] = chunk.index*nodeChunkSize + 1 + uint32(i)
				entryPtr++
			}
		}
	}
	sort.Slice(entryNodes, func(i, j int) bool {
		return tp.getNode(entryNodes[i]).nodeValue() < tp.getNode(entryNodes[j]).nodeValue()
	})
	pi := tp.newInstance()
	var j int
	for _, node := range entryNodes {
		if j > 0 && tp.getNode(entryNodes[j-1]).nodeValue() == tp.getNode(node).nodeValue() {
			pi.mergeEntryNodes(entryNodes[j-1], node)
		} else {
			entryNodes[j] = node
			j++
		}
	}
	pi = nil
	entryNodes = entryNodes[:j]
	entryCount = j
	tp.finalizeHeap = finalizeHeap{
		getNode:    tp.getNode,
		treeHeight: tp.treeHeight,
		entryNodes: entryNodes,
		prevEntry:  make([]uint32, len(entryNodes)),
		nextEntry:  make([]uint32, len(entryNodes)),
		heapOrder:  make([]uint32, len(entryNodes)),
		heapIndex:  make([]uint32, len(entryNodes)),
	}
	for i := range entryNodes {
		if i > 0 {
			tp.finalizeHeap.prevEntry[i] = uint32(i - 1)
		} else {
			tp.finalizeHeap.prevEntry[i] = math.MaxUint32
		}
		if i < len(entryNodes)-1 {
			tp.finalizeHeap.nextEntry[i] = uint32(i + 1)
		} else {
			tp.finalizeHeap.nextEntry[i] = math.MaxUint32
		}
		tp.finalizeHeap.heapOrder[i] = uint32(i)
		tp.finalizeHeap.heapIndex[i] = uint32(i)
	}
	heap.Init(&tp.finalizeHeap)
	for tp.finalizeHeap.Len() != 0 {
		entryNode := heap.Pop(&tp.finalizeHeap).(uint32)
		switch finalResult.logicState() {
		case lsDecidedTrue:
			tp.traverse(setFalse, entryNode)
		case lsAssumedTrue:
			tp.traverse(trySetFalse, entryNode)
			switch finalResult.logicState() {
			case lsAssumedTrue:
				tp.traverse(confirmSetFalse, entryNode)
			case lsAssumedFalse:
				tp.traverse(revertSetFalse, entryNode)
				tp.traverse(setTrue, entryNode)
			default:
				panic("unexpected logic state for final result node after trySetFalse")
			}
		default:
			panic("unexpected logic state for final result node")
		}
	}
	tp.finalizeHeap.prevEntry, tp.finalizeHeap.nextEntry, tp.finalizeHeap.heapOrder, tp.finalizeHeap.heapIndex = nil, nil, nil, nil
	entryIndices := make([]uint64, 0, entryCount)
	for _, entryNode := range tp.finalizeHeap.entryNodes {
		if entryNode != 0 {
			entryIndices = append(entryIndices, tp.getNode(entryNode).nodeValue())
		}
	}
	tp.finalizeHeap.entryNodes = nil
	tp.nodeChunks = nil

}

const (
	taNone = iota
	taPropagate
	taStop
)

func (tp *tableProver) traverse(traverseFn func(*logicNode) int, node uint32) bool {
	n := tp.getNode(node)
	switch traverseFn(n) {
	case taNone:
	case taPropagate:
		for i := range n.outputCount() {
			if tp.traverse(traverseFn, n.output[i]) {
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

func setFalse(n *logicNode) int {
	if n.logicState() != lsAssumedTrue {
		return taNone
	}
	switch n.nodeType() {
	case ntProvenEntry, ntAndGate:
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
	case ntFinalResult:
		panic("cannot set final result node to decided false state")
	default:
		panic("invalid node type")
	}
}

func trySetFalse(n *logicNode) int {
	if n.logicState() != lsAssumedTrue {
		return taNone
	}
	switch n.nodeType() {
	case ntProvenEntry, ntAndGate:
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
	case ntFinalResult:
		n.setLogicState(lsAssumedFalse)
		return taStop
	default:
		panic("invalid node type")
	}
}

func confirmSetFalse(n *logicNode) int {
	if n.logicState() != lsAssumedFalse {
		return taNone
	}
	switch n.nodeType() {
	case ntProvenEntry, ntAndGate, ntOrGate:
		n.setLogicState(lsDecidedFalse)
		return taPropagate
	case ntFinalResult:
		panic("cannot set final result node to decided false state")
	default:
		panic("invalid node type")
	}
}

func revertSetFalse(n *logicNode) int {
	switch n.nodeType() {
	case ntProvenEntry, ntAndGate:
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
	case ntFinalResult:
		if n.logicState() == lsAssumedFalse {
			n.setLogicState(lsAssumedTrue)
		}
		return taNone
	default:
		panic("invalid node type")
	}
}

func setTrue(n *logicNode) int {
	if n.logicState() != lsAssumedTrue {
		return taNone
	}
	switch n.nodeType() {
	case ntProvenEntry, ntOrGate:
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
	case ntFinalResult:
		n.setLogicState(lsDecidedTrue)
		return taNone
	default:
		panic("invalid node type")
	}
}

type finalizeHeap struct {
	getNode              func(uint32) *logicNode
	treeHeight           int
	entryNodes           []uint32
	prevEntry, nextEntry []uint32
	heapOrder, heapIndex []uint32
}

// number of merkle multiproof hashes required between adjacent proven entry
// indices a and b (assuming a < b)
func (fh *finalizeHeap) proofCost(a, b uint64) int {
	//
	switch {
	case a == math.MaxUint64 && b == math.MaxUint64:
		// no entries remaining; proof is just a root hash
		return 1
	case a == math.MaxUint64:
		// b has no left neighbor; each 1 bit in b costs a proof hash
		return bits.OnesCount64(b)
	case b == math.MaxUint64:
		// a has no right neighbor; each 0 bit in a costs a proof hash
		return fh.treeHeight - bits.OnesCount64(a)
	default:
		if a >= b {
			panic("proofCost: invalid index order")
		}
		// we ignore the shared binary prefix plus the first different bit (0 in a, 1 in b)
		ignorePrefix := bits.LeadingZeros64(a^b) + 1
		// in the remaining lower bits, each 0 bit in a and each 1 bit in b costs a proof hash
		return 64 - ignorePrefix - bits.OnesCount64(a<<ignorePrefix) + bits.OnesCount64(b<<ignorePrefix)
	}
}

// multiproof hash cost saved by removing given entryNodes index
func (fh *finalizeHeap) savedCost(entry uint32) int {
	var a, c uint64
	if prev := fh.prevEntry[entry]; prev != math.MaxUint32 {
		a = fh.getNode(fh.entryNodes[prev]).nodeValue()
	} else {
		a = math.MaxUint64
	}
	b := fh.getNode(fh.entryNodes[entry]).nodeValue()
	if next := fh.nextEntry[entry]; next != math.MaxUint32 {
		c = fh.getNode(fh.entryNodes[next]).nodeValue()
	} else {
		c = math.MaxUint64
	}
	return fh.proofCost(a, b) + fh.proofCost(b, c) - fh.proofCost(a, c)
}

func (fh *finalizeHeap) removeEntry(entry uint32) {
	prev := fh.prevEntry[entry]
	next := fh.nextEntry[entry]
	if prev != math.MaxUint32 {
		fh.nextEntry[prev] = next
	}
	if next != math.MaxUint32 {
		fh.prevEntry[next] = prev
	}
	heap.Remove(fh, int(fh.heapIndex[entry]))
	if prev != math.MaxUint32 {
		heap.Fix(fh, int(fh.heapIndex[prev]))
	}
	if next != math.MaxUint32 {
		heap.Fix(fh, int(fh.heapIndex[next]))
	}
}

func (fh *finalizeHeap) Len() int { return len(fh.heapOrder) }

func (fh *finalizeHeap) Less(i, j int) bool {
	return fh.savedCost(uint32(i)) > fh.savedCost(uint32(j))
}

func (fh *finalizeHeap) Swap(i, j int) {
	fh.heapOrder[i], fh.heapOrder[j] = fh.heapOrder[j], fh.heapOrder[i]
	fh.heapIndex[fh.heapOrder[i]] = uint32(i)
	fh.heapIndex[fh.heapOrder[j]] = uint32(j)
}

func (fh *finalizeHeap) Push(x any) {
	item := x.(uint32)
	fh.heapIndex[item] = uint32(len(fh.heapOrder))
	fh.heapOrder = append(fh.heapOrder, item)
}

func (fh *finalizeHeap) Pop() any {
	n := len(fh.heapOrder)
	item := fh.heapOrder[n-1]
	fh.heapIndex[item] = math.MaxUint32
	fh.heapOrder = fh.heapOrder[:n-1]
	return item
}

func (pi *proverInstance) mergeEntryNodes(node, node2 uint32) {
	n := pi.getNode(node)
	n2 := pi.getNode(node2)
	count := n.outputCount()
	count2 := n2.outputCount()
	if count+count2 <= maxOutputCount {
		copy(n.output[count:count+count2], n2.output[:count2])
	} else {
		node3 := pi.addOrGateNode()
		n3 := pi.getNode(node3)
		n3.output = n.output
		node4 := pi.addOrGateNode()
		n4 := pi.getNode(node4)
		n4.output = n2.output
		for i := range n.output {
			n.output[i] = 0
		}
		pi.connect(node, node3)
		pi.connect(node, node4)
	}
}

type proverInstance struct {
	prover       *tableProver
	cache        *lru.Cache[uint32, *logicNodeChunk]
	currentChunk *logicNodeChunk
}

func (pi *proverInstance) getNode(node uint32) *logicNode {
	if node == 0 {
		return nil
	}
	chunkIndex := (node - 1) / nodeChunkSize
	chunk, ok := pi.cache.Get(chunkIndex)
	if !ok {
		chunk = pi.prover.getChunk(chunkIndex)
		pi.cache.Add(chunkIndex, chunk)
	}
	return &chunk.nodes[(node-1)%nodeChunkSize]
}

func (pi *proverInstance) addProvenEntryNode(entryIndex uint64) uint32 {
	if entryIndex >= logicGateThreshold {
		panic("invalid entry index")
	}
	return pi.addNode(ntProvenEntry, entryIndex)
}

func (pi *proverInstance) addAndGateNode() uint32 {
	return pi.addNode(ntAndGate, 0)
}

func (pi *proverInstance) addOrGateNode() uint32 {
	return pi.addNode(ntOrGate, 0)
}

func (pi *proverInstance) addFinalResultNode() uint32 {
	return pi.addNode(ntFinalResult, 0)
}

func (pi *proverInstance) addNode(nodeType uint32, nodeValue uint64) uint32 {
	if nodeValue > nodeValueMask {
		panic("invalid node value")
	}
	if pi.currentChunk == nil {
		pi.currentChunk = pi.prover.newChunk()
		pi.cache.Add(pi.currentChunk.index, pi.currentChunk)
	}
	node := pi.currentChunk.index*nodeChunkSize + 1 + uint32(pi.currentChunk.count)
	pi.currentChunk.nodes[pi.currentChunk.count].typeStateValue = uint64(nodeType)<<nodeTypeShift + uint64(lsAssumedTrue)<<logicStateShift + nodeValue
	pi.currentChunk.count++
	if pi.currentChunk.count == nodeChunkSize {
		pi.currentChunk = nil
	}
	return node
}

func (pi *proverInstance) connect(source, target uint32) {
	t := pi.getNode(target)
	if t.nodeType() == ntProvenEntry {
		panic("logic connection target is a proven entry node")
	}
	s := pi.getNode(source)
	if oc := s.outputCount(); oc < maxOutputCount {
		s.output[oc] = target
		t.setNodeValue(t.nodeValue() + 1)
	} else {
		split := pi.addOrGateNode()
		ss := pi.getNode(split)
		ss.output = s.output
		for i := range s.output {
			s.output[i] = 0
		}
		pi.connect(source, split)
		pi.connect(source, target)
	}
}

type logicNodeChunk struct {
	nodes        [nodeChunkSize]logicNode
	index, count uint32
}

const (
	nodeTypeShift = 62
	nodeTypeMask  = uint64(3) << nodeTypeShift

	ntProvenEntry = iota
	ntAndGate
	ntOrGate
	ntFinalResult

	logicStateShift = 60
	logicStateMask  = uint64(3) << logicStateShift

	lsAssumedFalse = iota
	lsAssumedTrue
	lsDecidedFalse
	lsDecidedTrue

	nodeValueMask = (uint64(1) << logicStateShift) - 1
)

type logicNode struct {
	typeStateValue uint64
	output         [maxOutputCount]uint32
}

func (ln *logicNode) nodeType() uint32 {
	return uint32((ln.typeStateValue & nodeTypeMask) >> nodeTypeShift)
}

func (ln *logicNode) setNodeType(nt uint32) {
	if nt > ntFinalResult {
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
