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
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	minPriorRatio      = float64(1) / 8
	allowanceExpFactor = 4
	retryNoPrior       = time.Second * 20
	depositTimeout     = time.Minute * 5
	retryDepositFail   = time.Minute * 30

	spentRatioLength    = 64
	spentRatioMax       = uint64(1) << 32
	spentRatioThreshold = spentRatioMax / 16
)

type paymentModule interface {
	Deposit(context context.Context, payees []common.Address, amounts []uint64, revealNumber uint64) (common.Hash, uint64, error)
	IssueCheque(payee common.Address, minAmount, maxAmount uint64) ([]*lotterybook.Cheque, error)
	Allowance(id common.Hash) map[common.Address][2]uint64 // total, remaining
	ListLotteries() []*lotterybook.Lottery
	SubscribeLotteryEvent(ch chan<- []lotterybook.LotteryEvent) event.Subscription
}

type blockchain interface {
	// CurrentHeader retrieves the current header from the local chain.
	CurrentHeader() *types.Header

	// SubscribeChainHeadEvent registers a subscription of ChainHeadEvent.
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
}

type DepositCreatorSetup struct {
	supportedFlag, CanPayFlag    nodestate.Flags
	allowanceField, addressField nodestate.Field
}

func NewDepositCreatorSetup(setup *nodestate.Setup) DepositCreatorSetup {
	return DepositCreatorSetup{
		CanPayFlag:     setup.NewFlag("CanPay"),
		allowanceField: setup.NewField("allowance", reflect.TypeOf(nodeAllowance{})),
	}
}

// Connect sets the fields and flags used by DepositCreator as an input
func (dcs *DepositCreatorSetup) Connect(supportedFlag nodestate.Flags, addressField nodestate.Field) {
	dcs.supportedFlag = supportedFlag
	dcs.addressField = addressField
}

type DepositCreator struct {
	DepositCreatorSetup
	lock sync.Mutex
	ns   *nodestate.NodeStateMachine
	pm   paymentModule
	bc   blockchain
	quit chan struct{}

	priorWeight                        func(*enode.Node) uint64
	priorThreshold, depositAmount      uint64
	depositLifetime, depositCreateTime uint64

	active              map[common.Hash]struct{}
	lastActive, pending *depositState

	pendingHook, activeHook, triggerHook func()
}

type nodeAllowance struct {
	oldRemain, newTotal, newRemain, future uint64
}

func NewDepositCreator(ns *nodestate.NodeStateMachine, setup DepositCreatorSetup, pm paymentModule, bc blockchain, priorWeight func(*enode.Node) uint64, priorThreshold, depositAmount uint64) *DepositCreator {
	return &DepositCreator{
		ns:                  ns,
		DepositCreatorSetup: setup,
		pm:                  pm,
		bc:                  bc,
		priorWeight:         priorWeight,
		priorThreshold:      priorThreshold,
		depositAmount:       depositAmount,
		depositLifetime:     10000,
		depositCreateTime:   100,
		quit:                make(chan struct{}),
		active:              make(map[common.Hash]struct{}),
	}
}

func (dc *DepositCreator) Start() {
	go dc.depositLoop()
}

func (dc *DepositCreator) Stop() {
	close(dc.quit)
}

func (dc *DepositCreator) depositLoop() {
	for _, l := range dc.pm.ListLotteries() {
		dc.active[l.Id] = struct{}{}
		if dc.lastActive == nil || l.RevealNumber > dc.lastActive.revealNumber {
			dc.lastActive = &depositState{
				id:           l.Id,
				revealNumber: l.RevealNumber,
				cost:         l.TransactionCost,
				triggerCh:    make(chan struct{}),
			}
		}
	}
	for id := range dc.active {
		dc.updateAllowances(id, true)
	}
	//TODO set pending deposit if there is one

	lotteryEventCh := make(chan []lotterybook.LotteryEvent, 10)
	lotterySub := dc.pm.SubscribeLotteryEvent(lotteryEventCh)
	defer lotterySub.Unsubscribe()
	chainEventCh := make(chan core.ChainHeadEvent, 10)
	chainSub := dc.bc.SubscribeChainHeadEvent(chainEventCh)
	defer chainSub.Unsubscribe()

	var (
		triggerCh chan struct{}
		triggerAt uint64
	)
	if dc.lastActive != nil {
		triggerCh = dc.lastActive.triggerCh
	} else {
		triggerCh = dc.createNewDeposit()
	}

	for {
		select {
		case ev := <-chainEventCh:
			if triggerAt != 0 && ev.Block.NumberU64() >= triggerAt {
				dc.lock.Lock()
				triggerCh = dc.createNewDeposit()
				triggerAt = 0
				dc.lock.Unlock()
			}
		case <-triggerCh:
			dc.lock.Lock()
			triggerCh = dc.createNewDeposit()
			triggerAt = 0
			dc.lock.Unlock()
		case e := <-lotteryEventCh:
			for _, ev := range e {
				switch ev.Status {
				case lotterybook.LotteryActive:
					dc.lock.Lock()
					if dc.pending != nil && dc.pending.id == ev.Id {
						dc.active[ev.Id] = struct{}{}
						dc.lastActive = dc.pending
						triggerAt = dc.lastActive.revealNumber - dc.depositCreateTime
						triggerCh = make(chan struct{})
						dc.lastActive.triggerCh = triggerCh
						dc.pending = nil
						dc.updateAllowances(ev.Id, true)
					} else {
						log.Error("Unexpected deposit activated", "id", ev.Id)
					}
					var setCanPayFlag []*enode.Node
					dc.ns.ForEach(dc.supportedFlag, dc.CanPayFlag, func(node *enode.Node, state nodestate.Flags) {
						if a, ok := dc.ns.GetField(node, dc.allowanceField).(nodeAllowance); ok && a.newRemain != 0 {
							setCanPayFlag = append(setCanPayFlag, node)
						}
					})
					dc.lock.Unlock()
					for _, node := range setCanPayFlag {
						dc.ns.SetState(node, dc.CanPayFlag, nodestate.Flags{}, 0)
					}

					if dc.activeHook != nil {
						dc.activeHook()
						dc.activeHook = nil
					}
				case lotterybook.LotteryRevealSoon:
					dc.lock.Lock()
					if _, ok := dc.active[ev.Id]; ok {
						delete(dc.active, ev.Id)
						dc.updateAllowances(ev.Id, false)
						if dc.lastActive != nil && dc.lastActive.id == ev.Id {
							dc.lastActive = nil
						}
					} else {
						log.Error("Unexpected deposit deactivated", "id", ev.Id)
					}
					dc.lock.Unlock()
				}
			}
		case <-dc.quit:
			return
		}
	}
}

func (dc *DepositCreator) newAllowances(totalAmount uint64) (payees []common.Address, amounts []uint64) {
	var sumPrior, sumFuture uint64
	prior := make(map[*enode.Node]uint64)
	dc.ns.ForEach(dc.supportedFlag, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		if p := dc.priorWeight(node); p > 0 {
			prior[node] = p
			sumPrior += p
		}
		if a, ok := dc.ns.GetField(node, dc.allowanceField).(nodeAllowance); ok {
			sumFuture += a.future
		}
	})
	if sumPrior < dc.priorThreshold {
		return
	}
	var scalePrior, scaleFuture float64
	if sumFuture > 0 {
		scaleFuture = float64(totalAmount) * (1 - minPriorRatio) / float64(sumFuture)
		if scaleFuture > 1 {
			scaleFuture = 1
		}
	}
	scalePrior = (float64(totalAmount) - float64(sumFuture)*scaleFuture) / float64(sumPrior)
	dc.ns.ForEach(dc.supportedFlag, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		var amount uint64
		if a, ok := dc.ns.GetField(node, dc.allowanceField).(nodeAllowance); ok {
			if scaleFuture == 1 {
				amount = a.future
				a.future = 0
			} else {
				amount = uint64(float64(a.future) * scaleFuture)
				a.future = (a.future - amount) / allowanceExpFactor
			}
			dc.ns.SetField(node, dc.allowanceField, a)
		}
		amount += uint64(float64(prior[node]) * scalePrior)
		if amount > 0 {
			address, _ := dc.ns.GetField(node, dc.addressField).(common.Address)
			payees = append(payees, address)
			amounts = append(amounts, amount)
		}
	})
	return
}

func (dc *DepositCreator) createNewDeposit() chan struct{} {
	retryCh := make(chan struct{})
	payees, amounts := dc.newAllowances(dc.depositAmount) //TODO keep track of user balance
	if payees == nil {
		time.AfterFunc(retryNoPrior, func() { close(retryCh) })
		return retryCh
	}
	revealNumber := dc.bc.CurrentHeader().Number.Uint64() + dc.depositLifetime
	go func() {
		ctx, _ := context.WithTimeout(context.Background(), depositTimeout)
		if id, cost, err := dc.pm.Deposit(ctx, payees, amounts, revealNumber); err == nil {
			if cost == 0 {
				cost = 1
			}
			dc.lock.Lock()
			dc.pending = &depositState{
				id:           id,
				revealNumber: revealNumber,
				cost:         cost,
				totalAmount:  dc.depositAmount,
			}
			dc.lock.Unlock()
			if dc.pendingHook != nil {
				dc.pendingHook()
				dc.pendingHook = nil
			}
		} else {
			log.Error("Failed to create new deposit for payment", "error", err)
			time.AfterFunc(retryDepositFail, func() { close(retryCh) })
		}
	}()
	return retryCh
}

func (dc *DepositCreator) updateAllowances(id common.Hash, add bool) {
	latest := dc.lastActive != nil && id == dc.lastActive.id
	allowances := dc.pm.Allowance(id)
	dc.ns.ForEach(dc.supportedFlag, nodestate.Flags{}, func(node *enode.Node, state nodestate.Flags) {
		address, _ := dc.ns.GetField(node, dc.addressField).(common.Address)
		allowance := allowances[address]
		if a, ok := dc.ns.GetField(node, dc.allowanceField).(nodeAllowance); ok || allowance[0] != 0 {
			if latest {
				if add {
					a.oldRemain += a.newRemain
					a.newTotal, a.newRemain = allowance[0], allowance[1]
					if a.newTotal != a.newRemain {
						dc.lastActive.balancedSpendingCost(a.newTotal, 0, a.newTotal-a.newRemain, true)
					}
				} else {
					if a.newTotal != allowance[0] || a.newRemain != allowance[1] {
						//TODO warning
					}
					a.newTotal, a.newRemain = 0, 0
				}
			} else {
				if add {
					a.oldRemain += allowance[1]
				} else {
					if a.oldRemain >= allowance[1] {
						a.oldRemain -= allowance[1]
					} else {
						//TODO warning
						a.oldRemain = 0
					}
				}
			}
			if a != (nodeAllowance{}) {
				dc.ns.SetField(node, dc.allowanceField, a)
			} else {
				dc.ns.SetField(node, dc.allowanceField, nil)
			}
		}
	})
}

func (dc *DepositCreator) Pay(node *enode.Node, minAmount, maxAmount uint64) (amount, cost uint64, proofOfPayment []byte) {
	address, ok := dc.ns.GetField(node, dc.addressField).(common.Address)
	if !ok {
		return 0, 0, nil
	}
	cheques, err := dc.pm.IssueCheque(address, minAmount, maxAmount)
	if err != nil {
		dc.ns.SetState(node, nodestate.Flags{}, dc.CanPayFlag, 0)
		return 0, 0, nil
	}
	proofOfPayment, err = rlp.EncodeToBytes(cheques)
	if err != nil {
		log.Error("Error encoding cheques", "error", err)
		return 0, 0, nil
	}
	dc.lock.Lock()
	defer dc.lock.Unlock()

	allowance, _ := dc.ns.GetField(node, dc.allowanceField).(nodeAllowance)
	spentBefore := allowance.newTotal - allowance.newRemain
	for _, c := range cheques {
		amount += c.Amount
		if dc.lastActive != nil && c.LotteryId == dc.lastActive.id {
			if allowance.newRemain >= c.Amount {
				allowance.newRemain -= c.Amount
			} else {
				allowance.newRemain = 0
			}
		} else {
			if allowance.oldRemain >= c.Amount {
				allowance.oldRemain -= c.Amount
			} else {
				allowance.oldRemain = 0
			}
		}
	}
	allowance.future += amount * allowanceExpFactor
	dc.ns.SetField(node, dc.allowanceField, allowance)
	cost = amount + dc.lastActive.balancedSpendingCost(allowance.newTotal, spentBefore, allowance.newTotal-allowance.newRemain, true)
	if dc.lastActive.triggerCh == nil && dc.triggerHook != nil {
		dc.triggerHook()
		dc.triggerHook = nil
	}
	return
}

func (dc *DepositCreator) EstimateCost(node *enode.Node, amount uint64) uint64 {
	dc.lock.Lock()
	defer dc.lock.Unlock()

	allowance, _ := dc.ns.GetField(node, dc.allowanceField).(nodeAllowance)
	if allowance.oldRemain >= amount {
		return amount
	}
	spendNow := amount - allowance.oldRemain // do not check new allowance exhaustion, let TokenBuyer try paying and fail
	spentBefore := allowance.newTotal - allowance.newRemain
	return amount + dc.lastActive.balancedSpendingCost(allowance.newTotal, spentBefore, spentBefore+spendNow, false)
}

type depositState struct {
	id                           common.Hash
	revealNumber                 uint64
	totalAmount, cost, accounted uint64
	spentRatio                   [spentRatioLength]uint64
	sumRatio                     uint64
	triggerCh                    chan struct{}
}

func (ds *depositState) scaleAmount(amount uint64) uint64 {
	return uint64((float64(amount) / float64(ds.totalAmount)) * float64(spentRatioMax))
}

func spentRatioIndex(allowance, spent uint64) (int, uint64) {
	ratio := float64(spent) / float64(allowance)
	if ratio > 1 {
		ratio = 1
	}
	pos := ratio * spentRatioLength
	index := int(pos)
	if index >= spentRatioLength {
		index = spentRatioLength - 1
	}
	partial := uint64((pos - float64(index)) * float64(allowance))
	if partial > allowance {
		partial = allowance
	}
	return index, partial
}

func addSpentRatio(r *uint64, a uint64, add bool) uint64 {
	v := *r
	if v >= spentRatioThreshold {
		return 0
	}
	n := v + a
	if n >= spentRatioThreshold {
		n = spentRatioThreshold
	}
	if add {
		*r = n
	}
	return n - v
}

func (ds *depositState) balancedSpendingCost(allowance, oldSpent, newSpent uint64, add bool) uint64 {
	if ds.sumRatio == spentRatioThreshold*spentRatioLength {
		return 0
	}
	allowance = ds.scaleAmount(allowance)
	oldSpent = ds.scaleAmount(oldSpent)
	newSpent = ds.scaleAmount(newSpent)
	oldIndex, oldPartial := spentRatioIndex(allowance, oldSpent)
	newIndex, newPartial := spentRatioIndex(allowance, newSpent)
	sum := ds.sumRatio
	if oldIndex == newIndex {
		sum += addSpentRatio(&ds.spentRatio[oldIndex], newPartial-oldPartial, add)
	} else {
		sum += addSpentRatio(&ds.spentRatio[oldIndex], allowance-oldPartial, add)
		for i := oldIndex + 1; i < newIndex; i++ {
			sum += addSpentRatio(&ds.spentRatio[i], allowance, add)
		}
		sum += addSpentRatio(&ds.spentRatio[newIndex], newPartial, add)
	}
	if newSpent > allowance {
		sum += (newSpent - allowance) * spentRatioLength
	}
	newTotalCost := uint64(float64(ds.cost) * (float64(sum) / float64(spentRatioThreshold*spentRatioLength*0.95)))
	if newTotalCost > ds.cost {
		newTotalCost = ds.cost
	}
	if newTotalCost < ds.accounted {
		newTotalCost = ds.accounted
	}
	cost := newTotalCost - ds.accounted
	if add {
		ds.sumRatio = sum
		ds.accounted = newTotalCost
		if ds.triggerCh != nil && newTotalCost == ds.cost {
			close(ds.triggerCh)
			ds.triggerCh = nil
		}
	}
	return cost
}
