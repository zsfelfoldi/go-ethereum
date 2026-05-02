// Copyright 2026 The go-ethereum Authors
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

package logindex

type tableSet []rangeSet

type Params struct {
	tableLevels    []tableLevel
	protocolLevels []protocolLevel
}

type tableLevel struct {
	//TODO storage mode
	blockCount uint64
}

type tableID struct {
	level int
	index uint64
}

func (p *Params) blockRange(id tableID) common.Range[uint64] {
	return common.NewRange[uint64](p.tableLevels[id.level].blockCount*id.index, p.tableLevels[id.level].blockCount)
}

func (p *Params) rangeID(r common.Range[uint64]) (tableID, bool) {
	for i, tl := range p.tableLevels {
		if tl.blockCount == r.Count() {
			if r.First()%tl.blockCount != 0 {
				return tableID{}, false
			}
			return tableID{level: i, index: r.First() / tl.blockCount}, true
		}
	}
	return tableID{}, false
}

type protocolLevel struct {
	tailAge, headAge uint64
}

const (
	opNone = iota
	opMerge
	opDelete
)

type tableOperation struct {
	operation int
	id        tableID
}

func (p *Params) compareOps(a, b tableOperation) int {
	switch {
	case a.operation > b.operation:
		return 1
	case a.operation < b.operation:
		return -1
	}
	aa := p.blockRange(a.id).First()
	bb := p.blockRange(b.id).First()
	switch {
	case aa > bb:
		return 1
	case aa < bb:
		return -1
	default:
		return 0
	}
}

func (p *Params) nextAction(complete, partial, target tableSet) (tableOperation, common.Range[uint64]) {
	var (
		bestOp    tableOperation
		reqBlocks common.Range[uint64]
		required  rangeSet
	)
	for i := len(p.tableLevels) - 1; i >= 0; i-- {
		required = required.or(target[i])
		if remove := complete[i].or(partial[i]).andNot(required); !remove.isEmpty {
			op := tableOperation{
				operation: opDelete,
				id: tableID{
					level: i,
					index: remove.last(),
				},
			}
			if p.compareOps(op, bestOp) > 0 {
				bestOp = op
			}
		}
		required = required.andNot(complete[i])
		if i > 0 {
			merge := required.and(shiftTableLevel(avail[i-1], p.tableLevels[i-1], p.tableLevels[i], false))
			if !merge.isEmpty {
				op := tableOperation{
					operation: opMerge,
					id: tableID{
						level: i,
						index: merge.last(),
					},
				}
				if p.compareOps(op, bestOp) > 0 {
					bestOp = op
				}
			}
			required = shiftTableLevel(required.andNot(merge), p.tableLevels[i], p.tableLevels[i-1], false)
		} else {
			if !required.isEmpty() {
				reqBlocks = required.lastSection()
			}
		}
	}
	return bestOp, reqBlocks
}

func shiftTableLevel(rs rangeSet, from, to tableLevel, partial bool) rangeSet {
	for i, r := range rs {
		first := r.First()*from.blockCount + from.offset - to.offset
		if !partial {
			first += to.blockCount - 1
		}
		first /= to.blockCount
		afterLast := r.AfterLast()*from.blockCount + from.offset - to.offset
		if partial {
			afterLast += to.blockCount - 1
		}
		afterLast /= to.blockCount
		rs[i] = common.NewRange[uint64](first, afterLast-first)
	}
	rs.normalize()
	return rs
}

func (p *Params) rangeTarget(avail tableSet, blockRange rangeSet) tableSet {
	target := p.newTableSet()
	for i := len(p.tableLevels) - 1; i >= 0; i-- {
		fullTables := shiftTableLevel(blockRange, p.tableLevels[0], p.tableLevels[i], false)
		partialTables := shiftTableLevel(blockRange, p.tableLevels[0], p.tableLevels[i], true)
		target[i] = fullTables.or(partialTables.and(avail[i]))
		blockRange = blockRange.andNot(shiftTableLevel(target[i], p.tableLevels[i], p.tableLevels[0], false))
	}
	return target
}

type rangeSet[T uint32 | uint64] []common.Range[T]

func (a rangeSet[T]) includes(v T) bool {
	for _, r := range a {
		if r.Includes(v) {
			return true
		}
	}
	return false
}

func (a rangeSet[T]) closestLte(v T) (last T, found bool) {
	for _, r := range a {
		if r.First() > v {
			return
		}
		if r.AfterLast() > v {
			return v, true
		}
		last, found = r.Last(), true
	}
	return
}

func (a rangeSet[T]) closestGte(v T) (last T, found bool) {
	for _, r := range a {
		if r.First() > v {
			return r.First(), true
		}
		if r.AfterLast() > v {
			return v, true
		}
	}
	return
}

type rangeBoundary[T uint32 | uint64] struct {
	v T
	d int
}

type rangeBoundaries[T uint32 | uint64] []rangeBoundary[T]

func (rb *rangeBoundaries[T]) add(r common.Range[T], d int) {
	*rb = append((*rb), rangeBoundary[T]{v: r.First(), d: d}, rangeBoundary[T]{v: r.AfterLast(), d: -d})
}

func (rb rangeBoundaries[T]) makeSet(threshold int) rangeSet[T] {
	res := make(rangeSet[T], 0, len(rb)/2)
	sort.Slice(rb, func(i, j int) bool {
		return rb[i].v < rb[j].v
	})
	var (
		sum     int
		lastCmp bool
		start   T
	)
	for i, r := range rb {
		sum += r.d
		cmp := sum >= threshold
		if cmp != lastCmp && (i == len(rb)-1 || rb[i+1].v != r.v) {
			if cmp {
				start = r.v
			} else {
				res = append(res, common.NewRange[T](start, r.v-start))
			}
			lastCmp = cmp
		}
	}
	return res
}

func (a rangeSet[T]) intersection(b rangeSet[T]) rangeSet[T] {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	rb := make(rangeBoundaries[T], 0, (len(a)+len(b))*2)
	for _, r := range a {
		rb.add(r, 1)
	}
	for _, r := range b {
		rb.add(r, 1)
	}
	return rb.makeSet(2)
}

func (a rangeSet[T]) exclude(b rangeSet[T]) rangeSet[T] {
	if len(a) == 0 {
		return nil
	}
	if len(b) == 0 {
		return a
	}
	rb := make(rangeBoundaries[T], 0, (len(a)+len(b))*2)
	for _, r := range a {
		rb.add(r, 1)
	}
	for _, r := range b {
		rb.add(r, -1)
	}
	return rb.makeSet(1)
}

func (a rangeSet[T]) union(b rangeSet[T]) rangeSet[T] {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	rb := make(rangeBoundaries[T], 0, (len(a)+len(b))*2)
	for _, r := range a {
		rb.add(r, 1)
	}
	for _, r := range b {
		rb.add(r, 1)
	}
	return rb.makeSet(1)
}

// iter iterates all integers in the range set.
func (r rangeSet[T]) iter() iter.Seq[T] {
	return func(yield func(T) bool) {
		for _, rr := range r {
			for i := range rr.Iter() {
				if !yield(i) {
					break
				}
			}
		}
	}
}

func (a rangeSet[T]) count() T {
	var count T
	for _, r := range a {
		count += r.Count()
	}
	return count
}

func (a rangeSet[T]) singleRange() common.Range[T] {
	if len(a) > 1 {
		panic("singleRange called for non-continuous rangeSet")
	}
	if len(a) == 1 {
		return a[0]
	}
	return common.NewRange[T](0, 0)
}

func singleRangeSet[T uint32 | uint64](r common.Range[T]) rangeSet[T] {
	if r.IsEmpty() {
		return nil
	}
	return rangeSet[T]{r}
}

func (a rangeSet[T]) equal(b rangeSet[T]) bool {
	if len(a) != len(b) {
		return false
	}
	for i, r := range a {
		if b[i] != r {
			return false
		}
	}
	return true
}

func (a rangeSet[T]) isEmpty() bool {
	return len(a) == 0
}
