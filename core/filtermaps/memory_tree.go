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
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type memTree struct {
	lock      sync.RWMutex
	nodes     []memTreeNode
	nodeCount uint32
	blocks    common.Range[uint64]
	roots     map[uint64]uint32
}

type memTreeNode struct {
	node        TreeNode
	left, right uint32
}

func (mn *memTreeNode) leftChild() uint32  { return mn.left & (uint32(1)<<31 - 1) }
func (mn *memTreeNode) rightChild() uint32 { return mn.right & (uint32(1)<<31 - 1) }
func (mn *memTreeNode) isLeaf() bool       { return mn.leftChild() == uint32(1)<<31-1 }
func (mn *memTreeNode) isKnown() bool      { return mn.left&(uint32(1)<<31) != 0 }
func (mn *memTreeNode) isFinalized() bool  { return mn.right&(uint32(1)<<31) != 0 }
func (mn *memTreeNode) setChildren(left, right uint32) {
	mn.left, mn.right = mn.left&(uint32(1)<<31)+left, mn.right&(uint32(1)<<31)+right
}
func (mn *memTreeNode) setLeaf() { mn.setChildren(uint32(1)<<31-1, uint32(1)<<31-1) }
func (mn *memTreeNode) setKnown(b bool) {
	mn.left &= uint32(1)<<31 - 1
	if b {
		mn.left += uint32(1) << 31
	}
}
func (mn *memTreeNode) setFinalized(b bool) {
	mn.right &= uint32(1)<<31 - 1
	if b {
		mn.right += uint32(1) << 31
	}
}

func (mt *memTree) newReader(blockNumber uint64) *memTreeView {
	mt.lock.RLock()
	defer mt.lock.RUnlock()

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

	if mt.needExpand() {
		mt.expand()
	}
	newRoot := mt.addNode()
	if blockNumber > 0 {
		parentRoot, ok := mt.roots[blockNumber-1]
		if !ok {
			panic("parent block missing from memory tree")
		}
		mt.nodes[newRoot] = mt.nodes[parentRoot]
	} else {
		mt.nodes[newRoot] = memTreeNode{left: 1<<31 - 1, right: 1<<31 - 1}
	}
	mt.roots[blockNumber] = newRoot
	mv := &memTreeView{tree: mt}
	mv.lastNodePos[0] = newRoot
	return mv
}

// assumes read or write lock
func (mt *memTree) needExpand() bool {
	return uint32(len(mt.nodes)) < mt.nodeCount+1000
}

// assumes write lock
func (mt *memTree) expand() {
	mt.nodes = append(mt.nodes, make([]memTreeNode, len(mt.nodes)/8+1000)...)
}

// assumes read or write lock
func (mt *memTree) addNode() uint32 {
	newNode := mt.nodeCount
	mt.nodeCount++
	return newNode
}

func (mt *memTree) hashNode(index treeIndex, nodeIndex uint32) {
	node := &mt.nodes[nodeIndex]
	if node.isKnown() {
		return
	}
	if node.isLeaf() {
		fmt.Printf("unknown %016x%016x\n", index.hi, index.lo)
		panic("unknown leaf encountered during hashing")
	}
	mt.hashNode(index.append(0, 1), node.leftChild())
	mt.hashNode(index.append(1, 1), node.rightChild())
	hasher := sha256.New()
	hasher.Write(mt.nodes[node.leftChild()].node[:])
	hasher.Write(mt.nodes[node.rightChild()].node[:])
	hasher.Sum(node.node[:0])
	node.setKnown(true)
}

func (mt *memTree) prune(beforeBlock uint64) {
	mt.lock.Lock()
	defer mt.lock.Unlock()

	if !mt.blocks.Includes(beforeBlock) {
		panic("invalid prune limit block number")
	}
	nodeBoundary := mt.roots[beforeBlock]
	posMap := make([]uint32, nodeBoundary)
	// mark nodes referenced by first remaining block
	var mark func(nodePos uint32)
	mark = func(nodePos uint32) {
		posMap[nodePos] = 1
		if node := &mt.nodes[nodePos]; !node.isFinalized() && !node.isLeaf() {
			mark(node.leftChild())
			mark(node.rightChild())
		}
	}
	mark(nodeBoundary)
	var newPos uint32
	for pos, v := range posMap {
		if v == 0 {
			continue
		}
		posMap[pos] = newPos
		if newPos != uint32(pos) {
			mt.nodes[newPos] = mt.nodes[pos]
		}
		newPos++
	}
	copy(mt.nodes[newPos:mt.nodeCount+newPos-nodeBoundary], mt.nodes[nodeBoundary:mt.nodeCount])
	for pos := mt.nodeCount + newPos - nodeBoundary; pos < mt.nodeCount; pos++ {
		mt.nodes[pos] = memTreeNode{}
	}

	posMapping := func(oldPos uint32) uint32 {
		if oldPos < nodeBoundary {
			return posMap[oldPos]
		}
		return oldPos + newPos - nodeBoundary
	}
	for pos := range mt.nodeCount {
		node := &mt.nodes[pos]
		if !node.isLeaf() {
			node.setChildren(posMapping(node.leftChild()), posMapping(node.rightChild()))
		}
	}
	for block, root := range mt.roots {
		if block < beforeBlock {
			delete(mt.roots, block)
		} else {
			mt.roots[block] = posMapping(root)
		}
	}
	mt.blocks.SetFirst(beforeBlock)
	mt.nodeCount = posMapping(mt.nodeCount)
}

func (mt *memTree) knownNodes() (res uint32) {
	for _, node := range mt.nodes[:mt.nodeCount] {
		if node.isKnown() {
			res++
		}
	}
	return
}

type memTreeView struct {
	tree           *memTree
	lastShiftIndex treeIndex
	lastHeight     uint
	lastNodePos    [128]uint32
}

func (mv *memTreeView) findPosition(index treeIndex) (uint32, uint, uint) {
	/*fmt.Printf("find %016x%016x\n", index.hi, index.lo)
	for i, node := range mv.tree.nodes[:mv.tree.nodeCount] {
		fmt.Println(" *", i, node.leftChild(), node.rightChild())
	}*/
	height := 127 - index.leadingZeros()
	shiftIndex := index.shiftLeft(128 - height)
	//fmt.Printf(" shi %016x%016x\n", shiftIndex.hi, shiftIndex.lo)
	nodeHeight := min(shiftIndex.xor(mv.lastShiftIndex).leadingZeros(), height, mv.lastHeight)
	//fmt.Println("  ht", nodeHeight)
	nodePos := mv.lastNodePos[nodeHeight]
	for nodeHeight < height && !mv.tree.nodes[nodePos].isLeaf() {
		//fmt.Println("  fp", height, nodeHeight, nodePos)
		if shiftIndex.bit(127-nodeHeight) == 0 {
			nodePos = mv.tree.nodes[nodePos].leftChild()
		} else {
			nodePos = mv.tree.nodes[nodePos].rightChild()
		}
		nodeHeight++
		mv.lastNodePos[nodeHeight] = nodePos
	}
	//fmt.Println("  nt", nodeHeight)
	mv.lastShiftIndex, mv.lastHeight = shiftIndex, nodeHeight
	return nodePos, nodeHeight, height - nodeHeight
}

func (mv *memTreeView) tryGet(index treeIndex) (TreeNode, bool, uint) {
	//fmt.Printf("tget %016x%016x\n", index.hi, index.lo)
	mv.tree.lock.RLock()
	defer mv.tree.lock.RUnlock()

	nodePos, _, below := mv.findPosition(index)
	n := &mv.tree.nodes[nodePos]
	return n.node, n.isKnown(), below
}

func (mv *memTreeView) get(index treeIndex) TreeNode {
	//fmt.Printf("get  %016x%016x\n", index.hi, index.lo)
	mv.tree.lock.RLock()
	defer mv.tree.lock.RUnlock()

	nodePos, _, below := mv.findPosition(index)
	if below != 0 {
		fmt.Printf("!exist  %016x%016x\n", index.hi, index.lo)
		panic("cannot read non-existent node")
	}
	n := &mv.tree.nodes[nodePos]
	if !n.isKnown() {
		fmt.Printf("unknown  %016x%016x\n", index.hi, index.lo)
		panic("cannot read unknown node contents")
	}
	return n.node
}

func (mv *memTreeView) isLeaf(index treeIndex) bool {
	mv.tree.lock.RLock()
	defer mv.tree.lock.RUnlock()

	nodePos, _, below := mv.findPosition(index)
	if below != 0 {
		panic("cannot read non-existent node")
	}
	n := &mv.tree.nodes[nodePos]
	return n.isLeaf()
}

func (mv *memTreeView) isKnown(index treeIndex) bool {
	mv.tree.lock.RLock()
	defer mv.tree.lock.RUnlock()

	nodePos, _, below := mv.findPosition(index)
	if below != 0 {
		panic("cannot read non-existent node")
	}
	n := &mv.tree.nodes[nodePos]
	return n.isKnown()
}

func (mv *memTreeView) addNewPath(index treeIndex, oldHeight uint, copyOldNodes bool) *memTreeNode {
	nodeHeight := oldHeight
	//fmt.Println(" anp", mv.lastNodePos[:nodeHeight+1])
	for mv.lastNodePos[nodeHeight] < mv.lastNodePos[0] {
		nodeHeight--
	}
	nodePos := mv.lastNodePos[nodeHeight]
	node := &mv.tree.nodes[nodePos]
	oldNode := node
	if oldNode.isKnown() {
		oldNode.setKnown(false) // we assume that no tree hashing happened and only leaves of the new writer view can be known
	}
	targetHeight := 127 - index.leadingZeros()
	//fmt.Println("    ", nodeHeight, targetHeight)
	for nodeHeight < targetHeight {
		newNodePos := mv.tree.addNode()
		var newSibling uint32
		copySibling := oldNode != nil && !oldNode.isLeaf()
		if !copySibling {
			newSibling = mv.tree.addNode()
			mv.tree.nodes[newSibling].setLeaf()
		}
		if index.bit(targetHeight-nodeHeight-1) == 0 {
			if copyOldNodes {
				mv.tree.nodes[newNodePos] = mv.tree.nodes[oldNode.leftChild()]
			}
			if copySibling {
				newSibling = oldNode.rightChild()
			}
			node.setChildren(newNodePos, newSibling)
		} else {
			if copyOldNodes {
				mv.tree.nodes[newNodePos] = mv.tree.nodes[oldNode.rightChild()]
			}
			if copySibling {
				newSibling = oldNode.leftChild()
			}
			node.setChildren(newSibling, newNodePos)
		}
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

func (mv *memTreeView) set(index treeIndex, value TreeNode) {
	//fmt.Printf("set  %016x%016x\n", index.hi, index.lo)
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
	node := mv.addNewPath(index, oldHeight, false)
	node.node = value
	node.setLeaf()
	node.setKnown(true)
}

func (mv *memTreeView) finalize(index treeIndex) {
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

	_, oldHeight, below := mv.findPosition(index)
	if below != 0 {
		fmt.Printf("finalize  %016x%016x  below %d\n", index.hi, index.lo, below)
		panic("cannot finalize non-existent node")
	}
	node := mv.addNewPath(index, oldHeight, true)
	node.setFinalized(true)
}

func (mv *memTreeView) rootHash() common.Hash {
	mv.tree.hashNode(rootIndex, mv.lastNodePos[0])
	return common.Hash(mv.tree.nodes[mv.lastNodePos[0]].node)
}
