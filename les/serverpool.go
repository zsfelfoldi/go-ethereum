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
	clock                                           mclock.Clock
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
	sfDiscovered = lpu.NewNodeStateFlag("discovered", false, true)
	sfHasValue   = lpu.NewNodeStateFlag("hasValue", true, false)
	sfSelected   = lpu.NewNodeStateFlag("selected", false, false)
	sfDialed     = lpu.NewNodeStateFlag("dialed", false, false)
	sfConnected  = lpu.NewNodeStateFlag("connected", false, false)
	sfRedialWait = lpu.NewNodeStateFlag("redialWait", false, true)

	keepNodeRecord       = []*lpu.NodeStateFlag{sfDiscovered, sfHasValue}
	knownSelectorRequire = []*lpu.NodeStateFlag{sfHasValue}
	knownSelectorDisable = []*lpu.NodeStateFlag{sfSelected, sfDialed, sfConnected, sfRedialWait}
)

func newServerPool(db ethdb.Database, dbKey []byte, discovery enode.Iterator, clock mclock.Clock, ulServers []string) *serverPool {
	//TODO connect to ulServers
	s := &serverPool{
		clock: clock,
		ns:    lpu.NewNodeStateMachine(db, dbKey, time.Minute*10, clock),
		vt:    lpc.NewValueTracker(db, clock, requestList, time.Minute, 1/float64(time.Hour), 1/float64(time.Hour*1000)),
	}
	enrFieldId := s.ns.RegisterField(reflect.TypeOf(enr.Record{}), s.ns.GetStates(keepNodeRecord))
	knownSelector := lpc.NewWrsIterator(s.ns, s.ns.GetStates(knownSelectorRequire), s.ns.GetStates(knownSelectorDisable), s.ns.GetState(sfSelected), s.knownSelectWeight, enrFieldId)
	stDiscovered := s.ns.GetState(sfDiscovered)
	discEnrStored := enode.Filter(discovery, func(node *enode.Node) bool {
		s.ns.UpdateState(node.ID(), stDiscovered, 0, time.Hour)
		s.ns.SetField(node.ID(), enrFieldId, node.Record())
		return true
	})
	iter := enode.NewFairMix(time.Second)
	iter.AddSource(discEnrStored)
	iter.AddSource(knownSelector)
	// preNegotiationFilter will be added in series with iter here when les4 is available

	s.stDialed = s.ns.GetState(sfDialed)
	s.stConnected = s.ns.GetState(sfConnected)
	s.stRedialWait = s.ns.GetState(sfRedialWait)
	s.stHasValue = s.ns.GetState(sfHasValue)
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
	p.setValueTracker(s.vt.Register(p.ID()))
	p.updateVtParams(s.vt)
}

func (s *serverPool) unregisterPeer(p *serverPeer) {
	s.ns.UpdateState(p.ID(), s.stRedialWait, s.stConnected, time.Second*10)
	s.vt.Unregister(p.ID())
	p.setValueTracker(nil)
}

func (s *serverPool) knownSelectWeight(i interface{}) uint64 {
	nvt := s.vt.GetNodeValueTracker(i.(enode.ID))
	if nvt == nil {
		return 0
	}
	return uint64(nvt.TotalReqValue(time.Second)) //TODO use expRT value based on global stats
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
