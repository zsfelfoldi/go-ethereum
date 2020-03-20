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
	"time"
)

const (
	minResponseTime   = time.Millisecond * 10
	maxResponseTime   = time.Second * 10
	timeStatLength    = 32
	weightScaleFactor = 1000000
)

type TimeStats [timeStatLength]uint64

var timeStatsLogFactor = (timeStatLength - 1) / math.Log(float64(maxResponseTime)/float64(minResponseTime))

func timeToSlot(t time.Duration) float64 {
	if t <= minResponseTime {
		return 0
	} else if t >= maxResponseTime {
		return timeStatLength - 1
	}
	return math.Log(float64(t)/float64(minResponseTime)) * timeStatsLogFactor
}

func slotToTime(slot float64) time.Duration {
	if slot <= 0 {
		return minResponseTime
	} else if slot >= float64(timeStatLength-1) {
		return maxResponseTime
	}
	return time.Duration(math.Exp(slot/timeStatsLogFactor) * float64(minResponseTime))
}

func (ts *TimeStats) Add(respTime time.Duration, weight float64) {
	slot := timeToSlot(respTime)
	index := int(slot)
	bias := slot - float64(index)

	weight *= weightScaleFactor
	ts[index] += uint64(weight * (1 - bias))
	if index < timeStatLength-1 {
		ts[index+1] += uint64(weight * bias)
	}
}

func (ts *TimeStats) Value(expRT time.Duration) (float64, float64) {
	var (
		v   float64
		sum uint64
	)
	for slot, weight := range ts[:] {
		sum += weight
		t := slotToTime(float64(slot))
		w := 1 - float64(t)/float64(expRT)
		if w < -1 {
			w = -1
		}
		v += float64(weight) * w
	}
	if sum == 0 || v < 0 {
		return 0, 0
	}
	return v / weightScaleFactor, v / float64(sum)
}

func (ts *TimeStats) Mean() time.Duration {
	var (
		total       float64
		totalWeight uint64
	)
	for slot, weight := range ts {
		if weight == 0 {
			continue
		}
		totalWeight += weight
		total += float64(slotToTime(float64(slot))) * float64(weight)
	}
	return time.Duration(int64(total / float64(totalWeight)))
}

func (ts *TimeStats) Expire(exp float64) {
	for i, s := range ts[:] {
		sub := uint64(float64(s) * exp)
		if sub < s {
			ts[i] -= sub
		} else {
			ts[i] = 0
		}
	}
}

func (ts *TimeStats) AddStats(s *TimeStats) {
	for i, v := range s[:] {
		ts[i] += v
	}
}

func (ts *TimeStats) SubStats(s *TimeStats) {
	for i, v := range s[:] {
		if v < ts[i] {
			ts[i] -= v
		} else {
			ts[i] = 0
		}
	}
}

func (ts *TimeStats) Timeout(failRatio float64) time.Duration {
	var sum uint64
	for _, v := range ts {
		sum += v
	}
	s := uint64(float64(sum) * failRatio)
	i := timeStatLength - 1
	for i > 0 && s >= ts[i] {
		s -= ts[i]
		i--
	}
	r := float64(i) + 0.5
	if ts[i] > 0 {
		r -= float64(s) / float64(ts[i])
	}
	th := slotToTime(r)
	if th < minResponseTime {
		th = minResponseTime
	}
	if th > maxResponseTime {
		th = maxResponseTime
	}
	return th
}
