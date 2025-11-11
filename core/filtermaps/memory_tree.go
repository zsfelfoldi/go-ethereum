// Copyright 2025 The go-ethereum Authors
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

package filtermaps

import (
	"crypto/sha256"
	"math"
	"sort"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common/lru"
)

type merkleTreeNode struct {
	value                         merkle.Value
	metaInfo, parent, left, right uint32
}

var (
	mnEmptySubtreeMask = uint32(1) << 0
	mnRehashMask       = uint32(1) << 1
	mnCompleteShift    = uint(2)
	mnCompleteMask     = (uint32(1) << 14) - (uint32(1) << 2)
	//
	mnStoredMask      = uint32(1) << 14
	mnWeightShift     = uint(15)
	mnWeightMask      = (uint32(1) << 31) - (uint32(1) << 15)
	mnSubtreeRootMask = uint32(1) << 31
)

const (
	maxFinalizedAge = 4093
	mcfModulus      = 4094
	mcfIncomplete   = 4094
	mcfFinalized    = 4095
)

func (n *merkleTreeNode) isEmptySubtree() bool { return n.metaInfo&mnEmptySubtreeMask != 0 }
func (n *merkleTreeNode) needsRehash() bool    { return n.metaInfo&mnRehashMask != 0 }
func (n *merkleTreeNode) isComplete() bool {
	return (n.metaInfo&mnCompleteMask)>>mnCompleteShift != mcfIncomplete
}
func (n *merkleTreeNode) completedByBlock(finalized uint64) uint64 {
	switch cf := (n.metaInfo & mnCompleteMask) >> mnCompleteShift; cf {
	case mcfIncomplete:
		return math.MaxUint64
	case mcfFinalized:
		return finalized
	default:
		return finalized + uint64((cf+mcfModulus-uint32(finalized%mcfModulus))%mcfModulus)
	}
}
func (n *merkleTreeNode) isFinalized() bool {
	return (n.metaInfo&mnCompleteMask)>>mnCompleteShift == mcfFinalized
}
func (n *merkleTreeNode) isStored() bool      { return n.metaInfo&mnStoredMask != 0 }
func (n *merkleTreeNode) isSubtreeRoot() bool { return n.metaInfo&mnSubtreeRootMask != 0 }
func (n *merkleTreeNode) weight() uint32      { return (n.metaInfo & mnWeightMask) >> mnWeightShift }
func (n *merkleTreeNode) setFlag(mask uint32, b bool) {
	if b {
		n.metaInfo |= mask
	} else {
		n.metaInfo &= ^mask
	}
}
func (n *merkleTreeNode) setField(mask uint32, shift uint, value uint32) {
	if value > mask>>shift {
		panic("invalid node meta info field")
	}
	n.metaInfo = (n.metaInfo & ^mask) + (value << shift)
}
func (n *merkleTreeNode) setEmptySubtree(b bool) { n.setFlag(mnEmptySubtreeMask, b) }
func (n *merkleTreeNode) setRehash(b bool)       { n.setFlag(mnRehashMask, b) }
func (n *merkleTreeNode) setIncomplete() {
	n.setField(mnCompleteMask, mnCompleteShift, mcfIncomplete)
}
func (n *merkleTreeNode) setCompleted(finalized, current uint64) {
	if current > finalized+maxFinalizedAge {
		panic("finalized block too old")
	}
	n.setField(mnCompleteMask, mnCompleteShift, uint32(current%mcfModulus))
}
func (n *merkleTreeNode) setFinalized() {
	n.setField(mnCompleteMask, mnCompleteShift, mcfFinalized)
}
func (n *merkleTreeNode) setStored(b bool) { n.setFlag(mnStoredMask, b) }
func (n *merkleTreeNode) setWeight(weight uint32) {
	n.setField(mnWeightMask, mnWeightShift, weight)
}
func (n *merkleTreeNode) setSubtreeRoot(b bool) { n.setFlag(mnSubtreeRootMask, b) }

var nullPtr = uint32(math.MaxUint32)

type merkleTree struct {
	params         *Params
	nodes          []merkleTreeNode
	firstFree      uint32
	emptyValues    map[merkle.Value]struct{ left, right merkle.Value } //TODO
	finalizedBlock uint64
	storedSubtrees storedSubtrees
}

func (params *Params) newMerkleTree() *merkleTree {
	return &merkleTree{
		params: params,
		nodes: []merkleTreeNode{merkleTreeNode{
			//TODO meta fields?
			parent: nullPtr,
			left:   nullPtr,
			right:  nullPtr,
		}},
		firstFree: nullPtr,
	}
}

func (mt *merkleTree) deleteNode(node uint32) {
	mt.nodes[node].right = mt.firstFree
	mt.firstFree = node
}

func (mt *merkleTree) newNode(parent uint32) uint32 {
	n := merkleTreeNode{
		parent: parent,
		left:   nullPtr,
		right:  nullPtr,
	}
	if mt.firstFree != nullPtr {
		node := mt.firstFree
		mt.firstFree = mt.nodes[node].right
		mt.nodes[node] = n
		return node
	}
	node := uint32(len(mt.nodes))
	mt.nodes = append(mt.nodes, n)
	return node
}

func (mt *merkleTree) getDescendant(node uint32, subIndex treeIndex) uint32 {
	for subIndex != rootIndex {
		n := &mt.nodes[node]
		if n.left == nullPtr {
			if !n.isEmptySubtree() {
				panic("cannot expand non-empty subtree")
			}
			children, ok := mt.emptyValues[n.value]
			if !ok {
				panic("unknown empty subtree hash")
			}
			n.left = mt.newNode(node)
			mt.nodes[n.left].value = children.left
			mt.nodes[n.left].setEmptySubtree(true)
			mt.nodes[n.left].setIncomplete()
			n.right = mt.newNode(node)
			mt.nodes[n.right].value = children.right
			mt.nodes[n.right].setEmptySubtree(true)
			mt.nodes[n.right].setIncomplete()
		}
		switch {
		case subIndex.matchRoot(2):
			node = n.left
		case subIndex.matchRoot(3):
			node = n.right
		default:
			panic("invalid descendant subIndex")
		}
	}
	return node
}

func (mt *merkleTree) rehashNode(node uint32) {
	n := &mt.nodes[node]
	if n.left == nullPtr || n.right == nullPtr {
		panic("cannot hash node with no children")
	}
	a := &mt.nodes[n.left]
	b := &mt.nodes[n.right]
	if a.needsRehash() {
		mt.rehashNode(n.left)
	}
	if b.needsRehash() {
		mt.rehashNode(n.right)
	}
	hasher := sha256.New()
	hasher.Write(a.value[:])
	hasher.Write(b.value[:])
	hasher.Sum(n.value[:0])
	n.setEmptySubtree(a.isEmptySubtree() && b.isEmptySubtree())
	n.setRehash(false)
	if a.isComplete() && b.isComplete() {
		if a.isFinalized() && b.isFinalized() {
			n.setFinalized()
		} else {
			n.setCompleted(mt.finalizedBlock, max(a.completedByBlock(mt.finalizedBlock), b.completedByBlock(mt.finalizedBlock)))
		}
	} else {
		n.setIncomplete()
		n.setStored(false)
		n.setWeight(0)
		n.setSubtreeRoot(false)
		return
	}
	stored := a.isStored()
	subtreeRoot := false
	if b.isStored() != stored {
		panic("completed siblings with different stored flag")
	}
	weight := a.weight() + b.weight()
	if weight >= uint32(mt.params.nodeWeightThreshold) {
		weight = uint32(mt.params.singleHashWeight)
		if stored {
			subtreeRoot = true
		} else {
			stored = true
		}
	}
	n.setWeight(weight)
	n.setStored(stored)
	n.setSubtreeRoot(subtreeRoot)
}

/*func (mt *merkleTree) getTreeIndex(node uint32) treeIndex {
	ti := rootIndex
	for node != 0 {
		parent := mt.nodes[node].parent
		ti = ti.parent()
		if mt.nodes[parent].right == node {
			ti = ti.setBit(127, true)
		}
		node = parent
	}
	return ti
}*/

func (mt *merkleTree) setDescendantsCompleted(node uint32, completedAt uint64) {
	panic("TODO")
}

func (mt *merkleTree) setCompleted(node uint32, completedAt uint64) {
	panic("TODO")
	n := &mt.nodes[node]
	if n.parent == nullPtr {
		panic("root node cannot be completed")
	}
	if n.needsRehash() {
		panic("node with unknown hash value cannot be completed")
	}
	mt.setDescendantsCompleted()
	n.setCompleted(mt.finalizedBlock, completedAt)
	for {
		parent := n.parent
		p := &mt.nodes[parent]
		sibling := p.left + p.right - node
		s := &mt.nodes[sibling]
		if !s.isComplete() {
			break
		}
		if n.isStored() && !s.isStored() {
			s.setStored(true)
			s.setWeight(uint32(mt.params.singleHashWeight))
			mt.collapseSubtree(sibling)
		}
		if !n.isStored() && s.isStored() {
			n.setStored(true)
			n.setWeight(uint32(mt.params.singleHashWeight))
			mt.collapseSubtree(node)
		}
		if n.isSubtreeRoot() {
			mt.storedSubtrees = append(mt.storedSubtrees, mt.collapseAndStoreSubtree(n.parent))
		}
		mt.rehashNode(parent)
		if p.isStored() && !n.isStored() {
			mt.collapseSubtree(parent)
		}
		n, node = p, parent
	}
	return
}

func (mt *merkleTree) setFinalizedBlock(finalized uint64) {
	panic("TODO")
}

// completed if weight != 0
func (mt *merkleTree) setValue(node uint32, value merkle.Value, weight uint32, blockNumber uint64) {
	n := &mt.nodes[node]
	n.value = value
	n.setEmptySubtree(false)
	n.setWeight(weight)
	if weight != 0 {
		n.setCompleted(mt.finalizedBlock, blockNumber)
	}
	for n.parent != nullPtr {
		n = &mt.nodes[n.parent]
		if n.needsRehash() {
			break
		}
		n.setRehash(true)
	}
	if weight != 0 {
		mt.setCompleted(node, blockNumber)
	}
}

const (
	tsCollectShapeBits = iota
	tsCollectLeavesAndDelete
	tsDelete
)

func (mt *merkleTree) traverseSubtree(node uint32, action int, encBytes *serializedSubtree, encBitPtr *int) {
	n := &mt.nodes[node]
	if action == tsCollectShapeBits {
		if *encBitPtr == 0 {
			*encBytes = append(*encBytes, 0)
		}
		if n.left == nullPtr {
			(*encBytes)[len(*encBytes)-1] += byte(1) << *encBitPtr
		}
		(*encBitPtr)++
		if *encBitPtr == 8 {
			*encBitPtr = 0
		}
	}
	if n.left == nullPtr {
		if action == tsCollectLeavesAndDelete {
			*encBytes = append(*encBytes, n.value[:]...)
		}
		return
	}
	mt.traverseSubtree(n.left, action, encBytes, encBitPtr)
	mt.traverseSubtree(n.right, action, encBytes, encBitPtr)
	if action == tsCollectLeavesAndDelete || action == tsDelete {
		mt.deleteNode(n.left)
		mt.deleteNode(n.right)
		n.left, n.right = nullPtr, nullPtr
	}
}

func (mt *merkleTree) collapseSubtree(node uint32) {
	mt.traverseSubtree(node, tsDelete, nil, nil)
}

func (mt *merkleTree) collapseAndStoreSubtree(node uint32) (res storedSubtree) {
	var bitPtr int
	mt.traverseSubtree(node, tsCollectShapeBits, &res.nodeEnc, &bitPtr)
	mt.traverseSubtree(node, tsCollectLeavesAndDelete, &res.nodeEnc, nil)
	return
}

func (mt *merkleTree) getStoredSubtrees() storedSubtrees {
	st := mt.storedSubtrees
	mt.storedSubtrees = nil
	sort.Sort(st)
	return st
}

type serializedSubtree []byte

func (s serializedSubtree) shapeBit(bitIndex int) bool {
	return s[bitIndex/8]&(byte(1)<<(bitIndex%8)) != 0
}

func (s serializedSubtree) node(index treeIndex) (value merkle.Value, leaf, internal bool) {
	l := len(s)
	leafCount := (l*8 + 1) / 258
	shapeOffset := l - leafCount*32
	if shapeOffset != (leafCount*2+6)/8 {
		panic("invalid serialized subtree")
	}
	var bitIndex, leafIndex int
	for index != rootIndex {
		if s.shapeBit(bitIndex) {
			return // index points beyond subtree leaf
		}
		bitIndex++
		if index.matchRoot(3) { // right subtree; skip left subtree shape
			for expLeaves := 1; expLeaves > 0; {
				if s.shapeBit(bitIndex) {
					expLeaves--
					leafIndex++
				} else {
					expLeaves++
				}
				bitIndex++
			}
		} else {
			index.matchRoot(2) // left subtree
		}
	}
	if s.shapeBit(bitIndex) { // index points to subtree leaf
		copy(value[:], s[shapeOffset+32*leafIndex:shapeOffset+32*(leafIndex+1)])
		leaf = true
		return
	}
	internal = true // index points to internal node
	return
}

type storedSubtree struct {
	index   treeIndex
	nodeEnc serializedSubtree
}

type storedSubtrees []storedSubtree

func (s storedSubtrees) Len() int           { return len(s) }
func (s storedSubtrees) Less(i, j int) bool { return s[i].index.lessThan(s[j].index) }
func (s storedSubtrees) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// assumes sorted list
func (s storedSubtrees) subtree(index treeIndex) serializedSubtree {
	a, b := 0, len(s)
	for a < b {
		m := (a + b) / 2
		if s[m].index == index {
			return s[m].nodeEnc
		}
		if s[m].index.lessThan(index) {
			a = m + 1
		} else {
			b = m
		}
	}
	return nil
}

type subtreeReader interface {
	subtree(index treeIndex) serializedSubtree
}

type treeNodeReader interface {
	node(index treeIndex) (merkle.Value, uint32)
}

// implements treeNodeReader
type subtreeNodeReader struct {
	params   *Params
	reader   subtreeReader
	cache    *lru.Cache[treeIndex, cachedNode]
	fallback treeNodeReader
}

type cachedNode struct {
	value  merkle.Value
	weight uint32
}

// Note that the non-existence of nodes is also cached, associated with the
// specified global tree index. This assumes that every index is looked up from
// the closest ancestor subtree and it is indeed globally not present in the
// subtree set when not found in the given subtree.
func (n *subtreeNodeReader) nodeFromSubtree(subtree serializedSubtree, subtreeLevel uint, index treeIndex) (merkle.Value, uint32) {
	if node, ok := n.cache.Get(index); ok {
		return node.value, node.weight
	}
	value, leaf, internal := subtree.node(index.subIndex(subtreeLevel))
	var weight uint32
	if leaf {
		weight = uint32(n.params.singleHashWeight)
	}
	if internal {
		left, lw := n.nodeFromSubtree(subtree, subtreeLevel, index.leftChild())
		if lw == 0 {
			panic("child of internal subtree node not found")
		}
		right, rw := n.nodeFromSubtree(subtree, subtreeLevel, index.rightChild())
		if rw == 0 {
			panic("child of internal subtree node not found")
		}
		hasher := sha256.New()
		hasher.Write(left[:])
		hasher.Write(right[:])
		hasher.Sum(value[:0])
		weight = lw + rw
	}
	n.cache.Add(index, cachedNode{value: value, weight: weight})
	return value, uint32(n.params.singleHashWeight)
}

func (n *subtreeNodeReader) node(index treeIndex) (merkle.Value, uint32) {
	if node, ok := n.cache.Get(index); ok {
		return node.value, node.weight
	}
	si := index
	subtreeLevel := index.level()
loop:
	for {
		if subtree := n.reader.subtree(si); subtree != nil {
			if node, weight := n.nodeFromSubtree(subtree, subtreeLevel, index); weight != 0 {
				return node, weight
			} else {
				break loop
			}
		}
		if subtreeLevel == 0 {
			break loop
		}
		si = si.parent()
		subtreeLevel--
	}
	if n.fallback != nil {
		return n.fallback.node(index)
	}
	return merkle.Value{}, 0
}
