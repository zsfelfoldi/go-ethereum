// Copyright 2019 The go-ethereum Authors
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
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

type zeroExpirer struct{}

func (z zeroExpirer) SetRate(now mclock.AbsTime, rate float64)                 {}
func (z zeroExpirer) SetLogOffset(now mclock.AbsTime, logOffset utils.Fixed64) {}
func (z zeroExpirer) LogOffset(now mclock.AbsTime) utils.Fixed64               { return 0 }

func expval(v uint64) utils.ExpiredValue {
	return utils.ExpiredValue{Base: v}
}

var btTestClientField = utils.NewNodeStateField("btTestClient", reflect.TypeOf(&btTestClient{}), nil, false, nil, nil)

type btTestClient struct {
	id     enode.ID
	freeID string
}

func (c *btTestClient) FreeID() string {
	return c.freeID
}

func (c *btTestClient) Updated() {}

type balanceTestSetup struct {
	clock          *mclock.Simulated
	ns             *utils.NodeStateMachine
	bt             *BalanceTracker
	nodeFieldIndex int
}

func newBalanceTestSetup() *balanceTestSetup {
	clock := &mclock.Simulated{}
	ns := utils.NewNodeStateMachine(nil, nil, clock)
	nodeFieldIndex := ns.MustRegisterField(btTestClientField)
	db := memorydb.New()
	bt := NewBalanceTracker(ns, db, clock, zeroExpirer{}, zeroExpirer{}, btTestClientField)
	ns.Start()
	return &balanceTestSetup{
		clock:          clock,
		ns:             ns,
		bt:             bt,
		nodeFieldIndex: nodeFieldIndex,
	}
}

func (b *balanceTestSetup) newNode() *NodeBalance {
	c := &btTestClient{}
	b.ns.SetField(c.id, b.nodeFieldIndex, c)
	return b.bt.GetNode(c.id)
}

func (b *balanceTestSetup) stop() {
	b.ns.Stop()
	b.bt.Stop()
}

func TestSetBalance(t *testing.T) {
	b := newBalanceTestSetup()
	defer b.stop()
	node := b.newNode()

	var inputs = []struct {
		pos, neg utils.ExpiredValue
	}{
		{expval(1000), expval(0)},
		{expval(0), expval(1000)},
		{expval(1000), expval(1000)},
	}

	for _, i := range inputs {
		node.setBalance(i.pos, i.neg)
		pos, neg := node.getBalance(b.clock.Now())
		if pos != i.pos {
			t.Fatalf("Positive balance mismatch, want %v, got %v", i.pos, pos)
		}
		if neg != i.neg {
			t.Fatalf("Negative balance mismatch, want %v, got %v", i.neg, neg)
		}
	}
}

func TestBalanceTimeCost(t *testing.T) {
	b := newBalanceTestSetup()
	defer b.stop()
	node := b.newNode()

	node.setCapacity(1000)
	node.SetFactors(PriceFactors{1, 0, 1}, PriceFactors{1, 0, 1})
	node.setBalance(expval(uint64(time.Minute)), expval(0)) // 1 minute time allowance

	var inputs = []struct {
		runTime time.Duration
		expPos  uint64
		expNeg  uint64
	}{
		{time.Second, uint64(time.Second * 59), 0},
		{0, uint64(time.Second * 59), 0},
		{time.Second * 59, 0, 0},
		{time.Second, 0, uint64(time.Second)},
	}
	for _, i := range inputs {
		b.clock.Run(i.runTime)
		if pos, _ := node.getBalance(b.clock.Now()); pos != expval(i.expPos) {
			t.Fatalf("Positive balance mismatch, want %v, got %v", i.expPos, pos)
		}
		if _, neg := node.getBalance(b.clock.Now()); neg != expval(i.expNeg) {
			t.Fatalf("Negative balance mismatch, want %v, got %v", i.expNeg, neg)
		}
	}

	node.setBalance(expval(uint64(time.Minute)), expval(0)) // Refill 1 minute time allowance
	for _, i := range inputs {
		b.clock.Run(i.runTime)
		if pos, _ := node.getBalance(b.clock.Now()); pos != expval(i.expPos) {
			t.Fatalf("Positive balance mismatch, want %v, got %v", i.expPos, pos)
		}
		if _, neg := node.getBalance(b.clock.Now()); neg != expval(i.expNeg) {
			t.Fatalf("Negative balance mismatch, want %v, got %v", i.expNeg, neg)
		}
	}
}

func TestBalanceReqCost(t *testing.T) {
	b := newBalanceTestSetup()
	defer b.stop()
	node := b.newNode()

	node.setCapacity(1000)
	node.SetFactors(PriceFactors{1, 0, 1}, PriceFactors{1, 0, 1})

	node.setBalance(expval(uint64(time.Minute)), expval(0)) // 1 minute time serving time allowance
	var inputs = []struct {
		reqCost uint64
		expPos  uint64
		expNeg  uint64
	}{
		{uint64(time.Second), uint64(time.Second * 59), 0},
		{0, uint64(time.Second * 59), 0},
		{uint64(time.Second * 59), 0, 0},
		{uint64(time.Second), 0, uint64(time.Second)},
	}
	for _, i := range inputs {
		node.requestCost(i.reqCost)
		if pos, _ := node.getBalance(b.clock.Now()); pos != expval(i.expPos) {
			t.Fatalf("Positive balance mismatch, want %v, got %v", i.expPos, pos)
		}
		if _, neg := node.getBalance(b.clock.Now()); neg != expval(i.expNeg) {
			t.Fatalf("Negative balance mismatch, want %v, got %v", i.expNeg, neg)
		}
	}
}

func TestBalanceToPriority(t *testing.T) {
	b := newBalanceTestSetup()
	defer b.stop()
	node := b.newNode()

	node.setCapacity(1000)
	node.SetFactors(PriceFactors{1, 0, 1}, PriceFactors{1, 0, 1})

	var inputs = []struct {
		pos      uint64
		neg      uint64
		priority int64
	}{
		{1000, 0, 1},
		{2000, 0, 2}, // Higher balance, higher priority value
		{0, 0, 0},
		{0, 1000, -1000},
	}
	for _, i := range inputs {
		node.setBalance(expval(i.pos), expval(i.neg))
		priority := node.Priority(b.clock.Now(), 1000)
		if priority != i.priority {
			t.Fatalf("Priority mismatch, want %v, got %v", i.priority, priority)
		}
	}
}

func TestEstimatedPriority(t *testing.T) {
	b := newBalanceTestSetup()
	defer b.stop()
	node := b.newNode()

	node.setCapacity(1000000000)
	node.SetFactors(PriceFactors{1, 0, 1}, PriceFactors{1, 0, 1})

	node.setBalance(expval(uint64(time.Minute)), expval(0))
	var inputs = []struct {
		runTime    time.Duration // time cost
		futureTime time.Duration // diff of future time
		reqCost    uint64        // single request cost
		priority   int64         // expected estimated priority
	}{
		{time.Second, time.Second, 0, 58},
		{0, time.Second, 0, 58},

		// 2 seconds time cost, 1 second estimated time cost, 10^9 request cost,
		// 10^9 estimated request cost per second.
		{time.Second, time.Second, 1000000000, 55},

		// 3 seconds time cost, 3 second estimated time cost, 10^9*2 request cost,
		// 4*10^9 estimated request cost.
		{time.Second, 3 * time.Second, 1000000000, 48},

		// All positive balance is used up
		{time.Second * 55, 0, 0, 0},

		// 1 minute estimated time cost, 4/58 * 10^9 estimated request cost per sec.
		{0, time.Minute, 0, -int64(time.Minute) - int64(time.Second)*120/29},
	}
	for _, i := range inputs {
		b.clock.Run(i.runTime)
		node.requestCost(i.reqCost)
		priority := node.EstMinPriority(b.clock.Now()+mclock.AbsTime(i.futureTime), 1000000000, true)
		if priority != i.priority {
			t.Fatalf("Estimated priority mismatch, want %v, got %v", i.priority, priority)
		}
	}
}

func TestCallbackChecking(t *testing.T) {
	b := newBalanceTestSetup()
	defer b.stop()
	node := b.newNode()

	node.setCapacity(1000000)
	node.SetFactors(PriceFactors{1, 0, 1}, PriceFactors{1, 0, 1})

	var inputs = []struct {
		priority int64
		expDiff  time.Duration
	}{
		{500, time.Millisecond * 500},
		{0, time.Second},
		{-int64(time.Second), 2 * time.Second},
	}
	node.setBalance(expval(uint64(time.Second)), expval(0))
	for _, i := range inputs {
		diff, _ := node.timeUntil(i.priority)
		if diff != i.expDiff {
			t.Fatalf("Time difference mismatch, want %v, got %v", i.expDiff, diff)
		}
	}
}

func TestCallback(t *testing.T) {
	b := newBalanceTestSetup()
	defer b.stop()
	node := b.newNode()

	node.setCapacity(1000)
	node.SetFactors(PriceFactors{1, 0, 1}, PriceFactors{1, 0, 1})

	callCh := make(chan struct{}, 1)
	node.setBalance(expval(uint64(time.Minute)), expval(0))
	node.addCallback(balanceCallbackZero, 0, func() { callCh <- struct{}{} })

	b.clock.Run(time.Minute)
	select {
	case <-callCh:
	case <-time.NewTimer(time.Second).C:
		t.Fatalf("Callback hasn't been called yet")
	}

	node.setBalance(expval(uint64(time.Minute)), expval(0))
	node.addCallback(balanceCallbackZero, 0, func() { callCh <- struct{}{} })
	node.removeCallback(balanceCallbackZero)

	b.clock.Run(time.Minute)
	select {
	case <-callCh:
		t.Fatalf("Callback shouldn't be called")
	case <-time.NewTimer(time.Millisecond * 100).C:
	}
}
