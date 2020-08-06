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

type TokenBuyerSetup struct {
	recentQueryFlag, canPayFlag, activeFlag, PaidFlag nodestate.Flags
	lastPriceField, queryWeightField                  nodestate.Field
}

func NewTokenBuyerSetup(setup *nodestate.Setup) TokenBuyerSetup {
	return TokenBuyerSetup{
		recentQueryFlag: setup.NewFlag("recentQuery"),
		PaidFlag:        setup.NewFlag("paid"),
		lastPriceField:  setup.NewField("lastPrice", reflect.TypeOf(int64(0))),
	}
}

// Connect sets the fields and flags used by TokenBuyer as an input
func (tbs *TokenBuyerSetup) Connect(canPayFlag, activeFlag nodestate.Flags, queryWeightField nodestate.Field) {
	tbs.canPayFlag = canPayFlag
	tbs.activeFlag = activeFlag
	tbs.queryWeightField = queryWeightField
}

type TokenBuyer struct {
	ns             *nodestate.NodeStateMachine
	vt             *ValueTracker
	ups            usagePatternStats
	recentQuerySet *ThresholdSet
	queryIterator  *WrsIterator

	paymentBuffer   uint64
	targetTokenTime float64
}

type PriceQueryFunc func(*enode.Node, *PriceQuery) bool

func NewTokenBuyer(ns *nodestate.NodeStateMachine, setup TokenBuyerSetup, udp bool) *TokenBuyer {
	wrsRequire := setup.canPayFlag
	if !udp {
		wrsRequire = nodestate.MergeFlags(setup.canPayFlag, setup.activeFlag)
	}
	tb := &TokenBuyer{
		TokenBuyerSetup: setup,
		ns:              ns,
		vt:              vt, //???
		recentQuerySet:  NewThresholdSet(ns, setup.lastPriceField, nodestate.Flags{}, setup.recentQueryFlag),
		queryIterator:   NewWrsIterator(ns, wrsRequire, setup.recentQueryFlag, setup.queryWeightField),
	}
	tb.ups.init()
	go tb.thresholdLoop()
	go tb.priceQueryLoop()

	return tb
}

func (tb *TokenBuyer) Stop() {
	close(tb.quit)
	tb.queryIterator.Close()
	//TODO ensure that all pending payments are stored in the db
}

func (tb *TokenBuyer) futureTokenValue() float64 {
	var sum float64
	// cache
	ns.ForEach(nodestate.MergeFlags(tb.activeFlag, tb.paidFlag), nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		sum += tb.vt.GetNode(node.ID()).tokenMirror.predictTokenValue() // locking, ???
	})
	return sum
}

func (tb *TokenBuyer) futureTokenTime() float64 {
	// cache
	return tb.ups.predictTime(now, tb.futureTokenValue())
}

func (tb *TokenBuyer) futureTokenTimeDiff(diffValue float64) (float64, float64) {
	return tb.ups.predictTimeDiff(now, tb.futureTokenValue(), diffValue)
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

func (tb *TokenBuyer) updateThreshold(delayCh chan struct{}) {
	ftt, dftt := tb.futureTokenTimeDiff(tb.minPurchasedReqValue)
	chargeRate := 1 - ftt/tb.targetTokenTime
	if chargeRate > 0 {
		tb.paymentBuffer += tb.maxPaymentRate * chargeRate * adjustPeriod
	}
	var priceThreshold int64
	/*if dftt > 1e-10 && tb.paymentBuffer > 0 {
		priceThreshold := (dftt * float64(tb.paymentBuffer)) / ((ftt + dftt) * tb.minPurchasedReqValue * float64(recentQueryTC))
		tb.recentQuerySet.Remove(int64(math.Log(priceThreshold)) - int64(tb.clock.Now()))
	}*/
	if delayCh != nil {
		tb.delayCh = delayCh
		tb.delayThreshold = priceThreshold
	} else if tb.delayCh != nil {
		diff := priceThreshold - tb.delayThreshold
		if diff >= delayThreshold {
			close(tb.delayCh)
			tb.delayCh = nil
		}
		if diff < 0 {
			tb.delayThreshold = priceThreshold
		}
	}

}

func (tb *TokenBuyer) thresholdLoop() {
	for {
		select {
		case <-tb.clock.After(adjustPeriod):
			tb.updateThreshold()
		case delayCh := <-tb.triggerCh:
			tb.updateThreshold(delayCh)
		case <-tb.quit:
			return
		}
	}
}

func (tb *TokenBuyer) priceQueryLoop() {
	for tb.queryIterator.Next() {
		node := tb.queryIterator.Node()
		go tb.query(node)
		delayCh := make(chan struct{})
		tb.triggerCh <- delayCh
		select {
		case <-delayCh:
		case <-tb.quickQueryCh:
		case <-tb.quit:
			return
		}
	}
}

func (tb *TokenBuyer) queryAndBuy() {
	queryCh := time.After(time.Second)
	selectCh := time.After(time.Second * 3)
	for i := 0; i < 10; i++ {
		select {
		case tb.quickQueryCh <- struct{}{}:
		case <-queryCh:
			break
		case <-tb.quit:
			return
		}
	}
	select {
	case <-selectCh:
	case <-tb.quit:
		return
	}
	var queries []*PriceQuery
	ns.ForEach(tb.canBuyFlag, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		query, _ := tb.ns.GetField(node, tb.queryField).(*PriceQuery)
		tb.ns.SetState(node, nodestate.Flags{}, tb.canBuyFlag, 0)
		if query != nil {
			queries = append(queries, query)
		}
	})
	if queries != nil {
		tb.lock.Lock()
		buyList := tb.createBuyRequests(queries)
		tb.lock.Unlock()
		if len(buyList) > 0 {
			for _, buy := range buyList {
				go tb.buyFunc(buy.node, buy)
				//TODO check results and update token mirror, set paid flag
			}
		}
	}
}

func (tb *TokenBuyer) query(node *enode.Node) {
	tb.lock.Lock()
	tm := &tb.vt.GetNode(node).tokenMirror
	query := tb.createPriceQuery(tm)
	tb.lock.Unlock()
	if tb.queryFunc(node, query) {
		tb.lock.Lock()
		canBuy := tb.evaluateOffer(query)
		tb.lock.Unlock()
		if canBuy {
			tb.ns.SetField(node, tb.queryField, query)
			tb.ns.SetState(node, tb.canBuyFlag, nodestate.Flags{}, offerTimeout)
		} else {
			tb.ns.SetState(node, tb.recentQueryFlag, nodestate.Flags{}, 0)
			tb.ns.SetField(node, tb.lastPriceField, tb.priceThreshold(query)) //lock?
		}
	} else {
		var wait time.Duration
		if t, ok := tb.ns.GetField(node, tb.queryTimeoutField).(queryTimeout); ok {
			wait = time.Second * (t.end - t.start)
		} else {

		}
		tb.ns.SetState(node, tb.recentQueryFlag, nodestate.Flags{}, wait)
	}
}

type SinglePriceQuery struct {
	Amount, Cost uint64
}

type PriceQuery struct {
	Queries              []SinglePriceQuery
	node                 *enode.Node
	tm                   *tokenMirror
	offerRate, maxAmount float64
}

type linearApproxPrice PriceQuery

func (lc *linearApproxPrice) Len() int {
	return len(lc.Queries)
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

func (tb *TokenBuyer) createPriceQuery(node *enode.Node, tm *tokenMirror) *PriceQuery {
	tv := tb.purchaseLimit()
	if tv2 := tm.purchaseLimit(); tv2 < tv {
		tv = tv2
	}
	query := &PriceQuery{
		node:    node,
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
	cost := uint64(utils.PwlValue((*linearApproxPrice)(query), amount))
	cost = tb.paymentBackend.EstimateCost(query.node, cost)
	if cost == 0 {
		cost = 1
	}
	return tb.purchasedTokenTime(query.tm, amount) * tb.paymentBuffer * tm.tvFactor / (ftt * float64(cost))
}

func (tb *TokenBuyer) evaluateOffer(query *PriceQuery) bool {
	// lock?
	// paymentBuffer update
	ftt := tb.futureTokenTime()
	query.offerRate = tb.offerRate(query, query.Queries[0].Amount)
	return query.offerRate >= 1
}

func (tb *TokenBuyer) buyAmount(query *PriceQuery) uint64 {
	minAmount := query.Queries[0].Amount
	maxAmount := float64(query.Queries[len(query.Queries)-1].Amount)
	for maxAmount > minAmount*1.01 {
		m := math.Sqrt(minAmount * maxAmount)
		if tb.offerRate(query, m) < 1 {
			maxAmount = m
		} else {
			minAmount = m
		}
	}
	return uint64(minAmount)
}

type BuyRequest struct {
	Proof          []byte
	expectedAmount uint64
	node           *enode.Node
	tm             *tokenMirror
}

func (tb *TokenBuyer) createBuyRequests(queries []*PriceQuery) (buy []*BuyRequest) {
	// !!! mindig legyen minPurchasedReqValue, mert a threshold is azzal szamol, meg ez az alap, hogy annyiert mar erdemes ugrani
	for len(queries) > 0 {
		bestRate := 0
		for i := 1; i < len(queries); i++ {
			if queries[i].offerRate > queries[bestRate].offerRate {
				bestRate = i
			}
		}
		buyQuery := queries[bestRate]
		queries[bestRate] = queries[len(queries)-1]
		queries = queries[:len(queries)-1]
		//TODO remove flag
		maxCost := uint64(utils.PwlValue((*linearApproxPrice)(query), tb.buyAmount(buyQuery)))
		cost, totalCost, proof := tb.paymentBackend.Pay(buyQuery.node, buyQuery.Queries[0].Cost, maxCost)
		if proof != nil {
			buy = append(buy, &BuyRequest{
				Proof:          proof,
				expectedAmount: uint64(utils.PwlInverse((*linearApproxPrice)(buyQuery), cost)),
				node:           buyQuery.node,
				tm:             buyQuery.tm,
			})
			tb.paymentBuffer -= totalCost
			//TODO invalidate ftt cache
			// after successful payment re-evaluate remaining offers and see if we can buy more
			for i := 0; i < len(queries); {
				if evaluateOffer(queries[i]) {
					i++
				} else {
					queries[i] = queries[len(queries)-1]
					queries = queries[:len(queries)-1]
					//TODO remove flag
				}
			}
		}
	}
}

type tokenMirror struct {
	tokensExpected               uint64
	tokenBalance                 utils.ExpiredValue // expired by remoteExpirer
	remoteExpirer                utils.Expirer
	tvSpent, tvLost              utils.ExpiredValue // expired by statsExpirer
	capacity                     uint64
	tvFactor, paidRatio, capCost float64
	bufferRatio                  uint32
	lastUpdate                   mclock.AbsTime
}

func (tm *tokenMirror) capUpdate(now mclock.AbsTime, logOffset utils.Fixed64) {
	dt := now - tm.lastUpdate
	tm.lastUpdate = now
	if dt > 0 {
		tm.tokenBalance.Add(-int64(float64(dt)*float64(tm.capacity)*tm.capCost), logOffset)
	}
}

func (tm *tokenMirror) update(now mclock.AbsTime, statsExpFactor utils.ExpirationFactor, value, cost, balance uint64) {
	logOffset := tm.remoteExpirer.LogOffset(now)
	tm.capUpdate(now, logOffset)
	localBalance := tm.tokenBalance.Value(logOffset)
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

func (tm *tokenMirror) setPriceInfo(now mclock.AbsTime, rvFactor, capCost float64, bufferRatio, expiration uint32) {
	tm.capUpdate(now, tm.remoteExpirer.LogOffset(now))
	tm.capCost = capCost
	var rate float64
	if expiration > 0 {
		rate = 1 / float64(time.Second*time.Duration(expiration))
	}
	tm.remoteExpirer.SetRate(now, rate)
	tm.rvFactor = rvFactor
	tm.bufferRatio = bufferRatio
}

func (tm *tokenMirror) setValueFactor() {
	tvFactor := tm.rvFactor / (1 + tm.capCost*1000000*ups.capacityRatio(xxx))
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
