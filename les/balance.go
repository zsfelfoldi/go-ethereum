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
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/metrics"
)

const maxBalance = math.MaxInt64

const (
	balanceCallbackQueue = iota
	balanceCallbackZero
	balanceCallbackCount
)

// expirationController controls the exponential expiration of token balance.
type expirationController interface {
	expiration(mclock.AbsTime) fixed64
}

// priceFactors determine the pricing policy (may apply either to positive or
// negative balances which may have different factors).
// - timeFactor is cost unit per nanosecond of connection time
// - capacityFactor is cost unit per nanosecond of connection time per 1000000 capacity
// - requestFactor is cost unit per request "realCost" unit
type priceFactors struct {
	timeFactor, capacityFactor, requestFactor float64
}

// timePrice returns the price per nanosecond based on the given capacity.
func (p priceFactors) timePrice(cap uint64) float64 {
	return p.timeFactor + float64(cap)*p.capacityFactor/1000000
}

// reqPrice returns the price per request cost.
func (p priceFactors) reqPrice() float64 {
	return p.requestFactor
}

type estimator interface {
	Rate5() float64
	Mark(int64)
}

type noopEstimator struct {
	value float64
}

func (e *noopEstimator) Rate5() float64 { return e.value }
func (e *noopEstimator) Mark(v int64)   { e.value = float64(v) }

// balanceTracker keeps track of the positive and negative balances of a connected
// client and calculates actual and projected future priority values required by
// prque.LazyQueue.
//
// balanceTracker can be either active or inactive. The default status is inactive.
// The balance updating is only enabled when the status is active but balance
// expiration is happened all the time.
type balanceTracker struct {
	lock                   sync.Mutex
	clock                  mclock.Clock
	posExp, negExp         expirationController
	stopped                bool
	active                 bool
	capacity               uint64
	balance                balance
	posFactor, negFactor   priceFactors
	reqEstimator           estimator
	lastUpdate, nextUpdate mclock.AbsTime
	updateEvent            mclock.Timer
	// since only a limited and fixed number of callbacks are needed, they
	// are stored in a fixed size array ordered by priority threshold.
	callbacks [balanceCallbackCount]balanceCallback
	// callbackIndex maps balanceCallback constants to callbacks array indexes (-1 if not active)
	callbackIndex [balanceCallbackCount]int
	callbackCount int // number of active callbacks
}

// balance represents a pair of positive and negative balances
type balance struct {
	pos, neg expiredValue
}

// balanceCallback represents a single callback that is activated when client priority
// reaches the given threshold
type balanceCallback struct {
	id        int
	threshold int64
	callback  func()
}

// newBalanceTracker creates a balance tracker with given parameters.
// The balance accounting is disabled by default unless activate is
// called explicitly.
func newBalanceTracker(posExp, negExp expirationController, clock mclock.Clock, capacity uint64, pos, neg expiredValue, posFactor, negFactor priceFactors) balanceTracker {
	bt := balanceTracker{
		clock:     clock,
		posExp:    posExp,
		negExp:    negExp,
		capacity:  capacity,
		balance:   balance{pos: pos, neg: neg},
		posFactor: posFactor,
		negFactor: negFactor,
	}
	for i := range bt.callbackIndex {
		bt.callbackIndex[i] = -1
	}
	return bt
}

// activate marks the active as true and starts balance updating.
func (bt *balanceTracker) activate() {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.lastUpdate = bt.clock.Now()
	bt.active = true
	// The default request usage estimator is five-minute exponentially-weighted
	// moving average. In general, the larger the coeff of EWMA, the smoother
	// its sampling, but there will be a delay between the sampled value and the
	// latest change trend. 5 minute time window is large enough for sampling.
	bt.reqEstimator = metrics.NewMeterForced()
}

// testActivate marks the active as true and starts balance updating.
// It's only used in testing.
func (bt *balanceTracker) testActivate() {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.lastUpdate = bt.clock.Now()
	bt.active = true
	bt.reqEstimator = &noopEstimator{}
}

// activate marks the active as false and stop balance updating.
func (bt *balanceTracker) deactivate() {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.updateBalance(bt.clock.Now())
	bt.active = false
}

// stop shuts down the balance tracker
func (bt *balanceTracker) stop(now mclock.AbsTime) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.updateBalance(now)
	bt.stopped = true
	bt.active = false
	bt.posFactor = priceFactors{0, 0, 0}
	bt.negFactor = priceFactors{0, 0, 0}
	if bt.updateEvent != nil {
		bt.updateEvent.Stop()
		bt.updateEvent = nil
	}
}

// SETTERS

// setFactors sets the price factors.
func (bt *balanceTracker) setFactors(posFactor, negFactor priceFactors) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	if bt.stopped {
		return
	}
	now := bt.clock.Now()
	bt.updateBalance(now)
	bt.posFactor, bt.negFactor = posFactor, negFactor
	bt.checkCallbacks(now) // Priority changed, check callbacks
}

// setBalance sets the positive and negative balance to the given values
func (bt *balanceTracker) setBalance(pos, neg expiredValue) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	now := bt.clock.Now()
	bt.updateBalance(now)
	bt.balance.pos = pos
	bt.balance.neg = neg
	bt.checkCallbacks(now) // Priority changed, check callbacks
}

// setCapacity updates the capacity value used for priority calculation
// Note: capacity should never be zero
func (bt *balanceTracker) setCapacity(capacity uint64) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.capacity = capacity
	bt.checkCallbacks(bt.clock.Now()) // Priority changed, check callbacks
}

// GETTERS

// getFactors returns the price factors.
func (bt *balanceTracker) getFactors() (priceFactors, priceFactors) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	return bt.posFactor, bt.negFactor
}

// getBalance returns the current positive and negative balance
func (bt *balanceTracker) getBalance(now mclock.AbsTime) (expiredValue, expiredValue) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.updateBalance(now)
	return bt.balance.pos, bt.balance.neg
}

// getPriority returns the actual priority based on the current balance
func (bt *balanceTracker) getPriority(now mclock.AbsTime) int64 {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.updateBalance(now)
	return bt.balanceToPriority(bt.balance)
}

// estimatedPriority gives an upper estimate for the priority at a given time in the future.
// If addReqCost is true then an average request cost per time is assumed that is twice the
// average cost per time in the current session. If false, zero request cost is assumed.
func (bt *balanceTracker) estimatedPriority(at mclock.AbsTime, addReqCost bool) int64 {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	if !bt.active {
		return bt.balanceToPriority(bt.balance)
	}
	var avgReqCost float64
	if addReqCost {
		avgReqCost = 2 * bt.reqEstimator.Rate5() / float64(time.Millisecond)
	}
	return bt.balanceToPriority(bt.reducedBalance(at, avgReqCost))
}

// posBalanceMissing calculates the missing amount of positive balance in order to
// connect at targetCapacity, stay connected for the given amount of time and then
// still have a priority of targetPriority.
//
// Note using real cost calculation no matter the status is active or not.
func (bt *balanceTracker) posBalanceMissing(targetPriority int64, targetCapacity uint64, after time.Duration) uint64 {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	now := bt.clock.Now()
	if targetPriority > 0 {
		price := bt.negFactor.timePrice(targetCapacity)
		cost := uint64(float64(after) * price)
		neg := bt.balance.neg.value(bt.negExp.expiration(now))
		if cost+neg < uint64(targetPriority) {
			return 0
		}
		if uint64(targetPriority) > neg && price > 1e-100 {
			if negTime := time.Duration(float64(uint64(targetPriority)-neg) / price); negTime < after {
				after -= negTime
			} else {
				after = 0
			}
		}
		targetPriority = 0
	}
	price := bt.posFactor.timePrice(targetCapacity)
	required := uint64(float64(-targetPriority)*float64(targetCapacity)+float64(after)*price) + 1
	if required >= maxBalance {
		return math.MaxUint64 // target not reachable
	}
	pos := bt.balance.pos.value(bt.posExp.expiration(now))
	if required > pos {
		return required - pos
	}
	return 0
}

// INTERNALS

// updateBalance updates balance based on the time factor
func (bt *balanceTracker) updateBalance(now mclock.AbsTime) {
	if bt.active && now > bt.lastUpdate {
		bt.balance = bt.reducedBalance(now, 0)
		bt.lastUpdate = now
	}
}

// balanceToPriority converts a balance to a priority value. Higher priority means
// first to disconnect. Positive balance translates to negative priority. If positive
// balance is zero then negative balance translates to a positive priority.
func (bt *balanceTracker) balanceToPriority(b balance) int64 {
	if b.pos.base > 0 {
		return -int64(b.pos.value(bt.posExp.expiration(bt.clock.Now())) / bt.capacity)
	}
	return int64(b.neg.value(bt.negExp.expiration(bt.clock.Now())))
}

// reducedBalance estimates the reduced balance at a given time in the fututre based
// on the current balance, the time factor and an estimated average request cost per time ratio
func (bt *balanceTracker) reducedBalance(at mclock.AbsTime, avgReqCost float64) balance {
	dt := float64(at - bt.lastUpdate)
	b := bt.balance
	if b.pos.base != 0 {
		factor := bt.posFactor.timePrice(bt.capacity) + bt.posFactor.reqPrice()*avgReqCost
		diff := -int64(dt * factor)
		dd := b.pos.add(diff, bt.posExp.expiration(at))
		if dd == diff {
			dt = 0
		} else {
			dt += float64(dd) / factor
		}
	}
	if dt > 0 {
		factor := bt.negFactor.timePrice(bt.capacity) + bt.negFactor.reqPrice()*avgReqCost
		b.neg.add(int64(dt*factor), bt.negExp.expiration(at))
	}
	return b
}

// timeUntil calculates the remaining time needed to reach a given priority level
// assuming that no requests are processed until then. If the given level is never
// reached then (0, false) is returned.
// Note: the function assumes that the balance has been recently updated and
// calculates the time starting from the last update.
func (bt *balanceTracker) timeUntil(priority int64) (time.Duration, bool) {
	now := bt.clock.Now()
	var dt float64
	if bt.balance.pos.base != 0 {
		pos := bt.balance.pos.value(bt.posExp.expiration(now))
		price := bt.posFactor.timePrice(bt.capacity)
		if price < 1e-100 {
			return 0, false
		}
		if priority < 0 {
			newBalance := uint64(-priority) * bt.capacity
			if newBalance > pos {
				return 0, false
			}
			dt = float64(pos-newBalance) / price
			return time.Duration(dt), true
		} else {
			dt = float64(pos) / price
		}
	} else {
		if priority < 0 {
			return 0, false
		}
	}
	// if we have a positive balance then dt equals the time needed to get it to zero
	negBalance := bt.balance.neg.value(bt.negExp.expiration(now))
	price := bt.negFactor.timePrice(bt.capacity)
	if uint64(priority) > negBalance {
		if price < 1e-100 {
			return 0, false
		}
		dt += float64(uint64(priority)-negBalance) / price
	}
	return time.Duration(dt), true
}

// checkCallbacks checks whether the threshold of any of the active callbacks
// have been reached and calls them if necessary. It also sets up or updates
// a scheduled event to ensure that is will be called again just after the next
// threshold has been reached.
// Note: checkCallbacks assumes that the balance has been recently updated.
func (bt *balanceTracker) checkCallbacks(now mclock.AbsTime) {
	// Short circuit if nothing to check.
	if bt.callbackCount == 0 {
		return
	}
	// Spin up all callable callbacks.
	pri := bt.balanceToPriority(bt.balance)
	for bt.callbackCount != 0 && bt.callbacks[bt.callbackCount-1].threshold <= pri {
		bt.callbackCount--
		bt.callbackIndex[bt.callbacks[bt.callbackCount].id] = -1
		go bt.callbacks[bt.callbackCount].callback()
	}
	if bt.callbackCount == 0 {
		bt.nextUpdate = 0
		bt.updateAfter(0)
		return
	}
	// Note the status of tracker can be inactive, but we still
	// need to check the callbacks since expiration is still in
	// progress.
	d, ok := bt.timeUntil(bt.callbacks[bt.callbackCount-1].threshold)
	if !ok {
		bt.nextUpdate = 0
		bt.updateAfter(0)
		return
	}
	if bt.nextUpdate == 0 || bt.nextUpdate > now+mclock.AbsTime(d) {
		if d > time.Second {
			// Note: if the scheduled update is not in the very near future then we
			// schedule the update a bit earlier. This way we do need to update a few
			// extra times but don't need to reschedule every time a processed request
			// brings the expected firing time a little bit closer.
			d = ((d - time.Second) * 7 / 8) + time.Second
		}
		bt.nextUpdate = now + mclock.AbsTime(d)
		bt.updateAfter(d)
	}
}

// updateAfter schedules a balance update and callback check in the future
func (bt *balanceTracker) updateAfter(dt time.Duration) {
	if bt.updateEvent == nil || bt.updateEvent.Stop() {
		if dt == 0 {
			bt.updateEvent = nil
			return
		}
		bt.updateEvent = bt.clock.AfterFunc(dt, func() {
			bt.lock.Lock()
			defer bt.lock.Unlock()

			if bt.callbackCount != 0 {
				now := bt.clock.Now()
				bt.updateBalance(now)
				bt.checkCallbacks(now)
			}
		})
	}
}

// requestCost should be called after serving a request for the given peer.
// Returns the latest positive and negative balance.
func (bt *balanceTracker) requestCost(cost uint64) (uint64, uint64) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	if bt.stopped {
		return 0, 0
	}
	now := bt.clock.Now()
	bt.updateBalance(now)
	bt.reqEstimator.Mark(int64(cost / uint64(time.Microsecond)))

	var (
		updated bool
		fcost   = float64(cost)
		posExp  = bt.posExp.expiration(now)
		negExp  = bt.negExp.expiration(now)
	)
	if bt.balance.pos.base != 0 {
		if bt.posFactor.reqPrice() == 0 {
			return bt.balance.pos.value(posExp), bt.balance.neg.value(negExp)
		}
		c := -int64(fcost * bt.posFactor.reqPrice())
		cc := bt.balance.pos.add(c, posExp)
		if c == cc {
			fcost = 0
		} else {
			fcost *= 1 - float64(cc)/float64(c)
		}
		updated = true
	}
	if fcost > 0 && bt.negFactor.reqPrice() != 0 {
		bt.balance.neg.add(int64(fcost*bt.negFactor.reqPrice()), negExp)
		updated = true
	}
	if updated {
		bt.checkCallbacks(now)
	}
	return bt.balance.pos.value(posExp), bt.balance.neg.value(negExp)
}

// setCallback sets up a one-time callback to be called when priority reaches
// the threshold. If it has already reached the threshold the callback is called
// immediately.
func (bt *balanceTracker) addCallback(id int, threshold int64, callback func()) {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	bt.removeCb(id)
	idx := 0
	for idx < bt.callbackCount && threshold < bt.callbacks[idx].threshold {
		idx++
	}
	for i := bt.callbackCount - 1; i >= idx; i-- {
		bt.callbackIndex[bt.callbacks[i].id]++
		bt.callbacks[i+1] = bt.callbacks[i]
	}
	bt.callbackCount++
	bt.callbackIndex[id] = idx
	bt.callbacks[idx] = balanceCallback{id, threshold, callback}
	now := bt.clock.Now()
	bt.updateBalance(now)
	bt.checkCallbacks(now)
}

// removeCallback removes the given callback and returns true if it was active
func (bt *balanceTracker) removeCallback(id int) bool {
	bt.lock.Lock()
	defer bt.lock.Unlock()

	return bt.removeCb(id)
}

// removeCb removes the given callback and returns true if it was active
// Note: should be called while bt.lock is held
func (bt *balanceTracker) removeCb(id int) bool {
	idx := bt.callbackIndex[id]
	if idx == -1 {
		return false
	}
	bt.callbackIndex[id] = -1
	for i := idx; i < bt.callbackCount-1; i++ {
		bt.callbackIndex[bt.callbacks[i+1].id]--
		bt.callbacks[i] = bt.callbacks[i+1]
	}
	bt.callbackCount--
	return true
}
