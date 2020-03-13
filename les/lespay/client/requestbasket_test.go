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
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

func checkU64(t *testing.T, name string, value, exp uint64) {
	if value != exp {
		t.Errorf("Incorrect value for %s: got %d, expected %d", name, value, exp)
	}
}

func checkF64(t *testing.T, name string, value, exp, tol float64) {
	if value < exp-tol || value > exp+tol {
		t.Errorf("Incorrect value for %s: got %f, expected %f", name, value, exp)
	}
}

func TestServerBasket(t *testing.T) {
	var s serverBasket
	s.init(0, 2)
	// add some requests with different request value factors
	s.updateRvFactor(1)
	s.add(0, 1000, 10000)
	s.add(1, 3000, 60000)
	s.updateRvFactor(10)
	s.add(0, 4000, 4000)
	s.add(1, 2000, 4000)
	s.updateRvFactor(10)
	// check basket contents directly
	checkU64(t, "s.valueBasket[0].amount", s.valueBasket[0].amount, 5000*referenceFactor)
	checkU64(t, "s.valueBasket[0].value", s.valueBasket[0].value, 50000)
	checkU64(t, "s.valueBasket[1].amount", s.valueBasket[1].amount, 5000*referenceFactor)
	checkU64(t, "s.valueBasket[1].value", s.valueBasket[1].value, 100000)
	// make a transfer after 1 minute with 50% per minute transfer rate
	transfer1 := s.transfer(mclock.AbsTime(time.Minute), math.Log(2)/float64(time.Minute))
	checkU64(t, "transfer1[0].amount", transfer1[0].amount, 2500*referenceFactor)
	checkU64(t, "transfer1[0].value", transfer1[0].value, 25000)
	checkU64(t, "transfer1[1].amount", transfer1[1].amount, 2500*referenceFactor)
	checkU64(t, "transfer1[1].value", transfer1[1].value, 50000)
	// add more requests
	s.updateRvFactor(100)
	s.add(0, 1000, 100)
	// make another transfer after 2 more minutes with 50% per minute transfer rate
	transfer2 := s.transfer(mclock.AbsTime(time.Minute*3), math.Log(2)/float64(time.Minute))
	checkU64(t, "transfer2[0].amount", transfer2[0].amount, (2500+1000)*3/4*referenceFactor)
	checkU64(t, "transfer2[0].value", transfer2[0].value, (25000+10000)*3/4)
	checkU64(t, "transfer2[1].amount", transfer2[1].amount, 2500*3/4*referenceFactor)
	checkU64(t, "transfer2[1].value", transfer2[1].value, 50000*3/4)
}

func TestConvertMapping(t *testing.T) {
	b := requestBasket{{3, 3}, {1, 1}, {2, 2}}
	oldMap := []string{"req3", "req1", "req2"}
	newMap := []string{"req1", "req2", "req3", "req4"}
	init := requestBasket{{2, 2}, {4, 4}, {6, 6}, {8, 8}}
	bc := b.convertMapping(oldMap, newMap, init)
	checkU64(t, "bc[0].amount", bc[0].amount, 1)
	checkU64(t, "bc[1].amount", bc[1].amount, 2)
	checkU64(t, "bc[2].amount", bc[2].amount, 3)
	checkU64(t, "bc[3].amount", bc[3].amount, 4) // 8 should be scaled down to 4
}

func TestReqValueFactor(t *testing.T) {
	var ref referenceBasket
	ref.refBasket = make(requestBasket, 4)
	for i, _ := range ref.refBasket {
		ref.refBasket[i].amount = uint64(i+1) * referenceFactor
		ref.refBasket[i].value = uint64(i+1) * referenceFactor
	}
	ref.init(0, 4)
	rvf := ref.reqValueFactor([]uint64{1000, 2000, 3000, 4000})
	// expected value is (1000000+2000000+3000000+4000000) / (1*1000+2*2000+3*3000+4*4000) = 10000000/30000 = 333.333
	checkF64(t, "reqValueFactor", rvf, 333.333, 1)
}

func TestReqValueAdjustment(t *testing.T) {
	var s1, s2 serverBasket
	s1.init(0, 3)
	s2.init(0, 3)
	cost1 := []uint64{30000, 60000, 90000}
	cost2 := []uint64{100000, 200000, 300000}
	var ref referenceBasket
	ref.refBasket = make(requestBasket, 3)
	for i, _ := range ref.refBasket {
		ref.refBasket[i].amount = 123 * referenceFactor
		ref.refBasket[i].value = 123 * referenceFactor
	}
	ref.init(0, 3)
	// initial reqValues are expected to be {1, 1, 1}
	checkF64(t, "reqValues[0]", ref.reqValues[0], 1, 0.01)
	checkF64(t, "reqValues[1]", ref.reqValues[1], 1, 0.01)
	checkF64(t, "reqValues[2]", ref.reqValues[2], 1, 0.01)
	for period := 0; period < 1000; period++ {
		s1.updateRvFactor(ref.reqValueFactor(cost1))
		s2.updateRvFactor(ref.reqValueFactor(cost2))
		// throw in random requests into each basket using their internal pricing
		for i := 0; i < 1000; i++ {
			reqType, reqAmount := uint32(rand.Intn(3)), uint32(rand.Intn(10)+1)
			reqCost := uint64(reqAmount) * cost1[reqType]
			s1.add(reqType, reqAmount, reqCost)
			reqType, reqAmount = uint32(rand.Intn(3)), uint32(rand.Intn(10)+1)
			reqCost = uint64(reqAmount) * cost2[reqType]
			s2.add(reqType, reqAmount, reqCost)
		}
		ref.add(s1.transfer(mclock.AbsTime(period)*mclock.AbsTime(time.Minute), 0.1/float64(time.Minute)))
		ref.add(s2.transfer(mclock.AbsTime(period)*mclock.AbsTime(time.Minute), 0.1/float64(time.Minute)))
		ref.normalizeAndExpire(0.1)
		ref.updateReqValues()
	}
	checkF64(t, "reqValues[0]", ref.reqValues[0], 0.5, 0.01)
	checkF64(t, "reqValues[1]", ref.reqValues[1], 1, 0.01)
	checkF64(t, "reqValues[2]", ref.reqValues[2], 1.5, 0.01)
}
