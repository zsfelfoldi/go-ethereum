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
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

const (
	accQueuedMax     = time.Second
	ccRaiseRate      = 1 / float64(time.Second*10)
	ccDropRate       = 1 / float64(time.Second*100)
	ccRaiseThreshold = 1.25
	ccDropThreshold  = 0.8
	ccRequestTimeout = time.Second * 2
	ccLimitTimeout   = time.Second * 20
)

var (
	peakField            = clientSetup.NewField("peakInfo", reflect.TypeOf(&PeakInfo{}))
	capacityControlField = clientSetup.NewField("capControl", reflect.TypeOf(&NodeCapacityControl{}))
	capacityField        = clientSetup.NewField("capacity", reflect.TypeOf(uint64(0)))
)

type CapacityControl struct {
	ns         *nodestate.NodeStateMachine
	clock      mclock.Clock
	exp        *utils.Expirer
	capRequest capRequestFunc
}

type capRequestFunc func(*enode.Node, uint64)

func NewCapacityControl(ns *nodestate.NodeStateMachine, clock mclock.Clock, capRequest capRequestFunc) *CapacityControl {
	cc := &CapacityControl{
		ns:         ns,
		clock:      clock,
		exp:        &utils.Expirer{},
		capRequest: capRequest,
	}
	cc.exp.SetRate(clock.Now(), ccDropRate)
	return cc
}

func (cc *CapacityControl) Register(node *enode.Node, minCap uint64) *NodeCapacityControl {
	nc := &NodeCapacityControl{
		cc:      cc,
		node:    node,
		peakCh:  make(chan *PeakInfo, 10),
		minimum: minCap,
		quitCh:  make(chan struct{}),
	}
	cc.ns.SetField(node, capacityControlField, nc)
	go nc.eventLoop()
	return nc
}

func (cc *CapacityControl) Unregister(node *enode.Node) {
	if nc, _ := cc.ns.GetField(node, capacityControlField).(*NodeCapacityControl); nc != nil {
		close(nc.quitCh)
		cc.ns.SetField(node, capacityControlField, nil)
	}
}

type NodeCapacityControl struct {
	cc     *CapacityControl
	node   *enode.Node
	quitCh chan struct{}
	lock   sync.Mutex

	peak                                *PeakInfo
	peakCh                              chan *PeakInfo
	peakFieldSet                        bool
	target                              utils.ExpiredValue
	capacity, minimum, limit, requested uint64
	limitTimeout, requestTimeout        mclock.AbsTime
}

func (nc *NodeCapacityControl) UpdateCapacity(capacity uint64, onRequest bool) {
	nc.lock.Lock()
	var compare uint64
	if onRequest && nc.requested != 0 {
		compare = nc.requested
	} else {
		compare = nc.capacity
	}
	if capacity < compare {
		nc.limit = capacity
		nc.limitTimeout = nc.cc.clock.Now() + mclock.AbsTime(ccLimitTimeout)
	}
	nc.capacity = capacity
	if onRequest {
		nc.requested = 0
		nc.requestTimeout = nc.cc.clock.Now() + mclock.AbsTime(ccRequestTimeout)
	}
	nc.lock.Unlock()
}

// new peak is always in queued state
func (nc *NodeCapacityControl) NewPeak(now mclock.AbsTime) *PeakInfo {
	peak := &PeakInfo{
		nc:          nc,
		lastUpdate:  now,
		queued:      true,
		peakReqs:    make(map[uint64]peakRequest),
		pendingReqs: make(map[uint64]peakRequest),
	}
	select {
	case nc.peakCh <- peak:
	default: // very unlikely, just make sure that we are never going to block here
		log.Error("NodeCapacityControl: cannot send to peak channel")
	}
	return peak
}

func (nc *NodeCapacityControl) update() time.Duration {
	var (
		requestCap   uint64
		setPeakField bool
		peak         *PeakInfo
	)
	nc.lock.Lock()
	defer func() {
		nc.lock.Unlock()
		if requestCap != 0 {
			nc.cc.capRequest(nc.node, requestCap)
		}
		if setPeakField {
			nc.cc.ns.SetField(nc.node, peakField, peak)
		}
	}()

	now := nc.cc.clock.Now()
	logOffset := nc.cc.exp.LogOffset(now)
	target := nc.target.Value(logOffset)
	if target < nc.minimum {
		target = nc.minimum
	}

	if nc.peak != nil {
		startedAt, length, _, qcCost, _, finalized := nc.peak.Status()

		if !nc.peakFieldSet && startedAt != 0 && time.Duration(now-startedAt) >= time.Millisecond*600 {
			setPeakField, peak = true, nc.peak
			nc.peakFieldSet = true
		}

		var diff int64
		if length > time.Millisecond*100 {
			peakCapacity := float64(qcCost) * 1000 / float64(length)
			diff = int64((float64(target) - peakCapacity) * math.Expm1(-float64(length)*ccRaiseRate))
		}

		if finalized {
			if -diff < int64(target-nc.minimum) {
				target += uint64(diff)
			} else {
				target = nc.minimum
			}
			nc.target = utils.ExpiredValue{}
			nc.target.Add(int64(target), logOffset)

			nc.peak = nil
			if nc.peakFieldSet {
				setPeakField, peak = true, nil
				nc.peakFieldSet = false
			}
		} else if diff > 0 {
			target += uint64(diff)
		}
		if now <= nc.limitTimeout && target > nc.limit {
			target = nc.limit
		}

		// check if capacity raise should be requested now
		if nc.requested == 0 && now >= nc.requestTimeout && nc.capacity < target {
			threshold := uint64(float64(nc.capacity) * ccRaiseThreshold)
			if threshold > nc.limit {
				threshold = nc.limit
			}
			if target >= threshold {
				requestCap = target
				nc.requested = target
			}
		}

		if !finalized {
			return time.Millisecond * 200
		}
	}

	// check if capacity drop should be requested now
	if nc.capacity == nc.minimum {
		return 0
	}
	threshold := uint64(float64(nc.capacity) * ccDropThreshold)
	if threshold < nc.minimum {
		threshold = nc.minimum
	}
	var wait time.Duration
	if target > threshold {
		wait = time.Duration(math.Log(float64(target)/float64(nc.minimum)) / ccDropRate)
	}
	if nc.requested != 0 {
		wait = time.Second
	} else if t := time.Duration(nc.requestTimeout - now); t > wait {
		wait = t
	}
	if wait == 0 {
		requestCap = target
		nc.requested = target
		return ccRequestTimeout
	}
	return wait
}

func (nc *NodeCapacityControl) eventLoop() {
	timer := nc.cc.clock.NewTimer(0)
	wait := time.Second // init to non zero because the timer is running now
	for {
		if wait != 0 && !timer.Stop() {
			<-timer.C()
		}
		wait = nc.update()
		if wait != 0 {
			timer.Reset(wait)
		}
		if nc.peak == nil {
			select {
			case nc.peak = <-nc.peakCh:
			case <-timer.C():
			case <-nc.quitCh:
				nc.cc.ns.SetField(nc.node, peakField, nil)
				return
			}
		} else {
			select {
			case <-timer.C():
			case <-nc.quitCh:
				nc.cc.ns.SetField(nc.node, peakField, nil)
				return
			}
		}
	}
}

type PeakInfo struct {
	nc   *NodeCapacityControl
	lock sync.Mutex

	startedAt, endedAt, lastUpdate, lastSent mclock.AbsTime
	accLength, accQueued                     time.Duration
	accCost, accQcCost                       int64
	queued, started, ended, finalized        bool
	peakReqs, pendingReqs                    map[uint64]peakRequest
}

type peakRequest struct {
	dt           time.Duration
	cost, qcCost int64
}

func (pi *PeakInfo) update(now mclock.AbsTime) {
	if pi.ended {
		return
	}
	dt := time.Duration(now - pi.lastUpdate)
	pi.lastUpdate = now
	if pi.queued {
		pi.accQueued += dt
		if pi.accQueued >= accQueuedMax {
			pi.accQueued = accQueuedMax
		}
	} else {
		if pi.accQueued > dt {
			pi.accQueued -= dt
		} else {
			pi.accQueued = 0
			pi.ended = true
			pi.pendingReqs = nil
			if len(pi.peakReqs) == 0 {
				pi.finalized = true
			}
		}
	}
}

func (pi *PeakInfo) SetQueuedState(now mclock.AbsTime, queued bool) bool {
	pi.lock.Lock()
	defer pi.lock.Unlock()

	pi.update(now)
	pi.queued = queued
	return !pi.ended
}

func (pi *PeakInfo) SentRequest(now mclock.AbsTime, id uint64) bool {
	pi.lock.Lock()
	defer pi.lock.Unlock()

	pi.update(now)
	if pi.ended {
		return false
	}

	if pi.started {
		// started is false when the first queued request arrived (we are deliberately not counting it)
		req := peakRequest{
			dt:     time.Duration(now - pi.lastSent),
			cost:   math.MinInt64,
			qcCost: math.MinInt64,
		}
		if pi.queued {
			pi.peakReqs[id] = req
			pi.endedAt = now // not final until pi.ended is true
			if len(pi.pendingReqs) > 0 {
				for id, req := range pi.pendingReqs {
					if req.cost == math.MinInt64 {
						pi.peakReqs[id] = req
					} else {
						pi.accLength += req.dt
						pi.accCost += req.cost
						pi.accQcCost += req.qcCost
					}
					delete(pi.pendingReqs, id)
				}
			}
		} else {
			pi.pendingReqs[id] = req
		}
	} else if pi.queued {
		pi.started = true
		pi.startedAt = now
		pi.endedAt = now
	}
	pi.lastSent = now
	return true
}

func (pi *PeakInfo) AnsweredRequest(now mclock.AbsTime, id uint64, cost, qcCost int64) {
	pi.lock.Lock()
	defer pi.lock.Unlock()

	pi.update(now)
	if pi.finalized {
		return
	}

	if req, ok := pi.peakReqs[id]; ok {
		pi.accLength += req.dt
		pi.accCost += cost
		pi.accQcCost += qcCost
		delete(pi.peakReqs, id)
		if pi.ended && len(pi.peakReqs) == 0 {
			pi.finalized = true
		}
	} else if req, ok := pi.pendingReqs[id]; ok {
		req.cost = cost
		req.qcCost = qcCost
		pi.pendingReqs[id] = req
	}
}

func (pi *PeakInfo) Status() (started mclock.AbsTime, length time.Duration, cost, qcCost int64, ended, finalized bool) {
	pi.lock.Lock()
	defer pi.lock.Unlock()

	return pi.startedAt, time.Duration(pi.endedAt - pi.startedAt), pi.accCost, pi.accQcCost, pi.ended, pi.finalized
}
