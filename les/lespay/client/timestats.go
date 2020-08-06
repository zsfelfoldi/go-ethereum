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
	"time"

	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	minResponseTime   = time.Millisecond * 50
	maxResponseTime   = time.Second * 10
	timeStatLength    = 32
	weightScaleFactor = 1000000
)

// ResponseTimeStats is the response time distribution of a set of answered requests,
// weighted with request value, either served by a single server or aggregated for
// multiple servers.
// It it a fixed length (timeStatLength) distribution vector with linear interpolation.
// The X axis (the time values) are not linear, they should be transformed with
// TimeToStatScale and StatScaleToTime.
type (
	ResponseTimeStats struct {
		stats [timeStatLength]uint64
		exp   uint64
	}
	ResponseTimeWeights [timeStatLength]float64
)

var timeStatsLogFactor = (timeStatLength - 1) / (math.Log(float64(maxResponseTime)/float64(minResponseTime)) + 1)

// TimeToStatScale converts a response time to a distribution vector index. The index
// is represented by a float64 so that linear interpolation can be applied.
func TimeToStatScale(d time.Duration) float64 {
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

// StatScaleToTime converts a distribution vector index to a response time. The index
// is represented by a float64 so that linear interpolation can be applied.
func StatScaleToTime(r float64) time.Duration {
	r /= timeStatsLogFactor
	if r > 1 {
		r = math.Exp(r - 1)
	}
	return time.Duration(r * float64(minResponseTime))
}

// TimeoutWeights calculates the weight function used for calculating service value
// based on the response time distribution of the received service.
// It is based on the request timeout value of the system. It consists of a half cosine
// function starting with 1, crossing zero at timeout and reaching -1 at 2*timeout.
// After 2*timeout the weight is constant -1.
func TimeoutWeights(timeout time.Duration) (res ResponseTimeWeights) {
	for i := range res {
		t := StatScaleToTime(float64(i))
		if t < 2*timeout {
			res[i] = math.Cos(math.Pi / 2 * float64(t) / float64(timeout))
		} else {
			res[i] = -1
		}
	}
	return
}

// EncodeRLP implements rlp.Encoder
func (rt *ResponseTimeStats) EncodeRLP(w io.Writer) error {
	enc := struct {
		Stats [timeStatLength]uint64
		Exp   uint64
	}{rt.stats, rt.exp}
	return rlp.Encode(w, &enc)
}

// DecodeRLP implements rlp.Decoder
func (rt *ResponseTimeStats) DecodeRLP(s *rlp.Stream) error {
	var enc struct {
		Stats [timeStatLength]uint64
		Exp   uint64
	}
	if err := s.Decode(&enc); err != nil {
		return err
	}
	rt.stats, rt.exp = enc.Stats, enc.Exp
	return nil
}

// Add adds a new response time with the given weight to the distribution.
func (rt *ResponseTimeStats) Add(respTime time.Duration, weight float64, expFactor utils.ExpirationFactor) {
	rt.setExp(expFactor.Exp)
	weight *= expFactor.Factor * weightScaleFactor
	r := TimeToStatScale(respTime)
	i := int(r)
	r -= float64(i)
	rt.stats[i] += uint64(weight * (1 - r))
	if i < timeStatLength-1 {
		rt.stats[i+1] += uint64(weight * r)
	}
}

// setExp sets the power of 2 exponent of the structure, scaling base values (the vector
// itself) up or down if necessary.
func (rt *ResponseTimeStats) setExp(exp uint64) {
	if exp > rt.exp {
		shift := exp - rt.exp
		for i, v := range rt.stats {
			rt.stats[i] = v >> shift
		}
		rt.exp = exp
	}
	if exp < rt.exp {
		shift := rt.exp - exp
		for i, v := range rt.stats {
			rt.stats[i] = v << shift
		}
		rt.exp = exp
	}
}

// Value calculates the total service value based on the given distribution, using the
// specified weight function.
func (rt ResponseTimeStats) Value(weights ResponseTimeWeights, expFactor utils.ExpirationFactor) float64 {
	var v float64
	for i, s := range rt.stats {
		v += float64(s) * weights[i]
	}
	if v < 0 {
		return 0
	}
	return expFactor.Value(v, rt.exp) / weightScaleFactor
}

// AddStats adds the given ResponseTimeStats to the current one.
func (rt *ResponseTimeStats) AddStats(s *ResponseTimeStats) {
	rt.setExp(s.exp)
	for i, v := range s.stats {
		rt.stats[i] += v
	}
}

// SubStats subtracts the given ResponseTimeStats from the current one.
func (rt *ResponseTimeStats) SubStats(s *ResponseTimeStats) {
	rt.setExp(s.exp)
	for i, v := range s.stats {
		if v < rt.stats[i] {
			rt.stats[i] -= v
		} else {
			rt.stats[i] = 0
		}
	}
}

// Timeout suggests a timeout value based on the previous distribution. The parameter
// is the desired rate of timeouts assuming a similar distribution in the future.
// Note that the actual timeout should have a sensible minimum bound so that operating
// under ideal working conditions for a long time (for example, using a local server
// with very low response times) will not make it very hard for the system to accommodate
// longer response times in the future.
func (rt ResponseTimeStats) Timeout(failRatio float64) time.Duration {
	var sum uint64
	for _, v := range rt.stats {
		sum += v
	}
	s := uint64(float64(sum) * failRatio)
	i := timeStatLength - 1
	for i > 0 && s >= rt.stats[i] {
		s -= rt.stats[i]
		i--
	}
	r := float64(i) + 0.5
	if rt.stats[i] > 0 {
		r -= float64(s) / float64(rt.stats[i])
	}
	if r < 0 {
		r = 0
	}
	th := StatScaleToTime(r)
	if th > maxResponseTime {
		th = maxResponseTime
	}
	return th
}

// RtDistribution represents a distribution as a series of (X, Y) chart coordinates,
// where the X axis is the response time in seconds while the Y axis is the amount of
// service value received with a response time close to the X coordinate.
type RtDistribution [timeStatLength][2]float64

// Distribution returns a RtDistribution, optionally normalized to a sum of 1.
func (rt ResponseTimeStats) Distribution(normalized bool, expFactor utils.ExpirationFactor) (res RtDistribution) {
	var mul float64
	if normalized {
		var sum uint64
		for _, v := range rt.stats {
			sum += v
		}
		if sum > 0 {
			mul = 1 / float64(sum)
		}
	} else {
		mul = expFactor.Value(float64(1)/weightScaleFactor, rt.exp)
	}
	for i, v := range rt.stats {
		res[i][0] = float64(StatScaleToTime(float64(i))) / float64(time.Second)
		res[i][1] = float64(v) * mul
	}
	return
}

type diffExpFilter struct {
	timeConst, expSum, diffSum float64
}

func (df *diffExpFilter) add(value, dt float64) {
	e := math.Expm1(dt / qf.timeConst)
	df.expSum += value
	df.diffSum += df.diffSum*e - dt/(qf.timeConst*qf.timeConst)*df.expSum*(e+1)
	df.expSum += df.expSum * e
}

const filterMaxX = 1e100

type diffExpFilters []diffExpFilter

func (df diffExpFilters) Len() int {
	return len(df) + 2
}

func (df diffExpFilters) X(i int) float64 {
	if i == 0 {
		return 0
	}
	if i > len(df) {
		return filterMaxX
	}
	return df[i-1].timeConst
}

func (df diffExpFilters) Y(i int) float64 {
	if i == 0 {
		return 0
	}
	if i > len(df) {
		return df[len(df)-1].expSum + df[len(df)-1].diffSum*(filterMaxX-df[len(df)-1].timeConst)
	}
	return df[i-1].expSum
}

func (df diffExpFilters) DY(i int) float64 {
	if i == 0 {
		return 0
	}
	if i > len(df) {
		return df[len(df)-1].diffSum
	}
	return df[i-1].timeConst
}

type quadExpFilter struct {
	timeConst, expSum, quadExpSum float64
}

func (qf *quadExpFilter) add(value, dt, exp float64) {
	e := math.Expm1(dt / qf.timeConst)
	qf.expSum += value / qf.timeConst
	d := qf.expSum * e
	qf.quadExpSum += qf.expSum*d*(-1-e*0.5)/qf.timeConst + qf.quadExpSum*exp
	qf.expSum += d
}

type quadExpFilters []quadExpFilter

func (qf quadExpFilters) Len() int {
	return len(qf) + 2
}

func (qf quadExpFilters) X(i int) float64 {
	if i == 0 {
		return 0
	}
	if i > len(qf) {
		return filterMaxX
	}
	return qf[i-1].timeConst
}

func (qf quadExpFilters) Y(i int) float64 {
	if i == 0 {
		return 0
	}
	if i >= len(qf) {
		return 1
	}
	if a, b := qf[len(qf)-1].quadExpSum, qf[i-1].quadExpSum; a < b {
		return a / b
	}
	return 1
}

type usagePatternStats struct {
	lock            sync.RWMutex
	lastUpdate      int64  // 64bit aligned mclock.AbsTime
	lastSumValue    uint64 // 64bit aligned
	usageFilters    diffExpFilters
	capacityFilters quadExpFilters
	capExpRate      float64
}

// call add(now, 0) to update before accessing the contents of the filters
func (ups *usagePatternStats) add(now mclock.AbsTime, value uint64) {
	for {
		lastUpdate := mclock.AbsTime(atomic.LoadInt64(&ups.lastUpdate))
		dt := time.Duration(now - lastUpdate)
		if dt < time.Millisecond*50 {
			atomic.AddUint64(&ups.lastSumValue, value)
			return
		}
		lastSumValue := atomic.LoadUint64(&ups.lastSumValue)
		ups.lock.Lock()
		if atomic.CompareAndSwapInt64(&ups.lastUpdate, int64(lastUpdate), int64(now)) {
			// add lastSumValue to the filters at lastUpdate
			// current value is not added to the filters yet
			// make sure that if another thread added some value since the load then it is not lost
			atomic.AddUint64(&ups.lastSumValue, value-lastSumValue)
			value := float64(lastSumValue)
			dtf := float64(dt)
			exp := math.Expm1(dtf * ups.capExpRate)
			for i := range ups.usageFilters {
				ups.usageFilters[i].add(value, dtf)
			}
			for i := range ups.capacityFilters {
				ups.capacityFilters[i].add(value, dtf, exp)
			}
			ups.lock.Unlock()
			return
		}
		ups.lock.Unlock()
	}
}

func (ups *usagePatternStats) predictDemand(now mclock.AbsTime, dt float64) float64 {
	ups.add(now, 0)
	ups.lock.Lock()
	defer ups.lock.Unlock()

	return utils.PwcValue(ups.usageFilters, dt)
}

func (ups *usagePatternStats) predictTime(now mclock.AbsTime, demand float64) float64 {
	ups.add(now, 0)
	ups.lock.Lock()
	defer ups.lock.Unlock()

	return utils.PwcInverse(ups.usageFilters, demand)
}

func (ups *usagePatternStats) predictTimeDiff(now mclock.AbsTime, demand, diff float64) (float64, float64) {
	ups.add(now, 0)
	ups.lock.Lock()
	defer ups.lock.Unlock()

	t := utils.PwcInverse(ups.usageFilters, demand)
	return t, utils.PwcInverse(ups.usageFilters, demand+diff) - t
}

func (ups *usagePatternStats) capacityValueFactor(now mclock.AbsTime, bufLimitRatio float64) float64 {
	ups.add(now, 0)
	ups.lock.Lock()
	defer ups.lock.Unlock()

	return utils.PwcValue(ups.capacityFilters, bufLimitRatio*500000)
}
