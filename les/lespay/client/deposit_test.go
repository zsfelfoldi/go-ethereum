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
	"context"
	"errors"
	"math/big"
	"math/rand"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/contracts/lotterybook"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

const (
	dcTestNodeCount     = 32
	dcTestMaxPayment    = 1000
	dcTestDepositTxCost = 1000000
	dcTestDepositAmount = 10000000
)

var (
	dcTestSupportedFlag = testSetup.NewFlag("paymentSupported")
	dcTestAddressField  = testSetup.NewField("paymentAddress", reflect.TypeOf(common.Address{}))
	dcTestSetup         = NewDepositCreatorSetup(testSetup)
)

func init() {
	dcTestSetup.Connect(dcTestSupportedFlag, dcTestAddressField)
}

func TestDepositAmounts(t *testing.T) {
	dt := newDepositTestBackend()
	dt.nodePrice = func(int) float64 { return 1 }
	dt.nodesWorking = dcTestNodeCount / 8
	var expUsed, oldUsed float64
	priorUsed := float64(1) / 8
	for i := 0; i < 10; i++ {
		oldUsed += expUsed
		future := expUsed * allowanceExpFactor
		if future > 1-minPriorRatio {
			future = 1 - minPriorRatio
		}
		expUsed = future + priorUsed*(1-future)
		dt.runCycle(t, false)
		minExpTotal := uint64(dcTestDepositAmount * (oldUsed + expUsed*0.8))
		maxExpTotal := uint64(dcTestDepositAmount * (oldUsed + expUsed))
		if dt.totalAmount < minExpTotal || dt.totalAmount > maxExpTotal {
			t.Errorf("Total used deposit amount outside expected range (expected %d to %d, got %d)", minExpTotal, maxExpTotal, dt.totalAmount)
		}
	}
	dt.dc.Stop()
}

func TestDepositTrigger(t *testing.T) {
	dt := newDepositTestBackend()
	dt.nodePrice = func(int) float64 { return 1 }
	dt.nodesWorking = int((dcTestNodeCount - 1) * spentRatioThreshold / spentRatioMax) // not enough to trigger
	dt.runCycle(t, true)
	dt.nextDeposit()
	dt.runCycle(t, false) // now the working nodes have bigger allowance which is enough to trigger
	dt.dc.Stop()
}

func TestDepositCostAmortization(t *testing.T) {
	dt := newDepositTestBackend()
	balancedSpendingCost := float64(dcTestDepositTxCost) / float64(dcTestDepositAmount) * float64(spentRatioMax) / float64(spentRatioThreshold)
	count := int(dcTestNodeCount * spentRatioThreshold / spentRatioMax)
	dt.nodePrice = func(i int) float64 {
		if i < count {
			return 1
		}
		if i < count*2 {
			return 1 + balancedSpendingCost/2 // expect to spend allowance at this price
		}
		return 1 + balancedSpendingCost*2 // do not expect to spend allowance at this price
	}
	dt.runCycle(t, false)
	maxExpTotal := dcTestDepositAmount * uint64(count*2) / dcTestNodeCount
	minExpTotal := maxExpTotal * 8 / 10
	if dt.totalAmount < minExpTotal || dt.totalAmount > maxExpTotal {
		t.Errorf("Total used deposit amount outside expected range (expected %d to %d, got %d)", minExpTotal, maxExpTotal, dt.totalAmount)
	}
	dt.dc.Stop()
}

type testDeposit struct {
	id           common.Hash
	revealNumber uint64
	allowance    map[common.Address][2]uint64
}

type depositTestBackend struct {
	dc                     *DepositCreator
	lotteryFeed, chainFeed event.Feed
	depositCh              chan struct{}
	nodePrice              func(int) float64

	deposit      map[common.Hash]*testDeposit
	lastActive   common.Hash
	head         uint64
	nodesWorking int

	totalAmount, totalTxCost uint64
	expAmount, expTxCost     uint64
}

func newDepositTestBackend() *depositTestBackend {
	ns := nodestate.NewNodeStateMachine(nil, nil, &mclock.Simulated{}, testSetup)
	dt := &depositTestBackend{
		deposit:      make(map[common.Hash]*testDeposit),
		depositCh:    make(chan struct{}, 1),
		nodesWorking: dcTestNodeCount,
	}
	dt.dc = NewDepositCreator(ns, dcTestSetup, dt, dt, func(*enode.Node) uint64 { return 1 }, 1, dcTestDepositAmount)
	ns.Start()
	for i := 0; i < dcTestNodeCount; i++ {
		var addr common.Address
		rand.Read(addr[:])
		ns.SetState(testNode(i), dcTestSupportedFlag, nodestate.Flags{}, 0)
		ns.SetField(testNode(i), dcTestAddressField, addr)
	}
	dt.dc.Start()
	return dt
}

func (dt *depositTestBackend) runCycle(t *testing.T, expAllUsed bool) {
	<-dt.depositCh // new deposit activated
	dt.expAmount = dt.totalAmount + dcTestDepositAmount*uint64(dt.nodesWorking)/dcTestNodeCount
	dt.expTxCost = dt.totalTxCost + dcTestDepositTxCost
	for {
		if allUsed, newTriggered := dt.tryPay(); allUsed || newTriggered {
			if allUsed && !expAllUsed {
				t.Errorf("Entire deposit used, expected trigger")
			}
			if !allUsed && expAllUsed {
				t.Errorf("Unexpected deposit trigger, expected using the entire deposit")
			}
			if allUsed && dt.totalAmount != dt.expAmount {
				t.Errorf("Total used deposit amount mismatch (expected %d, got %d)", dt.expAmount, dt.totalAmount)
			}
			if !allUsed && dt.totalTxCost != dt.expTxCost {
				t.Errorf("Total transaction cost mismatch (expected %d, got %d)", dt.expTxCost, dt.totalTxCost)
			}
			return
		}
	}
}

func (dt *depositTestBackend) nextDeposit() {
	dt.head += dt.dc.depositLifetime
	dt.chainFeed.Send(core.ChainHeadEvent{Block: types.NewBlock(dt.CurrentHeader(), nil, nil, nil)})
}

func (dt *depositTestBackend) tryPay() (allUsed, newTriggered bool) {
	var (
		bestNode  *enode.Node
		bestPrice float64
	)
	dt.dc.ns.ForEach(dt.dc.CanPayFlag, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		nodeIndex := testNodeIndex(node.ID())
		if nodeIndex >= dt.nodesWorking {
			return
		}
		amount, cost := dt.dc.EstimateCost(node, 1, dcTestMaxPayment)
		if amount > 0 {
			price := float64(cost) * dt.nodePrice(nodeIndex) / float64(amount)
			if bestNode == nil || price < bestPrice {
				bestNode, bestPrice = node, price
			}
		}
	})
	if bestNode == nil {
		return true, false
	}
	dt.dc.triggerHook = func() {
		newTriggered = true
	}
	amount, cost, _ := dt.dc.Pay(bestNode, 1, dcTestMaxPayment)
	dt.totalAmount += amount
	dt.totalTxCost += cost - amount // cost includes transaction cost plus paid amount
	//fmt.Println("pay", testNodeIndex(bestNode.ID()), dt.totalAmount, dt.totalCost)
	//dt.dc.Pay(bestNode, 1, dcTestMaxPayment)
	/*
		for i := 0; i < dcTestNodeCount; i++ {
			a, _ := dt.dc.ns.GetField(testNode(i), dt.dc.allowanceField).(nodeAllowance)
			fmt.Print(a.newTotal-a.newRemain, " ")
		}
		fmt.Println()
	*/
	return
}

func (dt *depositTestBackend) Deposit(context context.Context, payees []common.Address, amounts []uint64, revealNumber uint64) (common.Hash, uint64, error) {
	var id common.Hash
	rand.Read(id[:])
	d := &testDeposit{
		id:           id,
		revealNumber: revealNumber,
		allowance:    make(map[common.Address][2]uint64),
	}
	for i, p := range payees {
		a := amounts[i]
		d.allowance[p] = [2]uint64{a, a}
	}
	dt.deposit[id] = d
	dt.dc.activeHook = func() {
		dt.depositCh <- struct{}{}
	}
	dt.dc.pendingHook = func() {
		dt.lastActive = id
		dt.lotteryFeed.Send([]lotterybook.LotteryEvent{{Id: id, Status: lotterybook.LotteryActive}})
	}
	return id, dcTestDepositTxCost, nil
}

func (dt *depositTestBackend) IssueCheque(payee common.Address, minAmount, maxAmount uint64) (cheques []*lotterybook.Cheque, err error) {
	var remain, paid uint64
	for _, d := range dt.deposit {
		remain += d.allowance[payee][1]
	}
	if remain < minAmount {
		return nil, errors.New("Insufficient allowance")
	}
	pay := func(id common.Hash) {
		a := dt.deposit[id].allowance[payee]
		var p uint64
		if paid+a[1] <= maxAmount {
			p = a[1]
		} else {
			p = maxAmount - paid
		}
		paid += p
		a[1] -= p
		dt.deposit[id].allowance[payee] = a
		if p > 0 {
			cheques = append(cheques, &lotterybook.Cheque{LotteryId: id, Amount: p})
		}
	}
	for id := range dt.deposit {
		if id != dt.lastActive {
			pay(id)
		}
	}
	pay(dt.lastActive)
	return
}

func (dt *depositTestBackend) Allowance(id common.Hash) map[common.Address][2]uint64 {
	return dt.deposit[id].allowance
}

func (dt *depositTestBackend) ListLotteries() []*lotterybook.Lottery {
	return nil
}

func (dt *depositTestBackend) SubscribeLotteryEvent(ch chan<- []lotterybook.LotteryEvent) event.Subscription {
	return dt.lotteryFeed.Subscribe(ch)
}

func (dt *depositTestBackend) CurrentHeader() *types.Header {
	return &types.Header{Number: big.NewInt(int64(dt.head))}
}

func (dt *depositTestBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return dt.chainFeed.Subscribe(ch)
}
