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

import (
	//"fmt"

	"github.com/ethereum/go-ethereum/common"
)

type tableSet []common.RangeSet[uint64]

type tableLevel struct {
	blockCount  uint64
	leanStorage bool //TODO
}

type protocolLevel struct {
	tailAge, headAge uint64
}

type tableID struct {
	level int
	index uint64
}

func (p *Params) newTableSet() tableSet {
	return make(tableSet, len(p.tableLevels))
}

func (ts tableSet) add(id tableID) {
	ts[id.level] = ts[id.level].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](id.index, 1)))
}

func (ts tableSet) remove(id tableID) {
	ts[id.level] = ts[id.level].Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](id.index, 1)))
}

func (ts tableSet) includes(id tableID) bool {
	return ts[id.level].Includes(id.index)
}

func (ts tableSet) isEmpty() bool {
	for _, rs := range ts {
		if !rs.IsEmpty() {
			return false
		}
	}
	return true
}

func (ts tableSet) count() uint64 {
	var count uint64
	for _, rs := range ts {
		count += rs.Count()
	}
	return count
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

const (
	opNone = iota
	opMerge
	opDelete
)

type tableOperation struct {
	operation int
	id        tableID
}

func (p *Params) compareOps(a, b tableOperation, lowLevelFirst bool) int {
	switch {
	case a.operation > b.operation:
		return 1
	case a.operation < b.operation:
		return -1
	}
	if lowLevelFirst {
		switch {
		case a.id.level < b.id.level:
			return 1
		case a.id.level > b.id.level:
			return -1
		}
	}
	aa := p.blockRange(a.id).First()
	bb := p.blockRange(b.id).First()
	switch {
	case aa > bb:
		return 1
	case aa < bb:
		return -1
	}
	if !lowLevelFirst {
		switch {
		case a.id.level < b.id.level:
			return 1
		case a.id.level > b.id.level:
			return -1
		}
	}
	return 0
}

func (p *Params) addToOps(ops *[]tableOperation, newOp tableOperation, maxThreads int, lowLevelFirst bool) bool {
	pos := len(*ops)
loop:
	for i, op := range *ops {
		switch p.compareOps(newOp, op, lowLevelFirst) {
		case 0:
			return true
		case 1:
			pos = i
			break loop
		}
	}
	if pos >= maxThreads {
		return false
	}
	if len(*ops) < maxThreads {
		*ops = append(*ops, tableOperation{})
	}
	copy((*ops)[pos+1:len(*ops)], (*ops)[pos:len(*ops)-1])
	(*ops)[pos] = newOp
	return true
}

func (p *Params) nextTableOperations(complete, partial, target tableSet, lowLevelMergeThreads, mergeThreads int) ([]tableOperation, common.RangeSet[uint64]) {
	var (
		bestLowLevelOps, bestOps []tableOperation
		required                 common.RangeSet[uint64]
	)
	for i := len(p.tableLevels) - 1; i >= 0; i-- {
		required = required.Union(target[i])
		//fmt.Println("level", i, "complete", complete[i], "partial", partial[i], "target", target[i], "required", required)
		if remove := complete[i].Union(partial[i]).Difference(required); !remove.IsEmpty() {
			//fmt.Println(" remove", remove)
			// Note that we deliberately only add one delete operation candidate
			// per level in order to avoid delete operations always interrupting
			// all merge operations
			op := tableOperation{
				operation: opDelete,
				id: tableID{
					level: i,
					index: remove.Last(),
				},
			}
			p.addToOps(&bestLowLevelOps, op, lowLevelMergeThreads, true)
			p.addToOps(&bestOps, op, mergeThreads, false)
		}
		required = required.Difference(complete[i])
		if i > 0 {
			merge := required.Intersection(shiftRangeSetLevel(complete[i-1], p.tableLevels[i-1], p.tableLevels[i], false))
			for !merge.IsEmpty() {
				//fmt.Println(" merge", merge)
				op := tableOperation{
					operation: opMerge,
					id: tableID{
						level: i,
						index: merge.Last(),
					},
				}
				add1 := p.addToOps(&bestLowLevelOps, op, lowLevelMergeThreads, true)
				add2 := p.addToOps(&bestOps, op, mergeThreads, false)
				if !add1 && !add2 {
					break
				}
				merge = merge.Difference(common.SingleRangeSet[uint64](common.NewRange[uint64](merge.Last(), 1)))
			}
			required = shiftRangeSetLevel(required /*.Difference(merge)*/, p.tableLevels[i], p.tableLevels[i-1], false)
		}
	}
	//fmt.Println("nextTableOperations  best", bestOps, "bestLL", bestLowLevelOps, "required blocks", required)
	ops := bestLowLevelOps
	for _, op := range bestOps {
		if !p.addToOps(&ops, op, mergeThreads, true) {
			break
		}
	}
	//fmt.Println(" final", ops)
	return ops, required
}

// TODO partial helyett vmi jobb nev
func shiftRangeSetLevel(rs common.RangeSet[uint64], from, to tableLevel, partial bool) (result common.RangeSet[uint64]) {
	for _, r := range rs {
		rr := shiftRangeLevel(r, from, to, partial)
		if !rr.IsEmpty() {
			result = result.Union(common.SingleRangeSet[uint64](rr))
		}
	}
	return result
}

func shiftRangeLevel(r common.Range[uint64], from, to tableLevel, partial bool) common.Range[uint64] {
	first := r.First() * from.blockCount
	if !partial {
		first += to.blockCount - 1
	}
	first /= to.blockCount
	afterLast := r.AfterLast() * from.blockCount
	if partial {
		afterLast += to.blockCount - 1
	}
	afterLast /= to.blockCount
	if afterLast <= first {
		return common.Range[uint64]{}
	}
	return common.NewRange[uint64](first, afterLast-first)
}

func (p *Params) rangeTarget(avail tableSet, blockRange common.RangeSet[uint64]) tableSet {
	target := p.newTableSet()
	for i := len(p.tableLevels) - 1; i >= 0; i-- {
		fullTables := shiftRangeSetLevel(blockRange, p.tableLevels[0], p.tableLevels[i], false)
		partialTables := shiftRangeSetLevel(blockRange, p.tableLevels[0], p.tableLevels[i], true)
		target[i] = fullTables.Union(partialTables.Intersection(avail[i]))
		blockRange = blockRange.Difference(shiftRangeSetLevel(target[i], p.tableLevels[i], p.tableLevels[0], false))
	}
	return target
}
