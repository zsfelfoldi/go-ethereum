// Copyright 2022 The go-ethereum Authors
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

package les

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"reflect"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethdb"
	lru "github.com/hashicorp/golang-lru"
	"github.com/minio/sha256-simd"
)

type merkleValue [32]byte

var (
	merkleValueT = reflect.TypeOf(merkleValue{})
	merkleZero   [64]merkleValue //TODO merkleValues encode
)

func init() {
	hasher := sha256.New()
	for i := 1; i < 64; i++ {
		hasher.Reset()
		hasher.Write(merkleZero[i-1][:])
		hasher.Write(merkleZero[i-1][:])
		hasher.Sum(merkleZero[i][:0])
	}
}

// UnmarshalJSON parses a merkle value in hex syntax.
func (m *merkleValue) UnmarshalJSON(input []byte) error {
	return hexutil.UnmarshalFixedJSON(merkleValueT, input, m[:])
}

type merkleValues []merkleValue //TODO merkleValues RLP enc

// represents the database version
type merkleList struct {
	db        ethdb.Database
	cache     *lru.Cache
	dbKey     []byte
	zeroLevel int
}

func (m *merkleList) zeroValue(index uint64) merkleValue {
	//	return merkleValue{}
	if i := bits.LeadingZeros64(index) + m.zeroLevel - 63; i > 0 {
		return merkleZero[i]
	}
	return merkleValue{}
}

func (m *merkleList) get(period, index uint64) merkleValue {
	l := len(m.dbKey)
	key := make([]byte, l+16)
	copy(key[:l], m.dbKey)
	binary.BigEndian.PutUint64(key[l:l+8], period)
	binary.BigEndian.PutUint64(key[l+8:l+16], index)
	if v, ok := m.cache.Get(string(key)); ok {
		if vv, ok := v.(merkleValue); ok {
			return vv
		} else {
			return m.zeroValue(index)
		}
	}
	var mv merkleValue
	if v, err := m.db.Get(key); err == nil && len(v) == 32 {
		copy(mv[:], v)
		m.cache.Add(string(key), mv)
		return mv
	}
	m.cache.Add(string(key), nil) // cache empty value
	return m.zeroValue(index)
}

func (m *merkleList) put(batch ethdb.Batch, period, index uint64, value merkleValue) {
	l := len(m.dbKey)
	key := make([]byte, l+16)
	copy(key[:l], m.dbKey)
	binary.BigEndian.PutUint64(key[l:l+8], period)
	binary.BigEndian.PutUint64(key[l+8:l+16], index)
	if value == m.zeroValue(index) {
		m.cache.Add(string(key), nil)
		batch.Delete(key)
	} else {
		m.cache.Add(string(key), value)
		batch.Put(key, value[:])
	}
}

// represents an immutable version even if the underlying database is changed
// If useNext is 1 then the underlying database has changed (represented by the next version) and reverse diffs should be applied.
//
type merkleListVersion struct {
	list     *merkleList
	useDiffs uint32 // atomic flag, zero for the root version
	parent   *merkleListVersion
	diffs    map[diffIndex]merkleValue
}

type diffIndex struct {
	period, index uint64
}

func (m *merkleListVersion) newChild() *merkleListVersion {
	return &merkleListVersion{
		list:     m.list,
		useDiffs: 1,
		parent:   m,
		diffs:    make(map[diffIndex]merkleValue),
	}
}

func (m *merkleListVersion) get(period, index uint64) merkleValue {
	if atomic.LoadUint32(&m.useDiffs) == 0 {
		value := m.list.get(period, index)
		if atomic.LoadUint32(&m.useDiffs) == 0 {
			return value
		}
	}
	if v, ok := m.diffs[diffIndex{period, index}]; ok {
		return v
	}
	return m.parent.get(period, index)
}

// should not be called on the root version
func (m *merkleListVersion) put(period, index uint64, value merkleValue) {
	m.diffs[diffIndex{period, index}] = value
}

// should only be called on the root version, with the child directly descending from the root
// child becomes the new root, valid after committing the batch to the db
func (root *merkleListVersion) commit(batch ethdb.Batch, child *merkleListVersion) {
	commitDiffs := child.diffs
	if atomic.SwapUint32(&child.useDiffs, 0) != 1 {
		panic(nil)
	}
	if child.parent != root {
		panic(nil)
	}
	child.parent = nil
	child.diffs = nil
	root.parent = child
	root.diffs = make(map[diffIndex]merkleValue)
	for i := range commitDiffs {
		root.diffs[i] = root.list.get(i.period, i.index)
	}
	if atomic.SwapUint32(&root.useDiffs, 1) != 0 {
		panic(nil)
	}
	for i, v := range commitDiffs {
		root.list.put(batch, i.period, i.index, v)
	}
}

type merkleListPeriodRepeat struct {
	list  *merkleListVersion
	depth int
}

func (m *merkleListPeriodRepeat) get(period, index uint64) merkleValue {
	depth := m.depth
	for {
		v := m.list.get(period, index)
		if v != m.list.list.zeroValue(index) || period == 0 || depth == 0 {
			return v
		}
		period--
		depth--
	}
}

func (m *merkleListPeriodRepeat) put(period, index uint64, value merkleValue) {
	m.list.put(period, index, value)
}

type merkleListHasher merkleListPeriodRepeat

var recalculateValue = merkleValue{1}

func newMerkleListHasher(root *merkleListVersion, depth int) *merkleListHasher {
	return &merkleListHasher{
		list:  root.newChild(),
		depth: depth,
	}
}

func (m *merkleListHasher) get(period, index uint64) merkleValue {
	mr := (*merkleListPeriodRepeat)(m)
	v := mr.get(period, index)
	if v != recalculateValue {
		return v
	}
	var hash merkleValue
	left := m.get(period, index*2)
	right := m.get(period, index*2+1)
	//if (left != merkleValue{} || right != merkleValue{}) {
	hasher := sha256.New()
	hasher.Write(left[:])
	hasher.Write(right[:])
	hasher.Sum(hash[:0])
	//}
	mr.put(period, index, hash)
	return hash
}

func (m *merkleListHasher) put(period, index uint64, value merkleValue) {
	mr := (*merkleListPeriodRepeat)(m)
	mr.put(period, index, value)
	index /= 2
	for index > 0 {
		mr.put(period, index, recalculateValue)
		index /= 2
	}
}

/*func (m *merkleListHasher) getSingleProof(period, index uint64) merkleValues {
	var proof merkleValues
	for index > 1 {
		proof = append(proof, m.get(period, index^1))
		index /= 2
	}
	return proof
}*/

type merkleListWriter struct {
	list          *merkleListPeriodRepeat
	format        proofFormat
	period, index uint64
}

func (m merkleListWriter) children() (left, right proofWriter) {
	lf, rf := m.format.children()
	if lf == nil {
		return nil, nil
	}
	return merkleListWriter{list: m.list, format: lf, period: m.period, index: m.index * 2},
		merkleListWriter{list: m.list, format: rf, period: m.period, index: m.index*2 + 1}
}

func (m merkleListWriter) writeNode(node merkleValue) {
	m.list.put(m.period, m.index, node)
}

func (m *merkleListHasher) addMultiProof(period uint64, proof multiProof) {
	writer := merkleListWriter{
		list:   (*merkleListPeriodRepeat)(m),
		format: proof.format,
		period: period,
		index:  1,
	}
	traverseProof(proof.reader(nil), writer)
}

// invalidates all other diff instances based on the same merkleList
func (m *merkleListHasher) commit(batch ethdb.Batch, child *merkleListHasher) {
	for i, v := range m.list.diffs {
		if v == recalculateValue {
			m.get(i.period, 1)
		}
	}
	m.list.commit(batch, child.list)
}

func verifySingleProof(proof merkleValues, index uint64, value merkleValue, bottomLevel int) (common.Hash, bool) {
	hasher := sha256.New()
	var proofIndex int
	for index > 1 {
		var proofHash merkleValue
		if proofIndex < len(proof) {
			proofHash = proof[proofIndex]
		} else {
			if i := bottomLevel - proofIndex - 1; i >= 0 {
				proofHash = merkleZero[i]
			} else {
				return common.Hash{}, false
			}
		}
		hasher.Reset()
		if index&1 == 0 {
			hasher.Write(value[:])
			hasher.Write(proofHash[:])
		} else {
			hasher.Write(proofHash[:])
			hasher.Write(value[:])
		}
		hasher.Sum(value[:0])
		index /= 2
		proofIndex++
	}
	if proofIndex < len(proof) {
		return common.Hash{}, false
	}
	return common.Hash(value), true
}

type proofFormat interface {
	children() (left, right proofFormat) // either both or neither should be nil
}

// Note: the hash of each traversed node is always requested. If the hash is not available then subtrees are always
// traversed (first left, then right). If it is available then subtrees are only traversed if needed by the writer.
type proofReader interface {
	children() (left, right proofReader) // subtrees accessible if not nil
	readNode() (merkleValue, bool)       // hash should be available if children are nil, optional otherwise
}

type proofWriter interface {
	children() (left, right proofWriter) // all non-nil subtrees are traversed
	writeNode(merkleValue)               // called for every traversed tree node (both leaf and internal)
}

func proofIndexMap(f proofFormat) map[uint64]int { // multiproof position index -> merkleValues slice index
	m := make(map[uint64]int)
	var pos int
	addToIndexMap(m, f, &pos, 1)
	return m
}

func addToIndexMap(m map[uint64]int, f proofFormat, pos *int, index uint64) {
	l, r := f.children()
	if l == nil {
		m[index] = *pos
		(*pos)++
	} else {
		addToIndexMap(m, l, pos, index*2)
		addToIndexMap(m, r, pos, index*2+1)
	}
}

func printIndices(f proofFormat, index uint64) { //TODO
	fmt.Print(" ", index)
	if l, r := f.children(); l != nil {
		printIndices(l, index*2)
		printIndices(r, index*2+1)
	}
	if index == 1 {
		fmt.Println()
	}
}

func childIndex(a, b uint64) uint64 {
	return (a-1)<<(63-bits.LeadingZeros64(b)) + b
}

// Reader subtrees are traversed if required by the writer of if the hash of the internal
// tree node is not available.
func traverseProof(reader proofReader, writer proofWriter) (common.Hash, bool) {
	var wl, wr proofWriter
	if writer != nil {
		wl, wr = writer.children()
	}
	node, nodeAvailable := reader.readNode()
	if nodeAvailable && wl == nil {
		if writer != nil {
			//			fmt.Print("W")
			writer.writeNode(node)
		} else {
			//			fmt.Print("O")
		}
		return common.Hash(node), true
	}
	rl, rr := reader.children()
	if rl == nil {
		//		fmt.Print("X")
		return common.Hash{}, false
	}
	//	fmt.Print("l")
	lhash, ok := traverseProof(rl, wl)
	//	fmt.Print("\\")
	if !ok {
		return common.Hash{}, false
	}
	//	fmt.Print("r")
	rhash, ok := traverseProof(rr, wr)
	//	fmt.Print("\\")
	if !ok {
		return common.Hash{}, false
	}
	if !nodeAvailable {
		hasher := sha256.New()
		hasher.Write(lhash[:])
		hasher.Write(rhash[:])
		hasher.Sum(node[:0])
	}
	if writer != nil {
		if wl != nil {
			//			fmt.Print("w")
		} else {
			//			fmt.Print("W")
		}
		writer.writeNode(node)
	}
	return common.Hash(node), true
}

type indexMapFormat struct {
	leaves map[uint64]proofFormat
	index  uint64
}

func newIndexMapFormat() indexMapFormat {
	return indexMapFormat{leaves: make(map[uint64]proofFormat), index: 1}
}

func (f indexMapFormat) addLeaf(index uint64, subtree proofFormat) indexMapFormat {
	if subtree != nil {
		f.leaves[index] = subtree
	}
	for index > 1 {
		index /= 2
		f.leaves[index] = nil
	}
	return f
}

func (f indexMapFormat) children() (left, right proofFormat) {
	if st, ok := f.leaves[f.index]; ok {
		if st != nil {
			return st.children()
		}
		return indexMapFormat{leaves: f.leaves, index: f.index * 2}, indexMapFormat{leaves: f.leaves, index: f.index*2 + 1}
	}
	return nil, nil
}

func parseMultiProof(proof []byte) (multiProof, error) {
	if len(proof) < 3 || proof[0] != 1 { // ????
		return multiProof{}, errors.New("invalid proof length")
	}
	leafCount := int(binary.LittleEndian.Uint16(proof[1:3]))
	if len(proof) != leafCount*34+1 {
		return multiProof{}, errors.New("invalid proof length")
	}
	valuesStart := leafCount*2 + 1
	format := newIndexMapFormat()
	if err := parseFormat(format.leaves, 1, proof[3:valuesStart]); err != nil {
		return multiProof{}, err
	}
	values := make(merkleValues, leafCount)
	for i := range values {
		copy(values[i][:], proof[valuesStart+i*32:valuesStart+(i+1)*32])
	}
	return multiProof{format: format, values: values}, nil
}

func parseFormat(leaves map[uint64]proofFormat, index uint64, format []byte) error {
	if len(format) == 0 {
		return nil
	}
	leaves[index] = nil
	boundary := int(binary.LittleEndian.Uint16(format[:2])) * 2
	if boundary > len(format) {
		return errors.New("invalid proof format")
	}
	if err := parseFormat(leaves, index*2, format[2:boundary]); err != nil {
		return err
	}
	if err := parseFormat(leaves, index*2+1, format[boundary:]); err != nil {
		return err
	}
	return nil
}

type mergedFormat []proofFormat // earlier one has priority

func (m mergedFormat) children() (left, right proofFormat) {
	l := make(mergedFormat, 0, len(m))
	r := make(mergedFormat, 0, len(m))
	for _, f := range m {
		if left, right := f.children(); left != nil {
			l = append(l, left)
			r = append(r, right)
		}
	}
	if len(l) > 0 {
		return l, r
	}
	return nil, nil
}

type rangeFormat struct {
	begin, end, index uint64 // begin and end should be on the same level
	subtree           func(uint64) proofFormat
}

func newRangeFormat(begin, end uint64, subtree func(uint64) proofFormat) rangeFormat {
	return rangeFormat{
		begin:   begin,
		end:     end,
		index:   1,
		subtree: subtree,
	}
}

func (rf rangeFormat) children() (left, right proofFormat) {
	lzr := bits.LeadingZeros64(rf.begin)
	lzi := bits.LeadingZeros64(rf.index)
	if lzi < lzr {
		return nil, nil
	}
	if lzi == lzr {
		if rf.subtree != nil && rf.index >= rf.begin && rf.index <= rf.end {
			if st := rf.subtree(rf.index); st != nil {
				return st.children()
			}
		}
		return nil, nil
	}
	i1, i2 := rf.index<<(lzi-lzr), ((rf.index+1)<<(lzi-lzr))-1
	if i1 <= rf.end && i2 >= rf.begin {
		return rangeFormat{begin: rf.begin, end: rf.end, index: rf.index * 2, subtree: rf.subtree},
			rangeFormat{begin: rf.begin, end: rf.end, index: rf.index*2 + 1, subtree: rf.subtree}
	}
	return nil, nil
}

type multiProof struct {
	format proofFormat
	values merkleValues
}

func (mp multiProof) reader(subtrees func(uint64) proofReader) *multiProofReader {
	values := mp.values
	return &multiProofReader{format: mp.format, values: &values, index: 1, subtrees: subtrees}
}

func (mp multiProof) rootHash() common.Hash {
	hash, _ := traverseProof(mp.reader(nil), nil)
	return hash
}

type multiProofReader struct {
	format   proofFormat
	values   *merkleValues
	index    uint64
	subtrees func(uint64) proofReader
}

func (mpr multiProofReader) children() (left, right proofReader) {
	lf, rf := mpr.format.children()
	if lf == nil {
		if mpr.subtrees != nil {
			if subtree := mpr.subtrees(mpr.index); subtree != nil {
				return subtree.children()
			}
		}
		return nil, nil
	}
	return multiProofReader{format: lf, values: mpr.values, index: mpr.index * 2, subtrees: mpr.subtrees},
		multiProofReader{format: rf, values: mpr.values, index: mpr.index*2 + 1, subtrees: mpr.subtrees}
}

func (mpr multiProofReader) readNode() (merkleValue, bool) {
	if l, _ := mpr.format.children(); l == nil && len(*mpr.values) > 0 {
		hash := (*mpr.values)[0]
		*mpr.values = (*mpr.values)[1:]
		return hash, true
	}
	return merkleValue{}, false
}

// should be checked after traverseProof if received from an untrusted source
func (mp *multiProofReader) finished() bool {
	return len(*mp.values) == 0
}

type mergedReader []proofReader

func (m mergedReader) children() (left, right proofReader) {
	l := make(mergedReader, 0, len(m))
	r := make(mergedReader, 0, len(m))
	for _, reader := range m {
		if left, right := reader.children(); left != nil {
			l = append(l, left)
			r = append(r, right)
		}
	}
	if len(l) > 0 {
		return l, r
	}
	return nil, nil
}

func (m mergedReader) readNode() (value merkleValue, ok bool) {
	var hasChildren bool
	for _, reader := range m {
		if left, _ := reader.children(); left != nil {
			// ensure that all readers are fully traversed
			hasChildren = true
		}
		if v, o := reader.readNode(); o {
			value, ok = v, o
		}
	}
	if hasChildren {
		return merkleValue{}, false
	}
	return
}

type multiProofWriter struct {
	format   proofFormat
	values   *merkleValues
	index    uint64
	subtrees func(uint64) proofWriter
}

// subtrees are not included in format
func newMultiProofWriter(format proofFormat, target *merkleValues, subtrees func(uint64) proofWriter) multiProofWriter {
	return multiProofWriter{format: format, values: target, index: 1, subtrees: subtrees}
}

func (mpw multiProofWriter) children() (left, right proofWriter) {
	lf, rf := mpw.format.children()
	if lf == nil {
		if mpw.subtrees != nil {
			if subtree := mpw.subtrees(mpw.index); subtree != nil {
				return subtree.children()
			}
		}
		return nil, nil
	}
	return multiProofWriter{format: lf, values: mpw.values, index: mpw.index * 2, subtrees: mpw.subtrees},
		multiProofWriter{format: rf, values: mpw.values, index: mpw.index*2 + 1, subtrees: mpw.subtrees}
}

func (mpw multiProofWriter) writeNode(node merkleValue) {
	if lf, _ := mpw.format.children(); lf == nil {
		*mpw.values = append(*mpw.values, node)
	}
	if mpw.subtrees != nil {
		if subtree := mpw.subtrees(mpw.index); subtree != nil {
			subtree.writeNode(node)
		}
	}
}

type valueWriter struct {
	format     proofFormat
	values     merkleValues
	index      uint64
	storeIndex func(uint64) int // if i := storeIndex(index); i >= 0 then value at given tree index is stored in values[i]
}

func newValueWriter(format proofFormat, target merkleValues, storeIndex func(uint64) int) valueWriter {
	return valueWriter{format: format, values: target, index: 1, storeIndex: storeIndex}
}

func (vw valueWriter) children() (left, right proofWriter) {
	lf, rf := vw.format.children()
	if lf == nil {
		return nil, nil
	}
	return valueWriter{format: lf, values: vw.values, index: vw.index * 2, storeIndex: vw.storeIndex},
		valueWriter{format: rf, values: vw.values, index: vw.index*2 + 1, storeIndex: vw.storeIndex}
}

func (vw valueWriter) writeNode(node merkleValue) {
	if i := vw.storeIndex(vw.index); i >= 0 {
		vw.values[i] = node
	}
}

type mergedWriter []proofWriter

func (m mergedWriter) children() (left, right proofWriter) {
	l := make(mergedWriter, 0, len(m))
	r := make(mergedWriter, 0, len(m))
	for _, w := range m {
		if left, right := w.children(); left != nil {
			l = append(l, left)
			r = append(r, right)
		}
	}
	if len(l) > 0 {
		return l, r
	}
	return nil, nil
}

func (m mergedWriter) writeNode(value merkleValue) {
	for _, w := range m {
		w.writeNode(value)
	}
}
