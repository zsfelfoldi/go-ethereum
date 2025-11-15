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

// Note: since merkleTreeNodes account for a big part of the log indexer memory
// usage, the three node pointers and three boolean flags have been merged into
// three uint32s consisting of a 31 bit node pointer and a 1 bit flag each.
type merkleTreeNode struct {
	value                                                          merkle.Value
	weight                                                         float32
	parentAndEmptySubtree, leftAndIsValueKnown, rightAndIsComplete uint32
}

var (
	mnFlagMask  = uint32(1) << 31
	mnValueMask = mnFlagMask - 1
	nullPtr     = mnValueMask // no parent/child
	rootPtr     = uint32(0)   // merkle tree root is always at index 0
)

func (n *merkleTreeNode) isEmptySubtree() bool   { return n.parentAndEmptySubtree&mnFlagMask != 0 }
func (n *merkleTreeNode) isValueKnown() bool     { return n.leftAndIsValueKnown&mnFlagMask != 0 }
func (n *merkleTreeNode) isComplete() bool       { return n.rightAndIsComplete&mnFlagMask != 0 }
func (n *merkleTreeNode) parent() uint32         { return n.parentAndEmptySubtree & mnValueMask }
func (n *merkleTreeNode) left() uint32           { return n.leftAndIsValueKnown & mnValueMask }
func (n *merkleTreeNode) right() uint32          { return n.rightAndIsComplete & mnValueMask }
func (n *merkleTreeNode) setEmptySubtree(f bool) { n.setFlag(&n.parentAndEmptySubtree, f) }
func (n *merkleTreeNode) setValueKnown(f bool)   { n.setFlag(&n.leftAndIsValueKnown, f) }
func (n *merkleTreeNode) setComplete(f bool)     { n.setFlag(&n.rightAndIsComplete, f) }
func (n *merkleTreeNode) setParent(v uint32)     { n.setValue(&n.parentAndEmptySubtree, v) }
func (n *merkleTreeNode) setLeft(v uint32)       { n.setValue(&n.leftAndIsValueKnown, v) }
func (n *merkleTreeNode) setRight(v uint32)      { n.setValue(&n.rightAndIsComplete, v) }
func (n *merkleTreeNode) setFlag(u *uint32, f bool) {
	if f {
		*u |= mnFlagMask
	} else {
		*u &= mnValueMask
	}
}
func (n *merkleTreeNode) setValue(u *uint32, v uint32) {
	if v > mnValueMask {
		panic("invalid node pointer")
	}
	*u = (*u & mnFlagMask) + v
}

type merkleTree struct {
	params      *Params
	nodes       []merkleTreeNode
	firstFree   uint32
	emptyValues map[merkle.Value]struct{ left, right merkle.Value } //TODO
	subtrees    storedSubtrees
}

func (params *Params) newMerkleTree() *merkleTree {
	return &merkleTree{
		params: params,
		nodes: []merkleTreeNode{merkleTreeNode{
			parentAndEmptySubtree: nullPtr,
			leftAndIsValueKnown:   nullPtr,
			rightAndIsComplete:    nullPtr,
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
		parentAndEmptySubtree: parent,
		leftAndIsValueKnown:   nullPtr,
		rightAndIsComplete:    nullPtr,
	}
	if mt.firstFree != nullPtr {
		node := mt.firstFree
		mt.firstFree = mt.nodes[node].right()
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
		if n.left() == nullPtr {
			if !n.isEmptySubtree() {
				panic("cannot expand non-empty subtree")
			}
			children, ok := mt.emptyValues[n.value]
			if !ok {
				panic("unknown empty subtree hash")
			}
			n.left = mt.newNode(node)
			mt.nodes[n.left].value = children.left
			mt.nodes[n.left].setValueKnown(true)
			mt.nodes[n.left].setEmptySubtree(true)
			n.right = mt.newNode(node)
			mt.nodes[n.right].value = children.right
			mt.nodes[n.right].setValueKnown(true)
			mt.nodes[n.right].setEmptySubtree(true)
		}
		switch {
		case subIndex.matchRoot(2):
			node = n.left()
		case subIndex.matchRoot(3):
			node = n.right()
		default:
			panic("invalid descendant subIndex")
		}
	}
	return node
}

func (mt *merkleTree) setComplete(node uint32) {
	n := &mt.nodes[node]
	if n.isComplete() {
		return
	}
	if n.left != nullPtr {
		mt.setComplete(n.left)
		mt.setComplete(n.right)
		return
	}
	if !n.isValueKnown() {
		panic("finalized node with unknown value")
	}
	n.setCompleted(true)
	// propagate completed state to ancestors and collapse completed subtrees if possible
	for n.parent() != nullPtr {
		parent := n.parent()
		p := &mt.nodes[parent]
		sibling := p.left() + p.right() - node
		s := &mt.nodes[sibling]
		if !s.isComplete() {
			break
		}
		p.setComplete(true)
		p.setWeight(n.weight().add(s.weight()))
		if !p.isValueKnown() {
			mt.getValue(parent)
		}
		if pl := mt.params.storageLevel(p.weight); pl == 0 {
			mt.collapseSubtree(parent)
		} else {
			if mt.params.storageLevel(n.weight) < pl {
				mt.subtrees = append(mt.subtrees, mt.collapseAndStoreSubtree(node))
			}
			if mt.params.storageLevel(s.weight) < pl {
				mt.subtrees = append(mt.subtrees, mt.collapseAndStoreSubtree(sibling))
			}
		}
		n, node = p, parent
	}
	return subtrees
}

func (mt *merkleTree) setValue(node uint32, value merkle.Value, weight float32) {
	n := &mt.nodes[node]
	n.value = value
	n.weight = weight
	n.setEmptySubtree(false)
	n.setValueKnown(true)
	// mark ancestors as value unknown (needs re-hashing)
	for n.parent() != nullPtr {
		n = &mt.nodes[n.parent()]
		if !n.isValueKnown() {
			break
		}
		n.setValueKnown(false)
	}
}

func (mt *merkleTree) getValue(node uint32) (merkle.Value, float32) {
	n := &mt.nodes[node]
	if !n.isValueKnown() {
		lv, _ := mt.getValue(n.left())
		rv, _ := mt.getValue(n.right())
		hasher := sha256.New()
		hasher.Write(lv[:])
		hasher.Write(rv[:])
		hasher.Sum(n.value[:0])
		n.setEmptySubtree(a.isEmptySubtree() && b.isEmptySubtree())
		n.setValueKnown(true)
	}
	return n.value, n.weight
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
	if n.left() == nullPtr {
		if action == tsCollectLeavesAndDelete {
			*encBytes = append(*encBytes, n.value[:]...)
		}
		return
	}
	mt.traverseSubtree(n.left(), action, encBytes, encBitPtr)
	mt.traverseSubtree(n.right(), action, encBytes, encBitPtr)
	if action == tsCollectLeavesAndDelete || action == tsDelete {
		mt.deleteNode(n.left())
		n.setLeft(nullPtr)
		mt.deleteNode(n.right())
		n.setRight(nullPtr)
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
	sort.Sort(mt.subtrees)
	return mt.subtrees
}

func (mt *merkleTree) clearStoredSubtrees() {
	mt.subtrees = nil
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
	weight  float32
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
