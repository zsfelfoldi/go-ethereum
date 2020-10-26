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

import (
	"bytes"
	"math"
	"math/big"
)

const (
	ServiceMapFilterName = "f"
	ServiceMapQueryName  = "q"
	CapacityQueryName    = "capq"
	ExchangeName         = "ex"
	PriceQueryName       = "pq"
)

type (
	Request struct {
		Service, Name string
		Params        []byte
	}
	Requests []Request

	// parameter encoding for individual request types
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

	ExchangeReq struct {
		PaymentId                         string
		MinTokens, MaxTokens, MaxCurrency IntOrInf
	}
	ExchangeResp struct {
		TokenBalance, CurrencyBalance, TokensEx, CurrencyEx IntOrInf
	}

	PriceQueryReq struct {
		PaymentId    string
		TokenAmounts []IntOrInf
	}
	PriceQueryResp []IntOrInf
)

type (
	Range struct {
		Shared, From, To []byte
	}
	ServiceDimension struct {
		Key   string
		Range Range
	}
	ServiceRange []ServiceDimension
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

const (
	IntNonNegative = iota
	IntNegative
	IntPlusInf
	IntMinusInf //TODO is this needed?
)

type IntOrInf struct {
	Type  uint8
	Value big.Int
}

func (i *IntOrInf) BigInt() *big.Int {
	switch i.Type {
	case IntNonNegative:
		return new(big.Int).Set(&i.Value)
	case IntNegative:
		return new(big.Int).Neg(&i.Value)
	case IntPlusInf:
		panic(nil) // caller should check Inf() before trying to convert to big.Int
	case IntMinusInf:
		panic(nil)
	}
	return &big.Int{} // invalid type decodes to 0 value
}

func (i *IntOrInf) Inf() int {
	switch i.Type {
	case IntPlusInf:
		return 1
	case IntMinusInf:
		return -1
	}
	return 0 // invalid type decodes to 0 value
}

func (i *IntOrInf) Int64() int64 {
	switch i.Type {
	case IntNonNegative:
		if i.Value.IsInt64() {
			return i.Value.Int64()
		} else {
			return math.MaxInt64
		}
	case IntNegative:
		if i.Value.IsInt64() {
			return -i.Value.Int64()
		} else {
			return math.MinInt64
		}
	case IntPlusInf:
		return math.MaxInt64
	case IntMinusInf:
		return math.MinInt64
	}
	return 0 // invalid type decodes to 0 value
}

func (i *IntOrInf) SetBigInt(v *big.Int) {
	if v.Sign() >= 0 {
		i.Type = IntNonNegative
		i.Value.Set(v)
	} else {
		i.Type = IntNegative
		i.Value.Neg(v)
	}
}

func (i *IntOrInf) SetInt64(v int64) {
	if v >= 0 {
		if v == math.MaxInt64 {
			i.Type = IntPlusInf
		} else {
			i.Type = IntNonNegative
			i.Value.SetInt64(v)
		}
	} else {
		if v == math.MinInt64 {
			i.Type = IntMinusInf
		} else {
			i.Type = IntNegative
			i.Value.SetInt64(-v)
		}
	}
}

func (i *IntOrInf) SetInf(sign int) {
	if sign == 1 {
		i.Type = IntPlusInf
	} else {
		i.Type = IntMinusInf
	}
}
