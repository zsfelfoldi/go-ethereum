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
	"time"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/common/mclock"
)

const (
	mtsEmpty = iota
	mtsPartial
	mtsComplete
)

type (
	nodeReader    func(gti treeIndex) (nodeWithWeight, bool, error)
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
	params        *Params
	nodes         []merkleTreeNode
	firstFree     uint32
	subtrees      storedSubtrees
	verticalNodes *verticalNodesWriter
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
	nmtStart = mclock.Now()
	if err := mt.initTree(getNode, nodeStatus, params.treeRoot, 0); err != nil {
		return nil, err
	}
	fmt.Println("newMerkleTree", len(mt.nodes))
	return mt, nil
}

var (
	lastIndex     uint32
	initTreeCount uint64
	nmtStart      mclock.AbsTime
)

// it is assumed that non-leaf nodes are only available when they are empty or completed.
func (mt *merkleTree) initTree(getNode nodeReader, nodeStatus nodeStatus, node mtNode, slThreshold uint) error {
	initTreeCount++
	if node.index > lastIndex+1000 {
		dt := time.Duration(mclock.Now() - nmtStart)
		fmt.Println("initTree", node, initTreeCount, initTreeCount/uint64(node.index), dt, dt/time.Duration(initTreeCount))
		lastIndex = node.index
	}
	nw, avail, err := getNode(node.gti)
	if err != nil {
		return err
	}
	var sl uint
	if avail {
		mt.nodes[node.index].value, mt.nodes[node.index].weight = nw.value, nw.weight
		mt.nodes[node.index].setValueKnown(true)
		sl = mt.params.storageLevel(nw.weight)
	}
	if !avail || sl > slThreshold {
		if avail {
			slThreshold = sl - 1
		}
		// initialize descendants recursively
		mt.nodes[node.index].setLeft(mt.newNode(node.index))
		if err := mt.initTree(getNode, nodeStatus, mt.leftChild(node), slThreshold); err != nil {
			return err
		}
		mt.nodes[node.index].setRight(mt.newNode(node.index))
		if err := mt.initTree(getNode, nodeStatus, mt.rightChild(node), slThreshold); err != nil {
			return err
		}
	}
	mt.getValue(node.index)
	switch nodeStatus(node.gti) {
	case mtsEmpty:
		mt.nodes[node.index].setEmptySubtree(true)
	case mtsPartial:
	case mtsComplete:
		mt.nodes[node.index].setComplete(true)
		if mt.params.storageLevel(mt.nodes[node.index].weight) == 0 {
			mt.collapseSubtree(node)
		}
	default:
		panic("invalid node status from tree init reader")
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
	n.setComplete(true)
	// propagate completed state to ancestors and collapse completed subtrees if possible
	for n.parent() != nullPtr {
		if !n.isValueKnown() {
			panic("completed node with unknown value")
		}
		if mt.verticalNodes != nil {
			mt.verticalNodes.setNode(node.gti, nodeWithWeight{value: n.value, weight: n.weight})
		}
		nl := mt.params.storageLevel(n.weight)
		if nl == 0 {
			mt.collapseSubtree(node)
		}
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
		//p.weight = n.weight + s.weight
		if !p.isValueKnown() {
			mt.getValue(parent.index)
		}
		sl := mt.params.storageLevel(s.weight)
		pl := mt.params.storageLevel(p.weight)
		if nl > 0 && nl < pl {
			mt.subtrees = append(mt.subtrees, mt.collapseAndStoreSubtree(node))
		}
		if sl > 0 && sl < pl {
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
		if n.weight == 0 {
			fmt.Println("getValue 0 weight:", nodeIndex, n.left(), lnw, n.right(), rnw)
		}
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
	return s[bitIndex/8]&(byte(1)<<(bitIndex%8)) != 0
}

func (s serializedSubtree) getNode(rti treeIndex) (nw nodeWithWeight, leaf, internal bool) {
	l := len(s)
	leafCount := (l*8 + 1) / ((32+4)*8 + 2)
	shapeOffset := l - leafCount*(32+4)
	if shapeOffset != (leafCount*2+6)/8 {
		panic("invalid serialized subtree")
	}
	//fmt.Println("sst.getNode", rti, "leafCount", leafCount, "shape", s[:shapeOffset])
	var bitIndex, leafIndex int
	for rti != gtiRoot {
		if s.shapeBit(bitIndex) {
			return nodeWithWeight{}, false, false // index points beyond subtree leaf
		}
		bitIndex++
		if rti.matchRoot(3) { // right subtree; skip left subtree shape
			for expLeaves := 1; expLeaves > 0; {
				//fmt.Println("  expLeaves", expLeaves)
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
		//fmt.Println(" rti", rti, "leafIndex", leafIndex)
	}
	if s.shapeBit(bitIndex) { // index points to subtree leaf
		offset := shapeOffset + (32+4)*leafIndex
		copy(nw.value[:], s[offset:offset+32])
		nw.weight = math.Float32frombits(binary.LittleEndian.Uint32(s[offset+32 : offset+32+4]))
		return nw, true, false
	}
	return nodeWithWeight{}, false, true
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

type serializedVerticalNodeList []byte

func (s serializedVerticalNodeList) hasNode(rowIndex uint32) bool {
	i := int(rowIndex) * (32 + 4)
	return binary.LittleEndian.Uint32(s[i+32:i+32+4]) != 0
}

func (s serializedVerticalNodeList) getNode(rowIndex uint32) (nw nodeWithWeight) {
	i := int(rowIndex) * (32 + 4)
	copy(nw.value[:], s[i:i+32])
	nw.weight = math.Float32frombits(binary.LittleEndian.Uint32(s[i+32 : i+32+4]))
	return
}

func (p *Params) newSerializedVerticalNodeList() serializedVerticalNodeList {
	return make(serializedVerticalNodeList, int(p.mapHeight)*(32+4))
}

func (s serializedVerticalNodeList) setNode(rowIndex uint32, nw nodeWithWeight) {
	i := int(rowIndex) * (32 + 4)
	copy(s[i:i+32], nw.value[:])
	binary.LittleEndian.PutUint32(s[i+32:i+32+4], math.Float32bits(nw.weight))

	if !s.hasNode(rowIndex) {
		fmt.Println("svnl.setNode: !hasNode", rowIndex, nw)
	}
}

type verticalNodeIndex struct {
	mapIndex uint32
	depth    uint8
}

type verticalNodesReader struct {
	params    *Params
	listMap   map[verticalNodeIndex]serializedVerticalNodeList
	getListFn func(verticalNodeIndex) (serializedVerticalNodeList, error)
}

type verticalNodesWriter struct {
	params  *Params
	lists   []serializedVerticalNodeList
	listMap map[verticalNodeIndex]serializedVerticalNodeList
}

func (p *Params) newVerticalNodesReader(getListFn func(verticalNodeIndex) (serializedVerticalNodeList, error)) *verticalNodesReader {
	return &verticalNodesReader{
		params:    p,
		listMap:   make(map[verticalNodeIndex]serializedVerticalNodeList),
		getListFn: getListFn,
	}
}

func (p *Params) newVerticalNodesWriter(mapIndex uint32) *verticalNodesWriter {
	s := &verticalNodesWriter{
		params:  p,
		listMap: make(map[verticalNodeIndex]serializedVerticalNodeList),
	}
	for depth := range p.logMapsPerEpoch + 1 {
		if depth >= p.verticalNodesMinDepth {
			list := p.newSerializedVerticalNodeList()
			s.listMap[verticalNodeIndex{mapIndex, uint8(depth)}] = list
			s.lists = append(s.lists, list)
		}
		if (mapIndex>>depth)&1 == 0 {
			break
		}
	}
	return s
}

func (s *verticalNodesReader) getNode(gti treeIndex) (nodeWithWeight, bool, error) {
	vni, rowIndex, ok := s.params.splitVerticalNodeIndex(gti)
	if !ok {
		return nodeWithWeight{}, false, nil
	}
	list, ok := s.listMap[vni]
	if !ok {
		var err error
		list, err = s.getListFn(vni)
		//fmt.Println("getListFn", vni, list != nil, len(list), err)
		if list == nil {
			return nodeWithWeight{}, false, err
		}
		s.listMap[vni] = list

	}
	return list.getNode(rowIndex), true, nil
}

var xxxParams *Params //TODO

func (s *verticalNodesWriter) setNode(gti treeIndex, nw nodeWithWeight) {
	vni, rowIndex, ok := s.params.splitVerticalNodeIndex(gti)
	//fmt.Println("vnw.setNode", gti, vni, rowIndex, ok)
	if !ok {
		return
	}
	if list, ok := s.listMap[vni]; ok {
		//fmt.Println(" list.setNode")
		if nw.weight == 0 {
			fmt.Println("vnw.setNode gti", gti, "vni", vni, "row", rowIndex, "nw", nw, "empty", xxxParams.treeRoot.empty.getNode(gti))
		}
		list.setNode(rowIndex, nw)
	} /* else {
		fmt.Println(" no list")
	}*/
}

func (s *verticalNodesWriter) isComplete() bool {
	fmt.Println("vnw.isComplete")
	ok := true
	for depth, list := range s.lists {
		var a, b int
		for rowIndex := range s.params.mapHeight {
			if list.hasNode(rowIndex) {
				a++
			} else {
				b++
				nw := list.getNode(rowIndex)
				fmt.Println(" depth", depth, "missing", rowIndex, "nw", nw)
				ok = false
			}
		}
		fmt.Println(" depth", depth, "present", a, "missing", b)
	}
	return ok
	/*for _, list := range s.lists {
		for rowIndex := range s.params.mapHeight {
			if !list.hasNode(rowIndex) {
				return false
			}
		}
	}
	return true*/
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

func (r *subtreeNodeReader) getNode(gti treeIndex) (nodeWithWeight, bool, error) {
	//fmt.Println("stnr.getNode", gti)
	cs, err := r.getCachedSubtree(gti)
	if err != nil {
		//fmt.Println(" err", err)
		return nodeWithWeight{}, false, err
	}
	//fmt.Println(" subtree", cs.subtree != nil)
	if cs.subtree == nil {
		return nodeWithWeight{}, false, nil
	}
	nw, avail, internal := cs.subtree.getNode(gti.subIndex(cs.subtreeLevel))
	//fmt.Println(" st.getNode", avail, nw)
	if internal && gti != gtiRoot {
		// maybe it is a subtree root and the parent's subtree has the value
		parentCs, err := r.getCachedSubtree(gti.parent())
		//fmt.Println(" parent subtree", parentCs.subtree != nil, parentCs.subtreeLevel != cs.subtreeLevel)
		if err != nil {
			return nodeWithWeight{}, false, err
		}
		if parentCs.subtree != nil && parentCs.subtreeLevel != cs.subtreeLevel {
			parentNw, parentAvail, _ := parentCs.subtree.getNode(gti.subIndex(parentCs.subtreeLevel))
			//fmt.Println(" pst.getNode", parentAvail, parentNw)
			if parentAvail {
				return parentNw, true, nil
			}
		}
	}
	//fmt.Println(" result", avail, nw)
	return nw, avail, nil
}

type mergedNodeReader []nodeReader

func (m mergedNodeReader) getNode(gti treeIndex) (nodeWithWeight, bool, error) {
	//fmt.Println("mergedNodeReader.getNode", gti)
	for _, getNode := range m {
		nw, avail, err := getNode(gti)
		//fmt.Println(" ", i, mergeAvail, mergeErr)
		if err != nil {
			return nodeWithWeight{}, false, err
		}
		if avail {
			return nw, true, nil
		}
	}
	return nodeWithWeight{}, false, nil
}
