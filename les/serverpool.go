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
	"github.com/ethereum/go-ethereum/les/utils"
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
	ns                                              *utils.NodeStateMachine
	vt                                              *lpc.ValueTracker
	dialIterator                                    enode.Iterator
	stDialed, stConnected, stRedialWait, stHasValue utils.NodeStateBitMask
	nodeHistoryFieldId                              int
	timeoutLock                                     sync.RWMutex
	timeout                                         time.Duration
	timeoutCounter                                  uint64
	quit                                            chan struct{}
}

type nodeHistory struct {
	// only dialCost is saved
	dialCost          utils.ExpiredValue
	lastTimeoutUpdate uint64
	totalValue        float64
}

func (n *nodeHistory) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &n.dialCost)
}

func (n *nodeHistory) DecodeRLP(s *rlp.Stream) error {
	return s.Decode(&n.dialCost)
}

var (
	sfDiscovered = utils.NewNodeStateFlag("discovered", false, true)
	sfHasValue   = utils.NewNodeStateFlag("hasValue", true, false)
	sfSelected   = utils.NewNodeStateFlag("selected", false, false)
	sfDialed     = utils.NewNodeStateFlag("dialed", false, false)
	sfConnected  = utils.NewNodeStateFlag("connected", false, false)
	sfRedialWait = utils.NewNodeStateFlag("redialWait", false, true)

	keepNodeRecord       = []*utils.NodeStateFlag{sfDiscovered, sfHasValue}
	knownSelectorRequire = []*utils.NodeStateFlag{sfHasValue}
	knownSelectorDisable = []*utils.NodeStateFlag{sfSelected, sfDialed, sfConnected, sfRedialWait}

	sfiEnr         = utils.NewNodeStateField("enr", reflect.TypeOf(enr.Record{}), keepNodeRecord)
	sfiNodeHistory = utils.NewNodeStateField("nodeHistory", reflect.TypeOf(nodeHistory{}), knownSelectorRequire)
)

func newServerPool(db ethdb.Database, dbKey []byte, discovery enode.Iterator, clock mclock.Clock, ulServers []string) *serverPool {
	//TODO connect to ulServers
	s := &serverPool{
		clock:   clock,
		ns:      utils.NewNodeStateMachine(db, dbKey, clock),
		vt:      lpc.NewValueTracker(db, clock, requestList, time.Minute, 1/float64(time.Hour), 1/float64(time.Hour*1000)),
		timeout: minTimeout,
		quit:    make(chan struct{}),
	}
	// Register all serverpool-defined states
	stDiscovered, _ := s.ns.RegisterState(sfDiscovered)
	s.stHasValue, _ = s.ns.RegisterState(sfHasValue)
	stSelected, _ := s.ns.RegisterState(sfSelected)
	s.stDialed, _ = s.ns.RegisterState(sfDialed)
	s.stConnected, _ = s.ns.RegisterState(sfConnected)
	s.stRedialWait, _ = s.ns.RegisterState(sfRedialWait)

	// Register all serverpool-defined node fields.
	enrFieldId, _ := s.ns.RegisterField(sfiEnr)
	s.nodeHistoryFieldId, _ = s.ns.RegisterField(sfiNodeHistory)

	knownSelector := lpc.NewWrsIterator(s.ns, s.ns.StatesMask(knownSelectorRequire), s.ns.StatesMask(knownSelectorDisable), stSelected, s.knownSelectWeight, enrFieldId)
	discEnrStored := enode.Filter(discovery, func(node *enode.Node) bool {
		s.ns.UpdateState(node.ID(), stDiscovered, 0, time.Hour)
		s.ns.SetField(node.ID(), enrFieldId, node.Record())
		return true
	})
	iter := enode.NewFairMix(time.Second)
	iter.AddSource(discEnrStored)
	iter.AddSource(knownSelector)

	// preNegotiationFilter will be added in series with iter here when les4 is available

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
