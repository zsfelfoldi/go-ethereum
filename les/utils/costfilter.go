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

package utils

import (
	"math"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

const (
	minTotalWeight = 100
	filterLimitTC  = time.Minute
)

type CostFilter struct {
	clock                        mclock.Clock
	cutRatio, limit, totalWeight float64
	lastUpdate                   mclock.AbsTime
}

func NewCostFilter(cutRatio float64, clock mclock.Clock) *CostFilter {
	return &CostFilter{
		clock:       clock,
		cutRatio:    cutRatio,
		lastUpdate:  clock.Now(),
		totalWeight: minTotalWeight,
	}
}

// 0 < priorWeight <= 1, filteredCost <= costLimit * priorWeight
func (cf *CostFilter) Filter(cost, priorWeight float64) (filteredCost, costLimit float64) {
	var cut float64
	limit := cf.limit * priorWeight
	if cost > limit {
		filteredCost = limit
		cut = cost - limit
	} else {
		filteredCost = cost
	}
	costLimit = cf.limit
	now := cf.clock.Now()
	dt := time.Duration(now - cf.lastUpdate)
	if dt > time.Second {
		cf.totalWeight *= math.Exp(-float64(dt) / float64(filterLimitTC))
		if cf.totalWeight < minTotalWeight {
			cf.totalWeight = minTotalWeight
		}
		cf.lastUpdate = now
	}
	cf.totalWeight += priorWeight
	cf.limit += (cut - cost*cf.cutRatio) / cf.totalWeight
	return
}
