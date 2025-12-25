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
	"fmt"
	"math"
	"sort"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/common/mclock"
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
	nodeReader    func(gti treeIndex) (nw nodeWithWeight, avail int, err error)
	nodeStatus    func(gti treeIndex) int
	subtreeReader func(gti treeIndex) (serializedSubtree, error)
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
	rootIndex   = uint32(0)   // merkle tree root is always at index 0
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

func (params *Params) newMerkleTree(getNode nodeReader, nodeStatus nodeStatus) (*merkleTree, error) {
	mt := &merkleTree{
		params: params,
		nodes: []merkleTreeNode{merkleTreeNode{
			parentAndEmptySubtree: nullPtr,
			leftAndIsValueKnown:   nullPtr,
			rightAndIsComplete:    nullPtr,
		}},
		firstFree: nullPtr,
	}
	nw, avail, err := getNode(gtiRoot)
	if err != nil {
		return nil, err
	}
	if err := mt.initTree(getNode, nodeStatus, params.treeRoot, nw, avail); err != nil {
		return nil, err
	}
	return mt, nil
}

func (mt *merkleTree) initTree(getNode nodeReader, nodeStatus nodeStatus, node mtNode, nw nodeWithWeight, avail int) error {
	fmt.Println("initTree", node)
	var recursiveInit bool
	switch avail {
	case mtaInternal:
		recursiveInit = true
	case mtaKnown:
		mt.nodes[node.index].value, mt.nodes[node.index].weight = nw.value, nw.weight
		mt.nodes[node.index].setValueKnown(true)
	default:
		panic("invalid node availability from tree init reader")
	}
	switch nodeStatus(node.gti) {
	case mtsEmpty:
		mt.nodes[node.index].setEmptySubtree(true)
	case mtsPartial:
		recursiveInit = true
	case mtsComplete:
		mt.setComplete(node)
	default:
		panic("invalid node status from tree init reader")
	}
	if !recursiveInit {
		return nil
	}
	// initialize descendants recursively
	nwLeft, availLeft, err := getNode(node.gti.leftChild())
	if err != nil {
		return err
	}
	nwRight, availRight, err := getNode(node.gti.rightChild())
	if err != nil {
		return err
	}
	if availLeft != mtaUnknown && availRight != mtaUnknown {
		mt.nodes[node.index].setLeft(mt.newNode(node.index))
		if err := mt.initTree(getNode, nodeStatus, mt.leftChild(node), nwLeft, availLeft); err != nil {
			return err
		}
		mt.nodes[node.index].setRight(mt.newNode(node.index))
		if err := mt.initTree(getNode, nodeStatus, mt.rightChild(node), nwRight, availRight); err != nil {
			return err
		}
	} else {
		if avail != mtaKnown {
			panic("unknown internal node with no descendants")
		}
	}
	return nil
}

func (mt *merkleTree) deleteNode(nodeIndex uint32) {
	mt.nodes[nodeIndex].setRight(mt.firstFree)
	mt.firstFree = nodeIndex
}

func (mt *merkleTree) newNode(parentIndex uint32) uint32 {
	n := merkleTreeNode{
		parentAndEmptySubtree: parentIndex,
		leftAndIsValueKnown:   nullPtr,
		rightAndIsComplete:    nullPtr,
	}
	if mt.firstFree != nullPtr {
		nodeIndex := mt.firstFree
		mt.firstFree = mt.nodes[nodeIndex].right()
		mt.nodes[nodeIndex] = n
		return nodeIndex
	}
	nodeIndex := uint32(len(mt.nodes))
	mt.nodes = append(mt.nodes, n)
	return nodeIndex
}

type mtNode struct {
	index uint32
	empty *emptySubtree
	gti   treeIndex
}

func (mt *merkleTree) debugPrint(nodeIndex uint32, rti uint64) {
	if nodeIndex == nullPtr {
		return
	}
	n := &mt.nodes[nodeIndex]
	fmt.Printf(" %b: %064x %f %v %v %v\n", rti, n.value, n.weight, n.isValueKnown(), n.isComplete(), n.isEmptySubtree())
	mt.debugPrint(n.left(), rti*2)
	mt.debugPrint(n.right(), rti*2+1)
}

func (mt *merkleTree) debugCountNodes(nodeIndex uint32) int {
	if nodeIndex == nullPtr {
		return 0
	}
	n := &mt.nodes[nodeIndex]
	return 1 + mt.debugCountNodes(n.left()) + mt.debugCountNodes(n.right())
}

func (mt *merkleTree) debugCountFree() int {
	nodeIndex := mt.firstFree
	var count int
	for nodeIndex != nullPtr {
		count++
		nodeIndex = mt.nodes[nodeIndex].right()
	}
	return count
}

func (mt *merkleTree) getDescendant(node mtNode, rti treeIndex) mtNode {
	TTgetDescendant -= mclock.Now()
	defer func() { TTgetDescendant += mclock.Now() }()

	for rti != gtiRoot {
		//fmt.Println("getDescendant", node.gti, node.index, rti)
		if mt.nodes[node.index].left() == nullPtr {
			if !mt.nodes[node.index].isEmptySubtree() {
				panic("cannot expand non-empty subtree")
			}
			leftIndex := mt.newNode(node.index)
			//fmt.Println(" leftIndex", leftIndex)
			mt.nodes[node.index].setLeft(leftIndex)
			l := &mt.nodes[leftIndex]
			l.value = node.empty.left.value
			l.setValueKnown(true)
			l.setEmptySubtree(true)
			rightIndex := mt.newNode(node.index)
			//fmt.Println(" rightIndex", rightIndex)
			mt.nodes[node.index].setRight(rightIndex)
			r := &mt.nodes[rightIndex]
			r.value = node.empty.right.value
			r.setValueKnown(true)
			r.setEmptySubtree(true)
		}
		switch {
		case rti.matchRoot(2):
			node = mt.leftChild(node)
		case rti.matchRoot(3):
			node = mt.rightChild(node)
		default:
			panic("invalid descendant subIndex")
		}
	}
	return node
}

func (mt *merkleTree) getRightNeighbor(node mtNode) mtNode {
	//fmt.Println("getRightNeighbor node", node)
	parent := mt.parent(node)
	//fmt.Println(" parent", parent, "left", mt.nodes[parent.index].left(), "right", mt.nodes[parent.index].right())
	rti := gtiRoot
	for mt.nodes[parent.index].right() == node.index {
		node = parent
		parent = mt.parent(node)
		//fmt.Println(" parent", parent)
		rti = rti.add(rti)
	}
	node = mt.rightChild(parent)
	//fmt.Println(" node (right child)", node)
	if rti != gtiRoot {
		node = mt.getDescendant(node, rti)
		//fmt.Println(" rti", rti, "descendant", node)
	}
	return node
}

func (mt *merkleTree) leftChild(node mtNode) mtNode {
	return mtNode{index: mt.nodes[node.index].left(), empty: node.empty.left, gti: node.gti.leftChild()}
}

func (mt *merkleTree) rightChild(node mtNode) mtNode {
	return mtNode{index: mt.nodes[node.index].right(), empty: node.empty.right, gti: node.gti.rightChild()}
}

func (mt *merkleTree) parent(node mtNode) mtNode {
	return mtNode{index: mt.nodes[node.index].parent(), empty: node.empty.parent, gti: node.gti.parent()}
}

func (mt *merkleTree) sibling(node mtNode) mtNode {
	parent := mt.parent(node)
	if node.gti == parent.gti.leftChild() {
		return mt.rightChild(parent)
	} else {
		return mt.leftChild(parent)
	}
}

func (mt *merkleTree) setComplete(node mtNode) {
	//fmt.Println("setComplete", node)
	n := &mt.nodes[node.index]
	if n.isComplete() {
		return
	}
	if n.left() != nullPtr {
		//fmt.Println(" starting children of", node)
		mt.setComplete(mt.leftChild(node))
		mt.setComplete(mt.rightChild(node))
		//fmt.Println(" finished children of", node)
		return
	}
	if !n.isValueKnown() {
		panic("finalized node with unknown value")
	}
	n.setComplete(true)
	// propagate completed state to ancestors and collapse completed subtrees if possible
	for n.parent() != nullPtr {
		parent := mt.parent(node)
		//fmt.Println(" parent", parent)
		p := &mt.nodes[parent.index]
		sibling := mt.sibling(node)
		//fmt.Println(" sibling", sibling)
		s := &mt.nodes[sibling.index]
		if !s.isComplete() {
			break
		}
		p.setComplete(true)
		p.weight = n.weight + s.weight
		if !p.isValueKnown() {
			mt.getValue(parent.index)
		}
		nl := mt.params.storageLevel(n.weight)
		sl := mt.params.storageLevel(s.weight)
		pl := mt.params.storageLevel(p.weight)
		if nl == 0 {
			mt.collapseSubtree(node)
		} else if nl < pl {
			mt.subtrees = append(mt.subtrees, mt.collapseAndStoreSubtree(node))
		}
		if sl == 0 {
			mt.collapseSubtree(sibling)
		} else if sl < pl {
			mt.subtrees = append(mt.subtrees, mt.collapseAndStoreSubtree(sibling))
		}
		n, node = p, parent
	}
}

func (mt *merkleTree) setValue(nodeIndex uint32, value merkle.Value, weight float32) {
	//fmt.Println("setValue", nodeIndex, value, weight)
	n := &mt.nodes[nodeIndex]
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

func (mt *merkleTree) getValue(nodeIndex uint32) nodeWithWeight {
	n := &mt.nodes[nodeIndex]
	if !n.isValueKnown() {
		lnw := mt.getValue(n.left())
		rnw := mt.getValue(n.right())
		n.value = treeHash(lnw.value, rnw.value)
		n.weight = lnw.weight + rnw.weight
		n.setEmptySubtree(mt.nodes[n.left()].isEmptySubtree() && mt.nodes[n.right()].isEmptySubtree())
		n.setValueKnown(true)
	}
	return nodeWithWeight{value: n.value, weight: n.weight}
}

const (
	tsCollectShapeBits = iota
	tsCollectLeavesAndDelete
	tsDelete
)

func (mt *merkleTree) traverseSubtree(nodeIndex uint32, action int, encBytes *serializedSubtree, encBitPtr *int) {
	n := &mt.nodes[nodeIndex]
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

func (mt *merkleTree) collapseSubtree(node mtNode) {
	mt.traverseSubtree(node.index, tsDelete, nil, nil)
}

func (mt *merkleTree) collapseAndStoreSubtree(node mtNode) (res storedSubtree) {
	var bitPtr int
	res.gti = node.gti
	mt.traverseSubtree(node.index, tsCollectShapeBits, &res.nodeEnc, &bitPtr)
	mt.traverseSubtree(node.index, tsCollectLeavesAndDelete, &res.nodeEnc, nil)
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

func (s serializedSubtree) getNode(rti treeIndex) (node nodeWithWeight, avail int) {
	l := len(s)
	leafCount := (l*8 + 1) / ((32+4)*8 + 2)
	shapeOffset := l - leafCount*(32+4)
	if shapeOffset != (leafCount*2+6)/8 {
		panic("invalid serialized subtree")
	}
	var bitIndex, leafIndex int
	for rti != gtiRoot {
		if s.shapeBit(bitIndex) {
			return // index points beyond subtree leaf
		}
		bitIndex++
		if rti.matchRoot(3) { // right subtree; skip left subtree shape
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
			rti.matchRoot(2) // left subtree
		}
	}
	if s.shapeBit(bitIndex) { // index points to subtree leaf
		offset := shapeOffset + (32+4)*leafIndex
		copy(node.value[:], s[offset:offset+32])
		node.weight = math.Float32frombits(binary.LittleEndian.Uint32(s[offset+32 : offset+32+4]))
		avail = mtaKnown
		return
	}
	avail = mtaInternal
	return
}

type storedSubtree struct {
	gti     treeIndex
	nodeEnc serializedSubtree
}

type storedSubtrees []storedSubtree

func (s storedSubtrees) Len() int           { return len(s) }
func (s storedSubtrees) Less(i, j int) bool { return s[i].gti.lessThan(s[j].gti) }
func (s storedSubtrees) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// assumes sorted list
func (s storedSubtrees) getSubtree(gti treeIndex) serializedSubtree {
	a, b := 0, len(s)
	for a < b {
		m := (a + b) / 2
		if s[m].gti == gti {
			return s[m].nodeEnc
		}
		if s[m].gti.lessThan(gti) {
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
	getSubtree subtreeReader
	// non-existent entries are cached in both caches
	cache *lru.Cache[treeIndex, cachedSubtree]
}

func newSubtreeNodeReader(reader subtreeReader) *subtreeNodeReader {
	return &subtreeNodeReader{
		getSubtree: reader,
		cache:      lru.NewCache[treeIndex, cachedSubtree](1000),
	}
}

type cachedSubtree struct {
	subtree      serializedSubtree
	subtreeLevel uint
}

type nodeWithWeight struct {
	value  merkle.Value
	weight float32
}

func (r *subtreeNodeReader) getCachedSubtree(gti treeIndex) (cachedSubtree, error) {
	if cs, ok := r.cache.Get(gti); ok {
		return cs, nil
	}
	var cs cachedSubtree
	st, err := r.getSubtree(gti)
	if err != nil {
		return cachedSubtree{}, err
	}
	if st != nil {
		cs = cachedSubtree{
			subtree:      st,
			subtreeLevel: gti.level(),
		}
	} else {
		if gti != gtiRoot {
			cs, err = r.getCachedSubtree(gti.parent())
			if err != nil {
				return cachedSubtree{}, err
			}
		}
	}
	r.cache.Add(gti, cs)
	return cs, nil
}

func (r *subtreeNodeReader) getNode(gti treeIndex) (nodeWithWeight, int, error) {
	cs, err := r.getCachedSubtree(gti)
	if err != nil {
		return nodeWithWeight{}, 0, err
	}
	if cs.subtree == nil {
		return nodeWithWeight{}, mtaUnknown, nil
	}
	nw, avail := cs.subtree.getNode(gti.subIndex(cs.subtreeLevel))
	if avail == mtaInternal && gti != gtiRoot {
		// maybe it is a subtree root and the parent's subtree has the value
		parentCs, err := r.getCachedSubtree(gti)
		if err != nil {
			return nodeWithWeight{}, 0, err
		}
		if parentCs.subtree != nil && parentCs.subtreeLevel != cs.subtreeLevel {
			parentNw, parentAvail := parentCs.subtree.getNode(gti.subIndex(parentCs.subtreeLevel))
			if parentAvail == mtaKnown {
				return parentNw, parentAvail, nil
			}
		}
	}
	return nw, avail, nil
}

type mergedNodeReader []nodeReader

func (m mergedNodeReader) getNode(gti treeIndex) (nw nodeWithWeight, avail int, err error) {
	//fmt.Println("mergedNodeReader.getNode", gti)
	for _, getNode := range m {
		mergeNw, mergeAvail, mergeErr := getNode(gti)
		//fmt.Println(" ", i, mergeAvail, mergeErr)
		if mergeErr != nil {
			err = mergeErr
			return
		}
		if mergeAvail > avail {
			nw, avail = mergeNw, mergeAvail
			if avail == mtaKnown {
				return
			}
		}
	}
	return
}
