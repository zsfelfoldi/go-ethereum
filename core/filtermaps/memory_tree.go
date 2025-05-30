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
	//"crypto/sha256"
	"math/bits"
	"sync"
)

type memTree struct {
	lock      sync.RWMutex
	nodes     []memTreeNode
	nodeCount uint32
	roots     map[uint64]uint32
}

type memTreeNode struct {
	node        TreeNode
	left, right uint32
}

func (mn *memTreeNode) leftChild() uint32  { return mn.left & (uint32(1)<<31 - 1) }
func (mn *memTreeNode) rightChild() uint32 { return mn.right & (uint32(1)<<31 - 1) }
func (mn *memTreeNode) isEdge() bool       { return mn.leftChild() == uint32(1)<<31-1 }
func (mn *memTreeNode) isKnown() bool      { return mn.left&(uint32(1)<<31) != 0 }
func (mn *memTreeNode) isCollapsed() bool  { return mn.right&(uint32(1)<<31) != 0 }
func (mn *memTreeNode) setChildren(left, right uint32) {
	mn.left, mn.right = mn.left&(uint32(1)<<31)+left, mn.right&(uint32(1)<<31)+right
}
func (mn *memTreeNode) setEdge() { mn.setChildren(uint32(1)<<31-1, uint32(1)<<31-1) }
func (mn *memTreeNode) setKnown(b bool) {
	mn.left &= uint32(1)<<31 - 1
	if b {
		mn.left += uint32(1) << 31
	}
}
func (mn *memTreeNode) setCollapsed(b bool) {
	mn.right &= uint32(1)<<31 - 1
	if b {
		mn.right += uint32(1) << 31
	}
}

func (mt *memTree) newReader(blockNumber uint64) *memTreeView {
	mt.lock.Lock()
	defer mt.lock.Unlock()

	root, ok := mt.roots[blockNumber]
	if !ok {
		panic("block number missing from memory tree")
	}
	mv := &memTreeView{tree: mt}
	mv.lastNodePos[0] = root
	return mv
}

func (mt *memTree) newWriter(blockNumber uint64) *memTreeView {
	mt.lock.Lock()
	defer mt.lock.Unlock()

	parentRoot, ok := mt.roots[blockNumber-1]
	if !ok {
		panic("parent block missing from memory tree")
	}
	if mt.needExpand() {
		mt.expand()
	}
	newRoot := mt.addNode()
	mt.nodes[newRoot] = mt.nodes[parentRoot]
	mt.roots[blockNumber] = newRoot
	mv := &memTreeView{tree: mt}
	mv.lastNodePos[0] = newRoot
	return mv
}

// assumes read or write lock
func (mt *memTree) needExpand() bool {
	return uint32(len(mt.nodes)) < mt.nodeCount+100
}

// assumes write lock
func (mt *memTree) expand() {
	mt.nodes = append(mt.nodes, make([]memTreeNode, len(mt.nodes)/8+100)...)
}

// assumes read or write lock
func (mt *memTree) addNode() uint32 {
	newNode := mt.nodeCount
	mt.nodeCount++
	return newNode
}

type memTreeView struct {
	tree           *memTree
	lastShiftIndex uint64
	lastHeight     int
	lastNodePos    [64]uint32
}

func (mv *memTreeView) findPosition(index uint64) (uint32, int, bool) {
	height := 63 - bits.LeadingZeros64(index)
	shiftIndex := index << (64 - height)
	nodeHeight := min(bits.LeadingZeros64(shiftIndex^mv.lastShiftIndex), height, mv.lastHeight)
	nodePos := mv.lastNodePos[nodeHeight]
	subIndex := shiftIndex << nodeHeight
	for nodeHeight < height && !mv.tree.nodes[nodePos].isEdge() {
		if subIndex&(uint64(1)<<63) == 0 {
			nodePos = mv.tree.nodes[nodePos].leftChild()
		} else {
			nodePos = mv.tree.nodes[nodePos].rightChild()
		}
		subIndex <<= 1
		nodeHeight++
		mv.lastNodePos[nodeHeight] = nodePos
	}
	mv.lastShiftIndex, mv.lastHeight = shiftIndex, nodeHeight
	return nodePos, nodeHeight, nodeHeight == height
}

func (mv *memTreeView) get(index uint64) (TreeNode, bool) {
	mv.tree.lock.RLock()
	defer mv.tree.lock.RUnlock()

	nodePos, _, ok := mv.findPosition(index)
	if !ok {
		return TreeNode{}, false
	}
	n := &mv.tree.nodes[nodePos]
	return n.node, n.isKnown()
}

func (mv *memTreeView) addNewPath(index uint64, oldHeight int) *memTreeNode {
	nodeHeight := oldHeight
	for mv.lastNodePos[nodeHeight] < mv.lastNodePos[0] {
		nodeHeight--
	}
	nodePos := mv.lastNodePos[nodeHeight]
	node := &mv.tree.nodes[nodePos]
	oldNode := node
	targetHeight := 63 - bits.LeadingZeros64(index)
	subIndex := index << (64 + nodeHeight - targetHeight)
	for nodeHeight < targetHeight {
		newNodePos := mv.tree.addNode()
		var newSibling uint32
		if oldNode == nil {
			newSibling = mv.tree.addNode()
			mv.tree.nodes[newSibling].setEdge()
		}
		if subIndex&(uint64(1)<<63) == 0 {
			if oldNode != nil {
				newSibling = oldNode.rightChild()
			}
			node.setChildren(newNodePos, newSibling)
		} else {
			if oldNode != nil {
				newSibling = oldNode.leftChild()
			}
			node.setChildren(newSibling, newNodePos)
		}
		subIndex <<= 1
		nodeHeight++
		nodePos = newNodePos
		node = &mv.tree.nodes[nodePos]
		if nodeHeight < oldHeight {
			oldNode = &mv.tree.nodes[mv.lastNodePos[nodeHeight]]
		} else {
			oldNode = nil
		}
		mv.lastNodePos[nodeHeight] = nodePos
	}
	return node
}

func (mv *memTreeView) set(index uint64, value TreeNode) {
	mv.tree.lock.RLock()
	defer func() {
		expand := mv.tree.needExpand()
		mv.tree.lock.RUnlock()
		if expand {
			mv.tree.lock.Lock()
			mv.tree.expand()
			mv.tree.lock.Unlock()
		}
	}()

	_, oldHeight, _ := mv.findPosition(index)
	node := mv.addNewPath(index, oldHeight)
	node.node = value
	node.setEdge()
	node.setKnown(true)
}

func (mv *memTreeView) collapse(index uint64) {
	mv.tree.lock.RLock()
	defer func() {
		expand := mv.tree.needExpand()
		mv.tree.lock.RUnlock()
		if expand {
			mv.tree.lock.Lock()
			mv.tree.expand()
			mv.tree.lock.Unlock()
		}
	}()

	oldPos, oldHeight, ok := mv.findPosition(index)
	if !ok {
		panic("trying to collapse non-existent node")
	}
	node := mv.addNewPath(index, oldHeight)
	*node = mv.tree.nodes[oldPos]
	node.setCollapsed(true)
}
