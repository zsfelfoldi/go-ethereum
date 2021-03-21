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
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/les/vflux"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	ErrNotConnected    = errors.New("client not connected")
	ErrNoPriority      = errors.New("priority too low to raise capacity")
	ErrCantFindMaximum = errors.New("Unable to find maximum allowed capacity")
)

const (
	tokenSaleTC     = time.Hour / 2
	tokenLimitTC    = time.Hour * 2
	basePriceTC     = time.Hour * 4
	targetFreeRatio = 0.1
)

// ClientPool implements a client database that assigns a priority to each client
// based on a positive and negative balance. Positive balance is externally assigned
// to prioritized clients and is decreased with connection time and processed
// requests (unless the price factors are zero). If the positive balance is zero
// then negative balance is accumulated.
//
// Balance tracking and priority calculation for connected clients is done by
// balanceTracker. PriorityQueue ensures that clients with the lowest positive or
// highest negative balance get evicted when the total capacity allowance is full
// and new clients with a better balance want to connect.
//
// Already connected nodes receive a small bias in their favor in order to avoid
// accepting and instantly kicking out clients. In theory, we try to ensure that
// each client can have several minutes of connection time.
//
// Balances of disconnected clients are stored in nodeDB including positive balance
// and negative banalce. Boeth positive balance and negative balance will decrease
// exponentially. If the balance is low enough, then the record will be dropped.
type ClientPool struct {
	*priorityPool
	*balanceTracker

	setup  *serverSetup
	clock  mclock.Clock
	closed bool
	ns     *nodestate.NodeStateMachine
	ndb    *nodeDB
	synced func() bool

	tokenSale                       *tokenSale
	tokenLock                       sync.Mutex
	capacityLimit, priorityCapacity uint64
	capacityFactor                  float64

	biasLock      sync.RWMutex
	connectedBias time.Duration

	minCap     uint64      // the minimal capacity value allowed for any client
	capReqNode *enode.Node // node that is requesting capacity change; only used inside NSM operation
}

// clientPeer represents a peer in the client pool. None of the callbacks should block.
type clientPeer interface {
	Node() *enode.Node
	FreeClientId() string                         // unique id for non-priority clients (typically a prefix of the network address)
	InactiveAllowance() time.Duration             // disconnection timeout for inactive non-priority peers
	UpdateCapacity(newCap uint64, requested bool) // signals a capacity update (requested is true if it is a result of a SetCapacity call on the given peer
	Disconnect()                                  // initiates disconnection (Unregister should always be called)
}

// NewClientPool creates a new client pool
func NewClientPool(balanceDb ethdb.KeyValueStore, minCap uint64, connectedBias time.Duration, clock mclock.Clock, synced func() bool) *ClientPool {
	setup := newServerSetup()
	ns := nodestate.NewNodeStateMachine(nil, nil, clock, setup.setup)
	ndb := newNodeDB(balanceDb, clock)
	cp := &ClientPool{
		priorityPool:   newPriorityPool(ns, setup, clock, minCap, connectedBias, 4, 100),
		balanceTracker: newBalanceTracker(ns, setup, ndb, clock, &utils.Expirer{}, &utils.Expirer{}),
		setup:          setup,
		ns:             ns,
		ndb:            ndb,
		clock:          clock,
		minCap:         minCap,
		connectedBias:  connectedBias,
		synced:         synced,
	}

	ns.SubscribeState(nodestate.MergeFlags(setup.activeFlag, setup.inactiveFlag, setup.priorityFlag), func(node *enode.Node, oldState, newState nodestate.Flags) {
		c, _ := ns.GetField(node, setup.clientField).(*clientInfo)
		if newState.Equals(setup.inactiveFlag) {
			// set timeout for non-priority inactive client
			var timeout time.Duration
			if c != nil {
				timeout = c.InactiveAllowance()
			}
			ns.AddTimeout(node, setup.inactiveFlag, timeout)
		}
		if oldState.Equals(setup.inactiveFlag) && newState.Equals(setup.inactiveFlag.Or(setup.priorityFlag)) {
			ns.SetStateSub(node, setup.inactiveFlag, nodestate.Flags{}, 0) // priority gained; remove timeout
		}
		if c != nil {
			// update priorityCapacity and token limit
			oldPri, newPri := oldState.HasAll(setup.priorityFlag), newState.HasAll(setup.priorityFlag)
			if oldPri != newPri {
				cp.tokenLock.Lock()
				if cp.priorityCapacity < c.lastPriorityCap {
					utils.Error("ClientPool state sub: negative priorityCapacity")
				}
				cp.priorityCapacity -= c.lastPriorityCap
				if newPri {
					cap, _ := ns.GetField(node, setup.capacityField).(uint64)
					cp.priorityCapacity += cap
					c.lastPriorityCap = cap
				} else {
					c.lastPriorityCap = 0
				}
				cp.updateTokenLimit()
				cp.tokenLock.Unlock()
			}
		}
		if newState.Equals(setup.activeFlag) {
			// active with no priority; limit capacity to minCap
			cap, _ := ns.GetField(node, setup.capacityField).(uint64)
			if cap > minCap {
				cp.requestCapacity(node, minCap, minCap, 0)
			}
		}
		if c != nil && newState.Equals(nodestate.Flags{}) {
			c.Disconnect()
		}
	})

	ns.SubscribeField(setup.capacityField, func(node *enode.Node, state nodestate.Flags, oldValue, newValue interface{}) {
		if c, ok := ns.GetField(node, setup.clientField).(*clientInfo); ok {
			newCap, _ := newValue.(uint64)
			c.UpdateCapacity(newCap, node == cp.capReqNode)
			if state.HasAll(setup.priorityFlag) {
				// update priorityCapacity and token limit
				cp.tokenLock.Lock()
				if cp.priorityCapacity < c.lastPriorityCap {
					utils.Error("ClientPool capacityField sub: negative priorityCapacity")
				}
				cp.priorityCapacity += newCap - c.lastPriorityCap
				c.lastPriorityCap = newCap
				cp.updateTokenLimit()
				cp.tokenLock.Unlock()
			}
		}
	})

	// add metrics
	cp.ns.SubscribeState(nodestate.MergeFlags(cp.setup.activeFlag, cp.setup.inactiveFlag), func(node *enode.Node, oldState, newState nodestate.Flags) {
		if oldState.IsEmpty() && !newState.IsEmpty() {
			clientConnectedMeter.Mark(1)
		}
		if !oldState.IsEmpty() && newState.IsEmpty() {
			clientDisconnectedMeter.Mark(1)
		}
		if oldState.HasNone(cp.setup.activeFlag) && oldState.HasAll(cp.setup.activeFlag) {
			clientActivatedMeter.Mark(1)
		}
		if oldState.HasAll(cp.setup.activeFlag) && oldState.HasNone(cp.setup.activeFlag) {
			clientDeactivatedMeter.Mark(1)
		}
		_, connected := cp.Active()
		totalConnectedGauge.Update(int64(connected))
	})
	return cp
}

func (cp *ClientPool) AddTokenSale(currencyID string, minBasePrice float64) {
	cp.tokenSale = newTokenSale(cp.balanceTracker, cp.ndb, cp.clock, currencyID, minBasePrice)
	cp.tokenSale.setBasePriceAdjustFactor(1 / float64(basePriceTC))
}

func (cp *ClientPool) AddPaymentReceiver(id string, pm paymentReceiver) {
	cp.tokenSale.addPaymentReceiver(id, pm)
}

// Start starts the client pool. Should be called before Register/Unregister.
func (cp *ClientPool) Start() {
	cp.ns.Start()
}

// Stop shuts the client pool down. The clientPeer interface callbacks will not be called
// after Stop. Register calls will return nil.
func (cp *ClientPool) Stop() {
	cp.balanceTracker.stop()
	cp.ns.Stop()
}

// Register registers the peer into the client pool. If the peer has insufficient
// priority and remains inactive for longer than the allowed timeout then it will be
// disconnected by calling the Disconnect function of the clientPeer interface.
func (cp *ClientPool) Register(peer clientPeer) ConnectedBalance {
	var fail bool
	cp.ns.Operation(func() {
		if cp.ns.GetField(peer.Node(), cp.setup.clientField) != nil {
			fail = true
			return
		}
		cp.ns.SetFieldSub(peer.Node(), cp.setup.clientField, &clientInfo{peer, 0})
	})
	if fail {
		return nil
	}
	balance, _ := cp.ns.GetField(peer.Node(), cp.setup.balanceField).(*nodeBalance)
	return balance
}

// Unregister removes the peer from the client pool
func (cp *ClientPool) Unregister(peer clientPeer) {
	cp.ns.Operation(func() {
		if c, ok := cp.ns.GetField(peer.Node(), cp.setup.clientField).(*clientInfo); ok {
			cp.tokenLock.Lock()
			if cp.priorityCapacity < c.lastPriorityCap {
				utils.Error("ClientPool.Unregister: negative priorityCapacity")
			}
			cp.priorityCapacity -= c.lastPriorityCap
			cp.updateTokenLimit()
			cp.tokenLock.Unlock()
			cp.ns.SetFieldSub(peer.Node(), cp.setup.clientField, nil)
		}
	})
}

// setConnectedBias sets the connection bias, which is applied to already connected clients
// So that already connected client won't be kicked out very soon and we can ensure all
// connected clients can have enough time to request or sync some data.
func (cp *ClientPool) SetConnectedBias(bias time.Duration) {
	cp.biasLock.Lock()
	cp.connectedBias = bias
	cp.setActiveBias(bias)
	cp.biasLock.Unlock()
}

// SetLimits sets the maximum number and total capacity of simultaneously active nodes
func (cp *ClientPool) SetLimits(maxCount, maxCap uint64) {
	cp.setLimits(maxCount, maxCap)
	cp.tokenLock.Lock()
	cp.capacityLimit = maxCap
	if cp.priorityCapacity > cp.capacityLimit {
		utils.Error("ClientPool.SetLimits: priorityCapacity > capacityLimit")
	}
	cp.updateTokenLimit()
	cp.tokenLock.Unlock()
}

// SetDefaultFactors sets the default price factors applied to subsequently connected clients
func (cp *ClientPool) SetDefaultFactors(posFactors, negFactors PriceFactors) {
	cp.setDefaultFactors(posFactors, negFactors)
	cp.tokenLock.Lock()
	cp.capacityFactor = posFactors.CapacityFactor
	cp.updateTokenLimit()
	cp.tokenLock.Unlock()
}

func (cp *ClientPool) updateTokenLimit() {
	if cp.tokenSale == nil {
		return
	}
	maxTokenLimit := float64(tokenSaleTC) * cp.capacityFactor * float64(cp.capacityLimit)
	freeRatio := 1 - float64(cp.priorityCapacity)/float64(cp.capacityLimit)
	cp.tokenSale.setLimitAdjustRate(int64(maxTokenLimit), (freeRatio-targetFreeRatio)*maxTokenLimit/float64(tokenLimitTC))
}

// SetCapacity sets the assigned capacity of a connected client
func (cp *ClientPool) SetCapacity(node *enode.Node, reqCap uint64, bias time.Duration, requested bool) (capacity uint64, err error) {
	cp.biasLock.RLock()
	if cp.connectedBias > bias {
		bias = cp.connectedBias
	}
	cp.biasLock.RUnlock()

	cp.ns.Operation(func() {
		balance, _ := cp.ns.GetField(node, cp.setup.balanceField).(*nodeBalance)
		if balance == nil {
			err = ErrNotConnected
			return
		}
		capacity, _ = cp.ns.GetField(node, cp.setup.capacityField).(uint64)
		if capacity == 0 {
			// if the client is inactive then it has insufficient priority for the minimal capacity
			// (will be activated automatically with minCap when possible)
			return
		}
		if reqCap < cp.minCap {
			// can't request less than minCap; switching between 0 (inactive state) and minCap is
			// performed by the server automatically as soon as necessary/possible
			reqCap = cp.minCap
		}
		if reqCap > cp.minCap && cp.ns.GetState(node).HasNone(cp.setup.priorityFlag) {
			err = ErrNoPriority
			return
		}
		if reqCap == capacity {
			return
		}
		if requested {
			// mark the requested node so that the UpdateCapacity callback can signal
			// whether the update is the direct result of a SetCapacity call on the given node
			cp.capReqNode = node
			defer func() {
				cp.capReqNode = nil
			}()
		}

		var minTarget, maxTarget uint64
		if reqCap > capacity {
			// Estimate maximum available capacity at the current priority level and request
			// the estimated amount.
			// Note: requestCapacity could find the highest available capacity between the
			// current and the requested capacity but it could cost a lot of iterations with
			// fine step adjustment if the requested capacity is very high. By doing a quick
			// estimation of the maximum available capacity based on the capacity curve we
			// can limit the number of required iterations.
			curve := cp.getCapacityCurve().exclude(node.ID())
			maxTarget = curve.maxCapacity(func(capacity uint64) int64 {
				return balance.estimatePriority(capacity, 0, 0, bias, false)
			})
			if maxTarget <= capacity {
				return
			}
			if maxTarget > reqCap {
				maxTarget = reqCap
			}
			// Specify a narrow target range that allows a limited number of fine step
			// iterations
			minTarget = maxTarget - maxTarget/20
			if minTarget < capacity {
				minTarget = capacity
			}
		} else {
			minTarget, maxTarget = reqCap, reqCap
		}
		if newCap := cp.requestCapacity(node, minTarget, maxTarget, bias); newCap >= minTarget && newCap <= maxTarget {
			capacity = newCap
			return
		}
		// we should be able to find the maximum allowed capacity in a few iterations
		log.Error("Unable to find maximum allowed capacity")
		err = ErrCantFindMaximum
	})
	return
}

// serveCapQuery serves a vflux capacity query. It receives multiple token amount values
// and a bias time value. For each given token amount it calculates the maximum achievable
// capacity in case the amount is added to the balance.
func (cp *ClientPool) serveCapQuery(id enode.ID, freeID string, data []byte) []byte {
	var req vflux.CapacityQueryRequest
	if rlp.DecodeBytes(data, &req) != nil {
		return nil
	}
	if l := len(req.AddTokens); l == 0 || l > vflux.CapacityQueryMaxLen {
		return nil
	}
	result := make(vflux.CapacityQueryReply, len(req.AddTokens))
	if !cp.synced() {
		capacityQueryZeroMeter.Mark(1)
		reply, _ := rlp.EncodeToBytes(&result)
		return reply
	}

	bias := time.Second * time.Duration(req.Bias)
	cp.biasLock.RLock()
	if cp.connectedBias > bias {
		bias = cp.connectedBias
	}
	cp.biasLock.RUnlock()

	// use capacityCurve to answer request for multiple newly bought token amounts
	curve := cp.getCapacityCurve().exclude(id)
	cp.BalanceOperation(id, freeID, nil, func(balance AtomicBalanceOperator) {
		pb, _ := balance.GetBalance()
		for i, addTokens := range req.AddTokens {
			add := addTokens.Int64()
			result[i] = curve.maxCapacity(func(capacity uint64) int64 {
				return balance.estimatePriority(capacity, add, 0, bias, false) / int64(capacity)
			})
			if add <= 0 && uint64(-add) >= pb && result[i] > cp.minCap {
				result[i] = cp.minCap
			}
			if result[i] < cp.minCap {
				result[i] = 0
			}
		}
	})
	// add first result to metrics (don't care about priority client multi-queries yet)
	if result[0] == 0 {
		capacityQueryZeroMeter.Mark(1)
	} else {
		capacityQueryNonZeroMeter.Mark(1)
	}
	reply, _ := rlp.EncodeToBytes(&result)
	return reply
}

// Handle implements Service
func (cp *ClientPool) Handle(id enode.ID, address string, name string, data []byte) []byte {
	switch name {
	case vflux.CapacityQueryName:
		return cp.serveCapQuery(id, address, data)
	default:
		if cp.tokenSale != nil {
			return cp.tokenSale.handle(id, address, name, data)
		}
		return nil
	}
}
