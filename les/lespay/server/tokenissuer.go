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
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

const tokenSellMaxRatio = 0.9 // total amount/supply limit ratio over which selling price does not increase further

// TokenIssuerSetup contains node state flags and fields used by TokenIssuer
type TokenIssuerSetup struct {
	activeFlag, priorityFlag nodestate.Flags
	capacityField            nodestate.Field
}

// Init sets the fields used by TokenIssuer as an input
func (tis *TokenIssuerSetup) Init(activeFlag, priorityFlag nodestate.Flags, capacityField nodestate.Field) {
	tis.activeFlag = activeFlag
	tis.priorityFlag = priorityFlag
	tis.capacityField = capacityField
}

// TokenIssuer controls token price and supply. The total token supply is controlled
// based on the target condition that some free service should be available at least
// 50% of the time. This also ensures that the sold tokens are usually all spendable.
// Tokens are issued and sold based on a bonding curve that is composed of a linear
// and a hyperbolical section. This means that token price depends on the ratio of
// current total token amount and total limit. The limit is enforced by rising the
// price until infinity before it is reached.
type TokenIssuer struct {
	TokenIssuerSetup
	ns                                         *nodestate.NodeStateMachine
	clock                                      mclock.Clock
	lock                                       sync.Mutex
	maxCap, priorityCap, freeClientCap         uint64
	capacityFactor, freeRatio, spendRate       float64
	tokenLimitTime, minLimitTime, maxLimitTime time.Duration
	lastUpdate                                 mclock.AbsTime
	expirers                                   []*TokenExpirer
	totalTokenAmount                           func() uint64
}

// NewTokenIssuer creates a new TokenIssuer
func NewTokenIssuer(ns *nodestate.NodeStateMachine, setup TokenIssuerSetup, clock mclock.Clock, freeClientCap uint64, minLimitTime, maxLimitTime time.Duration) *TokenIssuer {
	mask := setup.activeFlag.Or(setup.priorityFlag)
	ti := &TokenIssuer{
		ns:               ns,
		TokenIssuerSetup: setup,
		clock:            clock,
		freeClientCap:    freeClientCap,
		lastUpdate:       clock.Now(),
		minLimitTime:     minLimitTime,
		maxLimitTime:     maxLimitTime,
		tokenLimitTime:   minLimitTime,
	}
	ns.SubscribeState(mask, func(node *enode.Node, oldState, newState nodestate.Flags) {
		cap, _ := ns.GetField(node, ti.capacityField).(uint64)
		if newState.Equals(mask) {
			ti.lock.Lock()
			ti.priorityCap += cap
			ti.update()
			ti.lock.Unlock()
		}
		if oldState.Equals(mask) {
			ti.lock.Lock()
			ti.priorityCap -= cap
			ti.update()
			ti.lock.Unlock()
		}
	})
	ns.SubscribeField(ti.capacityField, func(node *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		if state.HasAll(mask) {
			ti.lock.Lock()
			oldCap, _ := oldValue.(uint64)
			newCap, _ := newValue.(uint64)
			ti.priorityCap += newCap - oldCap
			ti.update()
			ti.lock.Unlock()
		}
	})
	return ti
}

// TotalTokenLimit returns the current token supply limit
func (ti *TokenIssuer) TotalTokenLimit() uint64 {
	ti.lock.Lock()
	defer ti.lock.Unlock()

	ti.update()
	limit := float64(ti.tokenLimitTime) * ti.spendRate
	if limit > maxBalance {
		log.Warn("Calculated total token limit exceeds maxBalance", "limit", limit, "max", maxBalance)
		return maxBalance
	}
	return uint64(limit)
}

// SetCapacityFactor sets/updates the capacity factor which is used as a lower estimate
// for token spending rate based on the total priority client capacity
func (ti *TokenIssuer) SetCapacityFactor(capFactor float64) {
	ti.lock.Lock()
	ti.capacityFactor = capFactor
	ti.spendRate = ti.capacityFactor * float64(ti.maxCap)
	ti.update()
	ti.lock.Unlock()
}

// SetCapacityLimit sets/updates the total capacity allowance
func (ti *TokenIssuer) SetCapacityLimit(maxCap uint64) {
	ti.lock.Lock()
	ti.maxCap = maxCap
	ti.spendRate = ti.capacityFactor * float64(ti.maxCap)
	ti.update()
	ti.lock.Unlock()
}

// SetTotalAmountCallback installs the callback that allows querying the total token amount
func (ti *TokenIssuer) SetTotalAmountCallback(cb func() uint64) {
	ti.lock.Lock()
	ti.totalTokenAmount = cb
	ti.lock.Unlock()
}

// PriorityCapacity returns the current total capacity of priority nodes
func (ti *TokenIssuer) PriorityCapacity() uint64 {
	ti.lock.Lock()
	defer ti.lock.Unlock()

	return ti.priorityCap
}

// NewTokenExpirer creates a child TokenExpirer that is controlled by the issuer's freeRatio
func (ti *TokenIssuer) NewTokenExpirer() *TokenExpirer {
	ti.lock.Lock()
	te := &TokenExpirer{
		freeRatio: ti.freeRatio,
	}
	ti.expirers = append(ti.expirers, te)
	ti.lock.Unlock()
	return te
}

// update updates freeRatio, tokenLimit and token expirers based on free service
// availability. Should be called after capLimit or priorityActive is changed.
func (ti *TokenIssuer) update() {
	now := ti.clock.Now()
	if dt := now - ti.lastUpdate; dt > 0 {
		// update tokenLimitTime based on elapsed time and freeRatio
		limitDiff := time.Duration(float64(dt) * (ti.freeRatio*2 - 1))
		oldLimit := ti.tokenLimitTime
		ti.tokenLimitTime += limitDiff
		if limitDiff > 0 {
			// do not increase over 2x the current amount
			maxLimit := time.Duration(float64(ti.totalTokenAmount()) * 2 / ti.spendRate)
			if ti.maxLimitTime < maxLimit {
				maxLimit = ti.maxLimitTime
			}
			if ti.tokenLimitTime > maxLimit {
				ti.tokenLimitTime = maxLimit
			}
			if ti.tokenLimitTime < oldLimit {
				ti.tokenLimitTime = oldLimit
			}
		} else {
			if ti.tokenLimitTime < ti.minLimitTime {
				ti.tokenLimitTime = ti.minLimitTime
			}
		}
	}
	// update freeRatio
	ti.lastUpdate = now
	ti.freeRatio = 0
	if ti.priorityCap < ti.maxCap {
		freeCap := ti.maxCap - ti.priorityCap
		if freeCap > ti.freeClientCap {
			freeCapThreshold := ti.maxCap / 4
			if freeCap > freeCapThreshold {
				ti.freeRatio = 1
			} else {
				ti.freeRatio = float64(freeCap-ti.freeClientCap) / float64(freeCapThreshold-ti.freeClientCap)
			}
		}
	}
	for _, e := range ti.expirers {
		e.setFreeRatio(now, ti.freeRatio)
	}
}

// tokenPrice returns the relative cost units (RCUs) required to buy the specified amount
// of service tokens or the RCUs received when selling the given amount of tokens.
// Returns false if not possible.
// Note: RCU does not represent any specific currency type, it is calculated as the actual
// currency amount divided by the "base price" which is the average cost per token value
// calulated for recent sales. Therefore RCU has the same dimension as service tokens and
// relative prices (RCU per token) are dimensionless values.
// The relative price of each token unit depends on the current amount of existing tokens
// and the total token limit, first raising from 0 to 1 linearly, then tends to infinity
// as tokenAmount approaches tokenLimit.
//
// if 0 <= tokenAmount <= tokenLimit/2:
//   tokenPrice = tokenAmount/(tokenLimit/2)
// if tokenLimit/2 <= tokenAmount < tokenLimit:
//   tokenPrice = tokenLimit/2/(tokenLimit-tokenAmount)
//
// The price of multiple tokens is calculated as an integral based on the above formula.
func (ti *TokenIssuer) tokenPrice(buySellAmount uint64, buy bool) (float64, bool) {
	tokenLimit := ti.TotalTokenLimit()
	tokenAmount := ti.totalTokenAmount()
	if buy {
		if tokenAmount+buySellAmount >= tokenLimit {
			return 0, false
		}
	} else {
		maxAmount := uint64(float64(tokenLimit) * tokenSellMaxRatio)
		if tokenAmount > maxAmount {
			tokenAmount = maxAmount
		}
		if tokenAmount < buySellAmount {
			buySellAmount = tokenAmount
		}
		tokenAmount -= buySellAmount
	}
	r := float64(tokenAmount) / float64(tokenLimit)
	b := float64(buySellAmount) / float64(tokenLimit)
	var relPrice float64
	if r < 0.5 {
		// first purchased token is in the linear range
		if r+b <= 0.5 {
			// all purchased tokens are in the linear range
			relPrice = b * (r + r + b)
			b = 0
		} else {
			// some purchased tokens are in the 1/x range, calculate linear price
			// update starting point and amount left to buy in the 1/x range
			relPrice = (0.5 - r) * (r + 0.5)
			b = r + b - 0.5
			r = 0.5
		}
	}
	if b > 0 {
		// some purchased tokens are in the 1/x range
		l := 1 - r
		if l < 1e-10 {
			return 0, false
		}
		l = -b / l
		if l < -1+1e-10 {
			return 0, false
		}
		relPrice += -math.Log1p(l) / 2
	}
	return float64(tokenLimit) * relPrice, true
}

// TokenBuyPrice returns the RCU amount required to buy the specified amount of
// service tokens. Returns false if not possible.
func (ti *TokenIssuer) TokenBuyPrice(buyAmount uint64) (float64, bool) {
	return ti.tokenPrice(buyAmount, true)
}

// TokenSellPrice returns the RCU amount received when selling the specified amount of
// service tokens. Returns false if not possible.
func (ti *TokenIssuer) TokenSellPrice(sellAmount uint64) (float64, bool) {
	return ti.tokenPrice(sellAmount, false)
}

// TokenBuyAmount returns the service token amount currently available for the given
// sum of RCUs
func (ti *TokenIssuer) TokenBuyAmount(price float64) uint64 {
	tokenLimit := ti.TotalTokenLimit()
	tokenAmount := ti.totalTokenAmount()
	if tokenLimit <= tokenAmount {
		return 0
	}
	r := float64(tokenAmount) / float64(tokenLimit)
	c := price / float64(tokenLimit)
	var relTokens float64
	if r < 0.5 {
		// first purchased token is in the linear range
		relTokens = math.Sqrt(r*r+c) - r
		if r+relTokens <= 0.5 {
			// all purchased tokens are in the linear range, no more to spend
			c = 0
		} else {
			// some purchased tokens are in the 1/x range, calculate linear amount
			// update starting point and available funds left to buy in the 1/x range
			relTokens = 0.5 - r
			c -= (0.5 - r) * (r + 0.5)
			r = 0.5
		}
	}
	if c > 0 {
		relTokens -= math.Expm1(-2*c) * (1 - r)
	}
	return uint64(relTokens * float64(tokenLimit))
}

// TokenSellAmount returns the service token amount that needs to be sold in order
// to receive the given sum of RCUs. Returns false if not possible.
func (ti *TokenIssuer) TokenSellAmount(price float64) (uint64, bool) {
	tokenLimit := ti.TotalTokenLimit()
	tokenAmount := ti.totalTokenAmount()
	r := float64(tokenAmount) / float64(tokenLimit)
	if r > tokenSellMaxRatio {
		r = tokenSellMaxRatio
	}
	c := price / float64(tokenLimit)
	var relTokens float64
	if r > 0.5 {
		// first sold token is in the 1/x range
		relTokens = math.Expm1(2*c) * (1 - r)
		if r-relTokens >= 0.5 || 1-r < 1e-10 {
			// all sold tokens are in the 1/x range, no more to sell
			c = 0
		} else {
			// some sold tokens are in the linear range, calculate price in 1/x range
			// update starting point and remaining price to sell for in the linear range
			relTokens = r - 0.5
			c -= math.Log1p(relTokens/(1-r)) / 2
			r = 0.5
		}
	}
	if c > 0 {
		// some sold tokens are in the linear range
		if x := r*r - c; x >= 0 {
			relTokens += r - math.Sqrt(x)
		} else {
			return 0, false
		}
	}
	return uint64(relTokens * float64(tokenLimit)), true
}

// TokenExpirer implements utils.ValueExpirer with an extra feature: it applies a multiplier
// on the expiration rate that is controlled by the parent TokenIssuer. If the pool is
// full and token spendability is potentially limited then the token expiration is also
// slowed down or suspended.
type TokenExpirer struct {
	exp             utils.Expirer
	lock            sync.RWMutex
	rate, freeRatio float64
}

// setFreeRatio updates the "free ratio" which acts as a multiplier for the basic
// expiration rate
func (te *TokenExpirer) setFreeRatio(now mclock.AbsTime, freeRatio float64) {
	te.lock.Lock()
	te.freeRatio = freeRatio
	te.exp.SetRate(now, te.rate*freeRatio)
	te.lock.Unlock()
}

// SetRate implements utils.ValueExpirer
func (te *TokenExpirer) SetRate(now mclock.AbsTime, rate float64) {
	te.lock.Lock()
	te.rate = rate
	te.exp.SetRate(now, rate*te.freeRatio)
	te.lock.Unlock()
}

// SetLogOffset implements utils.ValueExpirer
func (te *TokenExpirer) SetLogOffset(now mclock.AbsTime, logOffset utils.Fixed64) {
	te.lock.Lock()
	te.exp.SetLogOffset(now, logOffset)
	te.lock.Unlock()
}

// LogOffset implements utils.ValueExpirer
func (te *TokenExpirer) LogOffset(now mclock.AbsTime) utils.Fixed64 {
	te.lock.RLock()
	logOffset := te.exp.LogOffset(now)
	te.lock.RUnlock()
	return logOffset
}
