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
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

var (
	testSetup         = &nodestate.Setup{}
	ppTestClientField = testSetup.NewField("ppTestClient", reflect.TypeOf(&ppTestClient{}))
	ppUpdateFlag      = testSetup.NewFlag("ppUpdateFlag")
	ppTestSetup       = NewPriorityPoolSetup(testSetup)
)

func init() {
	ppTestSetup.Connect(ppTestClientField, statusField, ppUpdateFlag)
}

const (
	testCapacityStepDiv      = 100
	testCapacityToleranceDiv = 10
)

type ppTestClient struct {
	node         *enode.Node
	balance, cap uint64
}

func (c *ppTestClient) Priority(now mclock.AbsTime, cap uint64) int64 {
	return int64(c.balance / cap)
}

func (c *ppTestClient) EstMinPriority(until mclock.AbsTime, cap uint64, update bool) int64 {
	return int64(c.balance / cap)
}

func TestPriorityPool(t *testing.T) {
	clock := &mclock.Simulated{}
	ns := nodestate.NewNodeStateMachine(nil, nil, clock, testSetup)

	ns.SubscribeField(ppTestSetup.CapacityField, func(node *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		if n := ns.GetField(node, ppTestSetup.priorityField); n != nil {
			c := n.(*ppTestClient)
			c.cap = newValue.(uint64)
		}
	})
	pp := NewPriorityPool(ns, ppTestSetup, clock, 100, 0, testCapacityStepDiv)
	ns.Start()
	pp.SetLimits(100, 1000000)
	clients := make([]*ppTestClient, 100)
	raise := func(c *ppTestClient) {
		for {
			if _, ok := pp.RequestCapacity(c.node, c.cap+c.cap/testCapacityStepDiv, 0, true); !ok {
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
			node:    enode.SignNull(&enr.Record{}, enode.ID{byte(i)}),
			balance: 1000000000,
			cap:     1000,
		}
		sumBalance += c.balance
		clients[i] = c
		ns.SetState(c.node, dummyFlag, nodestate.Flags{}, 0)
		ns.SetField(c.node, ppTestSetup.priorityField, c)
		ns.SetField(c.node, statusField, NewClientStatus(Inactive, ""))
		raise(c)
		check(c)
	}

	for count := 0; count < 100; count++ {
		c := clients[rand.Intn(len(clients))]
		oldBalance := c.balance
		c.balance = uint64(rand.Int63n(1000000000) + 1000000000)
		sumBalance += c.balance - oldBalance
		pp.ns.SetState(c.node, ppUpdateFlag, nodestate.Flags{}, 0)
		pp.ns.SetState(c.node, nodestate.Flags{}, ppUpdateFlag, 0)
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
}

// TestPriorityStateTransition ensures whether the proritypool can handle the
// client state transition correctly. State transition includes:
//
//     CONNECTED -> INACTIVE
//     INACTIVE  -> ACTIVE
//     ACTIVE    -> INACTIVE
//     *         -> DISCONNECTED
func TestPriorityStateTransition(t *testing.T) {
	clock := &mclock.Simulated{}
	ns := nodestate.NewNodeStateMachine(nil, nil, clock, testSetup)

	pool := NewPriorityPool(ns, ppTestSetup, clock, 100, 0, testCapacityStepDiv)
	ns.Start()
	pool.SetLimits(10, 1000)

	// Connect to empty pool, active status should be marked
	clients := make([]*ppTestClient, 10)
	for i := range clients {
		c := &ppTestClient{
			node:    enode.SignNull(&enr.Record{}, enode.ID{byte(i)}),
			balance: 1000,
			cap:     100,
		}
		clients[i] = c
		ns.SetState(c.node, dummyFlag, nodestate.Flags{}, 0)
		ns.SetField(c.node, ppTestSetup.priorityField, c)
		ns.SetField(c.node, statusField, NewClientStatus(Inactive, ""))

		status, _ := ns.GetField(c.node, statusField).(*ClientStatus)
		if !status.IsStatus(Active) {
			t.Fatalf("Connected client should be grant an active flag")
		}
	}

	// Connect more clients with higher priority, some old clients
	// should be tagged as inactive
	clients2 := make([]*ppTestClient, len(clients))
	for i := 0; i < len(clients); i++ {
		c := &ppTestClient{
			node:    enode.SignNull(&enr.Record{}, enode.ID{byte(i + len(clients))}),
			balance: 100000, // Higher priority
			cap:     100,
		}
		ns.SetState(c.node, dummyFlag, nodestate.Flags{}, 0)
		ns.SetField(c.node, ppTestSetup.priorityField, c)
		ns.SetField(c.node, statusField, NewClientStatus(Inactive, ""))
		clients2[i] = c
	}
	for _, client := range clients {
		status, _ := ns.GetField(client.node, statusField).(*ClientStatus)
		if !status.IsStatus(Inactive) {
			t.Fatalf("Connected client should be grant an inactive flag")
		}
	}

	// Allocate more tokens for old clients, promotion should happen
	for _, client := range clients {
		client.balance = 1000000
		ns.SetState(client.node, ppUpdateFlag, nodestate.Flags{}, 0)
		ns.SetState(client.node, nodestate.Flags{}, ppUpdateFlag, 0)

		status, _ := ns.GetField(client.node, statusField).(*ClientStatus)
		if !status.IsStatus(Active) {
			t.Fatalf("Connected client should be grant an active flag")
		}
	}

	// Drop the old clients, automatic promotion should happen
	for _, client := range clients {
		ns.SetField(client.node, statusField, nil)
	}
	for _, client := range clients2 {
		status, _ := ns.GetField(client.node, statusField).(*ClientStatus)
		if !status.IsStatus(Active) {
			t.Fatalf("Connected client should be grant an active flag")
		}
	}
}
