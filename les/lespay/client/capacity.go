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

// CapacityControlSetup contains node state flags and fields used by CapacityControl
type CapacityControlSetup struct {
	// controlled by CapacityControl
	PeakField, CapacityControlField, CapacityRequestField nodestate.Field
	// external connections
	capacityField nodestate.Field
}

// NewCapacityControlSetup creates a new CapacityControlSetup and initializes the fields
// and flags controlled by CapacityControl
func NewCapacityControlSetup(setup *nodestate.Setup) CapacityControlSetup {
	return CapacityControlSetup{
		PeakField:            setup.NewField("peakInfo", reflect.TypeOf(&PeakInfo{})),
		CapacityControlField: setup.NewField("capControl", reflect.TypeOf(&NodeCapacityControl{})),
		CapacityRequestField: setup.NewField("capRequest", reflect.TypeOf(uint64(0))),
	}
}

// Connect sets the fields and flags used by CapacityControl as an input
func (ccs *CapacityControlSetup) Connect(capacityField nodestate.Field) {
	ccs.capacityField = capacityField
}

type capacityControl struct {
	ns    *nodestate.NodeStateMachine
	setup CapacityControlSetup
	clock mclock.Clock
	exp   *utils.Expirer
}

func NewCapacityControl(ns *nodestate.NodeStateMachine, setup CapacityControlSetup, clock mclock.Clock, minCap uint64) {
	cc := &capacityControl{
		ns:    ns,
		setup: setup,
		clock: clock,
		exp:   &utils.Expirer{},
	}
	cc.exp.SetRate(clock.Now(), ccDropRate)

	ns.SubscribeField(setup.capacityField, func(n *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		if oldValue == nil {
			ns.SetFieldSub(n, setup.CapacityControlField, cc.newNodeCapacityControl(n, minCap, newValue.(uint64)))
			return
		}
		if newValue == nil {
			nc := ns.GetField(n, setup.CapacityControlField).(*NodeCapacityControl)
			nc.stop()
			ns.SetFieldSub(n, setup.CapacityControlField, nil)
			return
		}
		nc := ns.GetField(n, setup.CapacityControlField).(*NodeCapacityControl)
		nc.setCapacity(newValue.(uint64))
	})
}

func (cc *capacityControl) newNodeCapacityControl(node *enode.Node, minCap, capacity uint64) *NodeCapacityControl {
	nc := &NodeCapacityControl{
		cc:       cc,
		node:     node,
		peakCh:   make(chan *PeakInfo, 10),
		minimum:  minCap,
		capacity: capacity,
		quitCh:   make(chan struct{}),
	}
	go nc.eventLoop()
	return nc
}

type NodeCapacityControl struct {
	cc     *capacityControl
	node   *enode.Node
	quitCh chan struct{}

	capLock  sync.Mutex
	capacity uint64

	peak                         *PeakInfo
	peakCh                       chan *PeakInfo
	peakFieldSet                 bool
	target                       utils.ExpiredValue
	minimum, limit, requested    uint64
	limitTimeout, requestTimeout mclock.AbsTime
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

func (nc *NodeCapacityControl) stop() {
	close(nc.quitCh)
}

func (nc *NodeCapacityControl) setCapacity(capacity uint64) {
	nc.capLock.Lock()
	nc.capacity = capacity
	nc.capLock.Unlock()
}

func (nc *NodeCapacityControl) update() time.Duration {
	nc.capLock.Lock()
	capacity := nc.capacity
	nc.capLock.Unlock()

	now := nc.cc.clock.Now()
	logOffset := nc.cc.exp.LogOffset(now)
	target := nc.target.Value(logOffset)
	if target < nc.minimum {
		target = nc.minimum
	}

	if nc.requested != 0 && now >= nc.requestTimeout {
		if nc.capacity < nc.requested {
			nc.limit = nc.capacity
			nc.limitTimeout = now + mclock.AbsTime(ccLimitTimeout)
		}
		nc.requested = 0
	}

	if nc.peak != nil {
		startedAt, length, _, qcCost, _, finalized := nc.peak.Status()

		if !nc.peakFieldSet && startedAt != 0 && time.Duration(now-startedAt) >= time.Millisecond*600 {
			nc.cc.ns.SetField(nc.node, nc.cc.setup.PeakField, nc.peak)
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
				nc.cc.ns.SetField(nc.node, nc.cc.setup.PeakField, nil)
				nc.peakFieldSet = false
			}
		} else if diff > 0 {
			target += uint64(diff)
		}
		if now <= nc.limitTimeout && target > nc.limit {
			target = nc.limit
		}

		// check if capacity raise should be requested now
		if now >= nc.requestTimeout && capacity < target {
			threshold := uint64(float64(capacity) * ccRaiseThreshold)
			if threshold > nc.limit {
				threshold = nc.limit
			}
			if target >= threshold {
				nc.cc.ns.SetField(nc.node, nc.cc.setup.CapacityRequestField, target)
				nc.requestTimeout = now + mclock.AbsTime(ccRequestTimeout)
				nc.requested = target
			}
		}

		if !finalized {
			return time.Millisecond * 200
		}
	}

	// check if capacity drop should be requested now
	if capacity == nc.minimum {
		return 0
	}
	threshold := uint64(float64(capacity) * ccDropThreshold)
	if threshold < nc.minimum {
		threshold = nc.minimum
	}
	var wait time.Duration
	if target > threshold {
		wait = time.Duration(math.Log(float64(target)/float64(nc.minimum)) / ccDropRate)
	}
	if t := time.Duration(nc.requestTimeout - now); t > wait {
		wait = t
	}
	if wait == 0 {
		nc.cc.ns.SetField(nc.node, nc.cc.setup.CapacityRequestField, target)
		nc.requestTimeout = now + mclock.AbsTime(ccRequestTimeout)
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
				nc.cc.ns.SetField(nc.node, nc.cc.setup.PeakField, nil)
				return
			}
		} else {
			select {
			case <-timer.C():
			case <-nc.quitCh:
				nc.cc.ns.SetField(nc.node, nc.cc.setup.PeakField, nil)
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
	return
}
