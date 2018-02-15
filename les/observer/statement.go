// Copyright 2018 The go-ethereum Authors
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

package observer

import (
	"io"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto/sha3"
	"github.com/ethereum/go-ethereum/rlp"
)

// -----
// STATEMENT
// -----

// keyValue manages the key and value of a statement.
type keyValue struct {
	Key   []byte `json:"key" gencodec:"required"`
	Value []byte `json:"key" gencodec:"required"`

	// Signature values.
	// QUESTION: Will it be needed for statements?
	V *big.Int `json:"v" gencodec:"required"`
	R *big.Int `json:"r" gencodec:"required"`
	S *big.Int `json:"s" gencodec:"required"`
}

// Statement contains a combination of key and value.
type Statement struct {
	kv keyValue

	// Caches.
	hash atomic.Value
	size atomic.Value
}

// NewStatement creates a standard statement with a keyValue.
func NewStatement(key, value []byte) *Statement {
	if len(key) > 0 {
		key = common.CopyBytes(key)
	}
	if len(value) > 0 {
		value = common.CopyBytes(value)
	}
	kv := keyValue{
		Key:   key,
		Value: value,
		V:     new(big.Int),
		R:     new(big.Int),
		S:     new(big.Int),
	}
	return &Statement{
		kv: kv,
	}
}

// Key returns copy of the statements key.
func (st *Statement) Key() []byte {
	return common.CopyBytes(st.kv.Key)
}

// Value returns copy of the statements value.
func (st *Statement) Value() []byte {
	return common.CopyBytes(st.kv.Value)
}

// EncodeRLP implements rlp.Encoder.
func (st *Statement) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &st.kv)
}

// DecodeRLP implements rlp.Decoder.
func (st *Statement) DecodeRLP(s *rlp.Stream) error {
	_, size, _ := s.Kind()
	err := s.Decode(&st.kv)
	if err == nil {
		st.size.Store(common.StorageSize(rlp.ListSize(size)))
	}
	return err
}

// Hash hashes the RLP encoding of the statements key/value.
// It uniquely identifies it.
func (st *Statement) Hash() common.Hash {
	if hash := st.hash.Load(); hash != nil {
		return hash.(common.Hash)
	}
	h := rlpHash(st.kv)
	st.hash.Store(h)
	return h
}

// Size returns the storage size of the statement.
func (st *Statement) Size() common.StorageSize {
	if size := st.size.Load(); size != nil {
		return size.(common.StorageSize)
	}
	c := writeCounter(0)
	rlp.Encode(&c, &st.kv)
	st.size.Store(common.StorageSize(c))
	return common.StorageSize(c)
}

// -----
// STATEMENTS
// -----

// Statements contains a number of statements as slice.
type Statements []*Statement

// Len implements types.DerivableList and returns the number
// of statements.
func (sts Statements) Len() int {
	return len(sts)
}

// GetRlp implements types.DerivableList and returns the i'th
// statement of s in RLP encoding.
func (sts Statements) GetRlp(i int) []byte {
	enc, _ := rlp.EncodeToBytes(sts[i])
	return enc
}

// -----
// HELPERS
// -----

// rlpHash calculates a hash out of the passed data.
func rlpHash(x interface{}) (h common.Hash) {
	hw := sha3.NewKeccak256()
	rlp.Encode(hw, x)
	hw.Sum(h[:0])
	return h
}

// writeCounter helps counting the written bytes in total.
type writeCounter common.StorageSize

// Write implements io.Writer and counts the written bytes
// in total.
func (c *writeCounter) Write(b []byte) (int, error) {
	*c += writeCounter(len(b))
	return len(b), nil
}
