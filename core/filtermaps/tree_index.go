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
	"math/bits"

	"github.com/ethereum/go-ethereum/common"
	"lukechampine.com/uint128"
)

type treeIndex uint128.Uint128

var gtiRoot = treeIndex(uint128.From64(1))

func (t *treeIndex) matchRoot(relIndex uint64) bool {
	rLevel := uint(63 - bits.LeadingZeros64(relIndex))
	tLevel := t.level()
	if rLevel > tLevel || t.rsh(tLevel-rLevel) != ti64(relIndex) {
		return false
	}
	*t = t.rsh(rLevel)
	return true

}

func (t *treeIndex) splitRoot(levels uint) common.Range[uint64] {
	tl := t.level()
	if tl >= levels {
		subIndex := t.rsh(tl-levels).Lo - (uint64(1) << levels)
		m := gtiRoot.lsh(tl - levels)
		*t = t.and(m.sub(gtiRoot)).add(m)
		return common.NewRange[uint64](subIndex, 1)
	}
	subIndex := t.and64((uint64(1) << tl) - 1)
	*t = gtiRoot
	return common.NewRange[uint64](subIndex<<(levels-tl), uint64(1)<<(levels-tl))
}

func (t treeIndex) subIndex(subtreeLevel uint) treeIndex {
	tl := t.level()
	if tl < subtreeLevel {
		panic("subIndex: invalid subtreeLevel")
	}
	m := gtiRoot.lsh(tl - subtreeLevel)
	return t.and(m.sub(gtiRoot)).add(m)
}

func (t treeIndex) add(u treeIndex) treeIndex {
	return treeIndex(uint128.Uint128(t).Add(uint128.Uint128(u)))
}
func (t treeIndex) sub(u treeIndex) treeIndex {
	return treeIndex(uint128.Uint128(t).Sub(uint128.Uint128(u)))
}
func (t treeIndex) and(u treeIndex) treeIndex {
	return treeIndex(uint128.Uint128(t).And(uint128.Uint128(u)))
}
func (t treeIndex) cmp(u treeIndex) int      { return uint128.Uint128(t).Cmp(uint128.Uint128(u)) }
func (t treeIndex) add64(v uint64) treeIndex { return treeIndex(uint128.Uint128(t).Add64(v)) }
func (t treeIndex) and64(v uint64) uint64    { return uint128.Uint128(t).And64(v).Lo }
func (t treeIndex) leftChild() treeIndex     { return t.lsh(1) }
func (t treeIndex) rightChild() treeIndex    { return t.lsh(1).add64(1) }
func (t treeIndex) parent() treeIndex        { return t.rsh(1) }
func (t treeIndex) level() uint              { return 127 - uint(uint128.Uint128(t).LeadingZeros()) }

func (t treeIndex) gtSub(subIndex uint64) treeIndex {
	shift := uint(63 - bits.LeadingZeros64(subIndex))
	return t.lsh(shift).add64(subIndex - (uint64(1) << shift))
}

func (t treeIndex) arraySub(arrayIndex uint64, indexLen uint) treeIndex {
	return t.lsh(indexLen).add64(arrayIndex)
}

func ti64(i uint64) treeIndex                { return treeIndex(uint128.From64(i)) }
func (t treeIndex) lsh(shift uint) treeIndex { return treeIndex(uint128.Uint128(t).Lsh(shift)) }
func (t treeIndex) rsh(shift uint) treeIndex { return treeIndex(uint128.Uint128(t).Rsh(shift)) }
