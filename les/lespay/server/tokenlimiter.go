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

package server

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
)

const freeRatioTC = time.Hour // time constant of token supply control based on free service availability

type TokenLimiter struct {
	ns                                 *utils.NodeStateMachine
	clock                              mclock.Clock
	lock                               sync.RWMutex
	maxCap, priorityCap, freeClientCap uint64
	capacityFactor                     float64
	freeRatio, avgeFreeRatio           float64
	lastUpdate                         mclock.AbsTime
	expirers                           []*TokenExpirer
}

func NewTokenLimiter(ns *utils.NodeStateMachine, clock mclock.Clock, freeClientCap uint64) *TokenLimiter {
	mask := ns.StatesMask([]*utils.NodeStateFlag{ActiveFlag, PriorityFlag})
	capFieldIndex := ns.FieldIndex(CapacityField)
	tl := &TokenLimiter{
		ns:            ns,
		clock:         clock,
		freeClientCap: freeClientCap,
		lastUpdate:    clock.Now(),
	}
	ns.AddStateSub(mask, func(id enode.ID, oldState, newState utils.NodeStateBitMask) {
		cap, _ := ns.GetField(id, capFieldIndex).(uint64)
		if newState == mask {
			tl.lock.Lock()
			tl.priorityCap += cap
			tl.updateFreeRatio()
			tl.lock.Unlock()
		}
		if oldState == mask {
			tl.lock.Lock()
			tl.priorityCap -= cap
			tl.updateFreeRatio()
			tl.lock.Unlock()
		}
	})
	ns.AddFieldSub(capFieldIndex, func(id enode.ID, state utils.NodeStateBitMask, oldValue, newValue interface{}) {
		if state == mask {
			tl.lock.Lock()
			oldCap, _ := oldValue.(uint64)
			newCap, _ := newValue.(uint64)
			tl.priorityCap += newValue - oldValue
			tl.updateFreeRatio()
			tl.lock.Unlock()
		}
	})
}

// TotalTokenLimit returns the current token supply limit. Token prices are based
// on the ratio of total token amount and supply limit while the limit depends on
// averageFreeRatio, ensuring the availability of free service most of the time.
func (tl *TokenLimiter) TotalTokenLimit() uint64 {
	tl.lock.RLock()
	defer tl.lock.RUnlock()

	d := tl.averageFreeRatio(tl.clock.Now)
	if d > 0.5 {
		d = -math.Log(0.5/d) * float64(freeRatioTC)
	} else {
		d = 0
	}
	return uint64(d * float64(tl.maxCap) * tl.capacityFactor)
}

func (tl *TokenLimiter) SetCapacityFactor(capFactor float64) {
	tl.lock.Lock()
	tl.capacityFactor = capFactor
	tl.lock.Unlock()
}

func (tl *TokenLimiter) SetCapacityLimit(maxCap uint64) {
	tl.lock.Lock()
	tl.maxCap = maxCap
	tl.updateFreeRatio()
	tl.lock.Unlock()
}

func (tl *TokenLimiter) PriorityCapacity() uint64 {
	tl.lock.Lock()
	defer tl.lock.Unlock()

	return tl.priorityCap
}

func (tl *TokenLimiter) NewTokenExpirer() *TokenExpirer {
	tl.lock.Lock()
	te := &TokenExpirer{
		lock:      &te.lock,
		freeRatio: tl.freeRatio,
	}
	tl.expirers = append(tl.expirers, te)
	tl.lock.Unlock()
	return te
}

// updateFreeRatio updates freeRatio, averageFreeRatio, posExp and negExp based
// on free service availability. Should be called after capLimit or priorityActive
// is changed.
func (tl *TokenLimiter) updateFreeRatio() {
	now := tl.clock.Now()
	tl.avgFreeRatio = tl.averageFreeRatio(now)
	tl.lastUpdate = now

	tl.freeRatio = 0
	if tl.priorityCap < tl.maxCap {
		freeCap := tl.maxCap - tl.priorityCap
		if freeCap > tl.freeClientCap {
			freeCapThreshold := tl.maxCap / 4
			if freeCap > freeCapThreshold {
				tl.freeRatio = 1
			} else {
				tl.freeRatio = float64(freeCap-tl.freeClientCap) / float64(freeCapThreshold-tl.freeClientCap)
			}
		}
	}
	for _, e := range tl.expirers {
		e.setFreeRatio(now, tl.freeRatio)
	}
}

func (tl *TokenLimiter) averageFreeRatio(now mclock.AbsTime) float64 {
	dt := now - tl.lastUpdate
	if dt < 0 {
		dt = 0
	}
	return tl.avgFreeRatio - (tl.freeRatio-tl.averageFreeRatio)*math.Expm1(-float64(dt)/float64(freeRatioTC))
}

type TokenExpirer struct {
	exp             utils.Expirer
	lock            *sync.RWMutex
	rate, freeRatio float64
}

func (te *TokenExpirer) SetRate(now mclock.AbsTime, rate float64) {
	te.lock.Lock()
	te.rate = rate
	te.exp.SetRate(now, rate*te.freeRatio)
	te.lock.Unlock()
}

func (te *TokenExpirer) setFreeRatio(now mclock.AbsTime, freeRatio float64) {
	te.freeRatio = freeRatio
	te.exp.SetRate(now, te.rate*freeRatio)
}

func (te *TokenExpirer) SetLogOffset(now mclock.AbsTime, logOffset utils.Fixed64) {
	te.lock.Lock()
	te.exp.SetLogOffset(now, logOffset)
	te.lock.Unlock()
}

func (te *TokenExpirer) LogOffset(now mclock.AbsTime) utils.Fixed64 {
	te.lock.RLock()
	logOffset := te.exp.SetLogOffset(now)
	te.lock.RUnlock()
	return logOffset
}
