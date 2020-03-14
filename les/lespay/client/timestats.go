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
	minResponseTime = time.Millisecond * 50
	maxResponseTime = time.Second * 10
	timeStatLength  = 32
)

type responseTimeStats [timeStatLength]uint64

var timeStatsLogFactor = (timeStatLength - 1) / (math.Log(float64(maxResponseTime)/float64(minResponseTime)) + 1)

func timeToStatScale(d time.Duration) float64 {
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

func statScaleToTime(r float64) time.Duration {
	r /= timeStatsLogFactor
	if r > 1 {
		r = math.Exp(r - 1)
	}
	return time.Duration(r * float64(minResponseTime))
}

func (rt *responseTimeStats) add(respTime time.Duration, weight float64) {
	r := timeToStatScale(respTime)
	i := int(r)
	r -= float64(i)
	r1 := 1 - r
	w := weight
	rt[i] += uint64(w * r1)
	if i < timeStatLength-1 {
		rt[i+1] += uint64(w * r)
	}
}

func (rt *responseTimeStats) value(expRT time.Duration) (float64, float64) {
	var (
		v   float64
		sum uint64
	)
	for i, s := range rt[:] {
		sum += s
		t := statScaleToTime(float64(i))
		w := 1 - float64(t)/float64(expRT)
		if w < -1 {
			w = -1
		}
		v += float64(s) * w
	}
	if sum == 0 || v < 0 {
		return 0, 0
	}
	return v, v / float64(sum)
}

func (rt *responseTimeStats) expire(exp float64) {
	for i, s := range rt[:] {
		rt[i] = s - uint64(float64(s)*exp)
	}
}

func (rt *responseTimeStats) addStats(s *responseTimeStats) {
	for i, v := range s[:] {
		rt[i] += v
	}
}

func (rt *responseTimeStats) subStats(s *responseTimeStats) {
	for i, v := range s[:] {
		rt[i] -= v
	}
}
