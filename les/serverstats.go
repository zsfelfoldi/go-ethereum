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
	"crypto/ecdsa"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	srtoRecalcCount = 100
	minResponseTime = time.Millisecond * 50
	maxResponseTime = time.Second * 10
	timeStatsLength = 32
)

var timeStatsLogFactor = (timeStatsLength - 1) / (math.Log(float64(maxResponseTime)/float64(minResponseTime)) + 1)

type responseTimeStats struct {
	stats                     [timeStatLength]float64
	sum                       float64
	lastExpDecay              mclock.AbsTime
	expDecayRate, expDecayDiv float64
	recalcCounter             int
	peer                      *peer // nil if not connected
}

func (rt *responseTimeStats) EncodeRLP(w io.Writer) error {
	rt.expDecay(mclock.Now())
	var list [timeStatLength]uint64
	for i, v := range rt.stats[:] {
		list[i] = math.Float64bits(v / rt.expDecayDiv)
	}
	return rlp.Encode(w, list)
}

func (rt *responseTimeStats) DecodeRLP(st *rlp.Stream) error {
	var list [timeStatLength]uint64
	if err := st.Decode(&list); err != nil {
		return err
	}
	for i, v := range list {
		rt.stats[i] = math.Float64frombits(v)
	}
	rt.expDecayDiv = 1
	rt.lastExpDecay = mclock.Now()
	return nil
}

func (rt *responseTimeStats) expDecay(now mclock.AbsTime) {
	dt := now - rt.lastExpDecay
	if dt > 0 {
		rt.expDecayDiv *= math.Exp(dt * rt.expDecayRate)
	}
	if rt.expDecayDiv >= 256 {
		rt.expDecayDiv /= 256
		for i, v := range rt.stats {
			rt.stats[i] = v / 256
		}
	}
	rt.lastExpDecay = now
}

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

func (rt *responseTimeStats) add(respTime time.Duration, weight float64, global *responseTimeStats) {
	now := mclock.Now()
	rt.expDecay(now)
	global.expDecay(now)
	r := timeToStatScale(respTime)
	i := int(r)
	r -= float64(i)
	r1 := 1 - r
	wrt := weight * rt.expDecayDiv
	rt.stats[i] += wrt * r1
	rt.sum += wrt
	wglobal := weight * global.expDecayDiv
	global.stats[i] += wglobal * r1
	global.sum += wglobal
	if i < timeStatLength-1 {
		rt.stats[i+1] += wrt * r
		global.stats[i+1] += wglobal * r
	}

	if rt.peer != nil {
		if rt.recalcCounter >= srtoRecalcCount {
			rt.updateSoftRequestTimeout()
			rt.recalcCounter = 0
		}
	}
	global.recalcCounter++
}

func (rt *responseTimeStats) updateSoftRequestTimeout() {
	s := rt.sum * 0.1
	i := timeStatLength - 1
	for i > 0 && s >= rt.stats[i] {
		s -= rt.stats[i]
		i--
	}
	r := float64(i) + 0.5
	if rt.stats[i] > 1e-100 {
		r -= s / rt.stats[i]
	}
	srto := statScaleToTime(r) * 5 / 4
	if srto < minSoftRequestTimeout {
		srto = minSoftRequestTimeout
	}
	if srto > maxSoftRequestTimeout {
		srto = maxSoftRequestTimeout
	}
	atomic.StoreInt64((*int64)(&rt.peer.srto), int64(srto))
}

type statValueWeights [timeStatLength]float64

func (global *responseTimeStats) timePenalty(ratio float64) float64 {
	var (
		avgSum float64
	)
	sum := global.stats[0]
	lastw := -global.sum
	threshold := global.sum * ratio
	for i := 1; i < timeStatsLength; i++ {
		s := global.stats[i]
		t := statScaleToTime(float64(i))
		sum += s
		ft := float64(t)
		avgSum += s * ft
		w := 2*(sum-avgSum/ft) - global.sum
		if w > threshold {
			dw := w - lastw
			if dw < 1e-100 {
				return float64(i)
			}
			return float64(i) - (w-threshold)/dw
		}
		lastw = w
	}
	if avgSum < 1e-100 {
		return 0
	}
	return (1 - ratio) * sum / avgSum
}

func makeStatValueWeights(timePenalty float64) statValueWeights {
	var wt statValueWeights
	for i, _ := range w[:] {
		t := statScaleToTime(float64(i))
		w := 1 - float64(t)*timePenalty
		if w < -1 {
			w = -1
		}
		wt[i] = w
	}
	return wt
}

func (rt *responseTimeStats) value(weights *statValueWeights) float64 {
	rt.expDecay(mclock.Now())
	var v float64
	for i, s := range rt.stats[:] {
		v += s * weights[i]
	}
	if v > 0 {
		return v / rt.expDecayDiv
	}
	return 0
}

const (
	csVectorSize   = 20
	csMaxDecayRate = 1 / float64(time.Second*30)
	csMinDecayRate = csMaxDecayRate / float64(1<<(csVectorSize-1))
)

type (
	csVector     [csVectorSize]float64
	csVectorPair struct {
		weight, sum csVector
	}

	csFilterVector struct {
		vp           csVectorPair
		lastExpDecay mclock.AbsTime
	}
)

func (fv *csFilterVector) update(now mclock.AbsTime) {
	dt := now - rt.lastExpDecay
	rt.lastExpDecay = now
	if dt <= 0 {
		return
	}
	expm1 := math.Expm1(-dt * csMinDecayRate)
	for i := 0; i < csVectorSize; i++ {
		fv.vp.weight[i] += fv.vp.weight[i] * expm1
		fv.vp.sum[i] += fv.vp.sum[i] * expm1
		expm1 = expm1 * (expm1 + 2)
	}
}

func (fv *csFilterVector) get(now mclock.AbsTime) csVectorPair {
	fv.update(now)
	return vp
}

func (fv *csFilterVector) add(now mclock.AbsTime, value float64) {
	fv.update(now)
	for i := 0; i < csVectorSize; i++ {
		fv.vp.weight[i] += 1
		fv.vp.sum[i] += value
	}
}

const csDecayRate = 1 / float64(time.Day*100)

type connectionStats struct {
	average, weighted, scaled, wscaled    csVectorPair
	totalWeight, totalValue, totalSquared float64
	lastExpDecay, lastScaled              mclock.AbsTime
}

func (cs *connectionStats) update(now mclock.AbsTime, vp *csVectorPair, value, weight float64) {
	dt := now - rt.lastExpDecay
	rt.lastExpDecay = now
	var exp float64
	if dt > 0 {
		exp = math.Exp(-dt * csDecayRate)
	} else {
		exp = 1
	}
	value *= weight
	for i := 0; i < csVectorSize; i++ {
		cs.average.weight[i] = cs.average.weight[i]*exp + vp.weight[i]*weight
		cs.average.sum[i] = cs.average.sum[i]*exp + vp.sum[i]*weight
		if value != 0 {
			cs.weighted.weight[i] = cs.average.weight[i]*exp + vp.weight[i]*value
			cs.weighted.sum[i] = cs.average.sum[i]*exp + vp.sum[i]*value
		}
	}
	cs.totalWeight += weight
	cs.totalValue += value
	cs.totalSquared += value * value
}

func scaleVectorPair(target, source, weights *csVectorPair) {
	for i := 0; i < csVectorSize; i++ {
		target.weight[i] = source.weight[i] * weights.weight[i]
		target.sum[i] = source.sum[i] * weights.weight[i]
	}
}

func (cs *connectionStats) estimate(now mclock.AbsTime, vp *csVectorPair) float64 {
	var avg, wavg float64
	if (cs.totalWeight < 1e-100) || (cs.totalValue < 1e-100) {
		return 0
	}
	avg = cs.totalValue / cs.totalWeight
	wavg = cs.totalSquared / cs.totalValue

	if cs.lastScaled != now {
		scaleVectorPair(&cs.scaled, &cs.average, &cs.weighted)
		scaleVectorPair(&cs.wscaled, &cs.weighted, &cs.average)
		lastScaled = now
	}
	var scaled, wscaled, v csVectorPair
	scaleVectorPair(&scaled, &cs.scaled, vp)
	scaleVectorPair(&wscaled, &cs.wscaled, vp)
	scaleVectorPair(&v, &vp, &cs.scaled)

	var dotProduct, sqLen, x float64
	for i := 0; i < csVectorSize; i++ {
		var scale float64
		if v.weight[i] > 1e-100 {
			w := math.Cbrt(v.weight[i])
			scale = 1 / (w * w)
		}

		wv := (wscaled.sum[i] - scaled.sum[i]) * scale
		dotProduct += (v.sum[i] - scaled.sum[i]) * scale * wv
		sqLen += wv * wv
	}
	if sqLen > 1e-100 {
		if dotProduct < -1000*sqLen {
			x = -1000
		} else {

			if dotProduct > 1000*sqLen {
				x = 1000
			} else {
				x = dotProduct / sqLen
			}
		}
	}
	return x*(wavg-avg) + avg
}

type connectionPredictor struct {
	history                   *csFilterVector
	nodeStats, globalStats    *connectionStats
	lastVp                    csVectorPair
	estNode, estGlobal, ratio float64
}

func (cp *connectionPredictor) predict() float64 {
	now := mclock.Now()
	cp.lastVp = cp.history.get(now)
	cp.estNode = cp.nodeStats.estimate(now, &cp.lastVp)
	cp.estGlobal = cp.globalStats.estimate(now, &cp.lastVp)
	return cp.estNode*cp.ratio + cp.estGlobal*(1-cp.ratio)
}

func (cp *connectionPredictor) update(value, weight float64) {
	now := mclock.Now()
	cp.nodeStats.update(now, &cp.lastVp, value, weight)
	cp.globalStats.update(now, &cp.lastVp, value, weight)
	errNode = cp.estNode - value
	errNode *= errNode
	errGlobal = cp.estGlobal - value
	errGlobal *= errGlobal
	if errNode+errGlobal < 1e-100 {
		return
	}
	step := errGlobal*2/(errNode+errGlobal) - 1
	cp.ratio += step * 0.1 * weight
	if cp.ratio < 0 {
		cp.ratio = 0
	}
	if cp.ratio > 1 {
		cp.ratio = 1
	}
}
