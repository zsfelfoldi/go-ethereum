// Copyright 2020 The go-ethereum Authors
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

package lespay

import "bytes"

const (
	ServiceMapFilterName = "f"
	ServiceMapQueryName  = "q"
	CapacityQueryName    = "cq"
)

type (
	Request struct {
		Service, Name string
		Params        []byte
	}
	Requests []Request

	Range struct {
		Shared, From, To []byte
	}
	ServiceDimension struct {
		Key   string
		Range Range
	}
	ServiceRange []ServiceDimension

	ServiceMapFilterReq struct {
		FilterRange ServiceRange
		SetAlias    string
	}
	ServiceMapFilterResp []string // service IDs

	ServiceMapQueryReq  string
	ServiceMapQueryResp struct {
		Id, Desc string
		Distance uint64 // for request forwarding; not used yet
		Range    ServiceRange
	}

	CapacityQueryReq struct {
		Bias      uint64 // seconds
		AddTokens []uint64
	}
	CapacityQueryResp []uint64
)

func NewRange(begin, end []byte, includeBegin, includeEnd bool) Range {
	if !includeBegin {
		begin = append(begin, 0)
	}
	if includeEnd {
		end = append(end, 0)
	}
	if len(end) > 0 && bytes.Compare(begin, end) != -1 {
		panic("Invalid range")
	}
	var i int
	for i < len(begin) && i < len(end) && begin[i] == end[i] {
		i++
	}
	return Range{
		Shared: begin[:i],
		From:   begin[i:],
		To:     end[i:],
	}
}

func (r *Range) Includes(i *Range) bool {
	if !bytes.HasPrefix(i.Shared, r.Shared) {
		return false
	}
	from, to := i.From, i.To
	if len(i.Shared) > len(r.Shared) {
		diff := i.Shared[len(r.Shared):]
		from = append(diff, from...)
		to = append(diff, to...)
	}
	return bytes.Compare(r.From, from) <= 0 && bytes.Compare(r.To, to) >= 0
}

func (r *ServiceRange) Includes(i ServiceRange) bool {
	m := make(map[string]Range)
	for _, dim := range *r {
		m[dim.Key] = dim.Range
	}
	for _, dim := range i {
		if r, ok := m[dim.Key]; !ok || !r.Includes(&dim.Range) {
			return false
		}
	}
	return true
}

func (r *ServiceRange) AddDimension(key string, servedRange Range) {
	*r = append(*r, ServiceDimension{Key: key, Range: servedRange})
}
