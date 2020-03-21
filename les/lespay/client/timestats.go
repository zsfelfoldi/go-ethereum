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

	lpu "github.com/ethereum/go-ethereum/les/lespay/utils"
)

const (
	minResponseTime   = time.Millisecond * 50
	maxResponseTime   = time.Second * 10
	timeStatLength    = 32
	weightScaleFactor = 1000000
)

type ResponseTimeStats struct {
	stats [timeStatLength]uint64
	exp   uint64
}

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

func (rt *ResponseTimeStats) Add(respTime time.Duration, weight float64, expFactor lpu.ExpirationFactor) {
	rt.setExp(expFactor.Exp)
	weight *= expFactor.Factor * weightScaleFactor
	r := TimeToStatScale(respTime)
	i := int(r)
	r -= float64(i)
	rt.stats[i] += uint64(weight * (1 - r))
	if i < timeStatLength-1 {
		rt.stats[i+1] += uint64(weight * r)
	}
}

func (rt *ResponseTimeStats) setExp(exp uint64) {
	if exp > rt.exp {
		shift := exp - rt.exp
		for i, v := range rt.stats {
			rt.stats[i] = v >> shift
		}
		rt.exp = exp
	}
	if exp < rt.exp {
		shift := rt.exp - exp
		for i, v := range rt.stats {
			rt.stats[i] = v << shift
		}
		rt.exp = exp
	}
}

func (rt *ResponseTimeStats) Value(expRT time.Duration, expFactor lpu.ExpirationFactor) (float64, float64) {
	var (
		v   float64
		sum uint64
	)
	for i, s := range rt.stats {
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
	v = expFactor.Value(v, rt.exp)
	return v / weightScaleFactor, v / float64(sum)
}

func (rt *ResponseTimeStats) AddStats(s *ResponseTimeStats) {
	rt.setExp(s.exp)
	for i, v := range s.stats {
		rt.stats[i] += v
	}
}

func (rt *ResponseTimeStats) SubStats(s *ResponseTimeStats) {
	rt.setExp(s.exp)
	for i, v := range s.stats {
		if v < rt.stats[i] {
			rt.stats[i] -= v
		} else {
			rt.stats[i] = 0
		}
	}
}

func (rt *ResponseTimeStats) Timeout(failRatio float64) time.Duration {
	var sum uint64
	for _, v := range rt.stats {
		sum += v
	}
	s := uint64(float64(sum) * failRatio)
	i := timeStatLength - 1
	for i > 0 && s >= rt.stats[i] {
		s -= rt.stats[i]
		i--
	}
	r := float64(i) + 0.5
	if rt.stats[i] > 0 {
		r -= float64(s) / float64(rt.stats[i])
	}
	if r < 0 {
		r = 0
	}
	th := StatScaleToTime(r)
	if th > maxResponseTime {
		th = maxResponseTime
	}
	return th
}
