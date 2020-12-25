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

const accTimeMax = uint64(time.Second)

type peakDetector struct {
	requests      *list.List
	costThreshold uint64
}

type pdRequest struct {
	sent                        mclock.AbsTime
	queued                      bool
	peakStart, lastQueued       *pdRequest
	cost, qcCost                uint64
	accCost, accQcCost, accTime uint64
	element                     *list.Element
}

func (pd *peakDetector) init() {
	pd.requests = list.New()
	pd.costThreshold = 50000000 //xxx
}

func (pd *peakDetector) setCostThreshold(costThreshold uint64) {
	pd.costThreshold = costThreshold
}

func (pd *peakDetector) add(queued bool, sent mclock.AbsTime, cost, qcCost uint64) (updated, peakStart mclock.AbsTime, length time.Duration, peakCost, peakQcCost uint64) {
	req := &pdRequest{
		sent:   sent,
		queued: queued,
		cost:   cost,
		qcCost: qcCost,
	}
	e := pd.requests.Back()
	if e == nil || sent > e.Value.(*pdRequest).sent {
		req.element = pd.requests.PushBack(req)
	} else {
		for e.Prev() != nil && sent <= e.Prev().Value.(*pdRequest).sent {
			e = e.Prev()
		}
		req.element = pd.requests.InsertBefore(req, e)
	}

	e = req.element

	var (
		prevSent                                    mclock.AbsTime
		prevQueued                                  bool
		peakStart, lastQueued                       *pdRequest
		accCost, accQcCost, accTime, lastQueuedCost uint64
	)
	if e.Prev() != nil {
		p := e.Prev().Value.(*pdRequest)
		prevSent, prevQueued = p.sent, p.queued
		peakStart, lastQueued = p.peakStart, p.lastQueued
		accCost, accQcCost, accTime = p.accCost, p.accQcCost, p.accTime
		if lastQueued != nil {
			lastQueuedCost = lastQueued.accCost
		}
	}
	for e != nil {
		p := e.Value.(*pdRequest)

		accCost += p.cost
		accQcCost += p.qcCost
		dt := uint64(p.sent - prevSent)
		if p.queued && prevQueued {
			accTime += dt
			if accTime > accTimeMax {
				accTime = accTimeMax
			}
		} else {
			if dt >= accTime || accCost >= lastQueuedCost+pd.costThreshold {
				accTime = 0
			} else {
				accTime -= dt
			}
		}
		if p.queued {
			if accTime == 0 {
				peakStart = p
			}
			lastQueued = p
			lastQueuedCost = accCost
		}

		prevSent, prevQueued = p.sent, p.queued
		p.peakStart, p.lastQueued = peakStart, lastQueued
		p.accCost, p.accQcCost, p.accTime = accCost, accQcCost, accTime

		e = e.Next()
	}
	//	fmt.Println("xxx", last, queuedCount, metered, peakCount, peakStart, accCost, accTime)
	if accTime == accTimeMax {
		for pd.requests.Front() != peakStart.element {
			pd.requests.Remove(pd.requests.Front())
		}
		//dt, cost, qcCost := time.Duration(prevSent-peakStart.sent), accCost-peakStart.accCost, accQcCost-peakStart.accQcCost
		//fmt.Println("peak  count", peakCount, "  duration", dt, "  cost", cost, "  rate", float64(cost)/float64(dt))
		return true, peakStart.sent, time.Duration(prevSent - peakStart.sent), accCost - peakStart.accCost, accQcCost - peakStart.accQcCost
	}
	return
}

const (
	pfWindowPoints = 10
	pfWindowLength = time.Second
	pfWindowStep   = pfWindowLength / pfWindowPoints
)

var (
	rateStep          = 1.2
	rateLogMultiplier = 1 / math.Log(rateStep)
)

type peakFilter struct {
	lastStart              mclock.AbsTime
	lastLength, nextPoint  time.Duration
	lastValue, lastQcValue float64
	newPeak                bool
	pointer                int
	values, qcValues       [pfWindowPoints]float64
	serverModel            *serverModel
	events                 chan cmEvent
}

func (pf *peakFilter) init(events chan cmEvent, serverModel *serverModel) {
	pf.events = events
	pf.serverModel = serverModel
}

func (pf *peakFilter) addPeak(peakStart mclock.AbsTime, length time.Duration, value, qcValue float64) {
	if peakStart != pf.lastStart {
		pf.lastStart, pf.lastLength, pf.nextPoint, pf.lastValue, pf.lastQcValue = peakStart, 0, 0, 0, 0
	}
	for length >= pf.nextPoint {
		r := float64(pf.nextPoint-pf.lastLength) / float64(length-pf.lastLength)
		pf.addPoint(newPeak, pf.lastValue+(value-pf.lastValue)*r, pf.lastQcValue+(qcValue-pf.lastQcValue)*r)
		pf.nextPoint += pfWindowStep
	}
	pf.lastLength, pf.lastValue, pf.lastQcValue = length, value, qcValue
}

func (pf *peakFilter) addPoint(newPeak bool, relCapCost, value, qcValue float64) {
	if newPeak {
		pf.pointer = 0
		pf.newPeak = true
	}
	if !pf.newPeak {
		pf.addToHistory(relCapCost, value-pf.values[pf.pointer], qcValue-pf.qcValues[pf.pointer])
	}
	pf.values[pf.pointer] = value
	pf.qcValues[pf.pointer] = qcValue
	pf.pointer++
	if pf.pointer == pfWindowPoints {
		pf.pointer = 0
		pf.newPeak = false
	}
}

func (pf *peakFilter) addToHistory(relCapCost, value, qcValue float64) {
	if value < 1 {
		return
	}
	length := float64(pfWindowStep)
	lv, _, pos := requestRateScale.neighbors(value / length) //TODO rv/s (most rv is fel van szorozva, ido meg ns)
	column := requestRateScale.basePoint(lv)
	relCapCost *= length
	pf.clientModel.add(column, []uint64{uint64(length * (1 - pos))}, []uint64{uint64(length * pos)})
	pf.serverModel.add(column, []uint64{uint64(qcValue * (1 - pos)), uint64(qcValue * (1 - pos))}, []uint64{uint64(relCapCost * pos), uint64(relCapCost * pos)})
}

type capCtrlSetup struct {
	// controlled by PriorityPool
	ActiveFlag, InactiveFlag       nodestate.Flags
	CapacityField, ppNodeInfoField nodestate.Field
	// external connections
	updateFlag    nodestate.Flags
	priorityField nodestate.Field
}

// NewPriorityPoolSetup creates a new PriorityPoolSetup and initializes the fields
// and flags controlled by PriorityPool
func newCapCtrlSetup(setup *nodestate.Setup) PriorityPoolSetup {
	return PriorityPoolSetup{
		ActiveFlag:      setup.NewFlag("active"),
		InactiveFlag:    setup.NewFlag("inactive"),
		CapacityField:   setup.NewField("capacity", reflect.TypeOf(uint64(0))),
		ppNodeInfoField: setup.NewField("ppNodeInfo", reflect.TypeOf(&ppNodeInfo{})),
	}
}

// Connect sets the fields and flags used by PriorityPool as an input
func (pps *PriorityPoolSetup) Connect(priorityField nodestate.Field, updateFlag nodestate.Flags) {
	pps.priorityField = priorityField // should implement nodePriority
	pps.updateFlag = updateFlag       // triggers an immediate priority update
}
