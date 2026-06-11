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
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
)

const (
	nodeChunkSize  = 1024
	maxOutputCount = 4
)

type tableProver struct {
	lock         sync.Mutex
	reader       *tableReader
	treeHeight   int
	nodeChunks   []*logicNodeChunk
	blockProofs  map[uint64]*blockProof
	finalizeHeap finalizeHeap
}

func newTableProver(reader *tableReader) *tableProver {
	return &tableProver{
		reader:     reader,
		treeHeight: 64 - bits.LeadingZeros64(max(reader.entryCount, 1)-1),
	}
}

func (tp *tableProver) addBlockProofs(newProofs map[uint64]*blockProof) {
	fmt.Println("addBlockProofs", len(newProofs))
	if tp.blockProofs == nil {
		tp.blockProofs = newProofs
		return
	}
	for number, newProof := range newProofs {
		if proof, ok := tp.blockProofs[number]; ok {
			proof.merge(newProof)
		} else {
			tp.blockProofs[number] = newProof
		}
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

func (tp *tableProver) finalize() (tableQueryProof, error) {
	var (
		entryCount, allCount int
		finalResult          *logicNode
	)
	for _, chunk := range tp.nodeChunks {
		for i := range chunk.count {
			allCount++
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
	fmt.Println("finalize", tp.reader.blockRange(), "entry nodes", entryCount, "all nodes", allCount)
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
		entryNodes: entryNodes,                      // ntProvenEntry logic nodes sorted by entry index
		prevEntry:  make([]uint32, len(entryNodes)), // indices of entryNodes (sortedIndex)
		nextEntry:  make([]uint32, len(entryNodes)), // indices of entryNodes (sortedIndex)
		heapOrder:  make([]uint32, len(entryNodes)), // indices of entryNodes (sortedIndex)
		heapIndex:  make([]uint32, len(entryNodes)), // indices of heapOrder
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
		sortedIndex := heap.Pop(&tp.finalizeHeap).(uint32)
		//fmt.Println("heap.Pop", sortedIndex)
		tp.finalizeHeap.print()
		entryNode := tp.finalizeHeap.entryNodes[sortedIndex]
		switch finalResult.logicState() {
		case lsDecidedTrue:
			tp.traverse(setFalse, entryNode)
			tp.finalizeHeap.removedEntry(sortedIndex)
			entryCount--
		case lsAssumedTrue:
			tp.traverse(trySetFalse, entryNode)
			switch finalResult.logicState() {
			case lsAssumedTrue:
				tp.traverse(confirmSetFalse, entryNode)
				tp.finalizeHeap.removedEntry(sortedIndex)
				entryCount--
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
	if finalResult.logicState() != lsDecidedTrue {
		panic("invalid final result logic state")
	}
	tp.finalizeHeap.prevEntry, tp.finalizeHeap.nextEntry, tp.finalizeHeap.heapOrder, tp.finalizeHeap.heapIndex = nil, nil, nil, nil
	fmt.Println(" optimized entry node count", entryCount)

	blockNumbers := make([]uint64, 0, len(tp.blockProofs))
	for number, proof := range tp.blockProofs {
		blockNumbers = append(blockNumbers, number)
		entryCount += len(proof.matchingTxs) + 1
	}
	sort.Slice(blockNumbers, func(i, j int) bool {
		return blockNumbers[i] < blockNumbers[j]
	})
	proof := tableQueryProof{
		FirstBlock:   tp.reader.blockRange().First(),
		TableSize:    tp.reader.blockRange().Count(),
		EntryIndices: make([]uint64, 0, entryCount),
		BlockResults: make([]blockResults, len(blockNumbers)),
	}
	for _, bp := range tp.blockProofs {
		fmt.Println("block entry", bp.blockEntryIndex)
		proof.EntryIndices = append(proof.EntryIndices, bp.blockEntryIndex)
		for _, mtx := range bp.matchingTxs {
			fmt.Println("tx entry", mtx.txEntryIndex)
			proof.EntryIndices = append(proof.EntryIndices, mtx.txEntryIndex)
		}
	}
	for _, entryNode := range tp.finalizeHeap.entryNodes {
		if entryNode != 0 {
			proof.EntryIndices = append(proof.EntryIndices, tp.getNode(entryNode).nodeValue())
		}
	}
	sort.Slice(proof.EntryIndices, func(i, j int) bool {
		return proof.EntryIndices[i] < proof.EntryIndices[j]
	})

	for i, number := range blockNumbers {
		bp := tp.blockProofs[number]
		br := blockResults{
			Header:         *bp.header,
			ProvenReceipts: make([]uint, 0, len(bp.matchingTxs)),
			ReceiptsProof:  bp.receiptsProof.proofForStorage(),
		}
		for txi := range bp.matchingTxs {
			br.ProvenReceipts = append(br.ProvenReceipts, uint(txi))
		}
		sort.Slice(br.ProvenReceipts, func(i, j int) bool {
			return br.ProvenReceipts[i] < br.ProvenReceipts[j]
		})
		proof.BlockResults[i] = br
	}

	tp.finalizeHeap.entryNodes = nil
	tp.nodeChunks = nil
	entries := make(indexEntries, entryCount)
	lastIndex := uint64(math.MaxUint64)
	fmt.Println("proof.EntryIndices", proof.EntryIndices)
	for i, entryIndex := range proof.EntryIndices {
		entry, err := tp.reader.getEntry(entryIndex)
		if err != nil {
			return tableQueryProof{}, err
		}
		entries[i] = *entry
		if proof.ProofHashes, err = tp.makeProofHashes(proof.ProofHashes, lastIndex, entryIndex); err != nil {
			return tableQueryProof{}, err
		}
		lastIndex = entryIndex
	}
	var err error
	if proof.ProofHashes, err = tp.makeProofHashes(proof.ProofHashes, lastIndex, uint64(math.MaxUint64)); err != nil {
		return tableQueryProof{}, err
	}
	proof.ProvenEntries = entries.toStorage()
	return proof, nil
}

func (tp *tableProver) makeProofHashes(hashes []merkle.Value, a, b uint64) ([]merkle.Value, error) {
	iterateProofIndices(tp.treeHeight, a, b, func(gti uint64) error {
		hash, err := tp.reader.getHash(gti)
		if err != nil {
			return err
		}
		hashes = append(hashes, hash)
		return nil
	})
	return hashes, nil
}

func iterateProofIndices(treeHeight int, a, b uint64, callback func(uint64) error) error {
	switch {
	case a == math.MaxUint64 && b == math.MaxUint64:
		// no entries proven; merkle multiproof is just the root hash
		return callback(1)
	case a == math.MaxUint64:
		// b has no left neighbor; each 1 bit in b corresponds to a proven hash
		return iterateProofIndicesUp(treeHeight, 0, b, callback)
	case b == math.MaxUint64:
		// a has no right neighbor; each 0 bit in a corresponds to a proven hash
		return iterateProofIndicesDown(treeHeight, 0, a, callback)
	default:
		if a == b {
			return nil
		}
		if a > b {
			panic("iterateProofIndices: invalid index order")
		}
		// we ignore the shared binary prefix plus the first different bit (0 in a, 1 in b)
		splitHeight := treeHeight + bits.LeadingZeros64(a^b) - 63
		// in the remaining lower bits, each 0 bit in a and each 1 bit in b corresponds to a proven hash
		if err := iterateProofIndicesDown(treeHeight, splitHeight, a, callback); err != nil {
			return err
		}
		return iterateProofIndicesUp(treeHeight, splitHeight, b, callback)
	}
}

func iterateProofIndicesUp(treeHeight, fromHeight int, entryIndex uint64, callback func(uint64) error) error {
	for h := fromHeight; h < treeHeight; h++ { // h == 0 corresponds to entryIndex MSB
		if entryIndex&(uint64(1)<<(treeHeight-1-h)) != 0 {
			if err := callback((entryIndex>>(treeHeight-1-h) ^ 1) + uint64(1)<<(h+1)); err != nil {
				return err
			}
		}
	}
	return nil
}

func iterateProofIndicesDown(treeHeight, toHeight int, entryIndex uint64, callback func(uint64) error) error {
	for h := treeHeight - 1; h >= toHeight; h-- { // h == 0 corresponds to entryIndex MSB
		if entryIndex&(uint64(1)<<(treeHeight-1-h)) == 0 {
			if err := callback((entryIndex>>(treeHeight-1-h) ^ 1) + uint64(1)<<(h+1)); err != nil {
				return err
			}
		}
	}
	return nil
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
	fixedCostEntry       uint32 //TODO is this necessary?
	fixedCost            int
}

// number of merkle multiproof hashes required between adjacent proven entry
// indices a and b (assuming a < b)
func (fh *finalizeHeap) proofCost(a, b uint64) int {
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
	if entry == fh.fixedCostEntry {
		return fh.fixedCost
	}
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

func (fh *finalizeHeap) removedEntry(sortedIndex uint32) {
	prev := fh.prevEntry[sortedIndex]
	next := fh.nextEntry[sortedIndex]
	//fmt.Println("removedEntry", sortedIndex, "prev", prev, "next", next)
	fh.print()
	if next != math.MaxUint32 {
		// save cost of next node before doing changes that affect cost calculation
		fh.fixedCostEntry, fh.fixedCost = next, fh.savedCost(next)
		fh.prevEntry[next] = prev
	}
	if prev != math.MaxUint32 {
		fh.nextEntry[prev] = next
	}
	fh.entryNodes[sortedIndex] = 0
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
	fh.print()
}

func (fh *finalizeHeap) print() {
	return
	fmt.Println(" entryNodes:", fh.entryNodes)
	fmt.Println(" prevEntry: ", fh.prevEntry)
	fmt.Println(" nextEntry: ", fh.nextEntry)
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
	if entryIndex >= nodeValueMask {
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
	//fmt.Println("addNode", node, "nt", nodeType, "nv", nodeValue)
	return node
}

func (pi *proverInstance) connect(source, target uint32) {
	s := pi.getNode(source)
	t := pi.getNode(target)
	//fmt.Println("connect source", source, "nt", s.nodeType(), "target", target, "nt", t.nodeType())
	if t.nodeType() == ntProvenEntry {
		panic("logic connection target is a proven entry node")
	}
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

	ntProvenEntry = 0
	ntAndGate     = 1
	ntOrGate      = 2
	ntFinalResult = 3

	logicStateShift = 60
	logicStateMask  = uint64(3) << logicStateShift

	lsAssumedFalse = 0
	lsAssumedTrue  = 1
	lsDecidedFalse = 2
	lsDecidedTrue  = 3

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

type trieProofWriter map[common.Hash][]byte

func (t trieProofWriter) Put(key []byte, value []byte) error {
	if len(key) != common.HashLength {
		panic("invalid proof database key")
	}
	var hash common.Hash
	copy(hash[:], key)
	t[hash] = slices.Clone(value)
	return nil
}

func (t trieProofWriter) Delete(key []byte) error { panic("not implemented") }

func (t trieProofWriter) proofForStorage() [][]byte {
	proof := make([][]byte, len(t))
	proofHashes := make([]common.Hash, 0, len(t))
	for hash := range t {
		proofHashes = append(proofHashes, hash)
	}
	sort.Slice(proofHashes, func(i, j int) bool {
		return proofHashes[i].Cmp(proofHashes[j]) < 0
	})
	for i, hash := range proofHashes {
		proof[i] = t[hash]
	}
	return proof
}
