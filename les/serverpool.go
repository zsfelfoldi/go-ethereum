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
	"io"
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	lpc "github.com/ethereum/go-ethereum/les/lespay/client"
	lpu "github.com/ethereum/go-ethereum/les/lespay/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	minTimeout          = time.Millisecond * 500
	dialCost            = 10000
	nodeWeightMul       = 1000000
	nodeWeightThreshold = 100
)

type serverPool struct {
	clock                                           mclock.Clock
	ns                                              *lpu.NodeStateMachine
	vt                                              *lpc.ValueTracker
	dialIterator                                    enode.Iterator
	stDialed, stConnected, stRedialWait, stHasValue lpu.NodeStateBitMask
	nodeHistoryFieldId                              int
	timeoutLock                                     sync.RWMutex
	timeout                                         time.Duration
	timeoutCounter                                  uint64
	quit                                            chan struct{}
}

type nodeHistory struct {
	// only dialCost is saved
	dialCost          lpu.ExpiredValue
	lastTimeoutUpdate uint64
	totalValue        float64
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
		clock:   clock,
		ns:      lpu.NewNodeStateMachine(db, dbKey, time.Minute*10, clock),
		vt:      lpc.NewValueTracker(db, clock, requestList, time.Minute, 1/float64(time.Hour), 1/float64(time.Hour*1000)),
		timeout: minTimeout,
		quit:    make(chan struct{}),
	}
	enrFieldId := s.ns.RegisterField(reflect.TypeOf(enr.Record{}), s.ns.GetStates(keepNodeRecord))
	s.nodeHistoryFieldId = s.ns.RegisterField(reflect.TypeOf(nodeHistory{}), s.ns.GetStates(knownSelectorRequire))

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
		n, _ := s.ns.GetField(node.ID(), s.nodeHistoryFieldId).(*nodeHistory)
		if n == nil {
			n = &nodeHistory{}
		}
		n.dialCost.Add(dialCost, s.vt.StatsExpirer().LogOffset(s.clock.Now()))
		s.ns.SetField(node.ID(), s.nodeHistoryFieldId, n)
		s.ns.UpdateState(node.ID(), s.stDialed, 0, time.Second*10)
		return true
	})
	s.ns.LoadFromDb()
	go func() {
		for {
			rts := s.vt.RtStats()
			timeout := minTimeout
			if t := rts.Timeout(0.1); t > timeout {
				timeout = t
			}
			if t := rts.Timeout(0.5) * 2; t > timeout {
				timeout = t
			}
			s.timeoutLock.Lock()
			if s.timeout != timeout {
				s.timeout = timeout
				s.timeoutCounter++
			}
			s.timeoutLock.Unlock()
			select {
			case <-time.After(time.Minute * 10):
			case <-s.quit:
				return
			}
		}
	}()
	return s
}

func (s *serverPool) stop() {
	close(s.quit)
	s.dialIterator.Close()
	s.ns.SaveToDb()
}

func (s *serverPool) registerPeer(p *serverPeer) {
	s.ns.UpdateState(p.ID(), s.stConnected, s.stDialed, 0)
	p.setValueTracker(s.vt, s.vt.Register(p.ID()))
	p.updateVtParams()
}

func (s *serverPool) unregisterPeer(p *serverPeer) {
	set := s.stRedialWait
	if s.nodeWeight(p.ID(), true) >= nodeWeightThreshold {
		set |= s.stHasValue
	}
	s.ns.UpdateState(p.ID(), set, s.stConnected, time.Second*10)
	s.vt.Unregister(p.ID())
	p.setValueTracker(nil, nil)
}

func (s *serverPool) nodeWeight(id enode.ID, forceRecalc bool) uint64 {
	n, _ := s.ns.GetField(id, s.nodeHistoryFieldId).(*nodeHistory)
	if n == nil {
		return 0
	}
	nvt := s.vt.GetNodeValueTracker(id)
	if nvt == nil {
		return 0
	}
	div := n.dialCost.Value(s.vt.StatsExpirer().LogOffset(s.clock.Now()))
	if div < dialCost {
		div = dialCost
	}
	s.timeoutLock.RLock()
	timeout := s.timeout
	timeoutCounter := s.timeoutCounter
	s.timeoutLock.RUnlock()

	if forceRecalc || n.lastTimeoutUpdate != timeoutCounter {
		n.totalValue = s.vt.TotalReqValue(nvt, timeout)
		n.lastTimeoutUpdate = timeoutCounter
		s.ns.SetField(id, s.nodeHistoryFieldId, n)
	}
	return uint64(n.totalValue * nodeWeightMul / float64(div))
}

func (s *serverPool) knownSelectWeight(i interface{}) uint64 {
	id := i.(enode.ID)
	wt := s.nodeWeight(id, false)
	if wt < nodeWeightThreshold {
		s.ns.UpdateState(id, 0, s.stHasValue, 0)
		return 0
	}
	return wt
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

func (n *nodeHistory) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &n.dialCost)
}

func (n *nodeHistory) DecodeRLP(s *rlp.Stream) error {
	return s.Decode(&n.dialCost)
}
