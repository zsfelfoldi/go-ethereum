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

package beacon

import (
	"encoding/binary"
	"errors"
	"math/bits"
	"reflect"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/minio/sha256-simd"
)

type MerkleValue [32]byte

var (
	MerkleValueT = reflect.TypeOf(MerkleValue{})
	merkleZero   [64]MerkleValue
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
func (m *MerkleValue) UnmarshalJSON(input []byte) error {
	return hexutil.UnmarshalFixedJSON(MerkleValueT, input, m[:])
}

type MerkleValues []MerkleValue //TODO RLP encoding

func VerifySingleProof(proof MerkleValues, index uint64, value MerkleValue, bottomLevel int) (common.Hash, bool) {
	hasher := sha256.New()
	var proofIndex int
	for index > 1 {
		var proofHash MerkleValue
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

type ProofFormat interface {
	children() (left, right ProofFormat) // either both or neither should be nil
}

// Note: the hash of each traversed node is always requested. If the hash is not available then subtrees are always
// traversed (first left, then right). If it is available then subtrees are only traversed if needed by the writer.
type ProofReader interface {
	children() (left, right ProofReader) // subtrees accessible if not nil
	readNode() (MerkleValue, bool)       // hash should be available if children are nil, optional otherwise
}

type ProofWriter interface {
	children() (left, right ProofWriter) // all non-nil subtrees are traversed
	writeNode(MerkleValue)               // called for every traversed tree node (both leaf and internal)
}

func ProofIndexMap(f ProofFormat) map[uint64]int { // multiproof position index -> MerkleValues slice index
	m := make(map[uint64]int)
	var pos int
	addToIndexMap(m, f, &pos, 1)
	return m
}

func addToIndexMap(m map[uint64]int, f ProofFormat, pos *int, index uint64) {
	l, r := f.children()
	if l == nil {
		m[index] = *pos
		(*pos)++
	} else {
		addToIndexMap(m, l, pos, index*2)
		addToIndexMap(m, r, pos, index*2+1)
	}
}

func ChildIndex(a, b uint64) uint64 {
	return (a-1)<<(63-bits.LeadingZeros64(b)) + b
}

// Reader subtrees are traversed if required by the writer of if the hash of the internal
// tree node is not available.
func TraverseProof(reader ProofReader, writer ProofWriter) (common.Hash, bool) {
	var wl, wr ProofWriter
	if writer != nil {
		wl, wr = writer.children()
	}
	node, nodeAvailable := reader.readNode()
	if nodeAvailable && wl == nil {
		if writer != nil {
			writer.writeNode(node)
		} else {
		}
		return common.Hash(node), true
	}
	rl, rr := reader.children()
	if rl == nil {
		return common.Hash{}, false
	}
	lhash, ok := TraverseProof(rl, wl)
	if !ok {
		return common.Hash{}, false
	}
	rhash, ok := TraverseProof(rr, wr)
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
		} else {
		}
		writer.writeNode(node)
	}
	return common.Hash(node), true
}

type indexMapFormat struct {
	leaves map[uint64]ProofFormat
	index  uint64
}

func NewIndexMapFormat() indexMapFormat {
	return indexMapFormat{leaves: make(map[uint64]ProofFormat), index: 1}
}

func (f indexMapFormat) AddLeaf(index uint64, subtree ProofFormat) indexMapFormat {
	if subtree != nil {
		f.leaves[index] = subtree
	}
	for index > 1 {
		index /= 2
		f.leaves[index] = nil
	}
	return f
}

func (f indexMapFormat) children() (left, right ProofFormat) {
	if st, ok := f.leaves[f.index]; ok {
		if st != nil {
			return st.children()
		}
		return indexMapFormat{leaves: f.leaves, index: f.index * 2}, indexMapFormat{leaves: f.leaves, index: f.index*2 + 1}
	}
	return nil, nil
}

func ParseMultiProof(proof []byte) (MultiProof, error) {
	if len(proof) < 3 || proof[0] != 1 { // ????
		return MultiProof{}, errors.New("invalid proof length")
	}
	leafCount := int(binary.LittleEndian.Uint16(proof[1:3]))
	if len(proof) != leafCount*34+1 {
		return MultiProof{}, errors.New("invalid proof length")
	}
	valuesStart := leafCount*2 + 1
	format := NewIndexMapFormat()
	if err := parseFormat(format.leaves, 1, proof[3:valuesStart]); err != nil {
		return MultiProof{}, err
	}
	values := make(MerkleValues, leafCount)
	for i := range values {
		copy(values[i][:], proof[valuesStart+i*32:valuesStart+(i+1)*32])
	}
	return MultiProof{Format: format, Values: values}, nil
}

func parseFormat(leaves map[uint64]ProofFormat, index uint64, format []byte) error {
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

type MergedFormat []ProofFormat // earlier one has priority

func (m MergedFormat) children() (left, right ProofFormat) {
	l := make(MergedFormat, 0, len(m))
	r := make(MergedFormat, 0, len(m))
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
	subtree           func(uint64) ProofFormat
}

func NewRangeFormat(begin, end uint64, subtree func(uint64) ProofFormat) rangeFormat {
	return rangeFormat{
		begin:   begin,
		end:     end,
		index:   1,
		subtree: subtree,
	}
}

func (rf rangeFormat) children() (left, right ProofFormat) {
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

type MultiProof struct {
	Format ProofFormat
	Values MerkleValues
}

func (mp MultiProof) Reader(subtrees func(uint64) ProofReader) *multiProofReader {
	values := mp.Values
	return &multiProofReader{format: mp.Format, values: &values, index: 1, subtrees: subtrees}
}

func (mp MultiProof) rootHash() common.Hash {
	hash, _ := TraverseProof(mp.Reader(nil), nil)
	return hash
}

type multiProofReader struct {
	format   ProofFormat
	values   *MerkleValues
	index    uint64
	subtrees func(uint64) ProofReader
}

func (mpr multiProofReader) children() (left, right ProofReader) {
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

func (mpr multiProofReader) readNode() (MerkleValue, bool) {
	if l, _ := mpr.format.children(); l == nil && len(*mpr.values) > 0 {
		hash := (*mpr.values)[0]
		*mpr.values = (*mpr.values)[1:]
		return hash, true
	}
	return MerkleValue{}, false
}

// should be checked after TraverseProof if received from an untrusted source
func (mpr multiProofReader) Finished() bool { //TODO is it checked everywhere where needed?
	return len(*mpr.values) == 0
}

type MergedReader []ProofReader

func (m MergedReader) children() (left, right ProofReader) {
	l := make(MergedReader, 0, len(m))
	r := make(MergedReader, 0, len(m))
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

func (m MergedReader) readNode() (value MerkleValue, ok bool) {
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
		return MerkleValue{}, false
	}
	return
}

type multiProofWriter struct {
	format   ProofFormat
	values   *MerkleValues
	index    uint64
	subtrees func(uint64) ProofWriter
}

// subtrees are not included in format
// dummy writer if target is nil; only subtrees are stored
func NewMultiProofWriter(format ProofFormat, target *MerkleValues, subtrees func(uint64) ProofWriter) multiProofWriter {
	return multiProofWriter{format: format, values: target, index: 1, subtrees: subtrees}
}

func (mpw multiProofWriter) children() (left, right ProofWriter) {
	if mpw.subtrees != nil {
		if subtree := mpw.subtrees(mpw.index); subtree != nil {
			return subtree.children()
		}
	}
	lf, rf := mpw.format.children()
	if lf == nil {
		return nil, nil
	}
	return multiProofWriter{format: lf, values: mpw.values, index: mpw.index * 2, subtrees: mpw.subtrees},
		multiProofWriter{format: rf, values: mpw.values, index: mpw.index*2 + 1, subtrees: mpw.subtrees}
}

func (mpw multiProofWriter) writeNode(node MerkleValue) {
	if mpw.values != nil {
		if lf, _ := mpw.format.children(); lf == nil {
			*mpw.values = append(*mpw.values, node)
		}
	}
	if mpw.subtrees != nil {
		if subtree := mpw.subtrees(mpw.index); subtree != nil {
			subtree.writeNode(node)
		}
	}
}

type valueWriter struct {
	format     ProofFormat
	values     MerkleValues
	index      uint64
	storeIndex func(uint64) int // if i := storeIndex(index); i >= 0 then value at given tree index is stored in values[i]
}

func NewValueWriter(format ProofFormat, target MerkleValues, storeIndex func(uint64) int) valueWriter {
	return valueWriter{format: format, values: target, index: 1, storeIndex: storeIndex}
}

func (vw valueWriter) children() (left, right ProofWriter) {
	lf, rf := vw.format.children()
	if lf == nil {
		return nil, nil
	}
	return valueWriter{format: lf, values: vw.values, index: vw.index * 2, storeIndex: vw.storeIndex},
		valueWriter{format: rf, values: vw.values, index: vw.index*2 + 1, storeIndex: vw.storeIndex}
}

func (vw valueWriter) writeNode(node MerkleValue) {
	if i := vw.storeIndex(vw.index); i >= 0 {
		vw.values[i] = node
	}
}

type MergedWriter []ProofWriter

func (m MergedWriter) children() (left, right ProofWriter) {
	l := make(MergedWriter, 0, len(m))
	r := make(MergedWriter, 0, len(m))
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

func (m MergedWriter) writeNode(value MerkleValue) {
	for _, w := range m {
		w.writeNode(value)
	}
}
