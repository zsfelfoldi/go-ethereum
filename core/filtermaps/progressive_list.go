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
	"errors"
	"hash/fnv"
	"math"
	"math/bits"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

type treeData interface {
	getLeaf(leafIndex uint64) (merkle.Value, error)
	leafCount() (uint64, error)
}

type progressiveTree struct {
	logSubtreeSize        []uint
	leafOffset, treeIndex uint64
	data                  treeData
}

func (p progressiveTree) Subtree(subIndex uint64) merkle.TreeShape {
	if subIndex == 1 {
		return p
	}
	if len(p.logSubtreeSize) == 1 {
		newIndex := merkle.ChildIndex(r.treeIndex, subIndex)
		if p.treeIndex >= uint64(1)<<(p.logSubtreeSize[0]+1) {
			return nil
		}
		p.treeIndex = newIndex
		return p
	}
	side, newSubIndex := merkle.SplitIndex(subIndex, 1)
	p.treeIndex = 1
	switch side {
	case 2:
		p.logSubtreeSize = p.logSubtreeSize[:1]
	case 3:
		p.leafOffset += uint64(1) << p.logSubtreeSize[0]
		p.logSubtreeSize = p.logSubtreeSize[1:]
	default:
		panic("invalid tree index")
	}
	return p.Subtree(newSubIndex)
}

func (p progressiveTree) IsLeaf() bool {
	return len(p.logSubtreeSize) == 1 && p.treeIndex >= uint64(1)<<p.logSubtreeSize[0]
}

func (p progressiveTree) IsSymmetrical() bool {
	return len(p.logSubtreeSize) == 1 && p.treeIndex < uint64(1)<<p.logSubtreeSize[0]
}

func (p progressiveTree) IsEmpty() (bool, error) {
	if leafCount, err := p.data.leafCount(); err == nil {
		return p.leafOffset >= leafCount, nil
	} else {
		return false, err
	}
}

func (p progressiveTree) GetLeaf() (merkle.Value, error) {
	if len(p.logSubtreeSize) != 1 {
		return merkle.Value{}, errNotLeaf
	}
	firstLeaf := uint64(1) << p.logSubtreeSize[0]
	if p.treeIndex < firstLeaf {
		return merkle.Value{}, errNotLeaf
	}
	if p.treeIndex >= 2*firstLeaf {
		panic("invalid progressive tree index")
	}
	return p.data.getLeaf(p.treeIndex - firstLeaf)
}
