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
	"bytes"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	vtVersion  = 1 // database encoding format for ValueTracker
	nvtVersion = 1 // database encoding format for NodeValueTracker
)

var (
	vtKey     = []byte("vt:")
	vtNodeKey = []byte("vtNode:")
)

// NodeValueTracker collects service value statistics for a specific server node
type NodeValueTracker struct {
	lock sync.Mutex

	rtStats, lastRtStats ResponseTimeStats
	lastTransfer         mclock.AbsTime
	basket               serverBasket
	reqCosts             []uint64
	reqValues            *[]float64

	lastReqCosts []uint64 // accessed by ValueTracker only
}

func (nv *NodeValueTracker) init(now mclock.AbsTime, reqValues *[]float64) {
	reqTypeCount := len(*reqValues)
	nv.reqCosts = make([]uint64, reqTypeCount)
	nv.lastReqCosts = nv.reqCosts
	nv.lastTransfer = now
	nv.reqValues = reqValues
	nv.basket.init(reqTypeCount)
}

type ServedRequest struct {
	ReqType, Amount uint32
}

func (nv *NodeValueTracker) Served(reqs []ServedRequest, respTime time.Duration) {
	nv.lock.Lock()
	defer nv.lock.Unlock()

	var value float64
	for _, r := range reqs {
		nv.basket.add(r.ReqType, r.Amount, nv.reqCosts[r.ReqType]*uint64(r.Amount))
		value += (*nv.reqValues)[r.ReqType] * float64(r.Amount)
	}
	nv.rtStats.Add(respTime, value)
}

func (nv *NodeValueTracker) TotalReqValue(expRT time.Duration) float64 {
	nv.lock.Lock()
	defer nv.lock.Unlock()

	tv, _ := nv.rtStats.Value(expRT)
	return tv
}

func (nv *NodeValueTracker) RtStats() ResponseTimeStats {
	nv.lock.Lock()
	defer nv.lock.Unlock()

	return nv.rtStats
}

func (nv *NodeValueTracker) updateCosts(reqCosts []uint64, reqValues *[]float64, rvFactor float64) {
	nv.lock.Lock()
	defer nv.lock.Unlock()

	nv.reqCosts = reqCosts
	nv.reqValues = reqValues
	nv.basket.updateRvFactor(rvFactor)
}

func (nv *NodeValueTracker) transferStats(now mclock.AbsTime, transferRate, statsExpRate float64) (requestBasket, ResponseTimeStats) {
	nv.lock.Lock()
	defer nv.lock.Unlock()

	dt := now - nv.lastTransfer
	nv.lastTransfer = now
	if dt < 0 {
		dt = 0
	}
	exp := -math.Expm1(-float64(dt) * statsExpRate)
	recentRtStats := nv.rtStats
	recentRtStats.SubStats(&nv.lastRtStats)
	nv.rtStats.Expire(exp)
	recentRtStats.Expire(exp)
	nv.lastRtStats = nv.rtStats
	return nv.basket.transfer(-math.Expm1(-transferRate * float64(dt))), recentRtStats
}

// ValueTracker coordinates service value calculation for individual servers and updates
// global statistics
type ValueTracker struct {
	clock        mclock.Clock
	lock         sync.Mutex
	quit         chan chan struct{}
	db           ethdb.KeyValueStore
	connected    map[enode.ID]*NodeValueTracker
	reqTypeCount int

	refBasket      referenceBasket
	lastUpdate     mclock.AbsTime
	mappings       [][]string
	currentMapping int
	initRefBasket  requestBasket
	rtStats        ResponseTimeStats

	transferRate, statsExpRate float64
}

type valueTrackerEncV1 struct {
	Mappings         [][]string
	RefBasketMapping uint
	RefBasket        requestBasket
}

type nodeValueTrackerEncV1 struct {
	RtStats            ResponseTimeStats
	ValueBasketMapping uint
	ValueBasket        requestBasket
}

type RequestInfo struct {
	Name                  string
	InitAmount, InitValue float64
}

func NewValueTracker(db ethdb.KeyValueStore, clock mclock.Clock, reqInfo []RequestInfo, updatePeriod time.Duration, transferRate, statsExpRate float64) *ValueTracker {
	now := clock.Now()

	initRefBasket := make(requestBasket, len(reqInfo))
	mapping := make([]string, len(reqInfo))

	var sumAmount, sumValue float64
	for _, req := range reqInfo {
		sumAmount += req.InitAmount
		sumValue += req.InitAmount * req.InitValue
	}
	scaleValues := sumAmount * referenceFactor / sumValue
	for i, req := range reqInfo {
		mapping[i] = req.Name
		initRefBasket[i].amount = uint64(req.InitAmount * referenceFactor)
		initRefBasket[i].value = uint64(req.InitAmount * req.InitValue * scaleValues)
	}

	vt := &ValueTracker{
		clock:         clock,
		connected:     make(map[enode.ID]*NodeValueTracker),
		quit:          make(chan chan struct{}),
		lastUpdate:    now,
		db:            db,
		reqTypeCount:  len(initRefBasket),
		initRefBasket: initRefBasket,
		transferRate:  transferRate,
		statsExpRate:  statsExpRate,
	}
	if vt.loadFromDb(mapping) != nil {
		// previous state not saved or invalid, init with default values
		vt.refBasket.refBasket = initRefBasket
		vt.mappings = [][]string{mapping}
		vt.currentMapping = 0
	}
	vt.refBasket.init(now, vt.reqTypeCount)

	go func() {
		for {
			select {
			case <-clock.After(updatePeriod):
				vt.lock.Lock()
				vt.periodicUpdate()
				vt.lock.Unlock()
			case quit := <-vt.quit:
				close(quit)
				return
			}
		}
	}()
	return vt
}

func (vt *ValueTracker) loadFromDb(mapping []string) error {
	enc, err := vt.db.Get(vtKey)
	if err != nil {
		return err
	}
	r := bytes.NewReader(enc)
	var version uint
	if err := rlp.Decode(r, &version); err != nil {
		fmt.Println(1, err)
		log.Error("Decoding value tracker state failed", "err", err)
		return err
	}
	if version != vtVersion {
		fmt.Println("ver")
		log.Error("Unknown ValueTracker version", "stored", version, "current", nvtVersion)
		return fmt.Errorf("Unknown ValueTracker version %d (current version is %d)", version, vtVersion)
	}
	var vte valueTrackerEncV1
	if err := rlp.Decode(r, &vte); err != nil {
		fmt.Println(2, err)
		log.Error("Decoding value tracker state failed", "err", err)
		return err
	}
	vt.mappings = vte.Mappings
	vt.currentMapping = -1
loop:
	for i, m := range vt.mappings {
		if len(m) != len(mapping) {
			continue loop
		}
		for j, s := range mapping {
			if m[j] != s {
				continue loop
			}
		}
		vt.currentMapping = i
		break
	}
	if vt.currentMapping == -1 {
		vt.currentMapping = len(vt.mappings)
		vt.mappings = append(vt.mappings, mapping)
	}
	if int(vte.RefBasketMapping) == vt.currentMapping {
		vt.refBasket.refBasket = vte.RefBasket
	} else {
		if vte.RefBasketMapping >= uint(len(vt.mappings)) {
			log.Error("Unknown request basket mapping", "stored", vte.RefBasketMapping, "current", vt.currentMapping)
			return fmt.Errorf("Unknown request basket mapping %d (current version is %d)", vte.RefBasketMapping, vt.currentMapping)
		}
		vt.refBasket.refBasket = vte.RefBasket.convertMapping(vt.mappings[vte.RefBasketMapping], mapping, vt.initRefBasket)
	}
	return nil
}

func (vt *ValueTracker) saveToDb() {
	vte := valueTrackerEncV1{
		Mappings:         vt.mappings,
		RefBasketMapping: uint(vt.currentMapping),
		RefBasket:        vt.refBasket.refBasket,
	}
	enc1, err := rlp.EncodeToBytes(uint(vtVersion))
	if err != nil {
		fmt.Println(err)
		log.Error("Encoding value tracker state failed", "err", err)
		return
	}
	enc2, err := rlp.EncodeToBytes(&vte)
	if err != nil {
		fmt.Println(err)
		log.Error("Encoding value tracker state failed", "err", err)
		return
	}
	if err := vt.db.Put(vtKey, append(enc1, enc2...)); err != nil {
		fmt.Println(err)
		log.Error("Saving value tracker state failed", "err", err)
	}
}

func (vt *ValueTracker) Stop() {
	quit := make(chan struct{})
	vt.quit <- quit
	<-quit
	vt.lock.Lock()
	vt.periodicUpdate()
	for id, nv := range vt.connected {
		vt.saveNode(id, nv)

	}
	vt.connected = nil
	vt.saveToDb()
	vt.lock.Unlock()
}

func (vt *ValueTracker) Register(id enode.ID) *NodeValueTracker {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	if vt.connected == nil { // closed
		return nil
	}
	nv := vt.loadOrNewNode(id)
	nv.init(vt.clock.Now(), &vt.refBasket.reqValues)
	vt.connected[id] = nv
	return nv
}

func (vt *ValueTracker) Unregister(id enode.ID) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	if nv := vt.connected[id]; nv != nil {
		vt.saveNode(id, nv)
		delete(vt.connected, id)
	}
}

func (vt *ValueTracker) GetNodeValueTracker(id enode.ID) *NodeValueTracker {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	return vt.loadOrNewNode(id)
}

func (vt *ValueTracker) loadOrNewNode(id enode.ID) *NodeValueTracker {
	if nv, ok := vt.connected[id]; ok {
		return nv
	}
	nv := &NodeValueTracker{}
	enc, err := vt.db.Get(append(vtNodeKey, id[:]...))
	if err != nil {
		return nv
	}
	r := bytes.NewReader(enc)
	var version uint
	if err := rlp.Decode(r, &version); err != nil {
		fmt.Println(3, err)
		log.Error("Failed to decode node value tracker", "id", id, "err", err)
		return nv
	}
	if version != nvtVersion {
		fmt.Println("nv ver")
		log.Error("Unknown NodeValueTracker version", "stored", version, "current", nvtVersion)
		return nv
	}
	var sve nodeValueTrackerEncV1
	if err := rlp.Decode(r, &sve); err != nil {
		fmt.Println(4, err)
		log.Error("Failed to decode node value tracker", "id", id, "err", err)
		return nv
	}
	nv.rtStats = sve.RtStats
	if int(sve.ValueBasketMapping) == vt.currentMapping {
		nv.basket.valueBasket = sve.ValueBasket
	} else {
		if sve.ValueBasketMapping >= uint(len(vt.mappings)) {
			log.Error("Unknown request basket mapping", "stored", sve.ValueBasketMapping, "current", vt.currentMapping)
			return nv
		}
		nv.basket.valueBasket = sve.ValueBasket.convertMapping(vt.mappings[sve.ValueBasketMapping], vt.mappings[vt.currentMapping], vt.initRefBasket)
	}
	return nv
}

func (vt *ValueTracker) saveNode(id enode.ID, nv *NodeValueTracker) {
	sve := nodeValueTrackerEncV1{
		RtStats:            nv.rtStats,
		ValueBasketMapping: uint(vt.currentMapping),
		ValueBasket:        nv.basket.valueBasket,
	}
	enc1, err := rlp.EncodeToBytes(uint(nvtVersion))
	if err != nil {
		log.Error("Failed to encode service value information", "id", id, "err", err)
		return
	}
	enc2, err := rlp.EncodeToBytes(&sve)
	if err != nil {
		log.Error("Failed to encode service value information", "id", id, "err", err)
		return
	}
	if err := vt.db.Put(append(vtNodeKey, id[:]...), append(enc1, enc2...)); err != nil {
		log.Error("Failed to save service value information", "id", id, "err", err)
	}
}

func (vt *ValueTracker) UpdateCosts(nv *NodeValueTracker, reqCosts []uint64) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	nv.lastReqCosts = reqCosts
	nv.updateCosts(reqCosts, &vt.refBasket.reqValues, vt.refBasket.reqValueFactor(reqCosts))
}

func (vt *ValueTracker) RtStats() ResponseTimeStats {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	return vt.rtStats
}

func (vt *ValueTracker) periodicUpdate() {
	now := vt.clock.Now()
	dt := now - vt.lastUpdate
	vt.lastUpdate = now
	if dt < 0 {
		dt = 0
	}
	exp := -math.Expm1(-float64(dt) * vt.statsExpRate)
	vt.rtStats.Expire(exp)
	for _, nv := range vt.connected {
		basket, rtStats := nv.transferStats(now, vt.transferRate, vt.statsExpRate)
		vt.refBasket.add(basket)
		vt.rtStats.AddStats(&rtStats)
	}
	vt.refBasket.normalizeAndExpire(exp)
	vt.refBasket.updateReqValues()
	for _, nv := range vt.connected {
		nv.updateCosts(nv.lastReqCosts, &vt.refBasket.reqValues, vt.refBasket.reqValueFactor(nv.lastReqCosts))
	}
	vt.saveToDb()
}
