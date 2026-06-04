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
	lock       sync.Mutex
	nodeChunks []*logicNodeChunk
}

func newTableProver() *tableProver {
	return &tableProver{}
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
	entryNodes = entryNodes[:j]
	tp.finalizeHeap = finalizeHeap{
		getNode:    tp.getNode,
		entryNodes: entryNodes,
		prevEntry:  make([]uint32, len(entryNodes)),
		nextEntry:  make([]uint32, len(entryNodes)),
		heapOrder:  make([]uint32, len(entryNodes)),
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
}

type finalizeHeap struct {
	getNode              func(uint32) *logicNode
	entryNodes           []uint32
	prevEntry, nextEntry []uint32
	heapOrder, heapIndex []uint32
}

func (fh *finalizeHeap) Len() int { return len(fh.heapOrder) }

func (fh *finalizeHeap) Less(i, j int) bool {
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
