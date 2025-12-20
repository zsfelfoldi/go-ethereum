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
	"encoding/binary"
	"math"
	"sort"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common/lru"
)

const (
	//
	mtaUnknown  = iota // does not exist or outside the known range
	mtaInternal        // value unknown but has known descendants
	mtaKnown           // value known

	mtsEmpty = iota
	mtsPartial
	mtsComplete
)

type (
	nodeReader interface {
		getNode(index treeIndex) (nw nodeWithWeight, avail int, err error)
	}
	treeInitReader interface {
		initNode(index treeIndex) (nw nodeWithWeight, avail, status int, err error)
	}
	subtreeReader interface {
		getSubtree(index treeIndex) (serializedSubtree, error)
	}
)

func treeHash(left, right merkle.Value) (result merkle.Value) {
	hasher := sha256.New()
	hasher.Write(left[:])
	hasher.Write(right[:])
	hasher.Sum(result[:0])
	return
}

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
	params    *Params
	nodes     []merkleTreeNode
	firstFree uint32
	subtrees  storedSubtrees
}

func (params *Params) newMerkleTree(reader treeInitReader) (*merkleTree, error) {
	mt := &merkleTree{
		params: params,
		nodes: []merkleTreeNode{merkleTreeNode{
			parentAndEmptySubtree: nullPtr,
			leftAndIsValueKnown:   nullPtr,
			rightAndIsComplete:    nullPtr,
		}},
		firstFree: nullPtr,
	}
	nw, avail, status, err := reader.initNode(rootIndex)
	if err != nil {
		return nil, err
	}
	if err := mt.initTree(reader, rootIndex, rootPtr, nw, avail, status); err != nil {
		return nil, err
	}
	return mt, nil
}

func (mt *merkleTree) initTree(reader treeInitReader, index treeIndex, node uint32, nw nodeWithWeight, avail, status int) error {
	n := &mt.nodes[node]
	var recursiveInit bool
	switch avail {
	case mtaInternal:
		recursiveInit = true
	case mtaKnown:
		n.value, n.weight = nw.value, nw.weight
		n.setValueKnown(true)
	default:
		panic("invalid node availability from tree init reader")
	}
	switch status {
	case mtsEmpty:
		n.setEmptySubtree(true)
	case mtsPartial:
		recursiveInit = true
	case mtsComplete:
		mt.setComplete(node, index)
	default:
		panic("invalid node status from tree init reader")
	}
	if !recursiveInit {
		return nil
	}
	// initialize descendants recursively
	nwLeft, availLeft, statusLeft, err := reader.initNode(index.leftChild())
	if err != nil {
		return err
	}
	nwRight, availRight, statusRight, err := reader.initNode(index.leftChild())
	if err != nil {
		return err
	}
	if availLeft != mtaUnknown && availRight != mtaUnknown {
		n.setLeft(mt.newNode(node))
		if err := mt.initTree(reader, index.leftChild(), n.left(), nwLeft, availLeft, statusLeft); err != nil {
			return err
		}
		n.setRight(mt.newNode(node))
		if err := mt.initTree(reader, index.rightChild(), n.right(), nwRight, availRight, statusRight); err != nil {
			return err
		}
	} else {
		if avail != mtaKnown {
			panic("unknown internal node with no descendants")
		}
	}
	return nil
}

func (mt *merkleTree) deleteNode(node uint32) {
	mt.nodes[node].setRight(mt.firstFree)
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

type mtNode struct {
	node  uint32
	empty *emptySubtree
}

func (mt *merkleTree) getDescendant(node mtNode, subIndex treeIndex) mtNode {
	for subIndex != rootIndex {
		n := &mt.nodes[node.node]
		if n.left() == nullPtr {
			if !n.isEmptySubtree() {
				panic("cannot expand non-empty subtree")
			}
			n.setLeft(mt.newNode(node.node))
			l := &mt.nodes[n.left()]
			l.value = node.empty.left.value
			l.setValueKnown(true)
			l.setEmptySubtree(true)
			n.setRight(mt.newNode(node.node))
			r := &mt.nodes[n.right()]
			l.value = node.empty.right.value
			r.setValueKnown(true)
			r.setEmptySubtree(true)
		}
		switch {
		case subIndex.matchRoot(2):
			node = mtNode{node: n.left(), empty: node.empty.left}
		case subIndex.matchRoot(3):
			node = mtNode{node: n.right(), empty: node.empty.right}
		default:
			panic("invalid descendant subIndex")
		}
	}
	return node
}

func (mt *merkleTree) setComplete(node uint32, index treeIndex) {
	n := &mt.nodes[node]
	if n.isComplete() {
		return
	}
	if n.left() != nullPtr {
		mt.setComplete(n.left(), index.leftChild())
		mt.setComplete(n.right(), index.rightChild())
		return
	}
	if !n.isValueKnown() {
		panic("finalized node with unknown value")
	}
	n.setComplete(true)
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
		p.weight = n.weight + s.weight
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
		n.value = treeHash(lv, rv)
		n.setEmptySubtree(mt.nodes[n.left()].isEmptySubtree() && mt.nodes[n.right()].isEmptySubtree())
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
		if n.left() == nullPtr {
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
			var weightEnc [4]byte
			binary.LittleEndian.PutUint32(weightEnc[:], math.Float32bits(n.weight))
			*encBytes = append(*encBytes, weightEnc[:]...)
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

func (mt *merkleTree) collapseAndStoreSubtree(node uint32, index treeIndex) (res storedSubtree) {
	var bitPtr int
	res.index = index
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
	return s[4+bitIndex/8]&(byte(1)<<(bitIndex%8)) != 0
}

func (s serializedSubtree) getNode(index treeIndex) (node nodeWithWeight, nodeType int) {
	l := len(s)
	leafCount := (l*8 + 1) / ((32+4)*8 + 2)
	shapeOffset := l - leafCount*(32+4)
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
		offset := shapeOffset + (32+4)*leafIndex
		copy(node.value[:], s[offset:offset+32])
		node.weight = math.Float32frombits(binary.LittleEndian.Uint32(s[offset+32 : offset+32+4]))
		nodeType = mtaKnown
		return
	}
	nodeType = mtaInternal
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
func (s storedSubtrees) getSubtree(index treeIndex) serializedSubtree {
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

// implements nodeReader based on a subtreeReader
type subtreeNodeReader struct {
	//params   *Params
	reader subtreeReader
	// non-existent entries are cached in both caches
	cache *lru.Cache[treeIndex, cachedSubtreeNode]
}

type cachedSubtreeNode struct {
	subtree      serializedSubtree
	subtreeLevel uint
	node         nodeWithWeight
	nodeType     int
}

type nodeWithWeight struct {
	value  merkle.Value
	weight float32
}

func (r *subtreeNodeReader) getSubtreeAndNode(index treeIndex) (cachedSubtreeNode, error) {
	if sn, ok := r.cache.Get(index); ok {
		return sn, nil
	}
	st, err := r.reader.getSubtree(index)
	if err != nil {
		return cachedSubtreeNode{}, err
	}
	var sn cachedSubtreeNode
	if s != nil {
		sn = cachedSubtreeNode{
			subtree:      st,
			subtreeLevel: index.level(),
		}
	} else {
		if index != rootIndex {
			sn = r.getSubtreeAndNode(index.parent())
		}
	}
	if sn.subtree != nil {
		sn.node, sn.nodeType = sn.subtree.getNode(index.subIndex(sn.subtreeLevel))
	}
	r.subtreeCache.Add(index, sn)
	return cs, nil
}

func (r *subtreeNodeReader) getNode(index treeIndex) (nodeWithWeight, int, error) {
	if sn, err := r.getSubtreeAndNode(index); err == nil {
		return sn.node, sn.nodeType, nil
	} else {
		return nodeWithWeight{}, 0, err
	}
}
