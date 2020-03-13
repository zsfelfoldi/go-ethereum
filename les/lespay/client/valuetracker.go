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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	vtVersion = 1 // database encoding format
	svVersion = 1

	vtUpdatePeriod          = time.Minute
	vtRefBasketUpdatePeriod = time.Minute * 10
	vtBasketTransferRate    = 1 / float64(time.Hour)
)

var (
	vtState  = []byte("vtState")
	vtServer = []byte("vtServer:")
)

// ServerValueTracker collects service value statistics for a specific server
type ServerValueTracker struct {
	lock sync.Mutex

	rtStats     responseTimeStats
	lastExpired mclock.AbsTime
	basket      serverBasket
	reqCosts    []uint64
	reqValues   *[]float64

	lastReqCosts []uint64 // accessed by ValueTracker only
}

func (sv *ServerValueTracker) init(now mclock.AbsTime, reqValues *[]float64) {
	reqTypeCount := len(*reqValues)
	sv.reqCosts = make([]uint64, reqTypeCount)
	sv.lastReqCosts = sv.reqCosts
	sv.lastExpired = now
	sv.reqValues = reqValues
	sv.basket.init(now, reqTypeCount)
}

func (sv *ServerValueTracker) periodicUpdate(now mclock.AbsTime) {

}

type ServedRequest struct {
	ReqType, Amount uint32
}

func (sv *ServerValueTracker) Served(reqs []ServedRequest, respTime time.Duration) {
	sv.lock.Lock()
	defer sv.lock.Unlock()

	var value float64
	for _, r := range reqs {
		sv.basket.add(r.ReqType, r.Amount, sv.reqCosts[r.ReqType]*uint64(r.Amount))
		value += (*sv.reqValues)[r.ReqType] * float64(r.Amount)
	}
	sv.rtStats.add(respTime, value)
}

func (sv *ServerValueTracker) TotalReqValue(expRT time.Duration) float64 {
	sv.lock.Lock()
	defer sv.lock.Unlock()

	tv, _ := sv.rtStats.value(expRT)
	return tv
}

func (sv *ServerValueTracker) NormalizedRtStats() []float64 {
	sv.lock.Lock()
	defer sv.lock.Unlock()

	res := make([]float64, timeStatLength)
	var sum uint64
	for _, d := range sv.rtStats {
		sum += d
	}
	if sum > 0 {
		s := float64(sum)
		for i, d := range sv.rtStats {
			res[i] = float64(d) / s
		}
	}
	return res
}

func (sv *ServerValueTracker) updateCosts(reqCosts []uint64, reqValues *[]float64, rvFactor float64) {
	sv.lock.Lock()
	defer sv.lock.Unlock()

	sv.reqCosts = reqCosts
	sv.reqValues = reqValues
	sv.basket.updateRvFactor(rvFactor)
}

func (sv *ServerValueTracker) transferBasket(now mclock.AbsTime) requestBasket {
	sv.lock.Lock()
	defer sv.lock.Unlock()

	return sv.basket.transfer(now, vtBasketTransferRate)
}

// ValueTracker coordinates service value calculation for individual servers and updates
// global statistics
type ValueTracker struct {
	clock        mclock.Clock
	lock         sync.Mutex
	quit         chan chan struct{}
	db           ethdb.Database
	connected    map[enode.ID]*ServerValueTracker
	reqTypeCount int

	refBasket           referenceBasket
	lastRefBasketUpdate mclock.AbsTime
	mappings            [][]string
	currentMapping      int
	initRefBasket       requestBasket
}

type valueTrackerEncV1 struct {
	Mappings         [][]string
	RefBasketMapping uint
	RefBasket        requestBasket
}

type serverValueTrackerEncV1 struct {
	RtStats            responseTimeStats
	ValueBasketMapping uint
	ValueBasket        requestBasket
}

type RequestInfo struct {
	Name                  string
	InitAmount, InitValue float64
}

func NewValueTracker(db ethdb.Database, clock mclock.Clock, reqInfo []RequestInfo) *ValueTracker {
	now := clock.Now()

	initRefBasket := make(requestBasket, len(reqInfo))
	mapping := make([]string, len(reqInfo))
	for i, req := range reqInfo {
		mapping[i] = req.Name
		initRefBasket[i].amount = uint64(req.InitAmount * referenceFactor)
		initRefBasket[i].value = uint64(req.InitAmount * req.InitValue)
	}

	vt := &ValueTracker{
		clock:               clock,
		connected:           make(map[enode.ID]*ServerValueTracker),
		quit:                make(chan chan struct{}),
		lastRefBasketUpdate: now,
		db:                  db,
		reqTypeCount:        len(initRefBasket),
		initRefBasket:       initRefBasket,
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
			case <-time.After(vtUpdatePeriod):
				vt.periodicUpdate()
			case quit := <-vt.quit:
				close(quit)
				return
			}
		}
	}()
	return vt
}

func (vt *ValueTracker) loadFromDb(mapping []string) error {
	enc, err := vt.db.Get(vtState)
	if err != nil {
		return err
	}
	r := bytes.NewReader(enc)
	var version uint
	if err := rlp.Decode(r, &version); err != nil {
		log.Error("Decoding value tracker state failed", "err", err)
		return err
	}
	if version != vtVersion {
		log.Error("Unknown ValueTracker version", "stored", version, "current", svVersion)
		return fmt.Errorf("Unknown ValueTracker version %d (current version is %d)", version, vtVersion)
	}
	var vte valueTrackerEncV1
	if err := rlp.Decode(r, &vte); err != nil {
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
	enc, err := rlp.EncodeToBytes([]interface{}{uint(vtVersion), &vte})
	if err != nil {
		log.Error("Encoding value tracker state failed", "err", err)
		return
	}
	if err := vt.db.Put(vtState, enc); err != nil {
		log.Error("Saving value tracker state failed", "err", err)
	}
}

func (vt *ValueTracker) Stop() {
	quit := make(chan struct{})
	vt.quit <- quit
	<-quit
	vt.lock.Lock()
	vt.updateReferenceBasket()
	for id, sv := range vt.connected {
		vt.save(id, sv)

	}
	vt.connected = nil
	vt.saveToDb()
	vt.lock.Unlock()
}

func (vt *ValueTracker) Register(id enode.ID) *ServerValueTracker {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	if vt.connected == nil { // closed
		return nil
	}
	sv := vt.loadOrNew(id)
	sv.init(vt.clock.Now(), &vt.refBasket.reqValues)
	vt.connected[id] = sv
	return sv
}

func (vt *ValueTracker) Unregister(id enode.ID) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	if sv := vt.connected[id]; sv != nil {
		vt.save(id, sv)
		delete(vt.connected, id)
	}
}

func (vt *ValueTracker) GetServerValueTracker(id enode.ID) *ServerValueTracker {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	return vt.loadOrNew(id)
}

func (vt *ValueTracker) loadOrNew(id enode.ID) *ServerValueTracker {
	if sv, ok := vt.connected[id]; ok {
		return sv
	}
	sv := &ServerValueTracker{}
	enc, err := vt.db.Get(append(vtServer, id[:]...))
	if err != nil {
		return sv
	}
	r := bytes.NewReader(enc)
	var version uint
	if err := rlp.Decode(r, &version); err != nil {
		log.Error("Failed to decode service value information", "id", id, "err", err)
		return sv
	}
	if version != svVersion {
		log.Error("Unknown ServerValueTracker version", "stored", version, "current", svVersion)
		return sv
	}
	var sve serverValueTrackerEncV1
	if err := rlp.Decode(r, &sve); err != nil {
		log.Error("Failed to decode service value information", "id", id, "err", err)
		return sv
	}
	sv.rtStats = sve.RtStats
	if int(sve.ValueBasketMapping) == vt.currentMapping {
		sv.basket.valueBasket = sve.ValueBasket
	} else {
		if sve.ValueBasketMapping >= uint(len(vt.mappings)) {
			log.Error("Unknown request basket mapping", "stored", sve.ValueBasketMapping, "current", vt.currentMapping)
			return sv
		}
		sv.basket.valueBasket = sve.ValueBasket.convertMapping(vt.mappings[sve.ValueBasketMapping], vt.mappings[vt.currentMapping], vt.initRefBasket)
	}
	return sv
}

func (vt *ValueTracker) save(id enode.ID, sv *ServerValueTracker) {
	sve := serverValueTrackerEncV1{
		RtStats:            sv.rtStats,
		ValueBasketMapping: uint(vt.currentMapping),
		ValueBasket:        sv.basket.valueBasket,
	}
	enc, err := rlp.EncodeToBytes([]interface{}{uint(svVersion), &sve})
	if err != nil {
		log.Error("Failed to encode service value information", "id", id, "err", err)
		return
	}
	if err := vt.db.Put(append(vtServer, id[:]...), enc); err != nil {
		log.Error("Failed to save service value information", "id", id, "err", err)
	}
}

func (vt *ValueTracker) updateReferenceBasket() {
	now := vt.clock.Now()
	vt.lastRefBasketUpdate = now
	for _, sv := range vt.connected {
		vt.refBasket.add(sv.transferBasket(now))
	}
	vt.refBasket.normalizeAndExpire(0) //TODO add expiration
	vt.refBasket.updateReqValues()
	for _, sv := range vt.connected {
		sv.updateCosts(sv.lastReqCosts, &vt.refBasket.reqValues, vt.refBasket.reqValueFactor(sv.lastReqCosts))
	}
	vt.saveToDb()
}

func (vt *ValueTracker) UpdateCosts(sv *ServerValueTracker, reqCosts []uint64) {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	sv.lastReqCosts = reqCosts
	sv.updateCosts(reqCosts, &vt.refBasket.reqValues, vt.refBasket.reqValueFactor(reqCosts))
}

func (vt *ValueTracker) periodicUpdate() {
	vt.lock.Lock()
	defer vt.lock.Unlock()

	now := mclock.Now()
	for _, sv := range vt.connected {
		sv.periodicUpdate(now)
	}
	if now > vt.lastRefBasketUpdate+mclock.AbsTime(vtRefBasketUpdatePeriod) {
		vt.updateReferenceBasket()
	}
}
