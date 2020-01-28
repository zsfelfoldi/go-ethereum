// Copyright 2019 The go-ethereum Authors
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

package les

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

const (
	capValueFilterCount = 16
	minExpRT            = time.Millisecond * 100
	maxExpRT            = time.Second * 5
)

var (
	cvfExpRTs  [capValueFilterCount]float64
	cvfLogStep float64
)

func init() {
	cvfLogStep = math.Log(float64(maxExpRT)/float64(minExpRT)) / (capValueFilterCount - 1)
	for i, _ := range cvfExpRTs[:] {
		cvfExpRTs[i] = float64(minExpRT) * math.Exp(float64(i)*cvfLogStep)
	}
}

func cvfIndex(expRT time.Duration) float64 {
	return math.Log(float64(expRT)/float64(minExpRT)) / cvfLogStep
}

type valueTracker struct {
	maxBufLimit        [capValueFilterCount]float64
	maxBufLimitLastExp mclock.AbsTime

	costList                      RequestCostList
	capFactor, cvFactor, rvFactor float64
	capacity                      uint64

	paidTotal, freeTotal, freeExtra, expired, failed float64
	delivered                                        responseTimeStats
	vtLastExp                                        mclock.AbsTime
}

type reqValue struct {
	value, maxValue uint64
	lastExp         mclock.AbsTime
	expRate         float64
}

func (vt *valueTracker) setExpRate(expRate float64) {
	vt.applyContinuousCosts(mclock.Now())
	vt.tokenMirrorExpRate = expRate
}

func (vt *valueTracker) setCapacity(capacity uint64) {
	vt.applyContinuousCosts(mclock.Now())
	vt.capacity = capacity
	vt.tokenMirrorExpAllowed = capacity != 0
}

func (vt *valueTracker) updateCostTable(cl RequestCostList, capFactor float64) {
	vt.capFactor = capFactor
	vt.costList = costList
	oldCvFactor := vt.cvFactor
	vt.recalcCvFactor()
	if vt.cvFactor > oldCvFactor {
		scaleDown := oldCvFactor / vt.cvFactor
		for i, v := range vt.maxBufLimit[:] {
			vt.maxBufLimit[i] = v * scaleDown
		}
	}
}

type vtRequestInfo struct {
	at                                     mclock.AbsTime
	maxCost, realCost, balance, bufMissing uint64
	respTime                               time.Duration
}

// assumes that realCost <= maxCost
func (vt *valueTracker) addRequest(maxCost, realCost, balance, bufMissing uint64, respTime time.Duration) {
	now := mclock.Now()
	vt.checkProcessRequestQueue()
	if len(vt.reqQueue) >= vtRequestQueueLimit {
		dt := (now - vt.reqBurstStart) / vtRequestQueueLimit
		vt.reqBurstStart += dt
		vt.updateRespTimeStats(&vt.reqQueue[0], float64(dt)/float64(vtRequestBurstGap))
		vt.reqQueue = vt.reqQueue[1:]
	}
	req := vtRequestInfo{
		at:         now,
		maxCost:    maxCost,
		realCost:   realCost,
		balance:    balance,
		bufMissing: bufMissing,
		respTime:   respTime,
	}
	vt.updateTokenMirror(&req)
	if len(vt.reqQueue) == 0 {
		vt.reqBurstStart = now
	}
	vt.reqQueue = append(vt.reqQueue, req)
}

func (vt *valueTracker) checkProcessRequestQueue() {
	if len(vt.reqQueue) != 0 && time.Duration(now-vt.reqQueue[len(vt.reqQueue)-1].at) >= vtRequestBurstGap {
		dt := time.Duration(vt.reqQueue[len(vt.reqQueue)-1].at-vt.reqBurstStart) + vtRequestBurstGap
		rtWeight := float64(dt) / float64(vtRequestBurstGap*time.Duration(len(vt.reqQueue)))
		for i, _ := range vt.reqQueue {
			vt.updateRespTimeStats(&vt.reqQueue[i], rtWeight)
		}
		vt.reqQueue = nil
	}
}

func (vt *valueTracker) applyContinuousCosts(now mclock.AbsTime) {
	dt := now - vt.tokenMirrorLastUpdate
	vt.tokenMirrorLastUpdate = now
	if dt <= 0 {
		return
	}

	var sub uint64
	dtf := float64(dt)
	if vt.capacity != 0 {
		sub += uint64(float64(vt.capacity) * vt.capFactor * dtf)
	}
	if tokenMirrorExpAllowed {
		sub += uint64(-float64(vt.tokenMirror) * math.Expm1(-dtf*vt.tokenMirrorExpRate))
	}

	if sub < vt.tokenMirror {
		vt.tokenMirror -= sub
	} else {
		vt.tokenMirror = 0
	}
}

func (vt *valueTracker) updateTokenMirror(req *vtRequestInfo) {
	// update token mirror
	vt.applyContinuousCosts(req.at)
	balanceBefore := req.balance + req.realCost
	if vt.tokenMirror < balanceBefore {
		vt.tokenDiscount += balanceBefore - vt.tokenMirror
	} else {
		vt.tokensLost += vt.tokenMirror - balanceBefore
	}
	if req.realCost < req.maxCost {
		vt.tokenDiscount += req.maxCost - req.realCost
	}
	vt.tokensSpent += req.realCost
	vt.tokenMirror = req.balance
}

func (vt *valueTracker) updateRespTimeStats(req *vtRequestInfo, rtWeight float64) {
	// update response time stats
	vt.rtStats.add(respTime, rtWeight)
	// update capValue filters
	dt := now - vt.maxBufLimitLastExp
	if dt < 0 {
		dt = 0
		vt.maxBufLimitLastExp = now
	}
	expCorr := math.Exp(float64(dt) * cvfTC)
	if expCorr > 100 {
		for i, v := range vt.maxBufLimit[:] {
			vt.maxBufLimit[i] = v / expCorr
		}
		expCorr = 1
		vt.maxBufLimitLastExp = now
	}
	bl := float64(bufMissing) * expCorr

	for i, expRT := range cvfExpRTs[:] {
		mbl := vt.maxBufLimit[i]
		rtFactor := 1 - float64(respTime)/expRT
		if rtFactor < -1 {
			rtFactor = -1
		}
		neg := rtFactor < 0
		if neg == (bl < mbl) {
			step := (bl - mbl) * rtFactor * rtWeight * vtBufLimitUpdateRate
			if neg {
				mbl -= step
			} else {
				mbl += step
			}
		}
	}
}

type globalValueTracker struct {
	refBasket, newBasket requestBasket
	refCapReq, newCapReq basketItem
	refValueMul          float64
}

type (
	requestBasket map[uint32]requestItem
	basketItem    struct {
		amount, value uint64
	}
	requestItem struct {
		first, rest basketItem
	}
)

func (rb requestBasket) addRequest(reqType, reqAmount uint32, baseValue, reqValue uint64) {
	a := rb[reqType]
	a.totalCount++
	a.totalAmount += uint64(reqAmount)
	a.baseValue += baseValue
	a.reqValue += reqValue
	rb[reqType] = a
}

func (rb requestBasket) addBasket(ab requestBasket) {
	for rt, aa := range ab {
		a := rb[reqType]
		a.totalCount += aa.totalCount
		a.totalAmount += aa.totalAmount
		a.baseValue += aa.baseValue
		a.reqValue += aa.reqValue
		rb[reqType] = a
	}
}

// assumes that cost list contains all necessary request types
func (gv *globalValueTracker) valueFactors(costs RequestCostList, capCost float64) (cvf, rvf float64) {
	var sum float64
	for _, req := range costs {
		if ref, ok := gv.refBasket[req.MsgCode]; ok {
			sum += float64(req.BaseCost)*float64(ref.first.amount) + float64(req.ReqCost)*float64(ref.first.amount+ref.rest.amount)
		}
	}
	sum2 := sum + capCost*gv.refCapReq.amount
	sum *= gv.refValueMul
	if sum < 1e-100 {
		sum = 1e-100
	}
	sum2 *= gv.refValueMul
	if sum2 < 1e-100 {
		sum2 = 1e-100
	}
	return 1 / sum, 1 / sum2
}

// assumes that old basket contains all necessary request types
func (gv *globalValueTracker) updateReferenceBasket() {
	for vt, _ := range gv.servers {
		rb, rcr := vt.getBasket()
		gv.newBasket.addBasket(rb)
		gv.newCapReq.addItem(rcr)
	}

	var oldSum, newSum float64
	addItem := func(a, b basketItem) (sum basketItem) {
		sum.amount = a.amount + b.amount
		sum.value = a.value + b.value
		if sum.amount > 0 {
			avg := float64(sum.value) / float64(sum.amount)
			oldSum += float64(a.amount) * avg
			newSum += float64(b.amount) * avg
		}
	}

	for rt, a := range gv.refBasket {
		b := gv.newBasket[rt]
		gv.refBasket[rt] = requestItem{
			first: addItem(a.first, b.first),
			rest:  addItem(a.rest, b.rest),
		}
	}
	gv.refCapReq = addItem(gv.refCapReq, gv.newCapReq)
	if oldSum > 1e-10 {
		gv.refValueMul *= oldSum / (oldSum + newSum)
	}

	for vt, _ := range gv.servers {
		cvf, rvf := gv.valueFactors(vt.costList, vt.capCost)
		vt.setValueFactors(cvf, rvf)
	}
}

func (gv *globalValueTracker) xxx() {

}

func (gv *globalValueTracker) xxx() {

}
