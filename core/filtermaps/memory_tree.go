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
	"sync/atomic"
)

type memTree struct {
	nodes     []memTreeNode
	nodeCount int
	roots     map[uint64]uint32
}

type memTreeNode struct {
	node        TreeNode
	left, right uint32
}

type memTreeReader struct {
	tree           *memTree
	lastShiftIndex uint64
	lastHeight     int
	lastNodePos    [64]uint32
}

type memTreeWriter struct {
	memTreeReader
	newRoot uint64
}

func (mtr *memTreeReader) findPosition(index uint64) uint32 {
	height := 63 - bits.LeadingZeros64(index)
	shiftIndex := index << (64 - height)
	nodeHeight := min(bits.LeadingZeros64(shiftIndex^mtr.lastShiftIndex), height, mtr.lastHeight)
	nodePos := mtr.lastNodePos[nodeHeight]
	subIndex := shiftIndex << startHeight
	for nodeHeight < height && mtr.tree.nodes[nodePos].hasChildren() {
		if subIndex&(uint64(1)<<63) == 0 {
			nodePos = mtr.tree.nodes[nodePos].left
		} else {
			nodePos = mtr.tree.nodes[nodePos].right
		}
		subIndex <<= 1
		nodeHeight++
		mtr.lastNodePos[nodeHeight] = nodePos
	}
	mtr.lastShiftIndex, mtr.lastHeight = shiftIndex, nodeHeight
	return nodePos, nodeHeight, nodeHeight == height
}

func (mtr *memTreeReader) get(index uint64) TreeNode {

}

func (mtw *memTreeWriter) set(index uint64, node TreeNode) {
	nodePos, height, ok := mtw.findPosition(index)
	if ok && nodePos >= mtw.newRoot {
		mtw.tree.nodes[nodePos].node = node
		return
	}
}

func (mtw *memTreeWriter) collapse(index uint64) {

}
