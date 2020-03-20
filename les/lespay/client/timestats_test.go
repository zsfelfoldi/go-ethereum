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
	"math/rand"
	"testing"
	"time"
)

func TestTimeTransition(t *testing.T) {
	var inrange = []time.Duration{
		time.Millisecond * 10, time.Millisecond * 11,
		time.Second, time.Second * 9, time.Second * 10,
	}
	epsilon := 0.001
	for _, c := range inrange {
		got := slotToTime(timeToSlot(c))
		if float64(c)*(1+epsilon) < float64(got) || float64(c)*(1-epsilon) > float64(got) {
			t.Fatalf("Invalid transition %v", c)
		}
	}
	var outrange = []struct {
		input  time.Duration
		expect time.Duration
	}{
		{0, time.Millisecond * 10},
		{time.Millisecond * 9, time.Millisecond * 10},
		{time.Second * 11, time.Second * 10},
		{time.Second * 30, time.Second * 10},
	}
	for _, c := range outrange {
		got := slotToTime(timeToSlot(c.input))
		if got != c.expect {
			t.Fatalf("Invalid transition %v", c)
		}
	}
}

func TestValue(t *testing.T) {
	for i := 0; i < 1000; i++ {
		max := minResponseTime + time.Duration(rand.Int63n(int64(maxResponseTime-minResponseTime)))
		min := minResponseTime + time.Duration(rand.Int63n(int64(max-minResponseTime)))
		expRT := max/2 + time.Duration(rand.Int63n(int64(maxResponseTime-max/2)))
		s := makeRangeStats(min, max)
		value, relValue := s.Value(expRT)
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
		sum, expiredSum                 TimeStats
		sumValueExp, expiredSumValueExp float64
	)
	for i := 0; i < 1000; i++ {
		max := minResponseTime + time.Duration(rand.Int63n(int64(maxResponseTime-minResponseTime)))
		min := minResponseTime + time.Duration(rand.Int63n(int64(max-minResponseTime)))
		s := makeRangeStats(min, max)
		value, _ := s.Value(maxResponseTime)
		sum.AddStats(&s)
		sumValueExp += value
		expiredSum.AddStats(&s)
		expiredSumValueExp += value
		expiredSum.Expire(0.001)
		expiredSumValueExp -= expiredSumValueExp * 0.001
	}
	sumValue, _ := sum.Value(maxResponseTime)
	if sumValue < sumValueExp-1 || sumValue > sumValueExp+1 {
		t.Errorf("sumValue failed (expected %v, got %v)", sumValueExp, sumValue)
	}
	expiredSumValue, _ := expiredSum.Value(maxResponseTime)
	if expiredSumValue < expiredSumValueExp-1 || expiredSumValue > expiredSumValueExp+1 {
		t.Errorf("expiredSumValue failed (expected %v, got %v)", expiredSumValueExp, expiredSumValue)
	}
	diff := sum
	diff.SubStats(&expiredSum)
	diffValue, _ := diff.Value(maxResponseTime)
	diffValueExp := sumValueExp - expiredSumValueExp
	if diffValue < diffValueExp-1 || diffValue > diffValueExp+1 {
		t.Errorf("diffValue failed (expected %v, got %v)", diffValueExp, diffValue)
	}
}

func TestTimeout(t *testing.T) {
	testTimeoutRange(t, minResponseTime, time.Second)
	testTimeoutRange(t, time.Second, time.Second*2)
	testTimeoutRange(t, time.Second, maxResponseTime)
}

func testTimeoutRange(t *testing.T, min, max time.Duration) {
	s := makeRangeStats(min, max)
	for i := 2; i < 9; i++ {
		to := s.Timeout(float64(i) / 10)
		exp := max - (max-min)*time.Duration(i)/10
		tol := (max - min) / 50
		if to < exp-tol || to > exp+tol {
			t.Errorf("Timeout failed (expected %v, got %v)", exp, to)
		}
	}
}

func makeRangeStats(min, max time.Duration) TimeStats {
	var s TimeStats
	for i := 0; i < 1000; i++ {
		s.Add(min+(max-min)*time.Duration(i)/999, 1)
	}
	return s
}
