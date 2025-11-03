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

	"github.com/ethereum/go-ethereum/beacon/merkle"
)

type merkleTreeNode struct {
	value                     merkle.Value
	meta, parent, left, right uint32
}

var (
	mnWeightMask      = (uint32(1) << 28) - 1
	mnStoredMask      = uint32(1) << 28
	mnSubtreeRootMask = uint32(1) << 29
	mnUnknownMask     = uint32(1) << 30
	mnCompleteMask    = uint32(1) << 31
)

func (n *merkleTreeNode) isUnknown() bool     { return n.meta&mnUnknownMask != 0 }
func (n *merkleTreeNode) isComplete() bool    { return n.meta&mnCompleteMask != 0 }
func (n *merkleTreeNode) isStored() bool      { return n.meta&mnStoredMask != 0 }
func (n *merkleTreeNode) isSubtreeRoot() bool { return n.meta&mnSubtreeRootMask != 0 }
func (n *merkleTreeNode) weight() uint32      { return n.meta & mnWeightMask }

var nullPtr = uint32(math.MaxUint32)

type merkleTree struct {
	params         *Params
	nodes          []merkleTreeNode
	firstEmpty     uint32
	storedSubtrees []storedSubtree
}

func (params *Params) newMerkleTree() *merkleTree {
	return &merkleTree{
		params: params,
		nodes: []merkleTreeNode{merkleTreeNode{
			meta:   mnUnknownMask,
			parent: nullPtr,
			left:   nullPtr,
			right:  nullPtr,
		}},
		firstEmpty: nullPtr,
	}
}

func (mt *merkleTree) deleteNode(node uint32) {
	mt.nodes[node].right = mt.firstEmpty
	mt.firstEmpty = node
}

func (mt *merkleTree) newNode() uint32 {
	if mt.firstEmpty != nullPtr {
		node := mt.firstEmpty
		mt.firstEmpty = mt.nodes[node].right
		return node
	}
	node := uint32(len(mt.nodes))
	mt.nodes = append(mt.nodes, merkleTreeNode{})
	return node
}

func (mt *merkleTree) hashNode(node uint32) {
	n := &mt.nodes[node]
	if n.left == nullPtr || n.right == nullPtr {
		panic("cannot hash node with no children")
	}
	a := &mt.nodes[n.left]
	b := &mt.nodes[n.right]
	if a.isUnknown() {
		mt.hashNode(n.left)
	}
	if b.isUnknown() {
		mt.hashNode(n.right)
	}
	hasher := sha256.New()
	hasher.Write(a.value[:])
	hasher.Write(b.value[:])
	hasher.Sum(n.value[:0])
	n.meta = 0
	if !a.isComplete() || !b.isComplete() {
		return
	}
	n.meta += mnCompleteMask
	stored := a.isStored()
	if b.isStored() != stored {
		panic("completed siblings with different stored flag")
	}
	weight := a.meta&mnWeightMask + b.meta&mnWeightMask
	if weight >= mt.params.nodeWeightThreshold {
		weight = mt.params.singleHashWeight
		if stored {
			n.meta += mnSubtreeRootMask
		} else {
			stored = true
		}
	}
	n.meta += weight
	if stored {
		n.meta += mnStoredMask
	}
}

func (mt *merkleTree) getTreeIndex(node uint32) treeIndex {
	ti := rootIndex
	for node != 0 {
		parent := mt.nodes[node].parent
		ti = ti.shiftRight(1)
		if mt.nodes[parent].right == node {
			ti.hi += uint64(1) << 63
		}
		node = parent
	}
	return ti
}

func (mt *merkleTree) setCompleted(node uint32) {
	if node == 0 {
		panic("root node cannot be completed")
	}
	n := &mt.nodes[node]
	if n.isUnknown() {
		panic("unknown node cannot be completed")
	}
	n.meta |= mnCompleteMask
	for {
		parent := n.parent
		p := &mt.nodes[parent]
		sibling := p.left + p.right - node
		s := &mt.nodes[sibling]
		if !s.isComplete() {
			break
		}
		if n.isStored() && !s.isStored() {
			s.meta = mnStoredMask + mnCompleteMask + mt.params.singleHashWeight
			mt.collapseSubtree(sibling)
		}
		if !n.isStored() && s.isStored() {
			n.meta = mnStoredMask + mnCompleteMask + mt.params.singleHashWeight
			mt.collapseSubtree(node)
		}
		if n.isSubtreeRoot() {
			mt.storedSubtrees = append(mt.storedSubtrees, mt.collapseAndStoreSubtree(n.parent))
		}
		mt.hashNode(parent)
		if p.isStored() && !n.isStored() {
			mt.collapseSubtree(parent)
		}
		n, node = p, parent
	}
	return
}

// completed if weight != 0
func (mt *merkleTree) setValue(node uint32, value merkle.Value, weight uint32) {
	n := &mt.nodes[node]
	n.value = value
	n.meta = weight
	if weight != 0 {
		n.meta += mnCompleteMask
	}
	for n.parent != nullPtr {
		n := &mt.nodes[n.parent]
		if n.isUnknown() {
			break
		}
		n.meta = mnUnknownMask
	}
	if weight != 0 {
		mt.setCompleted(node)
	}
}

const (
	tsCollectShapeBits = iota
	tsCollectLeavesAndDelete
	tsDelete
)

func (mt *merkleTree) traverseSubtree(node uint32, action int, encBytes *[]byte, encBitPtr *int) {
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

type storedSubtree struct {
	index   treeIndex
	nodeEnc []byte
}

/*func (s []storedSubtree) Len() int           { return len(s) }
func (s []storedSubtree) Less(i, j int) bool { return s[i].index.lessThan(s[j].index) }
func (s []storedSubtree) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }*/

type treeIndex struct {
	lo, hi uint64
}

var rootIndex = treeIndex{hi: uint64(1) << 63}

func (a treeIndex) lessThan(b treeIndex) bool {
	return a.hi < b.hi || (a.hi == b.hi && a.lo < b.lo)
}

func (t treeIndex) shiftLeft(b uint) treeIndex {
	if b >= 64 {
		return treeIndex{hi: t.lo << (b - 64)}
	}
	return treeIndex{lo: t.lo << b, hi: t.hi<<b + t.lo>>(64-b)}
}

func (t treeIndex) shiftRight(b uint) treeIndex {
	if b >= 64 {
		return treeIndex{lo: t.hi >> (b - 64)}
	}
	return treeIndex{lo: t.lo>>b + t.hi<<(64-b), hi: t.hi >> b}
}
