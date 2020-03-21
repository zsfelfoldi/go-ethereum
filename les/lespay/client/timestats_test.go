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

	lpu "github.com/ethereum/go-ethereum/les/lespay/utils"
)

func TestValue(t *testing.T) {
	noexp := lpu.ExpirationFactor{Factor: 1}
	for i := 0; i < 1000; i++ {
		max := minResponseTime + time.Duration(rand.Int63n(int64(maxResponseTime-minResponseTime)))
		min := minResponseTime + time.Duration(rand.Int63n(int64(max-minResponseTime)))
		expRT := max/2 + time.Duration(rand.Int63n(int64(maxResponseTime-max/2)))
		s := makeRangeStats(min, max, 1000, noexp)
		value, relValue := s.Value(expRT, noexp)
		expv := 1 - float64((min+max)/2)/float64(expRT)
		if expv < 0 {
			expv = 0
		}
		if relValue < expv-0.01 || relValue > expv+0.01 {
			t.Errorf("Value failed (expected relValue %v, got %v)", expv, relValue)
		}
		expv *= 1000
		if value < expv-10 || value > expv+10 {
			t.Errorf("Value failed (expected %v, got %v)", expv, value)
		}
	}
}

func TestAddSubExpire(t *testing.T) {
	var (
		sum1, sum2                 ResponseTimeStats
		sum1ValueExp, sum2ValueExp float64
		logOffset                  lpu.Fixed64
	)
	for i := 0; i < 1000; i++ {
		exp := lpu.ExpFactor(logOffset)
		max := minResponseTime + time.Duration(rand.Int63n(int64(maxResponseTime-minResponseTime)))
		min := minResponseTime + time.Duration(rand.Int63n(int64(max-minResponseTime)))
		s := makeRangeStats(min, max, 1000, exp)
		value, _ := s.Value(maxResponseTime, exp)
		sum1.AddStats(&s)
		sum1ValueExp += value
		if rand.Intn(2) == 1 {
			sum2.AddStats(&s)
			sum2ValueExp += value
		}
		logOffset += lpu.Float64ToFixed64(0.001 / math.Log(2))
		sum1ValueExp -= sum1ValueExp * 0.001
		sum2ValueExp -= sum2ValueExp * 0.001
	}
	exp := lpu.ExpFactor(logOffset)
	sum1Value, _ := sum1.Value(maxResponseTime, exp)
	if sum1Value < sum1ValueExp*0.99 || sum1Value > sum1ValueExp*1.01 {
		t.Errorf("sum1Value failed (expected %v, got %v)", sum1ValueExp, sum1Value)
	}
	sum2Value, _ := sum2.Value(maxResponseTime, exp)
	if sum2Value < sum2ValueExp*0.99 || sum2Value > sum2ValueExp*1.01 {
		t.Errorf("sum2Value failed (expected %v, got %v)", sum2ValueExp, sum2Value)
	}
	diff := sum1
	diff.SubStats(&sum2)
	diffValue, _ := diff.Value(maxResponseTime, exp)
	diffValueExp := sum1ValueExp - sum2ValueExp
	if diffValue < diffValueExp*0.99 || diffValue > diffValueExp*1.01 {
		t.Errorf("diffValue failed (expected %v, got %v)", diffValueExp, diffValue)
	}
}

func TestTimeout(t *testing.T) {
	testTimeoutRange(t, 0, time.Second)
	testTimeoutRange(t, time.Second, time.Second*2)
	testTimeoutRange(t, time.Second, maxResponseTime)
}

func testTimeoutRange(t *testing.T, min, max time.Duration) {
	s := makeRangeStats(min, max, 1000, lpu.ExpirationFactor{Factor: 1})
	for i := 2; i < 9; i++ {
		to := s.Timeout(float64(i) / 10)
		exp := max - (max-min)*time.Duration(i)/10
		tol := (max - min) / 50
		if to < exp-tol || to > exp+tol {
			t.Errorf("Timeout failed (expected %v, got %v)", exp, to)
		}
	}
}

func makeRangeStats(min, max time.Duration, amount float64, exp lpu.ExpirationFactor) ResponseTimeStats {
	var s ResponseTimeStats
	amount /= 1000
	for i := 0; i < 1000; i++ {
		s.Add(min+(max-min)*time.Duration(i)/999, amount, exp)
	}
	return s
}
