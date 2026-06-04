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
	treeHeight   int
	nodeChunks   []*logicNodeChunk
	finalizeHeap finalizeHeap
}

func newTableProver(treeHeight int) *tableProver {
	return &tableProver{treeHeight: treeHeight}
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

func (tp *tableProver) finalize(finalNode uint32) {
	var entryCount int
	for _, chunk := range tp.nodeChunks {
		for i := range chunk.count {
			if !chunk.nodes[i].isGate() {
				entryCount++
			}
		}
	}
	entryNodes := make([]uint32, entryCount)
	var entryPtr int
	for _, chunk := range tp.nodeChunks {
		for i := range chunk.count {
			if !chunk.nodes[i].isGate() {
				entryNodes[entryPtr] = chunk.index*nodeChunkSize + 1 + uint32(i)
				entryPtr++
			}
		}
	}
	sort.Slice(entryNodes, func(i, j int) bool {
		return tp.getNode(entryNodes[i]).entryOrGate < tp.getNode(entryNodes[j]).entryOrGate
	})
	pi := tp.newInstance()
	var j int
	for i, node := range entryNodes {
		if j > 0 && tp.getNode(entryNodes[j-1]).entryOrGate == tp.getNode(node).entryOrGate {
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
			tp.finalizeHeap.prevEntry[i] = i - 1
		} else {
			tp.finalizeHeap.prevEntry[i] = math.MaxUint32
		}
		if i < len(entryNodes)-1 {
			tp.finalizeHeap.nextEntry[i] = i + 1
		} else {
			tp.finalizeHeap.nextEntry[i] = math.MaxUint32
		}
		tp.finalizeHeap.heapOrder[i] = i
		tp.finalizeHeap.heapIndex[i] = i
	}
	heap.Init(&tp.finalizeHeap)
	for tp.finalizeHeap.Len() != 0 {
		entry := heap.Pop(&tp.finalizeHeap).(uint32)
		if tp.canSetToFalse(tp.finalizeHeap.entryNodes[entry], finalNode) {
			tp.setFinalValue(tp.finalizeHeap.entryNodes[entry], false)
			tp.finalizeHeap.removeEntry(entry)
			tp.finalizeHeap.entryNodes[entry] = 0
			entryCount--
		} else {
			tp.setFinalValue(tp.finalizeHeap.entryNodes[entry], true)
		}
	}
	tp.finalizeHeap.prevEntry, tp.finalizeHeap.nextEntry, tp.finalizeHeap.heapOrder, tp.finalizeHeap.heapIndex = nil, nil, nil, nil
	entryIndices = make([]uint64, 0, entryCount)
	for _, entryNode := range tp.finalizeHeap.entryNodes {
		if entryNode != 0 {
			entryIndices = append(entryIndices, tp.getNode(entryNode).entryOrGate)
		}
	}
	tp.finalizeHeap.entryNodes = nil
	tp.nodeChunks = nil

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
		a = fh.getNode(fh.entryNodes[prev]).entryOrGate
	} else {
		a = math.MaxUint64
	}
	b := fh.getNode(fh.entryNodes[entry]).entryOrGate
	if next := fh.nextEntry[entry]; next != math.MaxUint32 {
		c = fh.getNode(fh.entryNodes[next]).entryOrGate
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
	heap.Remove(fh, fh.heapIndex[entry])
	if prev != math.MaxUint32 {
		heap.Fix(fh, fh.heapIndex[prev])
	}
	if next != math.MaxUint32 {
		heap.Fix(fh, fh.heapIndex[next])
	}
}

func (fh *finalizeHeap) Len() int { return len(fh.heapOrder) }

func (fh *finalizeHeap) Less(i, j int) bool {
	return fh.savedCost(i) > fh.savedCost(j)
}

func (fh *finalizeHeap) Swap(i, j int) {
	fh.heapOrder[i], fh.heapOrder[j] = fh.heapOrder[j], fh.heapOrder[i]
	fh.heapIndex[fh.heapOrder[i]] = i
	fh.heapIndex[fh.heapOrder[j]] = j
}

func (fh *finalizeHeap) Push(x any) {
	n := len(fh.heapOrder)
	item := x.(uint32)
	fh.heapIndex[item] = n
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
		n2.entryOrGate = logicOrGate
		node3 := pi.addOrNode()
		n3 := pi.getNode(node3)
		n3.output = n.output
		n.output[0] = node2
		n.output[1] = node3
		for i := 2; i < maxOutputCount; i++ {
			n.output[i] = 0
		}
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

func (pi *proverInstance) addLeafNode(entryIndex uint64) uint32 {
	if entryIndex >= logicGateThreshold {
		panic("invalid entry index")
	}
	return pi.addNode(entryIndex)
}

func (pi *proverInstance) addAndNode() uint32 {
	return pi.addNode(logicAndGate)
}

func (pi *proverInstance) addOrNode() uint32 {
	return pi.addNode(logicOrGate)
}

func (pi *proverInstance) addNode(entryOrGate uint64) uint32 {
	if pi.currentChunk == nil {
		pi.currentChunk = pi.prover.newChunk()
		pi.cache.Add(pi.currentChunk.index, pi.currentChunk)
	}
	node := pi.currentChunk.index*nodeChunkSize + 1 + uint32(pi.currentChunk.count)
	pi.currentChunk.nodes[pi.currentChunk.count].entryOrGate = entryOrGate
	pi.currentChunk.count++
	if pi.currentChunk.count == nodeChunkSize {
		pi.currentChunk = nil
	}
	return node
}

func (pi *proverInstance) connect(source, target uint32) {
	s := pi.getNode(source)
	if oc := s.outputCount(); oc < maxOutputCount {
		s.output[oc] = target
	} else {
		split := pi.addOrNode()
		ss := pi.getNode(split)
		ss.output = s.output
		s.output[0] = split
		s.output[1] = target
		for i := 2; i < maxOutputCount; i++ {
			s.output[i] = 0
		}
	}
	t := pi.getNode(target)
	if !t.isGate() {
		panic("logic connection target is not a gate node")
	}
	t.entryOrGate++
}

type logicNodeChunk struct {
	nodes        [nodeChunkSize]logicNode
	index, count uint32
}

type logicNode struct {
	entryOrGate uint64
	output      [maxOutputCount]uint32
}

func (ln *logicNode) isGate() bool {
	return ln.entryOrGate >= logicGateThreshold
}

func (ln *logicNode) outputCount() int {
	for i, v := range ln.output {
		if v == 0 {
			return i
		}
	}
	return maxOutputCount
}
