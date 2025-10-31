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
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

type merkleNode struct {
	value merkle.Value
	size  uint64 // 62 bits estimated storage size plus unknown and finalized flag
}

var (
	mnSizeMask      = (uint64(1) << 62) - 1
	mnUnknownMask   = uint64(1) << 62
	mnFinalizedMask = uint64(1) << 63
)

func merkleHash(a, b *merkleNode) (r merkleNode) {
	if (a.size || b.size)&mnUnknownMask != 0 {
		panic("cannot hash unknown node")
	}
	r.size = (a.size+b.size)&mnSizeMask + (a.size & b.size & mnFinalizedMask) // add sizes, binary and finalized flags
	hasher := sha256.New()
	hasher.Write(a.value[:])
	hasher.Write(b.value[:])
	hasher.Sum(r.value[:0])
	return
}

type merklePath struct {
	index uint64
	path  [][2]merkleNode // leaf to root
	root  merkleNode
}

func (p *merklePath) leaf() merkleNode {
	return p.path[0][p.index%2]
}

func (p *merklePath) setLeaf(v merkleNode) {
	index := p.index
	p.path[0][index%2] = v
	var i int
	for index > 1 {
		i++
		index /= 2
		if p.path[i][index%2].size&mnUnknownMask != 0 {
			break
		}
		p.path[i][index%2].size = mnUnknownMask
	}
	p.root.size = mnUnknownMask
}

func (p *merklePath) advance(index uint64) {}

func (p *merklePath) root() merkleNode {}
