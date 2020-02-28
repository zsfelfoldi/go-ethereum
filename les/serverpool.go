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
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

type serverPool struct {
	ns                                              *lpc.NodeStateMachine
	dialIterator                                    enode.Iterator
	stDialed, stConnected, stRedialWait, stHasValue lpc.NodeStateBitMask
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
		ns: lpc.NewNodeStateMachine(db, dbKey, smSaveImmediately, smSaveTimeout, time.Minute*10, clock),
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
}

func (s *serverPool) unregisterPeer(p *serverPeer) {
	s.ns.UpdateState(p.ID(), s.stRedialWait, s.stConnected, time.Second*10)
}

func (s *serverPool) knownSelectWeight(i interface{}) uint64 {
	return 1
}
