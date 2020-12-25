// Copyright 2021 The go-ethereum Authors
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
	"container/list"
	"math"
	//	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

var (
	futureTimeScale  = newFloatScale(4*16, 17*16, 16)      // second
	historyTimeScale = newFloatScale(4*16, 21*16, 16)      // second
	requestRateScale = newFloatScale(-4*4, 20*4, 4)        // reqValue/s
	historyRateScale = newFloatScale(-4*16-3, 20*16+3, 16) // reqValue/s, 4x oversampled
	relValueScale    = newFloatScale(0, -10*16, 16)        // dimensionless
	tokenValueScale  = newFloatScale(3*16, 19*16, 16)      // reqValue
)

type floatScale struct {
	baseMap  map[float64]int
	baseList []float64
}

func newFloatScale(first, last, res int) *floatScale {
	fs := &floatScale{
		baseMap:  make(map[float64]int),
		baseList: make([]float64, last+1-first),
	}
	for i := range fs.baseList {
		v := math.Pow(2, float64(first+i)/float64(res))
		fs.baseMap[v] = i
		fs.baseList[i] = v
	}
	return fs
}

func (fs *floatScale) basePoint(f float64) int {
	if i, ok := fs.baseMap[f]; ok {
		return i
	}
	return -1
}

func (fs *floatScale) neighbors(f float64) (left, right, r float64) {
	min, max := 0, len(fs.baseList)-1
	for max > min+1 {
		mid := (min + max) / 2
		if f >= fs.baseList[mid] {
			min = mid
		} else {
			max = mid
		}
	}
	left, right = fs.baseList[min], fs.baseList[max]
	r = (f - left) / (right - left)
	return
}

func (fs *floatScale) inverse(errFn func(float64) float64) float64 {
	min, max := 0, len(fs.baseList)-1
	for max > min+1 {
		mid := (min + max) / 2
		if err := errFn(fs.baseList[mid]); err >= 0 {
			if err == 0 {
				return fs.baseList[mid]
			}
			min = mid
		} else {
			max = mid
		}
	}
	left, right := fs.baseList[min], fs.baseList[max]
	leftErr, rightErr := errFn(left), errFn(right) // one of them is calculated twice but the calculation is cached
	if leftErr <= 0 {
		return left
	}
	if rightErr >= 0 {
		return right
	}
	return left + (right-left)*leftErr/(leftErr-rightErr)
}

type tickData struct {
	index uint64
}

type clientModel struct {
	reqValue *utils.MirrorCurve // single curve: qcValue vs. system time
	peakTime *utils.MirrorCurve // multi curve: peakTime vs. system time per rate bucket
	servers  map[enode.ID]*serverModel
	events   chan cmEvent
}

type serverModel struct {
	qcValue                      *utils.MirrorCurve // 0: reqValue vs. tokenValue  1: qcValue vs. reqValue
	peakHistory                  *utils.MirrorCurve // 2*rateBucket: peakQcValue  2*rateBucket+1: peakCapCost
	reqCostFactor, capCostFactor float64
}

type cmEvent struct {
	kind int
	data interface{}
}

const (
	cmeRegister = iota
	cmeUnregister
	cmeRequest
	cmePeak
)

//----------------------------------------

type clientSnapshot struct {
	reqValue         *utils.MirrorCurveSnapshot
	rateTime         map[int]*utils.MirrorCurveSnapshot
	minRate, maxRate int
	servers          map[enode.ID]*serverSnapshot
}

type serverSnapshot struct {
	reqValue, qcValue       *utils.MirrorCurveSnapshot
	peakHistory             map[int]*utils.MirrorCurveSnapshot // 2*rateBucket: peakQcValue  2*rateBucket+1: peakCapCost
	tokenValue, minCapValue float64
}

func (cs *clientSnapshot) futureTime() float64 {
	return futureTimeScale.inverse(func(futureTime float64) float64 {
		var sumRelValue float64
		for _, node := range cs.servers {
			sumRelValue += node.relValue(cs, futureTime)
		}
		return sumRelValue - 1
	})
}

type rateCurve struct {
	points    []float64
	firstRate uint
}

const rateCurveOversampling = 4

// l..r-1
func (rc *rateCurve) osRange() (uint, uint) {
	return rc.firstRate * rateCurveOversampling, (rc.firstRate+uint(len(rc.points))+1)*rateCurveOversampling - 1
}

func (rc *rateCurve) osValue(pos uint) float64 {
	p := (pos + 1) / rateCurveOversampling
	if p < rc.firstRate {
		return 0
	}
	r := (pos + 1) % rateCurveOversampling
	var v1, v2 float64
	end := rc.firstRate + uint(len(rc.points))
	if p < end {
		v2 = rc.points[p] / rateCurveOversampling
	}
	if r == 0 {
		return v2
	}
	if p > rc.firstRate && p+1 < end {
		v1 = rc.points[p-1] / rateCurveOversampling
	}
	return v1 + (v2-v1)*float64(r)/rateCurveOversampling
}

type cvPredictDemand struct {
	reqValue, peakValue float64
	rateTime            rateCurve
}

// futureTime bp
func (cs *clientSnapshot) predictDemand(futureTime float64) (reqValue, peakValue float64, rateTime rateCurve) {
	if pd, ok := cs.cachedPredictDemand[futureTime]; ok {
		return pd.reqValue, pd.peakValue, pd.rateTime
	}

	reqValue = utils.PwcValue(cs.reqValue, futureTime)
	rateTime = rateCurve{
		points:    make([]float64, cs.maxRate+1-cs.minRate),
		firstRate: cs.minRate,
	}
	for i, curve := range cs.rateTime {
		t := utils.PwcValue(curve, futureTime)
		rateTime.points[i] = t
		peakValue += t * requestRateScale.baseList[cs.minRate+i]
	}

	cs.cachedPredictDemand[futureTime] = cvPredictDemand{reqValue, peakValue, rateTime}
	return
}

// futureTime bp
func (ss *serverSnapshot) relValue(cs *clientSnapshot, futureTime float64) float64 {
	if rv, ok := ss.cacheRelValue[futureTime]; ok {
		return rv
	}

	reqValue, _, _ := cs.predictDemand(futureTime)
	relValue := relValueScale.inverse(func(relValue float64) float64 {
		peakTime, peakCost := ss.peakTimeCost(cs, futureTime, relValue)
		capValue := peakCost * targetPeakTimeRatio
		peakRatio := peakTime * targetPeakTimeRatio / futureTime
		if peakRatio > 0.01 {
			m := -math.Expm1(-1 / peakRatio)
			peakRatio *= m
			capValue *= m
		}
		capValue += (1 - peakRatio) * futureTime * ss.minCapValue
		qcValue := reqValue * relValue
		spentValue := tokenValueScale.inverse(func(spentValue float64) float64 {
			return qcValue - ss.qcValue(spentValue)
		})
		return ss.tokenValue - spentValue - capValue
	})

	ss.cacheRelValue[futureTime] = relValue
	return relValue
}

// spentValue bp
func (ss *serverSnapshot) qcValue(spentValue float64) {
	if tokenValue < 1e-10 {
		return 0
	}
	if qv, ok := ss.cacheQcValue[spentValue]; ok {
		return qv
	}

	reqValue := utils.PwcValue(ss.reqValue, tokenValue)
	reqValue = tokenValue * utils.InvLinHyper(reqValue/tokenValue)
	qcValue := utils.PwcValue(ss.qcValue, reqValue)

	ss.cacheQcValue[spentValue] = qcValue
	return qcValue
}

type (
	ckPeakTimeCost struct{ futureTime, relValue float64 }
	cvPeakTimeCost struct{ peakTime, peakCost float64 }
)

// futureTime, relValue bp
func (ss *serverSnapshot) peakTimeCost(cs *clientSnapshot, futureTime, relValue float64) (float64, float64) {
	cacheKey := ckPeakTimeCost{futureTime, relValue}
	if c, ok := ss.cachePeakTimeCost[cacheKey]; ok {
		return c.time, c.cost
	}

	_, peakValue, _ := cs.predictDemand(futureTime)
	if peakValue < 1e-10 {
		return 0
	}
	historyTime := historyTimeScale.inverse(func(historyTime float64) float64 {
		_, matchedValue, _ := ss.rateValueMatch(cs, futureTime, historyTime, relValue)
		return 0.95 - matchedValue/peakValue
	})
	matchedTime, matchedValue, matchedCost := ss.rateValueMatch(gp, futureTime, historyTime, relValue)
	var r float64
	if peakValue < 1000*matchedValue {
		r = peakValue / matchedValue
	} else {
		r = 1000
	}
	//TODO peakTime-ot kell r-rel szorozni? merre jobb tevedni?
	peakCost := matchedCost * r * r

	ss.cachePeakTimeCost[cacheKey] = cvPeakTimeCost{peakTime, peakCost}
	return peakTime, peakCost
}

type cvPeakValueCost struct{ rateValue, rateCost rateCurve }

// historyTime bp
func (ss *serverSnapshot) peakValueCost(historyTime float64) (rateValue, rateCost rateCurve) {
	if p, ok := ss.cachePeakHistory[historyTime]; ok {
		return p.rateValue, p.rateCost
	}

	rateValue = rateCurve{
		points:    make([]float64, ss.maxRate+1-ss.minRate),
		firstRate: ss.minRate,
	}
	rateCost = rateCurve{
		points:    make([]float64, ss.maxRate+1-ss.minRate),
		firstRate: ss.minRate,
	}
	for i, curve := range ss.peakHistory {
		v := utils.PwcValue(curve, historyTime)
		if i&1 == 0 {
			rateValue.points[i/2] = v
		} else {
			rateCost.points[i/2] = v
		}
	}

	ss.cachePeakHistory[historyTime] = cvPeakValueCost{rateValue, rateCost}
	return
}

type (
	ckRateValueMatch struct{ futureTime, historyTime, relValue float64 }
	cvRateValueMatch struct{ time, value, cost float64 }
)

// futureTime, relValue bp, historyTime ip
func (ss *serverSnapshot) rateValueMatch(cs *clientSnapshot, futureTime, historyTime, relValue float64) (matchedTime, matchedValue, matchedCost float64) {
	if relValue == 0 {
		return 0
	}
	cacheKey := ckRateValueMatch{futureTime, historyTime, relValue}
	if c, ok := ss.cacheRateValueMatch[cacheKey]; ok {
		return c.time, c.value, c.cost
	}

	historyIndex := historyTimeScale.basePoint(historyTime)
	if historyIndex == -1 {
		lo, hi, r := historyTimeScale.neighbors(historyTime)
		lt, lv, lc := ss.rateValueMatch(cs, futureTime, lo, relValue)
		ht, hv, hc := ss.rateValueMatch(cs, futureTime, hi, relValue)
		return lt + (ht-lt)*r, lv + (hv-lv)*r, lc + (hc-lc)*r
	}

	offset := relValueScale.basePoint(relValue)
	_, _, rateTime := cs.predictDemand(futureTime)
	rtBegin, rtEnd := rateTime.osRange()
	value, cost := ss.peakValueCost(historyTime)
	vcBegin, vcEnd := value.osRange()

	cptr := vcBegin
	cvalue, ccost := value.osValue(cptr), cost.osValue(cptr)

	for dptr := rtBegin; dptr < rtEnd; dptr++ {
		dtime := rateTime.osValue(dptr)
		if dtime == 0 {
			continue
		}
		if dptr > cptr+offset {
			cptr = dptr - offset
			if cptr >= vcEnd {
				return
			}
			cvalue, ccost = value.osValue(cptr), cost.osValue(cptr)
		}
		dvalue := dtime * historyRateScale.baseList[dptr-offset]
		for dvalue > 0 {
			if dvalue >= cvalue {
				matchedTime += cvalue / historyRateScale.baseList[cptr]
				matchedValue += cvalue
				matchedCost += ccost
				dvalue -= cvalue
				cptr++
				if cptr >= vcEnd {
					return
				}
				cvalue, ccost = value.osValue(cptr), cost.osValue(cptr)
			} else {
				mc := ccost * dvalue / cvalue
				matchedTime += dvalue / historyRateScale.baseList[cptr]
				matchedValue += dvalue
				matchedCost += mc
				cvalue -= dvalue
				ccost -= mc
				dvalue = 0
			}
		}
	}

	ss.cacheRateValueMatch[cacheKey] = cvRateValueMatch{matchedTime, matchedValue, matchedCost}
	return
}
