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
	minResponseTime   = time.Millisecond * 50
	maxResponseTime   = time.Second * 10
	timeStatLength    = 32
	weightScaleFactor = 1000000
)

type ResponseTimeStats [timeStatLength]uint64

var timeStatsLogFactor = (timeStatLength - 1) / (math.Log(float64(maxResponseTime)/float64(minResponseTime)) + 1)

func TimeToStatScale(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	r := float64(d) / float64(minResponseTime)
	if r > 1 {
		r = math.Log(r) + 1
	}
	r *= timeStatsLogFactor
	if r > timeStatLength-1 {
		return timeStatLength - 1
	}
	return r
}

func StatScaleToTime(r float64) time.Duration {
	r /= timeStatsLogFactor
	if r > 1 {
		r = math.Exp(r - 1)
	}
	return time.Duration(r * float64(minResponseTime))
}

func (rt *ResponseTimeStats) Add(respTime time.Duration, weight float64) {
	r := TimeToStatScale(respTime)
	i := int(r)
	r -= float64(i)
	weight *= weightScaleFactor
	rt[i] += uint64(weight * (1 - r))
	if i < timeStatLength-1 {
		rt[i+1] += uint64(weight * r)
	}
}

func (rt *ResponseTimeStats) Value(expRT time.Duration) (float64, float64) {
	var (
		v   float64
		sum uint64
	)
	for i, s := range rt[:] {
		sum += s
		t := StatScaleToTime(float64(i))
		w := 1 - float64(t)/float64(expRT)
		if w < -1 {
			w = -1
		}
		v += float64(s) * w
	}
	if sum == 0 || v < 0 {
		return 0, 0
	}
	return v / weightScaleFactor, v / float64(sum)
}

func (rt *ResponseTimeStats) Expire(exp float64) {
	for i, s := range rt[:] {
		sub := uint64(float64(s) * exp)
		if sub < s {
			s -= sub
		} else {
			s = 0
		}
		rt[i] = s
	}
}

func (rt *ResponseTimeStats) AddStats(s *ResponseTimeStats) {
	for i, v := range s[:] {
		rt[i] += v
	}
}

func (rt *ResponseTimeStats) SubStats(s *ResponseTimeStats) {
	for i, v := range s[:] {
		if v < rt[i] {
			rt[i] -= v
		} else {
			rt[i] = 0
		}
	}
}

func (rt *ResponseTimeStats) Timeout(failRatio float64) time.Duration {
	var sum uint64
	for _, v := range rt {
		sum += v
	}
	s := uint64(float64(sum) * failRatio)
	i := timeStatLength - 1
	for i > 0 && s >= rt[i] {
		s -= rt[i]
		i--
	}
	r := float64(i) + 0.5
	if rt[i] > 0 {
		r -= float64(s) / float64(rt[i])
	}
	th := StatScaleToTime(r)
	if th < minResponseTime {
		th = minResponseTime
	}
	if th > maxResponseTime {
		th = maxResponseTime
	}
	return th
}
