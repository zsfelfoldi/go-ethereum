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

/*func (tp *tableProver) getNode(node uint32) *logicNode {
	if node == 0 {
		return nil
	}
	return &tp.nodeChunks[(node-1)/nodeChunkSize].nodes[(node-1)%nodeChunkSize]
}*/

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
