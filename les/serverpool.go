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

package les

import (
	"reflect"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	lpc "github.com/ethereum/go-ethereum/les/lespay/client"
	lpu "github.com/ethereum/go-ethereum/les/lespay/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

type serverPool struct {
	ns                                              *lpu.NodeStateMachine
	vt                                              *lpc.ValueTracker
	dialIterator                                    enode.Iterator
	stDialed, stConnected, stRedialWait, stHasValue lpu.NodeStateBitMask
}

type serverPoolFields struct {
	totalValue, dialCount uint64
	tvUpdate              mclock.AbsTime
}

var (
	smSaveImmediately      = []string{"hasValue", "paymentEnabled", "paid"}
	smSaveTimeout          = []string{"queryDelay", "priceDelay", "redialWait"}
	smKnownSelectorRequire = []string{"hasValue"}
	smKnownSelectorDisable = []string{"iterSelected", "iterReturned", "query", "queryDelay", "canConnect", "dialed", "redialWait", "connected", "paid"}
)

func newServerPool(db ethdb.Database, dbKey []byte, discovery enode.Iterator, clock mclock.Clock) *serverPool {
	s := &serverPool{
		ns: lpu.NewNodeStateMachine(db, dbKey, smSaveImmediately, smSaveTimeout, time.Minute*10, clock),
		vt: lpc.NewValueTracker(db, clock, requestList),
	}
	enrFieldId := s.ns.RegisterField(reflect.TypeOf(enr.Record{}))
	knownSelector := lpc.NewWrsIterator(s.ns, s.ns.GetStates(smKnownSelectorRequire), s.ns.GetStates(smKnownSelectorDisable), s.knownSelectWeight, enrFieldId)
	discEnrStored := enode.Filter(discovery, func(node *enode.Node) bool {
		s.ns.SetField(node.ID(), enrFieldId, node.Record())
		return true
	})
	iter := enode.NewFairMix(time.Second)
	iter.AddSource(discEnrStored)
	iter.AddSource(knownSelector)
	// preNegotiationFilter will be added in series with iter here when les4 is available

	s.stDialed = s.ns.GetState("dialed")
	s.stConnected = s.ns.GetState("connected")
	s.stRedialWait = s.ns.GetState("redialWait")
	s.stHasValue = s.ns.GetState("hasValue")
	s.dialIterator = enode.Filter(iter, func(node *enode.Node) bool {
		s.ns.UpdateState(node.ID(), s.stDialed, 0, time.Second*10)
		return true
	})
	s.ns.LoadFromDb()
	return s
}

func (s *serverPool) stop() {
	s.dialIterator.Close()
	s.ns.SaveToDb()
}

func (s *serverPool) registerPeer(p *serverPeer) {
	s.ns.UpdateState(p.ID(), s.stConnected+s.stHasValue, s.stDialed, 0)
	sv := s.vt.Register(p.ID())
	s.updateParams(sv, p.fcCosts)
	p.updateParams = func() {
		s.updateParams(sv, p.fcCosts)
	}
}

func (s *serverPool) unregisterPeer(p *serverPeer) {
	s.ns.UpdateState(p.ID(), s.stRedialWait, s.stConnected, time.Second*10)
	s.vt.Unregister(p.ID())
	p.updateParams = nil
}

func (s *serverPool) updateParams(sv *lpc.ServiceValue, costTable requestCostTable) {
	if sv == nil {
		return
	}
	reqCosts := make([]uint64, len(requestList))
	for code, costs := range costTable {
		if m, ok := requestMapping[uint32(code)]; ok {
			reqCosts[m.first] = costs.baseCost + costs.reqCost
			if m.rest != -1 {
				reqCosts[m.rest] = costs.reqCost
			}
		}
	}
	s.vt.UpdateCosts(sv, reqCosts)
}

func (s *serverPool) knownSelectWeight(i interface{}) uint64 {
	sv := s.vt.GetServiceValue(i.(enode.ID))
	if sv == nil {
		return 0
	}
	return uint64(sv.TotalReqValue(time.Second)) //TODO use expRT value based on global stats
}

type reqMapping struct {
	first, rest int
}

var (
	requestList    []lpc.RequestInfo
	requestMapping map[uint32]reqMapping
)

func init() {
	requestMapping = make(map[uint32]reqMapping)
	for code, req := range requests {
		cost := reqAvgTimeCost[code]
		rm := reqMapping{len(requestList), -1}
		requestList = append(requestList, lpc.RequestInfo{
			Name:       req.name + ".first",
			InitAmount: req.refBasketFirst,
			InitValue:  float64(cost.baseCost + cost.reqCost),
		})
		if req.refBasketRest != 0 {
			rm.rest = len(requestList)
			requestList = append(requestList, lpc.RequestInfo{
				Name:       req.name + ".rest",
				InitAmount: req.refBasketRest,
				InitValue:  float64(cost.reqCost),
			})
		}
		requestMapping[uint32(code)] = rm
	}
}
