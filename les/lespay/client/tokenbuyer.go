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
	"io"
	"math"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
)

type tokenMirror struct {
	tokensExpected      uint64
	tokenBalance        utils.ExpiredValue // expired by remoteExpirer
	remoteExpirer       utils.Expirer
	tvSpent, tvLost     utils.ExpiredValue // expired by statsExpirer
	tvFactor, paidRatio float64
}

func (tm *tokenMirror) update(now mclock.AbsTime, statsExpFactor utils.ExpirationFactor, value, cost, balance uint64) {
	localBalance := tm.tokenBalance.Value(tm.remoteExpirer.LogOffset(now))
	if localBalance > cost {
		localBalance -= cost
	} else {
		localBalance = 0
	}
	tm.tvSpent += value
	if tm.tokensExpected > 0 && balance > localBalance {
		if balance >= localBalance+tm.tokensExpected {
			localBalance += tm.tokensExpected
			tm.tokensExpected = 0
		} else {
			tm.tokensExpected -= balance - localBalance
			localBalance = balance
		}
	}
	if balance > localBalance {
		// free tokens received, adjust paid ratio
		totalValue := float64(tm.tokensExpected+localBalance)*tm.tvFactor + float64(tm.tvSpent+tn.tvLost)
		paidValue := totalValue * tm.paidRatio
		tm.paidRatio = paidValue / (totalValue + float64(balance-localBalance)*tm.tvFactor)
	} else {
		tm.tvLost += float64(localBalance-balance) * tm.tvFactor
	}
	tm.tokenBalance = balance // exp?
}

func (tm *tokenMirror) setValueFactor(tvFactor float64) {
	if tvFactor < tm.tvFactor {
		tm.tokensExpected += uint64(float64(tm.tokensExpected+tm.tokenBalance) * (tm.tvFactor/tvFactor - 1)) //TODO safety check
	}
	//TODO expected timeout
	tm.tvFactor = tvFactor
}

func (tm *tokenMirror) predictTokenValue() float64 {
	promisedValue := float64(tm.tokensExpected+tm.tokenBalance) * tm.tvFactor
	return promisedValue * utils.InvLinHyper((tm.tvSpent-penaltyFactor*(promisedValue+tm.tvLost))/(tm.tvSpent*tm.paidRatio)) //TODO neg, inf
	// ?? service value, quality factor
}

func (tm *tokenMirror) predictPurchaseValue(buyAmount uint64) float64 {
	tm.tokensExpected += buyAmount
	v := tm.predictTotalValue()
	tm.tokensExpected -= buyAmount
	return tm.predictTotalValue() - v //TODO numerical precision, caching
}

func (tm *tokenMirror) buyTokens(buyAmount uint64) {
	tm.tokensExpected += buyAmount
	//TODO expected timeout, paidRatio update
}

type (
	TokenBuyer struct {
		ns  *nodestate.NodeStateMachine
		vt  *ValueTracker
		ups *usagePatternStats

		paymentBuffer   uint64
		targetTokenTime float64
	}

	PriceQueryFunc func(*enode.Node, *PriceQuery) bool
)

func NewTokenBuyer(ns *nodestate.NodeStateMachine, requireFlags, disableFlags nodestate.Flags) *TokenBuyer {

}

func (tb *TokenBuyer) futureTokenValue() float64 {
	var sum float64
	// cache
	ns.ForEach(tb.activeFlag.Or(tb.paidFlag), nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		sum += tb.vt.GetNode(node.ID()).tokenMirror.predictTokenValue() // locking, ???
	})
	return sum
}

func (tb *TokenBuyer) futureTokenTime() float64 {
	// cache
	return tb.ups.predictTime(now, tb.futureTokenValue())
}

func (tb *TokenBuyer) futureTokenTimeDiff(diffValue float64) float64 {
	return tb.ups.predictTimeDiff(tb.futureTokenValue(), diffValue)
}

func (tb *TokenBuyer) purchasedTokenTime(tm *tokenMirror, amount float64) float64 {
	return tb.futureTokenTimeDiff(tm.predictPurchaseValue(amount))
}

func (tb *TokenBuyer) purchaseLimit() float64 {
	// cache
	limit := tb.ups.predictDemand(now, tb.targetTokenTime) - tb.futureTokenValue()
	if limit < 0 {
		return 0
	}
	return limit
}

func (tb *TokenBuyer) adjustLoop() {
	for {
		select {
		case <-tb.clock.After(adjustPeriod):
			ftt, dftt := tb.futureTokenTime(), tb.futureTokenTimeDiff(tb.minPurchasedReqValue)
			chargeRate := 1 - ftt/tb.targetTokenTime
			if chargeRate > 0 {
				tb.paymentBuffer += tb.maxPaymentRate * chargeRate * adjustPeriod
			}
			if dftt > 1e-10 && tb.paymentBuffer > 0 {
				priceThreshold := (dftt * float64(tb.paymentBuffer)) / ((ftt + dftt) * tb.minPurchasedReqValue * float64(recentQueryTC))
				tb.recentQuerySet.Remove(int64(math.Log(priceThreshold)) - int64(tb.clock.Now()))
			}
		case <-tb.quit:
			return
		}
	}
}

func (tb *TokenBuyer) priceQueryLoop() {
	for tb.queryIterator.Next() {
		node := tb.queryIterator.Node()
		tm := &tb.vt.GetNode(node).tokenMirror
		query := tb.createPriceQuery(tm)
		go func() {
			if tb.queryFunc(node, query) {
				if tb.evaluateOffer {
					tb.ns.SetState(node, CanBuyFlag, nodestate.Flags{}, offerTimeout)
				} else {
					tb.ns.SetState(node, RecentQueryFlag, nodestate.Flags{}, 0)
					tb.ns.SetField(node, LastPriceField, tb.priceThreshold(query))
				}
			} else {
				var wait time.Duration
				if t, ok := tb.ns.GetField(node, QueryTimeoutField).(queryTimeout); ok {
					wait = time.Second * (t.end - t.start)
				} else {

				}
				tb.ns.SetState(node, RecentQueryFlag, nodestate.Flags{}, wait)
			}
		}()
	}
}

type SinglePriceQuery struct {
	Amount, Cost uint64
}

type PriceQuery struct {
	Queries              []SinglePriceQuery
	tm                   *tokenMirror
	offerRate, maxAmount float64
}

type linearApproxPrice PriceQuery

func (lc *linearApproxPrice) Params() (int, bool) {
	return len(lc.Queries), true
}

func (lc *linearApproxPrice) X(i int) float64 {
	if i == 0 {
		return 0
	}
	if i <= len(lc.Queries) {
		return float64(lc.Queries[i-1].Amount)
	}
	return float64(lc.Queries[len(lc.Queries)-1].Amount)
}

func (lc *linearApproxCost) Y(i int) float64 {
	if i == 0 {
		return 0
	}
	if i <= len(lc.Queries) {
		return float64(lc.Queries[i-1].Cost) / float64(lc.Queries[i-1].Amount)
	}
	return math.Inf(1)
}

func (tb *TokenBuyer) createPriceQuery(tm *tokenMirror) *PriceQuery {
	tv := tb.purchaseLimit()
	if tv2 := tm.purchaseLimit(); tv2 < tv {
		tv = tv2
	}
	query := &PriceQuery{
		tm:      tm,
		Queries: make([]SinglePriceQuery, priceQueryCount),
	}
	for i := priceQueryCount - 1; i >= 0; i-- {
		if tv < tb.minPurchasedReqValue {
			tv == tb.minPurchasedReqValue
		}
		query.Queries[i].Amount = uint64(tv / tm.tvFactor)
		if tv == tb.minPurchasedReqValue {
			query.Queries = query.Queries[i:]
			break
		}
		tv /= priceQueryStep
	}
	return query
}

func (tb *TokenBuyer) offerRate(query *PriceQuery, amount float64) float64 {
	ftt, _ := tb.futureTokenTime()
	return tb.purchasedTokenTime(query.tm, amount) * tb.paymentBuffer * tm.tvFactor / (ftt * utils.PwlValue((*linearApproxPrice)(query), amount))
}

func (tb *TokenBuyer) evaluateOffer(query *PriceQuery) bool {
	// lock?
	// paymentBuffer update
	minAmount := tb.minPurchasedReqValue / tm.tvFactor
	ftt := tb.futureTokenTime()
	query.offerRate = tb.offerRate(query, minAmount)
	if query.offerRate >= 1 {
		maxAmount := float64(query.Queries[len(query.Queries)-1].Amount)
		for maxAmount > minAmount*1.01 {
			m := math.Sqrt(minAmount * maxAmount)
			if tb.offerRate(query, m) < 1 {
				maxAmount = m
			} else {
				minAmount = m
			}
		}
		query.maxAmount = minAmount
		return true
	}
	return false
}

type BuyRequest struct {
	Cost, expectedAmount uint64
	tm                   *tokenMirror
}

func (tb *TokenBuyer) createBuyRequest(queries []*PriceQuery) []*BuyRequest {
	if len(queries) < 1 {
		return nil
	}
	// !!! mindig legyen minPurchasedReqValue, mert a threshold is azzal szamol, meg ez az alap, hogy annyiert mar erdemes ugrani
	var bestRate, bestAmount int
	for i, query := range queries {
		if query.offerRate > queries[bestRate].offerRate {
			bestRate = i
		}
		if query.maxAmount > queries[bestAmount].maxAmount {
			bestAmount = i
		}
	}
	queries[bestRate].offerRate
}
