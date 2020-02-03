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
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
	lru "github.com/hashicorp/golang-lru"
)

const (
	vtVersion           = 1
	capValueFilterCount = 16
	minExpRT            = time.Millisecond * 100
	maxExpRT            = time.Second * 5

	valueExpPeriod        = time.Minute * 10
	valueExpTC            = 1 / float64(time.Hour*1000)
	tokensExpectedTimeout = time.Second * 30
	vtRequestQueueLimit   = 64
	vtRequestBurstGap     = time.Second * 2
	bufferPeakTC          = 1 / float64(time.Minute*2)
	vtUpdatePeriod        = time.Second * 10
	refBasketUpdatePeriod = time.Minute * 10
)

var (
	cvfExpRTs  [capValueFilterCount]float64
	cvfLogStep float64

	vtKey = []byte("vt:")
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

type tokenValue struct {
	tokens, reqValue uint64
}

func (tv *tokenValue) value(rvFactor float64) uint64 {
	if tv.tokens == 0 {
		return tv.reqValue
	}
	t := uint64(float64(tv.tokens) * rvFactor)
	return t + tv.reqValue
}

func (tv *tokenValue) moveToValue(rvFactor float64) {
	if tv.tokens == 0 {
		return
	}
	t := uint64(float64(tv.tokens) * rvFactor)
	tv.tokens = 0
	tv.reqValue += t
}

type valueTracker struct {
	lock sync.Mutex

	bufferPeak        [capValueFilterCount]float64
	bufferPeakLastExp mclock.AbsTime
	reqQueue          []vtRequestInfo
	reqBurstStart     mclock.AbsTime

	capFactor, cvFactor, rvFactor float64
	capacity                      uint64
	basket                        requestBasket
	capacityUsed                  basketItem

	// these values are nominated in request value (token * rvFactor)
	// a uniform exponential expiration is applied
	paidTotal, freeTotal, freeCredited tokenValue
	delivered, expired, failed         tokenValue
	capValue                           [capValueFilterCount]uint64
	rtStats                            responseTimeStats
	lastValueExp, capValueLastUpdate   mclock.AbsTime

	// tokenMirror tracks service token balance according to the costs and
	// constants published by the server and is expected to closely match the
	// balance values reported by the server.
	// if rvFactor is updated then balance * rvFactor is expected to not change.
	// Request value of unexpected increases is added to freeTotal, decreases
	// are counted as failed promise value.
	tokenMirror, tokensExpected                  uint64
	tokenMirrorExpRate                           float64
	tokenMirrorExpAllowed                        bool
	tokenMirrorLastUpdate, tokensExpectedTimeout mclock.AbsTime

	// accessed directly by globalValueTracker
	lastCostList  RequestCostList
	lastCapFactor float64
}

type valueTrackerEnc struct {
	BufferPeak                         [capValueFilterCount]uint64
	PaidTotal, FreeTotal, FreeCredited uint64
	Delivered, Expired, Failed         uint64
	CapValue                           [capValueFilterCount]uint64
	RtStats                            responseTimeStats
	SavedAt                            uint64
	TokenMirror, TokensExpected        uint64
	TokenMirrorExpRate                 uint64
	LastCostList                       RequestCostList
	LastCapFactor                      uint64
}

// EncodeRLP implements rlp.Encoder
func (vt *valueTracker) EncodeRLP(w io.Writer) error {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	version := uint(vtVersion)
	if err := rlp.Encode(w, &version); err != nil {
		return err
	}
	now := mclock.Now()
	vt.update(now)
	vt.checkTokensExpected(now)
	vt.updateCapValue(now)
	dt := time.Duration(now - vt.lastValueExp)
	if dt < 0 {
		dt = 0
	}
	// we expire values even if dt==0 because it calls moveToValue
	vt.expireValues(dt)

	vte := valueTrackerEnc{
		PaidTotal:          vt.paidTotal.reqValue,
		FreeTotal:          vt.freeTotal.reqValue,
		FreeCredited:       vt.freeCredited.reqValue,
		Delivered:          vt.delivered.reqValue,
		Expired:            vt.expired.reqValue,
		Failed:             vt.failed.reqValue,
		CapValue:           vt.capValue,
		RtStats:            vt.rtStats,
		SavedAt:            uint64(time.Now().Unix()),
		TokenMirror:        vt.tokenMirror,
		TokensExpected:     vt.tokensExpected,
		TokenMirrorExpRate: math.Float64bits(vt.tokenMirrorExpRate),
		LastCostList:       vt.lastCostList,
		LastCapFactor:      math.Float64bits(vt.lastCapFactor),
	}
	for i, v := range vt.bufferPeak {
		vte.BufferPeak[i] = math.Float64bits(v)
	}
	return rlp.Encode(w, &vte)
}

// DecodeRLP implements rlp.Decoder
func (vt *valueTracker) DecodeRLP(s *rlp.Stream) error {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	var version uint
	if err := s.Decode(&version); err != nil {
		return err
	}
	if version != vtVersion {
		return fmt.Errorf("Unknown valueTracker version %d (current version is %d)", version, vtVersion)
	}

	var vte valueTrackerEnc
	if err := s.Decode(&vte); err != nil {
		return err
	}
	now := mclock.Now()
	for i, v := range vte.BufferPeak {
		vt.bufferPeak[i] = math.Float64frombits(v)
	}
	vt.bufferPeakLastExp = now
	vt.paidTotal.reqValue = vte.PaidTotal
	vt.freeTotal.reqValue = vte.FreeTotal
	vt.freeCredited.reqValue = vte.FreeCredited
	vt.delivered.reqValue = vte.Delivered
	vt.expired.reqValue = vte.Expired
	vt.failed.reqValue = vte.Failed
	vt.capValue = vte.CapValue
	vt.rtStats = vte.RtStats
	unixNow := uint64(time.Now().Unix())
	if unixNow > vte.SavedAt {
		dt := time.Second * time.Duration(unixNow-vte.SavedAt)
		vt.expireValues(dt)
	}
	vt.lastValueExp = now
	vt.capValueLastUpdate = now
	vt.tokenMirror = vte.TokenMirror
	vt.tokensExpected = vte.TokensExpected
	vt.tokenMirrorExpRate = math.Float64frombits(vte.TokenMirrorExpRate)
	vt.tokensExpectedTimeout = now
	vt.lastCostList = vte.LastCostList
	vt.lastCapFactor = math.Float64frombits(vte.LastCapFactor)
	vt.capFactor = vt.lastCapFactor
	return nil
}

func (vt *valueTracker) capValueAt(expRT time.Duration) float64 {
	i := cvfIndex(expRT)
	if i < 0 {
		i = 0
	}
	if i > capValueFilterCount-1 {
		i = capValueFilterCount - 1
	}
	index := int(i)
	subPos := i - float64(index)
	vi := float64(vt.capValue[index])
	var vd float64
	if index < capValueFilterCount-1 {
		vd = float64(vt.capValue[index+1]) - vi
	}
	return vi + vd*subPos
}

func (vt *valueTracker) periodicUpdate() {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	now := mclock.Now()
	vt.updateCapValue(now)
	dt := time.Duration(now - vt.lastValueExp)
	if dt >= valueExpPeriod {
		vt.expireValues(dt)
		vt.lastValueExp = now
	}
}

func (vt *valueTracker) totalValue(expRT time.Duration) float64 {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	return vt.totalValueLocked(expRT)
}

func (vt *valueTracker) totalValueLocked(expRT time.Duration) float64 {
	now := mclock.Now()
	vt.update(now)
	vt.checkTokensExpected(now)
	vt.updateCapValue(now)

	tv := float64(vt.delivered.value(vt.rvFactor))*vt.rtStats.valueFactor(expRT) + vt.capValueAt(expRT) - float64(vt.failed.value(vt.rvFactor))
	if tv < 0 {
		return 0
	}
	return tv
}

func (vt *valueTracker) maxPurchase(expRT time.Duration) uint64 {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	return uint64(vt.totalValueLocked(expRT) / vt.rvFactor)
}

func (vt *valueTracker) expectedTokenValueFactor(expRT time.Duration, buyAmount uint64) (expValue float64, paid func()) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	totalValue := vt.totalValue(expRT) // evaluate first to update values
	tokensSpent := float64(vt.freeTotal.value(vt.rvFactor)+vt.paidTotal.value(vt.rvFactor))/vt.rvFactor - float64(vt.tokenMirror)
	if tokensSpent < 1 || buyAmount < 1 {
		return 0, nil
	}
	// calculate average token value factor for all previous tokens (purchased and received for free)
	avgFactor := totalValue / tokensSpent
	// give credit for a limited amount of free service in order to estimate service received after
	// the potential token purchase
	var (
		freeCreditRatio    float64
		freeCreditReqValue uint64
	)
	ft, fc := vt.freeTotal.value(vt.rvFactor), vt.freeCredited.value(vt.rvFactor)
	if ft > fc {
		buyReqValue := float64(buyAmount) * vt.rvFactor
		freeCreditReqValue = ft - fc
		freeCreditRatio = float64(freeCreditReqValue) / buyReqValue
		if freeCreditRatio > 1 {
			freeCreditRatio = 2 - 1/freeCreditRatio
			freeCreditReqValue = uint64(freeCreditRatio * buyReqValue)
		}
	}
	oldRvFactor := vt.rvFactor
	return avgFactor * float64(buyAmount) * (1 + freeCreditRatio), func() {
		// offer accepted, expect tokens
		if oldRvFactor != vt.rvFactor {
			buyAmount = uint64(float64(buyAmount) * oldRvFactor / vt.rvFactor)
		}
		vt.paidTotal.tokens += buyAmount
		vt.tokensExpected += buyAmount
		vt.tokensExpectedTimeout = mclock.Now() + mclock.AbsTime(tokensExpectedTimeout)
		vt.freeCredited.reqValue += freeCreditReqValue
	}
}

func (vt *valueTracker) expireValues(dt time.Duration) {
	exp := -math.Expm1(-float64(dt) * valueExpTC)
	vt.expireValue(&vt.paidTotal, exp)
	vt.expireValue(&vt.freeTotal, exp)
	vt.expireValue(&vt.freeCredited, exp)
	vt.expireValue(&vt.delivered, exp)
	vt.expireValue(&vt.expired, exp)
	vt.expireValue(&vt.failed, exp)
	for i, cv := range vt.capValue[:] {
		vt.capValue[i] = cv - uint64(float64(cv)*exp)
	}
	vt.rtStats.expire(exp)
}

func (vt *valueTracker) expireValue(tv *tokenValue, exp float64) {
	tv.moveToValue(vt.rvFactor)
	tv.reqValue -= uint64(float64(tv.reqValue) * exp)
}

func (vt *valueTracker) setExpRate(expRate float64) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	now := mclock.Now()
	vt.update(now)
	vt.checkTokensExpected(now)
	vt.tokenMirrorExpRate = expRate
}

func (vt *valueTracker) setCapacity(capacity uint64) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	now := mclock.Now()
	vt.update(now)
	vt.updateCapValue(now)
	vt.capacity = capacity
	vt.tokenMirrorExpAllowed = capacity != 0
}

// assumes that cvf/rvf are not extremely large or small
func (vt *valueTracker) setFactors(cvf, rvf, capFactor float64, external bool) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	now := mclock.Now()
	if capFactor != vt.capFactor {
		vt.update(now)
		vt.updateCapValue(now)
		vt.capFactor = capFactor
	}

	if cvf < vt.cvFactor && external {
		vt.updateCapValue(mclock.Now())
		scaleDown := cvf / vt.cvFactor
		for i, v := range vt.bufferPeak[:] {
			vt.bufferPeak[i] = v * scaleDown
		}
	}
	vt.cvFactor = cvf

	if rvf < vt.rvFactor && external {
		// raise token expectations if token unit value drops
		// do not lower them if token value raises; discourage significant changes
		// to unit value while there are unfulfilled promises nominated in it
		vt.tokenMirror = uint64(float64(vt.tokenMirror) * vt.rvFactor / rvf)
		vt.tokensExpected = uint64(float64(vt.tokensExpected) * vt.rvFactor / rvf)
	}
	vt.paidTotal.moveToValue(vt.rvFactor)
	vt.freeTotal.moveToValue(vt.rvFactor)
	vt.freeCredited.moveToValue(vt.rvFactor)
	vt.delivered.moveToValue(vt.rvFactor)
	vt.expired.moveToValue(vt.rvFactor)
	vt.failed.moveToValue(vt.rvFactor)
	vt.rvFactor = rvf
}

// should be called before updating rvFactor
func (vt *valueTracker) recentBasket() (requestBasket, basketItem) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	vt.update(mclock.Now())
	b := vt.basket
	vt.basket = nil
	if b == nil {
		b = make(requestBasket)
	}
	for rt, item := range b {
		item.first.value = uint64(float64(item.first.value) * vt.rvFactor)
		item.rest.value = uint64(float64(item.rest.value) * vt.rvFactor)
		b[rt] = item
	}
	rcr := vt.capacityUsed
	vt.capacityUsed = basketItem{}
	rcr.value = uint64(float64(rcr.value) * vt.rvFactor)
	return b, rcr
}

type vtRequestInfo struct {
	at                                     mclock.AbsTime
	maxCost, realCost, balance, bufMissing uint64
	respTime                               time.Duration
}

// assumes that realCost <= maxCost
func (vt *valueTracker) addRequest(reqType, reqAmount uint32, baseCost, reqCost, realCost, balance, bufMissing uint64, respTime time.Duration) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	now := mclock.Now()
	if vt.basket == nil {
		vt.basket = make(requestBasket)
	}
	vt.basket.addRequest(reqType, reqAmount, baseCost+reqCost, reqCost)
	vt.checkProcessRequestQueue(now)
	if len(vt.reqQueue) >= vtRequestQueueLimit {
		dt := (now - vt.reqBurstStart) / vtRequestQueueLimit
		vt.reqBurstStart += dt
		vt.updateRespTimeStats(now, &vt.reqQueue[0], float64(dt)/float64(vtRequestBurstGap))
		vt.reqQueue = vt.reqQueue[1:]
	}
	req := vtRequestInfo{
		at:         now,
		maxCost:    baseCost + uint64(reqAmount)*reqCost,
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

func (vt *valueTracker) checkProcessRequestQueue(now mclock.AbsTime) {
	if len(vt.reqQueue) != 0 && time.Duration(now-vt.reqQueue[len(vt.reqQueue)-1].at) >= vtRequestBurstGap {
		dt := time.Duration(vt.reqQueue[len(vt.reqQueue)-1].at-vt.reqBurstStart) + vtRequestBurstGap
		rtWeight := float64(dt) / float64(vtRequestBurstGap*time.Duration(len(vt.reqQueue)))
		for i, _ := range vt.reqQueue {
			vt.updateRespTimeStats(now, &vt.reqQueue[i], rtWeight)
		}
		vt.reqQueue = nil
	}
}
func (vt *valueTracker) checkTokensExpected(now mclock.AbsTime) {
	if vt.tokensExpected != 0 && now >= vt.tokensExpectedTimeout {
		vt.failed.tokens += vt.tokensExpected
		vt.tokensExpected = 0
	}
}

func (vt *valueTracker) update(now mclock.AbsTime) {
	dt := now - vt.tokenMirrorLastUpdate
	vt.tokenMirrorLastUpdate = now
	if dt <= 0 {
		return
	}
	var sub uint64
	dtf := float64(dt)
	if vt.capacity != 0 {
		c := float64(vt.capacity) / 1000000 * dtf
		capCost := uint64(c * vt.capFactor)
		vt.capacityUsed.amount += uint64(c)
		vt.capacityUsed.value += capCost
		vt.delivered.tokens += capCost
		sub = capCost
	}
	if vt.tokenMirrorExpAllowed {
		expCost := uint64(-float64(vt.tokenMirror) * math.Expm1(-dtf*vt.tokenMirrorExpRate))
		vt.expired.tokens += expCost
		sub += expCost
	}

	if vt.tokenMirror >= sub {
		vt.tokenMirror -= sub
	} else {
		vt.freeTotal.tokens += sub - vt.tokenMirror
		vt.tokenMirror = 0
	}
}

func (vt *valueTracker) updateTokenMirror(req *vtRequestInfo) {
	// update token mirror
	vt.update(req.at)
	vt.freeTotal.tokens += req.maxCost - req.realCost
	if vt.tokenMirror >= req.realCost {
		vt.tokenMirror -= req.realCost
	} else {
		vt.freeTotal.tokens += req.realCost - vt.tokenMirror
		vt.tokenMirror = 0
	}
	tolerance := req.balance / 1000
	minTm := req.balance - tolerance
	maxTm := req.balance + tolerance
	if vt.tokensExpected > 0 && req.balance > vt.tokenMirror {
		diff := req.balance - vt.tokenMirror
		if vt.tokensExpected > diff {
			vt.tokensExpected -= diff
			vt.tokenMirror += diff
		} else {
			vt.tokenMirror += vt.tokensExpected
			vt.tokensExpected = 0
		}
	}
	if vt.tokenMirror > maxTm {
		vt.failed.tokens += vt.tokenMirror - maxTm
		vt.tokenMirror = maxTm
	}
	if vt.tokenMirror < minTm {
		vt.freeTotal.tokens += minTm - vt.tokenMirror
		vt.tokenMirror = minTm
	}
	vt.delivered.tokens += req.maxCost
	vt.checkTokensExpected(req.at)
}

// call every 10s
// call when updating: cvFactor, capacity
func (vt *valueTracker) updateCapValue(now mclock.AbsTime) {
	dtc := float64(now - vt.capValueLastUpdate)
	vt.capValueLastUpdate = now
	if dtc < float64(time.Second) {
		return
	}
	dtb := float64(now - vt.bufferPeakLastExp)
	if dtb < 0 {
		dtb = 0
	}
	bpMul := math.Exp((dtc/2-dtb)*bufferPeakTC) * 2 * dtc
	maxValue := uint64(float64(vt.capacity) * bufLimitRatio * vt.cvFactor * dtc)

	for i, bp := range vt.bufferPeak[:] {
		value := uint64(bp * bpMul)
		if value > maxValue {
			value = maxValue
		}
		vt.capValue[i] += value
	}
}

func (vt *valueTracker) updateRespTimeStats(now mclock.AbsTime, req *vtRequestInfo, rtWeight float64) {
	// update response time stats
	vt.rtStats.add(req.respTime, rtWeight)
	// update capValue filters
	dt := now - vt.bufferPeakLastExp
	if dt < 0 {
		dt = 0
		vt.bufferPeakLastExp = now
	}
	expCorr := math.Exp(float64(dt) * bufferPeakTC)
	if expCorr > 100 {
		for i, v := range vt.bufferPeak[:] {
			vt.bufferPeak[i] = v / expCorr
		}
		expCorr = 1
		vt.bufferPeakLastExp = now
	}
	bm := float64(req.bufMissing) * vt.cvFactor * expCorr

	for i, expRT := range cvfExpRTs[:] {
		bp := vt.bufferPeak[i]
		rtFactor := 1 - float64(req.respTime)/expRT
		if rtFactor < -1 {
			rtFactor = -1
		}
		neg := rtFactor < 0
		if neg == (bm < bp) {
			step := (bm - bp) * rtFactor * rtWeight
			oldBp := bp
			if neg {
				bp -= step
				if bp < 0 {
					bp = 0
				}
			} else {
				bp += step
			}
			if oldBp != bp {
				vt.updateCapValue(now)
				vt.bufferPeak[i] = bp
			}
		}
	}
}

const (
	minResponseTime = time.Millisecond * 50
	maxResponseTime = time.Second * 10
	timeStatLength  = 32
)

var timeStatsLogFactor = (timeStatLength - 1) / (math.Log(float64(maxResponseTime)/float64(minResponseTime)) + 1)

type responseTimeStats [timeStatLength]uint64

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
	w := weight * 0x1000000
	rt[i] += uint64(w * r1)
	if i < timeStatLength-1 {
		rt[i+1] += uint64(w * r)
	}
}

func (rt *responseTimeStats) valueFactor(expRT time.Duration) float64 {
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
	if sum == 0 {
		return 0
	}
	return v / float64(sum)
}

func (rt *responseTimeStats) expire(exp float64) {
	for i, s := range rt[:] {
		rt[i] = s - uint64(float64(s)*exp)
	}
}

type (
	requestBasket map[uint32]requestItem
	basketItem    struct {
		amount, value uint64
	}
	requestItem struct {
		first, rest basketItem
	}
	requestItemEnc struct {
		ReqType     uint32
		First, Rest basketItem
	}
)

// EncodeRLP implements rlp.Encoder
func (rb *requestBasket) EncodeRLP(w io.Writer) error {
	list := make([]requestItemEnc, 0, len(*rb))
	for rt, i := range *rb {
		list = append(list, requestItemEnc{rt, i.first, i.rest})
	}
	return rlp.Encode(w, list)
}

// DecodeRLP implements rlp.Decoder
func (rb *requestBasket) DecodeRLP(s *rlp.Stream) error {
	var list []requestItemEnc
	if err := s.Decode(&list); err != nil {
		return err
	}
	*rb = make(requestBasket)
	for _, item := range list {
		(*rb)[item.ReqType] = requestItem{item.First, item.Rest}
	}
	return nil
}

// EncodeRLP implements rlp.Encoder
func (b *basketItem) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{b.amount, b.value})
}

// DecodeRLP implements rlp.Decoder
func (b *basketItem) DecodeRLP(s *rlp.Stream) error {
	var item struct {
		Amount, Value uint64
	}
	if err := s.Decode(&item); err != nil {
		return err
	}
	b.amount, b.value = item.Amount, item.Value
	return nil
}

func (rb requestBasket) addRequest(reqType, reqAmount uint32, firstValue, restValue uint64) {
	a := rb[reqType]
	a.first.amount++
	a.first.value += firstValue
	if reqAmount > 1 {
		ra := uint64(reqAmount - 1)
		a.rest.amount += ra
		a.rest.value += ra * restValue
	}
	rb[reqType] = a
}

func (rb requestBasket) addBasket(ab requestBasket) {
	for reqType, aa := range ab {
		a := rb[reqType]
		a.first.addItem(aa.first)
		a.rest.addItem(aa.rest)
		rb[reqType] = a
	}
}

func (a *basketItem) addItem(b basketItem) {
	a.amount += b.amount
	a.value += b.value
}

type globalValueTracker struct {
	connected map[enode.ID]*valueTracker
	quit      chan chan struct{}
	lock      sync.Mutex
	db        ethdb.Database
	vtCache   *lru.Cache

	refBasket, newBasket      requestBasket
	refCapReq, newCapReq      basketItem
	refValueMul               float64
	lastReferenceBasketUpdate mclock.AbsTime
}

func newGlobalValueTracker(db ethdb.Database) *globalValueTracker {
	gv := &globalValueTracker{
		connected:                 make(map[enode.ID]*valueTracker),
		quit:                      make(chan chan struct{}),
		lastReferenceBasketUpdate: mclock.Now(),
	}
	go func() {
		for {
			select {
			case <-time.After(vtUpdatePeriod):
				gv.periodicUpdate()
			case quit := <-gv.quit:
				close(quit)
				return
			}
		}
	}()
	return gv
}

func (gv *globalValueTracker) stop() {
	quit := make(chan struct{})
	gv.quit <- quit
	<-quit
}

func (gv *globalValueTracker) register(id enode.ID) {
	gv.lock.Lock()
	defer gv.lock.Unlock()

	gv.connected[id] = gv.loadOrNew(id)
}

func (gv *globalValueTracker) unregister(id enode.ID) {
	gv.lock.Lock()
	defer gv.lock.Unlock()

	gv.save(id, gv.connected[id])
	delete(gv.connected, id)
}

func (gv *globalValueTracker) loadOrNew(id enode.ID) *valueTracker {
	key := append(vtKey, id[:]...)
	if item, ok := gv.vtCache.Get(string(key)); ok {
		return item.(*valueTracker)
	}
	vt := &valueTracker{}
	if enc, err := gv.db.Get(key); err == nil {
		if err := rlp.DecodeBytes(enc, vt); err == nil {
			cvf, rvf := gv.valueFactors(vt.lastCostList, vt.lastCapFactor)
			vt.setFactors(cvf, rvf, vt.lastCapFactor, false)
			return vt
		} else {
			log.Error("Failed to decode valueTracker", "err", err)
		}
	}
	return &valueTracker{}
}

func (gv *globalValueTracker) save(id enode.ID, vt *valueTracker) {
	key := append(vtKey, id[:]...)
	if enc, err := rlp.EncodeToBytes(vt); err == nil {
		gv.db.Put(key, enc)
	} else {
		log.Error("Failed to encode valueTracker", "err", err)
	}
	gv.vtCache.Add(string(key), vt)
}

// assumes that cost list contains all necessary request types
func (gv *globalValueTracker) valueFactors(costs RequestCostList, capFactor float64) (cvf, rvf float64) {
	var sum float64
	for _, req := range costs {
		if ref, ok := gv.refBasket[uint32(req.MsgCode)]; ok {
			sum += float64(req.BaseCost)*float64(ref.first.amount) + float64(req.ReqCost)*float64(ref.first.amount+ref.rest.amount)
		}
	}
	sum2 := sum + capFactor*float64(gv.refCapReq.amount)
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
	for _, vt := range gv.connected {
		rb, rcr := vt.recentBasket()
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
		return
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

	for _, vt := range gv.connected {
		cvf, rvf := gv.valueFactors(vt.lastCostList, vt.lastCapFactor)
		vt.setFactors(cvf, rvf, vt.lastCapFactor, false)
	}
}

func (gv *globalValueTracker) updateServerPrices(vt *valueTracker, costList RequestCostList, capFactor float64) {
	gv.lock.Lock()
	defer gv.lock.Unlock()

	rb, rcr := vt.recentBasket()
	gv.newBasket.addBasket(rb)
	gv.newCapReq.addItem(rcr)

	cvf, rvf := gv.valueFactors(costList, capFactor)
	vt.setFactors(cvf, rvf, capFactor, true)
	vt.lastCostList, vt.lastCapFactor = costList, capFactor
}

func (gv *globalValueTracker) periodicUpdate() {
	gv.lock.Lock()
	defer gv.lock.Unlock()

	now := mclock.Now()
	for _, vt := range gv.connected {
		vt.periodicUpdate()
	}
	if now > gv.lastReferenceBasketUpdate+mclock.AbsTime(refBasketUpdatePeriod) {
		gv.updateReferenceBasket()
		gv.lastReferenceBasketUpdate = now
	}
}
