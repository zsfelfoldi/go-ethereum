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

package server

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

var ppTestClientField = utils.NewNodeStateField("ppTestClient", reflect.TypeOf(&ppTestClient{}), nil, false, nil, nil)

const (
	testCapacityStepDiv      = 100
	testCapacityToleranceDiv = 10
)

type ppTestClient struct {
	id           enode.ID
	balance, cap uint64
}

func (c *ppTestClient) Priority(now mclock.AbsTime, cap uint64) int64 {
	return int64(c.balance / cap)
}

func (c *ppTestClient) EstMinPriority(until mclock.AbsTime, cap uint64) int64 {
	return int64(c.balance / cap)
}

func TestPriorityPool(t *testing.T) {
	clock := &mclock.Simulated{}
	ns := utils.NewNodeStateMachine(nil, nil, clock)
	ns.MustRegisterState(ActiveFlag)
	stInactive := ns.MustRegisterState(InactiveFlag)
	capFieldIndex := ns.MustRegisterField(CapacityField)
	nodeFieldIndex := ns.MustRegisterField(ppTestClientField)

	ns.AddFieldSub(capFieldIndex, func(id enode.ID, state utils.NodeStateBitMask, oldValue, newValue interface{}) {
		if n := ns.GetField(id, nodeFieldIndex); n != nil {
			c := n.(*ppTestClient)
			c.cap = newValue.(uint64)
		}
	})
	pp := NewPriorityPool(ns, clock, 100, 0, testCapacityStepDiv, ppTestClientField)
	ns.Start()
	pp.SetLimits(100, 1000000)
	clients := make([]*ppTestClient, 100)
	raise := func(c *ppTestClient) {
		for {
			if _, ok := pp.RequestCapacity(c.id, c.cap+c.cap/testCapacityStepDiv, 0, true); !ok {
				return
			}
		}
	}
	var sumBalance uint64
	check := func(c *ppTestClient) {
		expCap := 1000000 * c.balance / sumBalance
		capTol := expCap / testCapacityToleranceDiv
		if c.cap < expCap-capTol || c.cap > expCap+capTol {
			t.Errorf("Wrong node capacity (expected %d, got %d)", expCap, c.cap)
		}
	}

	for i := range clients {
		c := &ppTestClient{
			id:      enode.ID{byte(i)},
			balance: 1000000000,
			cap:     1000,
		}
		sumBalance += c.balance
		clients[i] = c
		ns.SetField(c.id, nodeFieldIndex, c)
		ns.UpdateState(c.id, stInactive, 0, 0)
		raise(c)
		check(c)
	}

	for count := 0; count < 100; count++ {
		c := clients[rand.Intn(len(clients))]
		oldBalance := c.balance
		c.balance = uint64(rand.Int63n(1000000000) + 1000000000)
		sumBalance += c.balance - oldBalance
		pp.UpdatePriority(c.id)
		if c.balance > oldBalance {
			raise(c)
		} else {
			for _, c := range clients {
				raise(c)
			}
		}
		for _, c := range clients {
			check(c)
		}
	}

	ns.Stop()
	pp.Stop()

}
