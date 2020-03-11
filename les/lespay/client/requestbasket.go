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

package client

import (
	"io"
	"math"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/rlp"
)

const reqAmountMultiplier = 1000000

type serverBasket struct {
	costBasket, valueBasket requestBasket
	rvFactor                float64
	lastTransfer            mclock.AbsTime
}

type referenceBasket struct {
	refBasket   requestBasket
	reqValues   []float64 // contents are read only, new slice is created for each update
	lastExpired mclock.AbsTime
}

func (s *serverBasket) init(now mclock.AbsTime, size int) {
	if s.costBasket == nil {
		s.costBasket = make(requestBasket, size)
	}
	if s.valueBasket == nil {
		s.valueBasket = make(requestBasket, size)
	}
	s.lastTransfer = now
}

func (s *serverBasket) add(reqType, reqAmount uint32, reqCost uint64) {
	i := &s.costBasket[reqType]
	i.amount += uint64(reqAmount)
	i.value += reqCost
}

func (s *serverBasket) updateRvFactor(rvFactor float64) {
	for i, c := range s.costBasket {
		s.valueBasket[i].amount += c.amount
		s.valueBasket[i].value += uint64(float64(c.value) * s.rvFactor)
	}
	s.rvFactor = rvFactor
}

func (s *serverBasket) transfer(now mclock.AbsTime, rate float64) requestBasket {
	s.updateRvFactor(s.rvFactor)
	res := make(requestBasket, len(s.valueBasket))
	dt := now - s.lastTransfer
	s.lastTransfer = now
	if dt > 0 {
		rate = -math.Expm1(-rate * float64(dt))
		for i, v := range s.valueBasket {
			ta := uint64(float64(v.amount) * rate)
			tv := uint64(float64(v.value) * rate)
			if ta > v.amount {
				ta = v.amount
			}
			if tv > v.value {
				tv = v.value
			}
			s.valueBasket[i] = basketItem{v.amount - ta, v.value - tv}
			res[i] = basketItem{ta, tv}
		}
	}
	return res
}

func (r *referenceBasket) init(now mclock.AbsTime, size int) {
	r.reqValues = make([]float64, size)
	r.lastExpired = now
	r.updateReqValues()
}

func (r *referenceBasket) add(newBasket requestBasket) {
	// scale newBasket to match service unit value
	var (
		totalCost  uint64
		totalValue float64
	)
	for i, v := range newBasket {
		totalCost += v.value
		totalValue += float64(v.amount) * r.reqValues[i]
	}
	if totalCost > 0 {
		// add to reference with scaled values
		scaleValues := totalValue / float64(totalCost)
		for i, v := range newBasket {
			r.refBasket[i].amount += v.amount * reqAmountMultiplier
			r.refBasket[i].value += uint64(float64(v.value) * scaleValues)
		}
	}
	r.updateReqValues()
}

// should be called after init too
func (r *referenceBasket) updateReqValues() {
	r.reqValues = make([]float64, len(r.reqValues))
	for i, b := range r.refBasket {
		if b.amount > 0 {
			r.reqValues[i] = float64(b.value) * reqAmountMultiplier / float64(b.amount)
		} else {
			r.reqValues[i] = 0
		}
	}
}

func (r *referenceBasket) reqValueFactor(costList []uint64) float64 {
	var (
		totalCost  float64
		totalValue uint64
	)
	for i, b := range r.refBasket {
		totalCost += float64(costList[i]) * float64(b.amount) // use floats to avoid overflow
		totalValue += b.value
	}
	if totalCost < 1 {
		return 0
	}
	return float64(totalValue) * reqAmountMultiplier / totalCost
}

func (r *referenceBasket) expire(exp float64) {
	for i, b := range r.refBasket {
		b.amount -= uint64(float64(b.amount) * exp)
		b.value -= uint64(float64(b.value) * exp)
		r.refBasket[i] = b
	}
}

type (
	requestBasket []basketItem
	basketItem    struct {
		amount, value uint64
	}
)

// EncodeRLP implements rlp.Encoder
func (b *basketItem) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{b.amount, b.value})
}

// DecodeRLP implements rlp.Decoder
func (b *basketItem) DecodeRLP(s *rlp.Stream) error {
	var item struct {
		Amount, Value uint64
	}
	if err := s.Decode(&item); err != nil {
		return err
	}
	b.amount, b.value = item.Amount, item.Value
	return nil
}

func (r requestBasket) convertMapping(oldMapping, newMapping []string, initBasket requestBasket) requestBasket {
	panic(nil) // xxxxxxxxxxxxxxxxxxxxxxx
	return nil
}
