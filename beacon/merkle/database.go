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

// Package merkle implements proof verifications in binary merkle trees.
package merkle

import (
	"crypto/sha256"
	"math/bits"
)

type TreeShape interface {
	Subtree(subIndex uint64) TreeShape
	IsLeaf() bool        // tree is a single leaf
	IsSymmetrical() bool // tree has two identically shaped child subtrees
}

type TreeHashable interface {
	TreeShape
	IsEmpty() (bool, error)  // all leafs of the tree return Value{}
	GetLeaf() (Value, error) // assumes that the tree is a single leaf
}

type Database struct {
	data        TreeHashable
	emptyValues *emptySubtreeValues
	db          ethdb.KeyValueStore
	cache       *lru.Cache[uint64, treeNode]
}

type treeNode struct {
	value     Value
	hashCount uint32
}

func NewDatabase(db ethdb.KeyValueStore, data TreeHashable, cacheSize int) *Database {
	return &Database{
		db:          db,
		data:        data,
		emptyValues: makeEmptyValues(data, 1),
		cache:       lru.NewCache[uint64, treeNode](cacheSize),
	}
}

func (db *Database) Get(index uint64) (Value, error) {
	node, err := db.get(index)
	if err != nil {
		return Value{}, err
	}
	db.cache.Put(index, node)
	return node.value, nil
}

func (db *Database) get(index uint64) (treeNode, error) {
	if !db.data.Exists(index) {
		return treeNode{}, ErrNonexistentNode
	}
	if node, ok := db.cache.Get(index); ok && node != invalidatedNode {
		return node, nil
	}
	if db.data.IsLeaf(index) {
		value := db.data.GetLeaf(index)
		return treeNode{value: value, hashCount: 0}, nil
	}
	key := db.dbKey(index)
	has, err := db.db.Has(key)
	if err != nil {
		return treeNode{}, err
	}
	if has {
		v, err := db.db.Get(key)
		if err != nil {
			return treeNode{}, err
		}
		if len(v) != len(Value) {
			return treeNode{}, errors.New("invalid tree node length")
		}
		var value Value
		copy(value[:], v)
		return treeNode{value: value, hashCount: 0}, nil
	}
	left, err := db.get(index * 2)
	if err != nil {
		return treeNode{}, err
	}
	right, err := db.get(index*2 + 1)
	if err != nil {
		return treeNode{}, err
	}
	node := treeNode{
		value:     db.hash(&left.value, &right.value),
		hashCount: left.hashCount + right.hashCount + 1,
	}
	if node.hashCount >= db.hashCountThreshold || index < db.storeAllBelow {
		if err := db.db.Put(key, node.value[:]); err != nil {
			return treeNode{}, err
		}
		node.hashCount = 0
	}
	return node, nil
}

func (db *Database) Invalidate(index uint64) {
	if !db.data.Exists(index) {
		return
	}
	if node, ok := db.cache.Get(index); ok && node == invalidatedNode {
		return
	}

}

type emptySubtreeValues struct {
	values      []Value
	left, right *emptySubtreeValues
}

func makeEmptyValues(data TreeShape, index uint64) *emptySubtreeValues {
	if data.IsLeaf(index) {
		return &emptySubtreeValues{values: []Value{Value{}}}
	}
	if data.IsSymmetrical(index) {
		e := makeEmptyValues(data, index*2)
		var hash Value
		hasher := sha256.New()
		hasher.Write(e.values[0][:])
		hasher.Write(e.values[0][:])
		hasher.Sum(hash[:0])
		e.values = append([]Value{hash}, e.values...)
		return e
	}
	e := &emptySubtreeValues{
		values: []Value{Value{}},
		left:   makeEmptyValues(data, index*2),
		right:  makeEmptyValues(data, index*2+1),
	}
	hasher := sha256.New()
	hasher.Write(e.left.values[0][:])
	hasher.Write(e.right.values[0][:])
	hasher.Sum(e.values[0][:0])
	return e
}

func (e *emptySubtreeValues) get(index uint64) Value {
	level := 63 - bits.LeadingZeros64(index)
	if level < len(e.values) {
		return e.values[level]
	}
	splitMask := uint64(1) << (level - len(e.values))
	subIndex := index&(splitMask-1) + splitMask
	if index&splitMask == 0 {
		return e.left.get(subIndex)
	}
	return e.right.get(subIndex)
}
