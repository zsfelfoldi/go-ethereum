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

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/rlp"
)

const referenceFactor = 1000000 // reference basket amount and value scale factor

// referenceBasket keeps track of global request usage statistics and the usual prices
// of each used request type relative to each other. The amounts in the basket are scaled
// up by referenceFactor because of the exponential expiration of long-term statistical data.
// Values are scaled so that the sum of all amounts and the sum of all values are equal.
//
// reqValues represent the internal relative value estimates for each request type and are
// calculated as value / amount. The average reqValue of all used requests is 1.
// In other words: SUM(refBasket[type].amount * reqValue[type]) = SUM(refBasket[type].amount)
type referenceBasket struct {
	refBasket   requestBasket
	reqValues   []float64 // contents are read only, new slice is created for each update
	lastExpired mclock.AbsTime
}

// serverBasket collects served request amount and value statistics for a single server.
// Served requests are first added to costBasket where value represents request cost
// according to the server's own cost table. These are scaled and added on demand to
// valueBasket where they are scaled by the request value factor of the server which is
// calculated and updated by the reference basket based on the announced cost table.
//
// Values are gradually transferred to the global reference basket with a long time
// constant so that each server basket represents long term usage and price statistics.
// When the transferred part is added to the reference basket the values are scaled so
// that their sum equals the total value calculated according to the previous reqValues.
// The ratio of request values coming from the server basket represent the pricing of
// the specific server and modify the global estimates with a weight proportional to
// the amount of service provided by the server.
type serverBasket struct {
	costBasket, valueBasket requestBasket
	rvFactor                float64
}

type (
	requestBasket []basketItem
	basketItem    struct {
		amount, value uint64
	}
)

// init initializes a new server basket with the given service vector size (number of
// different request types)
func (s *serverBasket) init(size int) {
	if s.costBasket == nil {
		s.costBasket = make(requestBasket, size)
	}
	if s.valueBasket == nil {
		s.valueBasket = make(requestBasket, size)
	}
}

// add adds the give type and amount of requests to the basket. Cost is calculated
// according to the server's own cost table.
func (s *serverBasket) add(reqType, reqAmount uint32, reqCost uint64) {
	i := &s.costBasket[reqType]
	i.amount += uint64(reqAmount) * referenceFactor
	i.value += reqCost
}

// updateRvFactor updates the request value factor that scales server costs into the
// local value dimensions.
func (s *serverBasket) updateRvFactor(rvFactor float64) {
	for i, c := range s.costBasket {
		s.valueBasket[i].amount += c.amount
		s.valueBasket[i].value += uint64(float64(c.value) * s.rvFactor)
		s.costBasket[i] = basketItem{}
	}
	s.rvFactor = rvFactor
}

// transfer decreases amounts and values in the basket with the given ratio and
// moves the removed amounts into a new basket which is returned and can be added
// to the global reference basket.
func (s *serverBasket) transfer(ratio float64) requestBasket {
	s.updateRvFactor(s.rvFactor)
	res := make(requestBasket, len(s.valueBasket))
	for i, v := range s.valueBasket {
		ta := uint64(float64(v.amount) * ratio)
		tv := uint64(float64(v.value) * ratio)
		if ta > v.amount {
			ta = v.amount
		}
		if tv > v.value {
			tv = v.value
		}
		s.valueBasket[i] = basketItem{v.amount - ta, v.value - tv}
		res[i] = basketItem{ta, tv}
	}
	return res
}

// init initializes the reference basket with the given service vector size (number of
// different request types)
func (r *referenceBasket) init(now mclock.AbsTime, size int) {
	r.reqValues = make([]float64, size)
	r.lastExpired = now
	r.normalizeAndExpire(0)
	r.updateReqValues()
}

// add adds the transferred part of a server basket to the reference basket while scaling
// value amounts so that their sum equals the total value calculated according to the
// previous reqValues.
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
			r.refBasket[i].amount += v.amount
			r.refBasket[i].value += uint64(float64(v.value) * scaleValues)
		}
	}
	r.updateReqValues()
}

// updateReqValues recalculates reqValues after adding transferred baskets. Note that
// values should be normalized first.
func (r *referenceBasket) updateReqValues() {
	r.reqValues = make([]float64, len(r.reqValues))
	for i, b := range r.refBasket {
		if b.amount > 0 {
			r.reqValues[i] = float64(b.value) / float64(b.amount)
		} else {
			r.reqValues[i] = 0
		}
	}
}

// normalizeAndExpire applies expiration of long-term statistical data and also ensures
// that the sum of values equal the sum of amounts in the basket.
func (r *referenceBasket) normalizeAndExpire(exp float64) {
	var sumAmount, sumValue uint64
	for _, b := range r.refBasket {
		sumAmount += b.amount
		sumValue += b.value
	}
	expValue := exp + float64(int64(sumValue-sumAmount))/float64(sumValue)
	for i, b := range r.refBasket {
		b.amount -= uint64(float64(b.amount) * exp)
		b.value -= uint64(float64(b.value) * expValue)
		r.refBasket[i] = b
	}
}

// reqValueFactor calculates the request value factor applicable to the server with
// the given announced request cost list
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
	return float64(totalValue) * referenceFactor / totalCost
}

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

// convertMapping converts a basket loaded from the database into the current format.
// If the available request types and their mapping into the service vector differ from
// the one used when saving the basket then this function reorders old fields and fills
// in previously unknown fields by scaling up amounts and values taken from the
// initialization basket.
func (r requestBasket) convertMapping(oldMapping, newMapping []string, initBasket requestBasket) requestBasket {
	nameMap := make(map[string]int)
	for i, name := range oldMapping {
		nameMap[name] = i
	}
	rc := make(requestBasket, len(newMapping))
	var scale, oldScale, newScale float64
	for i, name := range newMapping {
		if ii, ok := nameMap[name]; ok {
			rc[i] = r[ii]
			oldScale += float64(initBasket[i].amount) * float64(initBasket[i].amount)
			newScale += float64(rc[i].amount) * float64(initBasket[i].amount)
		}
	}
	if oldScale > 1e-10 {
		scale = newScale / oldScale
	} else {
		scale = 1
	}
	for i, name := range newMapping {
		if _, ok := nameMap[name]; !ok {
			rc[i].amount = uint64(float64(initBasket[i].amount) * scale)
			rc[i].value = uint64(float64(initBasket[i].value) * scale)
		}
	}
	return rc
}
