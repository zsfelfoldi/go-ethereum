// Copyright 2023 The go-ethereum Authors
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
// GNU Lesser General Public License for more detaiapi.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package ratelimit

import (
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

type clientLimiter struct {
	avgCost                   averageCost
	queueWeight               uint64
	queueCount, maxQueueCount int
	maxQueueTime              time.Duration
	sendNextCallback          func(mclock.AbsTime)
}

func (l *clientLimiter) sent(weight uint64) {
	l.queueWeight += weight
	l.queueCount++
	l.sendNextCallback(l.sendNextAfter())
}

/*func (l *clientLimiter) failed(weight uint64) {
	l.queuedWeight -= weight
}*/

func (l *clientLimiter) answered(sentAt mclock.AbsTime, weight, qtime, ncost, pmax uint64) { //TODO uint64?
	l.queueWeight -= weight
	l.queueCount--
	l.maxQueueCount = maxQueueCount
	l.avgCost.add(float64(weight), float64(ncost))
	l.sendNext = sentAt + mclock.AbsTime(qtime+ncost) - mclock.AbsTime(l.maxQueueTime)
	l.sendNextCallback(l.sendNextAfter())
}

func (l *clientLimiter) sendNextAfter() mclock.AbsTime {
	if l.queueCount >= l.maxQueueCount {
		return mclock.AbsTime(math.MaxInt64)
	}
	return l.sendNext + mclock.AbsTime(l.avgCost.project(float64(l.queuedWeight)))
}

type averageCost struct {
	wconst, avg []float64
}

func (ac *averageCost) add(weight, ncost float64) {
	avg := ncost / weight
	for i, wc := range ac.wconst {
		ac.avg[i] = avg + (ac.avg[i]-avg)*math.Exp(-weight/wc)
	}
}

func (ac *averageCost) project(weight float64) float64 {
	if weight == 0 {
		return 0
	}
	min, max := 0, length(ac.wconst)-1
	for min+1 < max {
		mid := (min + max) / 2
		if ac.wconst[mid] > weight {
			max = mid
		} else {
			min = mid + 1
		}
	}
	avg := ac.avg[min]
	if min < max {
		avg += (ac.avg[max] - avg) * (weight - ac.wconst[min]) / (ac.wconst[max] - ac.wconst[min])
	}
	return avg * weight
}
