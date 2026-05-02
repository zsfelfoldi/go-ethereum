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

type tableSet []common.RangeSet[uint64]

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

func (ts tableSet) add(id tableID) {
	ts[id.level] = ts[id.level].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](id.index, 1)))
}

func (ts tableSet) remove(id tableID) {
	ts[id.level] = ts[id.level].Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](id.index, 1)))
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
		required  common.RangeSet[uint64]
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

func shiftTableLevel(rs common.RangeSet[uint64], from, to tableLevel, partial bool) common.RangeSet[uint64] {
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

func (p *Params) rangeTarget(avail tableSet, blockRange common.RangeSet[uint64]) tableSet {
	target := p.newTableSet()
	for i := len(p.tableLevels) - 1; i >= 0; i-- {
		fullTables := shiftTableLevel(blockRange, p.tableLevels[0], p.tableLevels[i], false)
		partialTables := shiftTableLevel(blockRange, p.tableLevels[0], p.tableLevels[i], true)
		target[i] = fullTables.or(partialTables.and(avail[i]))
		blockRange = blockRange.andNot(shiftTableLevel(target[i], p.tableLevels[i], p.tableLevels[0], false))
	}
	return target
}
