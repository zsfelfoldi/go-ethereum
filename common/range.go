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

package common

import (
	"iter"
	"sort"
)

// Range represents a range of integers.
type Range[T uint32 | uint64] struct {
	first, afterLast T
}

// NewRange creates a new range based of first element and number of elements.
func NewRange[T uint32 | uint64](first, count T) Range[T] {
	afterLast := first + count
	if afterLast < first {
		panic("range overflow")
	}
	return Range[T]{first, afterLast}
}

// First returns the first element of the range.
func (r Range[T]) First() T {
	return r.first
}

// Last returns the last element of the range. This panics for empty ranges.
func (r Range[T]) Last() T {
	if r.first == r.afterLast {
		panic("last item of zero length range is not allowed")
	}
	return r.afterLast - 1
}

// AfterLast returns the first element after the range. This allows obtaining
// information about the end part of zero length ranges.
func (r Range[T]) AfterLast() T {
	return r.afterLast
}

// Count returns the number of elements in the range.
func (r Range[T]) Count() T {
	return r.afterLast - r.first
}

// IsEmpty returns true if the range is empty.
func (r Range[T]) IsEmpty() bool {
	return r.first == r.afterLast
}

// Includes returns true if the given element is inside the range.
func (r Range[T]) Includes(v T) bool {
	return v >= r.first && v < r.afterLast
}

// SetFirst updates the first element of the list.
func (r *Range[T]) SetFirst(v T) {
	r.first = v
	if r.afterLast < r.first {
		r.afterLast = r.first
	}
}

// SetAfterLast updates the end of the range by specifying the first element
// after the range. This allows setting zero length ranges.
func (r *Range[T]) SetAfterLast(v T) {
	r.afterLast = v
	if r.afterLast < r.first {
		r.first = r.afterLast
	}
}

// SetLast updates last element of the range.
func (r *Range[T]) SetLast(v T) {
	r.SetAfterLast(v + 1)
}

// Intersection returns the intersection of two ranges.
func (r Range[T]) Intersection(q Range[T]) Range[T] {
	i := Range[T]{first: max(r.first, q.first), afterLast: min(r.afterLast, q.afterLast)}
	if i.first > i.afterLast {
		return Range[T]{}
	}
	return i
}

// Union returns the union of two ranges. Panics for gapped ranges.
func (r Range[T]) Union(q Range[T]) Range[T] {
	if r.IsEmpty() {
		return q
	}
	if q.IsEmpty() {
		return r
	}
	if max(r.first, q.first) > min(r.afterLast, q.afterLast) {
		panic("cannot create union; gap between ranges")
	}
	return Range[T]{first: min(r.first, q.first), afterLast: max(r.afterLast, q.afterLast)}
}

// Iter iterates all integers in the range.
func (r Range[T]) Iter() iter.Seq[T] {
	return func(yield func(T) bool) {
		for i := r.first; i < r.afterLast; i++ {
			if !yield(i) {
				break
			}
		}
	}
}

type RangeSet[T uint32 | uint64] []Range[T]

func (a RangeSet[T]) Includes(v T) bool {
	for _, r := range a {
		if r.Includes(v) {
			return true
		}
	}
	return false
}

func (a RangeSet[T]) ClosestLte(v T) (last T, found bool) {
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

func (a RangeSet[T]) ClosestGte(v T) (last T, found bool) {
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

func (rb *rangeBoundaries[T]) add(r Range[T], d int) {
	*rb = append((*rb), rangeBoundary[T]{v: r.First(), d: d}, rangeBoundary[T]{v: r.AfterLast(), d: -d})
}

func (rb rangeBoundaries[T]) makeSet(threshold int) RangeSet[T] {
	res := make(RangeSet[T], 0, len(rb)/2)
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
				res = append(res, NewRange[T](start, r.v-start))
			}
			lastCmp = cmp
		}
	}
	return res
}

func (a RangeSet[T]) Intersection(b RangeSet[T]) RangeSet[T] {
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

func (a RangeSet[T]) Difference(b RangeSet[T]) RangeSet[T] {
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

func (a RangeSet[T]) Union(b RangeSet[T]) RangeSet[T] {
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
func (r RangeSet[T]) Iter() iter.Seq[T] {
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

func (a RangeSet[T]) Count() T {
	var count T
	for _, r := range a {
		count += r.Count()
	}
	return count
}

func (a RangeSet[T]) SingleRange() Range[T] {
	if len(a) > 1 {
		panic("singleRange called for non-continuous RangeSet")
	}
	if len(a) == 1 {
		return a[0]
	}
	return NewRange[T](0, 0)
}

func SingleRangeSet[T uint32 | uint64](r Range[T]) RangeSet[T] {
	if r.IsEmpty() {
		return nil
	}
	return RangeSet[T]{r}
}

func (a RangeSet[T]) Equal(b RangeSet[T]) bool {
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

func (a RangeSet[T]) IsEmpty() bool {
	return len(a) == 0
}

func (a RangeSet[T]) Last() T {
	if len(a) == 0 {
		panic("last item of empty range set is not allowed")
	}
	return a[len(a)-1].Last()
}

func (a RangeSet[T]) LastSection() Range[T] {
	if len(a) == 0 {
		panic("last section of empty range set is not allowed")
	}
	return a[len(a)-1]
}
