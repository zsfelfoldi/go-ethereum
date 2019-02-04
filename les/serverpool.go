// Copyright 2016 The go-ethereum Authors
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
	"crypto/ecdsa"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// After a connection has been ended or timed out, there is a waiting period
	// before it can be selected for connection again.
	// waiting period = base delay * (1 + random(1))
	// base delay = shortRetryDelay for the first shortRetryCnt times after a
	// successful connection, after that longRetryDelay is applied

	newRetryDelay      = time.Second * 30
	newRetryDelayExp   = 0.1
	knownRetryDelay    = time.Second * 30
	knownRetryDelayExp = 0.1

	dialPeriod = time.Second

	// maxNewEntries is the maximum number of newly discovered (never connected) nodes.
	// If the limit is reached, the least recently discovered one is thrown out.
	maxNewEntries = 1000
	// maxKnownEntries is the maximum number of known (already connected) nodes.
	// If the limit is reached, the least recently connected one is thrown out.
	// (note that unlike new entries, known entries are persistent)
	maxKnownEntries = 1000
	// target count for servers selected from the newly discovered and the known tables
	targetNewSelect   = 3
	targetKnownSelect = 3
	// after dialTimeout, consider the server unavailable and adjust statistics
	dialTimeout = time.Second * 30
	// targetConnTime is the minimum expected connection duration before a server
	// drops a client without any specific reason
	targetConnTime = time.Minute * 10
	// new entry selection weight calculation based on most recent discovery time:
	// unity until discoverExpireStart, then exponential decay with discoverExpireConst
	discoverExpireStart = time.Minute * 20
	discoverExpireConst = time.Minute * 20
	// known entry selection weight is dropped by a factor of exp(-failDropLn) after
	// each unsuccessful connection (restored after a successful one)
	failDropLn = 0.01
	// node address selection weight is dropped by a factor of exp(-addrFailDropLn) after
	// each unsuccessful connection (restored after a successful one)
	addrFailDropLn = 0.1
	// responseScoreTC and delayScoreTC are exponential decay time constants for
	// calculating selection chances from response times and block delay times
	responseScoreTC = time.Millisecond * 500
	delayScoreTC    = time.Second * 5
	// initStatsWeight is used to initialize previously unknown peers with good
	// statistics to give a chance to prove themselves
	initStatsWeight = 1
)

const (
	peConnect = iota
	peRegister
	peUpdateParams
	peDisconnect
)

type (
	peerEvent struct {
		eventType int
		req       interface{}
	}

	// connReq represents a request for peer connection.
	connReq struct {
		p      *peer
		node   *enode.Node
		result chan *poolEntry
	}

	// disconnReq represents a request for peer disconnection.
	disconnReq struct {
		entry *poolEntry
		done  chan struct{}
	}

	// registerReq represents a request for peer registration.
	registerReq struct {
		entry     *poolEntry
		done      chan struct{}
		capacity  float64
		costTable requestCostTable
	}

	// updateParamsReq represents a request for parameter update.
	updateParamsReq struct {
		entry     *poolEntry
		capacity  float64
		costTable requestCostTable
	}
)

// poolStatAdjust records are sent to adjust peer block delay/response time statistics
type poolStatAdjust struct {
	entry                      *poolEntry
	responseTime               time.Duration
	refCost, maxCost, recharge uint64
}

// serverPool implements a pool for storing and selecting newly discovered and already
// known light server nodes. It received discovered nodes, stores statistics about
// known nodes and takes care of always having enough good quality servers connected.
type serverPool struct {
	db     ethdb.Database
	dbKey  []byte
	server *p2p.Server
	quit   chan struct{}
	wg     *sync.WaitGroup

	topic discv5.Topic

	discSetPeriod chan time.Duration
	discNodes     chan *enode.Node
	discLookups   chan bool

	trustedNodes map[enode.ID]*enode.Node
	entries      map[enode.ID]*poolEntry
	timeout      chan *poolEntry
	dialCh       chan *dialCycle
	adjustStats  chan poolStatAdjust

	peerEvents chan peerEvent

	knownQueue, newQueue           poolEntryQueue
	knownSelect, newSelect         *weightedRandomSelect
	knownDialCycle, newDialCycle   *dialCycle
	knownDelayQueue, newDelayQueue *retryDelayQueue
	fastDiscover                   bool
	connCh                         chan *connReq
	disconnCh                      chan *disconnReq
	registerCh                     chan *registerReq

	reqCounter *requestCounter
}

// newServerPool creates a new serverPool instance
func newServerPool(db ethdb.Database, quit chan struct{}, wg *sync.WaitGroup, trustedNodes []string) *serverPool {
	pool := &serverPool{
		db:              db,
		quit:            quit,
		wg:              wg,
		entries:         make(map[enode.ID]*poolEntry),
		timeout:         make(chan *poolEntry, 1),
		adjustStats:     make(chan poolStatAdjust, 100),
		dialCh:          make(chan *dialCycle, 2),
		peerEvents:      make(chan peerEvent),
		knownSelect:     newWeightedRandomSelect(),
		newSelect:       newWeightedRandomSelect(),
		knownDelayQueue: (*retryDelayQueue)(prque.New(nil)),
		newDelayQueue:   (*retryDelayQueue)(prque.New(nil)),
		fastDiscover:    true,
		trustedNodes:    parseTrustedNodes(trustedNodes),
		reqCounter:      newRequestCounter(),
	}
	pool.knownDialCycle = &dialCycle{pool: pool, period: dialPeriod, sel: pool.knownSelect, suspended: true, required: targetKnownSelect}
	pool.newDialCycle = &dialCycle{pool: pool, period: dialPeriod, sel: pool.newSelect, suspended: true, required: targetNewSelect}
	pool.knownQueue = newPoolEntryQueue(maxKnownEntries, pool.removeEntry)
	pool.newQueue = newPoolEntryQueue(maxNewEntries, pool.removeEntry)
	return pool
}

func (pool *serverPool) start(server *p2p.Server, topic discv5.Topic) {
	pool.server = server
	pool.topic = topic
	pool.dbKey = append([]byte("serverPool/"), []byte(topic)...)
	pool.wg.Add(1)
	pool.loadPoolState()
	pool.connectToTrustedNodes()

	if pool.server.DiscV5 != nil {
		pool.discSetPeriod = make(chan time.Duration, 1)
		pool.discNodes = make(chan *enode.Node, 100)
		pool.discLookups = make(chan bool, 100)
		go pool.discoverNodes()
	}
	pool.knownDialCycle.resume()
	pool.newDialCycle.resume()
	go pool.eventLoop()
}

// discoverNodes wraps SearchTopic, converting result nodes to enode.Node.
func (pool *serverPool) discoverNodes() {
	ch := make(chan *discv5.Node)
	go func() {
		pool.server.DiscV5.SearchTopic(pool.topic, pool.discSetPeriod, ch, pool.discLookups)
		close(ch)
	}()
	for n := range ch {
		pubkey, err := decodePubkey64(n.ID[:])
		if err != nil {
			continue
		}
		pool.discNodes <- enode.NewV4(pubkey, n.IP, int(n.TCP), int(n.UDP))
	}
}

func paramsToCapacity(params flowcontrol.ServerParams) float64 {
	c := params.BufLimit / bufLimitRatio
	if params.MinRecharge < c {
		c = params.MinRecharge
	}
	return float64(c)
}

// connect should be called upon any incoming connection. If the connection has been
// dialed by the server pool recently, the appropriate pool entry is returned.
// Otherwise, the connection should be rejected.
// Note that whenever a connection has been accepted and a pool entry has been returned,
// disconnect should also always be called.
func (pool *serverPool) connect(p *peer, node *enode.Node) *poolEntry {
	log.Debug("Connect new entry", "enode", p.id)
	req := &connReq{p: p, node: node, result: make(chan *poolEntry, 1)}
	select {
	case pool.peerEvents <- peerEvent{eventType: peConnect, req: req}:
	case <-pool.quit:
		return nil
	}
	return <-req.result
}

// registered should be called after a successful handshake
func (pool *serverPool) registered(entry *poolEntry, params flowcontrol.ServerParams, costTable requestCostTable) {
	log.Debug("Registered new entry", "enode", entry.node.ID())
	req := &registerReq{entry: entry, done: make(chan struct{}), capacity: paramsToCapacity(params), costTable: costTable}
	select {
	case pool.peerEvents <- peerEvent{eventType: peRegister, req: req}:
	case <-pool.quit:
		return
	}
	<-req.done
}

// updateParams should be called after in case of a flow control parameter update from the server side
func (pool *serverPool) updateParams(entry *poolEntry, params flowcontrol.ServerParams, costTable requestCostTable) {
	log.Debug("Updated parameters for peer", "enode", entry.node.ID())
	req := &registerReq{entry: entry, capacity: paramsToCapacity(params), costTable: costTable}
	select {
	case pool.peerEvents <- peerEvent{eventType: peUpdateParams, req: req}:
	case <-pool.quit:
		return
	}
}

// disconnect should be called when ending a connection. Service quality statistics
// can be updated optionally (not updated if no registration happened, in this case
// only connection statistics are updated, just like in case of timeout)
func (pool *serverPool) disconnect(entry *poolEntry) {
	log.Debug("Disconnected old entry", "enode", entry.node.ID())
	req := &disconnReq{entry: entry, done: make(chan struct{})}
	select {
	case pool.peerEvents <- peerEvent{eventType: peDisconnect, req: req}:
	case <-pool.quit:
		return
	}
	<-req.done
}

// adjustStats adjusts the request cost and response time statistics of a node
func (pool *serverPool) adjustStats(entry *poolEntry, responseTime time.Duration, refCost, maxCost, recharge uint64) {
	if entry == nil {
		return
	}
	pool.adjustStats <- poolStatAdjust{entry: entry, time: time, responseTime: responseTime, refCost: refCost, maxCost: maxCost, recharge: recharge}
}

// adjustCorrFactor adjusts the bandwidth correction statistics of a node
func (pool *serverPool) adjustCorrFactor(entry *poolEntry, cost, recharge uint64) {
	if entry == nil {
		return
	}
	pool.adjustStats <- poolStatAdjust{adjustType: pseCorrFactor, entry: entry, cost: cost, recharge: recharge}
}

// eventLoop handles pool events and mutex locking for all internal functions
func (pool *serverPool) eventLoop() {
	lookupCnt := 0
	var convTime mclock.AbsTime
	if pool.discSetPeriod != nil {
		pool.discSetPeriod <- time.Millisecond * 100
	}

	// disconnect updates service quality statistics depending on the connection time
	// and disconnection initiator.
	disconnect := func(entry *poolEntry, stopped bool) {
		// Handle peer disconnection requests.
		if entry.state == psRegistered {
			connAdjust := float64(mclock.Now()-entry.regTime) / float64(targetConnTime)
			if connAdjust > 1 {
				connAdjust = 1
			}
			if stopped {
				// disconnect requested by ourselves.
				entry.connectStats.add(1, connAdjust)
			} else {
				// disconnect requested by server side.
				entry.connectStats.add(connAdjust, 1)
			}
			entry.addSessionStats()
		}
		entry.state = psNotConnected
		pool.server.RemovePeer(entry.node)

		entry.dialedBy.addConnected(-1)
		pool.knownDelayQueue.schedule((*knownEntry)(entry))
		pool.newDelayQueue.schedule((*discoveredEntry)(entry))
	}

	for {
		select {
		case entry := <-pool.timeout:
			if !entry.removed {
				pool.checkDialTimeout(entry)
			}

		case dial := <-pool.dialCh:
			pool.knownDelayQueue.check()
			pool.newDelayQueue.check()
			if entry := dial.next(); entry != nil {
				pool.dial(entry, dial)
			}

		case adj := <-pool.adjustStats:
			adj.entry.sessionValue += adj.refCost
			adj.entry.sessionRecharge += adj.recharge
			adj.entry.sessionMaxCost += adj.maxCost
			adj.entry.responseStats.add(adj.responseTime)

		case node := <-pool.discNodes:
			if pool.trustedNodes[node.ID()] == nil {
				entry := pool.findOrNewNode(node)
				pool.newSelect.update((*discoveredEntry)(entry))
				pool.newDialCycle.resume()
			}

		case conv := <-pool.discLookups:
			if conv {
				if lookupCnt == 0 {
					convTime = mclock.Now()
				}
				lookupCnt++
				if pool.fastDiscover && (lookupCnt == 50 || time.Duration(mclock.Now()-convTime) > time.Minute) {
					pool.fastDiscover = false
					if pool.discSetPeriod != nil {
						pool.discSetPeriod <- time.Minute
					}
				}
			}

		case ev := <-pool.peerEvents:
			switch ev.eventType {
			case peConnect:
				req := ev.req.(*connReq)
				if pool.trustedNodes[req.p.ID()] != nil {
					// ignore trusted nodes
					req.result <- nil
				} else {
					// Handle peer connection requests.
					entry := pool.entries[req.p.ID()]
					if entry == nil {
						entry = pool.findOrNewNode(req.node)
					}
					if entry.state == psConnected || entry.state == psRegistered {
						req.result <- nil
						continue
					}
					entry.peer = req.p
					entry.state = psConnected
					addr := &poolEntryAddress{
						ip:       req.node.IP(),
						port:     uint16(req.node.TCP()),
						lastSeen: mclock.Now(),
					}
					entry.lastConnected = addr
					entry.addr = make(map[string]*poolEntryAddress)
					entry.addr[addr.strKey()] = addr
					entry.addrSelect = *newWeightedRandomSelect()
					entry.addrSelect.update(addr)
					req.result <- entry
				}

			case peRegister:
				req := ev.req.(*registerReq)
				// Handle peer registration requests.
				entry := req.entry
				entry.capacity = req.capacity
				entry.costTable = req.costTable
				entry.cachedRefCost = 0
				entry.state = psRegistered
				entry.regTime = mclock.Now()
				entry.lastFreeCapStatsUpdate = entry.regTime
				if !entry.known {
					pool.newQueue.remove(entry)
					entry.known = true
				}
				pool.knownQueue.setLatest(entry)
				close(req.done)

			case peUpdateParams:
				req := ev.req.(*updateParamsReq)
				// Handle peer parameter updates
				if req.entry.costTable != req.costTable {
					req.entry.addSessionStats()
					req.entry.costTable = req.costTable
				}
				req.entry.capacity = req.capacity

			case peDisconnect:
				req := ev.req.(*disconnReq)
				// Handle peer disconnection requests.
				disconnect(req.entry, false)
				close(req.done)
			}

		case <-pool.quit:
			if pool.discSetPeriod != nil {
				close(pool.discSetPeriod)
			}
			for _, entry := range pool.entries {
				disconnect(entry, true)
			}
			pool.savePoolState()
			pool.wg.Done()
			return
		}
	}
}

func (pool *serverPool) findOrNewNode(node *enode.Node) *poolEntry {
	now := mclock.Now()
	entry := pool.entries[node.ID()]
	if entry == nil {
		log.Debug("Discovered new entry", "id", node.ID())
		entry = &poolEntry{
			pool:       pool,
			node:       node,
			addr:       make(map[string]*poolEntryAddress),
			addrSelect: *newWeightedRandomSelect(),
		}
		entry.initMaxWeights()
		pool.entries[node.ID()] = entry
		// initialize previously unknown peers with good statistics to give a chance to prove themselves
		entry.connectStats.add(1, initStatsWeight)
		entry.delayStats.add(0, initStatsWeight)
		entry.responseStats.add(0, initStatsWeight)
		entry.timeoutStats.add(0, initStatsWeight)
	}
	entry.lastDiscovered = now
	addr := &poolEntryAddress{ip: node.IP(), port: uint16(node.TCP())}
	if a, ok := entry.addr[addr.strKey()]; ok {
		addr = a
	} else {
		entry.addr[addr.strKey()] = addr
	}
	addr.lastSeen = now
	entry.addrSelect.update(addr)
	if !entry.known {
		pool.newQueue.setLatest(entry)
	}
	return entry
}

// loadPoolState loads server pool state from the database
func (pool *serverPool) loadPoolState() {
	enc, err := pool.db.Get(pool.dbKey)
	if err != nil {
		return
	}
	var poolState struct {
		List          []*poolEntry
		ReqCounter    *requestCounter
		ResponseStats *responseTimeStats
	}
	poolState.ReqCounter = pool.reqCounter
	poolState.ResponseStats = pool.responseStats
	err = rlp.DecodeBytes(enc, &poolState)
	if err != nil {
		log.Debug("Failed to decode node list", "err", err)
		return
	}
	for _, e := range poolState.List {
		log.Info("Loaded server stats", "id", e.node.ID(), "fails", e.lastConnected.fails,
			"conn", fmt.Sprintf("%v/%v", e.connectStats.recentAvg(), e.connectStats.weight),
			"delay", fmt.Sprintf("%v/%v", time.Duration(e.delayStats.recentAvg()), e.delayStats.weight),
			"response", fmt.Sprintf("%v/%v", time.Duration(e.responseStats.recentAvg()), e.responseStats.weight),
			"timeout", fmt.Sprintf("%v/%v", e.timeoutStats.recentAvg(), e.timeoutStats.weight),
			"bwCorr", fmt.Sprintf("%v/%v", e.bwCorrStats.recentAvg(), e.bwCorrStats.weight),
			"freeCap", fmt.Sprintf("%v/%v", e.freeCapStats.recentAvg(), e.freeCapStats.weight),
			"weight", fmt.Sprintf("%v", (*knownEntry)(e).Weight()))
		e.pool = pool
		e.initMaxWeights()
		pool.entries[e.node.ID()] = e
		if pool.trustedNodes[e.node.ID()] == nil {
			pool.knownQueue.setLatest(e)
			pool.knownSelect.update((*knownEntry)(e))
		}
	}
}

// connectToTrustedNodes adds trusted server nodes as static trusted peers.
//
// Note: trusted nodes are not handled by the server pool logic, they are not
// added to either the known or new selection pools. They are connected/reconnected
// by p2p.Server whenever possible.
func (pool *serverPool) connectToTrustedNodes() {
	//connect to trusted nodes
	for _, node := range pool.trustedNodes {
		pool.server.AddTrustedPeer(node)
		pool.server.AddPeer(node)
		log.Debug("Added trusted node", "id", node.ID().String())
	}
}

// parseTrustedNodes returns valid and parsed enodes
func parseTrustedNodes(trustedNodes []string) map[enode.ID]*enode.Node {
	nodes := make(map[enode.ID]*enode.Node)

	for _, node := range trustedNodes {
		node, err := enode.ParseV4(node)
		if err != nil {
			log.Warn("Trusted node URL invalid", "enode", node, "err", err)
			continue
		}
		nodes[node.ID()] = node
	}
	return nodes
}

// savePoolState saves server pool state into the database. Nodes are
// ordered from least to most recently connected.
func (pool *serverPool) savePoolState() {
	var poolState struct {
		List          []*poolEntry
		ReqCounter    *requestCounter
		ResponseStats *responseTimeStats
	}
	poolState.List = make([]*poolEntry, len(pool.knownQueue.queue))
	for i := range poolState.List {
		poolState.List[i] = pool.knownQueue.fetchOldest()
	}
	poolState.ReqCounter = pool.reqCounter
	poolState.ResponseStats = pool.responseStats
	enc, err := rlp.EncodeToBytes(poolState)
	if err == nil {
		pool.db.Put(pool.dbKey, enc)
	}
}

// removeEntry removes a pool entry when the entry count limit is reached.
// Note that it is called by the new/known queues from which the entry has already
// been removed so removing it from the queues is not necessary.
func (pool *serverPool) removeEntry(entry *poolEntry) {
	pool.newSelect.remove((*discoveredEntry)(entry))
	pool.knownSelect.remove((*knownEntry)(entry))
	entry.removed = true
	delete(pool.entries, entry.node.ID())
}

// dial initiates a new connection
func (pool *serverPool) dial(entry *poolEntry, dialedBy *dialCycle) {
	if pool.server == nil || entry.state != psNotConnected {
		return
	}
	entry.state = psDialed
	entry.dialedBy = dialedBy
	dialedBy.addConnected(1)
	addr := entry.addrSelect.choose().(*poolEntryAddress)
	log.Debug("Dialing new peer", "lesaddr", entry.node.ID().String()+"@"+addr.strKey(), "set", len(entry.addr), "known", dialedBy == pool.knownDialCycle)
	entry.dialed = addr
	go func() {
		pool.server.AddPeer(entry.node)
		select {
		case <-pool.quit:
		case <-time.After(dialTimeout):
			select {
			case <-pool.quit:
			case pool.timeout <- entry:
			}
		}
	}()
}

// checkDialTimeout checks if the node is still in dialed state and if so, resets it
// and adjusts connection statistics accordingly.
func (pool *serverPool) checkDialTimeout(entry *poolEntry) {
	if entry.state != psDialed {
		return
	}
	log.Debug("Dial timeout", "lesaddr", entry.node.ID().String()+"@"+entry.dialed.strKey())
	entry.state = psNotConnected
	pool.server.RemovePeer(entry.node)
	entry.dialedBy.addConnected(-1)
	pool.knownDelayQueue.schedule((*knownEntry)(entry))
	pool.newDelayQueue.schedule((*discoveredEntry)(entry))
	entry.connectStats.add(0, 1)
	entry.dialed.fails++
	if entry.dialedBy == pool.knownDialCycle {
		entry.knownDialFails++
	} else {
		entry.newDialFails++
	}
}

type dialCycle struct {
	pool                *serverPool
	period              time.Duration
	sel                 *weightedRandomSelect
	suspended           bool
	connected, required int
}

func (dc *dialCycle) next() *poolEntry {
	if dc.connected >= dc.required {
		dc.suspended = true
		return nil
	}
	item := dc.sel.choose()
	if item == nil {
		dc.suspended = true
		return nil
	}
	time.AfterFunc(dc.period, func() {
		select {
		case dc.pool.dialCh <- dc:
		case <-dc.pool.quit:
		}
	})
	if entry, ok := item.(*knownEntry); ok {
		return (*poolEntry)(entry)
	}
	entry := item.(*discoveredEntry)
	return (*poolEntry)(entry)
}

func (dc *dialCycle) resume() {
	if !dc.suspended {
		return
	}
	dc.suspended = false
	select {
	case dc.pool.dialCh <- dc:
	case <-dc.pool.quit:
	}
}

func (dc *dialCycle) addConnected(diff int) {
	dc.connected += diff
	if dc.suspended && dc.connected < dc.required {
		dc.resume()
	}
}

const (
	psNotConnected = iota
	psDialed
	psConnected
	psRegistered
)

// poolEntry represents a server node and stores its current state and statistics.
type poolEntry struct {
	pool                  *serverPool
	peer                  *peer
	pubkey                [64]byte // secp256k1 key of the node
	addr                  map[string]*poolEntryAddress
	node                  *enode.Node
	lastConnected, dialed *poolEntryAddress
	addrSelect            weightedRandomSelect

	totalRecharge, totalValue, sessionValue float64
	sessionRecharge, sessionMaxCost         uint64
	responseStats                           responseTimeStats
	lastFade                                mclock.AbsTime

	lastDiscovered mclock.AbsTime
	known          bool
	dialedBy       *dialCycle
	state          int
	regTime        mclock.AbsTime
	queueIdx       int
	removed        bool

	newDialDelayed, knownDialDelayed bool
	newDialRetryAt, knownDialRetryAt mclock.AbsTime
	newDialFails, knownDialFails     int

	capacity  float64
	costTable requestCostTable
}

// poolEntryEnc is the RLP encoding of poolEntry.
type poolEntryEnc struct {
	Pubkey                              []byte
	IP                                  net.IP
	Port                                uint16
	Fails                               uint
	TotalRecharge, TotalValue, LastFade uint64
	ResponseStats                       *responseTimeStats
}

func (e *poolEntry) fadeStatsBy(d time.Duration) {
	if d <= 0 {
		return
	}
	exp := math.Exp(-float64(d) / float64(fadeStatsTC))
	e.sumRecharge *= exp
	e.sumMaxCost *= exp
	e.totalValue *= exp
	e.responseStats.fade(exp)
}

func (e *poolEntry) fadeStats() {
	now := mclock.Now()
	dt := time.Duration(now - e.lastFade)
	if dt >= fadeStatsRecalc {
		e.fadeStatsBy(dt)
		e.lastFade = now
	}
}

func (e *poolEntry) addSessionStats() {
	e.fadeStats()
	e.totalValue += e.sessionValue
	if e.sessionMaxCost > e.sessionRecharge {
		e.totalRecharge += e.sessionValue * float64(e.sessionRecharge) / float64(e.sessionMaxCost)
	} else {
		e.totalRecharge += e.sessionValue
	}
	e.sessionValue = 0
	e.sessionRecharge = 0
	e.sessionMaxCost = 0
}

func (e *poolEntry) EncodeRLP(w io.Writer) error {
	e.fadeStatsBy(time.Duration(mclock.Now() - e.lastFade))
	return rlp.Encode(w, &poolEntryEnc{
		Pubkey:        encodePubkey64(e.node.Pubkey()),
		IP:            e.lastConnected.ip,
		Port:          e.lastConnected.port,
		Fails:         e.lastConnected.fails,
		TotalRecharge: math.Float64bits(e.totalRecharge),
		TotalValue:    math.Float64bits(e.totalValue),
		ResponseStats: &e.responseStats,
		LastFade:      uint64(time.Now().UnixNano()),
	})
}

func (e *poolEntry) DecodeRLP(s *rlp.Stream) error {
	var entry poolEntryEnc
	entry.ResponseStats = &e.responseStats
	if err := s.Decode(&entry); err != nil {
		return err
	}
	pubkey, err := decodePubkey64(entry.Pubkey)
	if err != nil {
		return err
	}
	addr := &poolEntryAddress{ip: entry.IP, port: entry.Port, fails: entry.Fails, lastSeen: mclock.Now()}
	e.node = enode.NewV4(pubkey, entry.IP, int(entry.Port), int(entry.Port))
	e.addr = make(map[string]*poolEntryAddress)
	e.addr[addr.strKey()] = addr
	e.addrSelect = *newWeightedRandomSelect()
	e.addrSelect.update(addr)
	e.lastConnected = addr
	e.known = true
	e.totalRecharge = math.Float64frombits(entry.TotalRecharge)
	e.totalValue = math.Float64frombits(entry.TotalValue)
	e.fadeStatsBy(time.Duration(time.Now().UnixNano() - int64(entry.LastFade)))
	e.lastFade = mclock.Now()
	return nil
}

func encodePubkey64(pub *ecdsa.PublicKey) []byte {
	return crypto.FromECDSAPub(pub)[1:]
}

func decodePubkey64(b []byte) (*ecdsa.PublicKey, error) {
	return crypto.UnmarshalPubkey(append([]byte{0x04}, b...))
}

type retryDelay interface {
	startDelay() mclock.AbsTime
	checkDelay()
}

type retryDelayQueue prque.Prque

func (rq *retryDelayQueue) schedule(d retryDelay) {
	(*prque.Prque)(rq).Push(d, int64(d.startDelay()))
}

func (rq *retryDelayQueue) check() {
	now := mclock.Now()
	for (*prque.Prque)(rq).Size() != 0 {
		item, time := (*prque.Prque)(rq).Pop()
		if mclock.AbsTime(time) > now {
			(*prque.Prque)(rq).Push(item, time)
			return
		}
		item.(retryDelay).checkDelay()
	}
}

// discoveredEntry implements wrsItem
type discoveredEntry poolEntry

// Weight calculates random selection weight for newly discovered entries
func (e *discoveredEntry) Weight() int64 {
	if e.state != psNotConnected || e.newDialDelayed {
		return 0
	}
	t := time.Duration(mclock.Now() - e.lastDiscovered)
	if t <= discoverExpireStart {
		return 1000000000
	}
	return int64(1000000000 * math.Exp(-float64(t-discoverExpireStart)/float64(discoverExpireConst)))
}

func (e *discoveredEntry) startDelay() mclock.AbsTime {
	dt := float64(newRetryDelay) * math.Exp(float64(e.newDialFails)*newRetryDelayExp) * (1 + rand.Float64())
	e.newDialDelayed = true
	e.newDialRetryAt = mclock.Now() + mclock.AbsTime(dt)
	return e.newDialRetryAt
}

func (e *discoveredEntry) checkDelay() {
	if e.newDialDelayed && e.newDialRetryAt <= mclock.Now() {
		e.newDialDelayed = false
		e.pool.newSelect.update(e)
		e.pool.newDialCycle.resume()
	}
}

// knownEntry implements wrsItem
type knownEntry poolEntry

// Weight calculates random selection weight for known entries
func (e *knownEntry) Weight() int64 {
	if e.state != psNotConnected || !e.known || e.knownDialDelayed {
		return 0
	}
	e.fadeStats()
	weight := e.totalValue
	if weight < 0 {
		weight = 0
	}
	if weight > 9e18/maxKnownEntries {
		weight = 9e18 / maxKnownEntries
	}
	return int64(weight)
}

func (e *knownEntry) startDelay() mclock.AbsTime {
	dt := float64(knownRetryDelay) * math.Exp(float64(e.knownDialFails)*knownRetryDelayExp) * (1 + rand.Float64())
	e.knownDialDelayed = true
	e.knownDialRetryAt = mclock.Now() + mclock.AbsTime(dt)
	return e.knownDialRetryAt
}

func (e *knownEntry) checkDelay() {
	if e.knownDialDelayed && e.knownDialRetryAt <= mclock.Now() {
		e.knownDialDelayed = false
		e.pool.knownSelect.update(e)
		e.pool.knownDialCycle.resume()
	}
}

// poolEntryAddress is a separate object because currently it is necessary to remember
// multiple potential network addresses for a pool entry. This will be removed after
// the final implementation of v5 discovery which will retrieve signed and serial
// numbered advertisements, making it clear which IP/port is the latest one.
type poolEntryAddress struct {
	ip       net.IP
	port     uint16
	lastSeen mclock.AbsTime // last time it was discovered, connected or loaded from db
	fails    uint           // connection failures since last successful connection (persistent)
}

func (a *poolEntryAddress) Weight() int64 {
	t := time.Duration(mclock.Now() - a.lastSeen)
	return int64(1000000*math.Exp(-float64(t)/float64(discoverExpireConst)-float64(a.fails)*addrFailDropLn)) + 1
}

func (a *poolEntryAddress) strKey() string {
	return a.ip.String() + ":" + strconv.Itoa(int(a.port))
}

// poolEntryQueue keeps track of its least recently accessed entries and removes
// them when the number of entries reaches the limit
type poolEntryQueue struct {
	queue                  map[int]*poolEntry // known nodes indexed by their latest lastConnCnt value
	newPtr, oldPtr, maxCnt int
	removeFromPool         func(*poolEntry)
}

// newPoolEntryQueue returns a new poolEntryQueue
func newPoolEntryQueue(maxCnt int, removeFromPool func(*poolEntry)) poolEntryQueue {
	return poolEntryQueue{queue: make(map[int]*poolEntry), maxCnt: maxCnt, removeFromPool: removeFromPool}
}

// fetchOldest returns and removes the least recently accessed entry
func (q *poolEntryQueue) fetchOldest() *poolEntry {
	if len(q.queue) == 0 {
		return nil
	}
	for {
		if e := q.queue[q.oldPtr]; e != nil {
			delete(q.queue, q.oldPtr)
			q.oldPtr++
			return e
		}
		q.oldPtr++
	}
}

// remove removes an entry from the queue
func (q *poolEntryQueue) remove(entry *poolEntry) {
	if q.queue[entry.queueIdx] == entry {
		delete(q.queue, entry.queueIdx)
	}
}

// setLatest adds or updates a recently accessed entry. It also checks if an old entry
// needs to be removed and removes it from the parent pool too with a callback function.
func (q *poolEntryQueue) setLatest(entry *poolEntry) {
	if q.queue[entry.queueIdx] == entry {
		delete(q.queue, entry.queueIdx)
	} else {
		if len(q.queue) == q.maxCnt {
			e := q.fetchOldest()
			q.remove(e)
			q.removeFromPool(e)
		}
	}
	entry.queueIdx = q.newPtr
	q.queue[entry.queueIdx] = entry
	q.newPtr++
}

type requestCountItem struct {
	sumReq, sumCount uint64
}

type requestCounter struct {
	counter  map[uint64]requestCountItem
	totalReq uint64
}

func newRequestCounter() *requestCounter {
	return &requestCounter{counter: make(map[uint64]requestCountItem)}
}

func (rc *requestCounter) add(code, count uint64) {
	if rc == nil {
		return
	}
	item := rc.counter[code]
	item.sumReq++
	item.sumCount += count
	rc.counter[code] = item
	rc.totalReq++
}

type requestCountListItem struct {
	Code, SumReq, SumCount uint64
}

func (rc *requestCounter) EncodeRLP(w io.Writer) error {
	list := make([]requestCountListItem, len(rc.counter))
	ptr := 0
	for code, item := range rc.counter {
		list[ptr] = requestCountListItem{code, item.sumReq, item.sumCount}
		ptr++
	}
	return rlp.Encode(w, list)
}

func (rc *requestCounter) DecodeRLP(st *rlp.Stream) error {
	var list []requestCountListItem
	if err := st.Decode(&list); err != nil {
		return err
	}
	for _, item := range list {
		rc.counter[item.Code] = requestCountItem{item.SumReq, item.SumCount}
		rc.totalReq += item.SumReq
	}
	return nil
}

type responseTimeStats struct {
	stats      [64]float64
	sum        float64
	srtoRecalc int
	peer       *peer // nil if not connected
}

func (rt *responseTimeStats) EncodeRLP(w io.Writer) error {
	var list [64]uint64
	for i, v := range rt.stats[:] {
		list[i] = math.math.Float64bits(v)
	}
	return rlp.Encode(w, list)
}

func (rt *responseTimeStats) DecodeRLP(st *rlp.Stream) error {
	var list [64]uint64
	if err := st.Decode(&list); err != nil {
		return err
	}
	for i, v := range list {
		rt.stats[i] = math.Float64frombits(v)
	}
	return nil
}

func timeToStatScale(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	r := float64(d) / float64(time.Millisecond*50)
	if r > 1 {
		r = math.Log(r) + 1
	}
	r *= 10
	if r > 63 {
		return 63
	}
	return r
}

func statScaleToTime(r float64) time.Duration {
	r /= 10
	if r > 1 {
		r = math.Exp(r - 1)
	}
	return time.Duration(r * float64(time.Millisecond*50))
}

func (rt *responseTimeStats) add(respTime time.Duration, weight float64) {
	r := timeToStatScale(respTime)
	i := int(r)
	r -= float64(i)
	rt.stats[i] += weight * (1 - r)
	if i < 63 {
		rt.stats[i+1] += weight * r
	}
	rt.sum += weight

	if rt.peer != nil {
		rt.srtoRecalc++
		if rt.srtoRecalc >= srtoRecalcCount {
			rt.updateSoftRequestTimeout()
			rt.srtoRecalc = 0
		}
	}
}

func (rt *responseTimeStats) updateSoftRequestTimeout() {
	s := rt.sum * 0.1
	i := 63
	for i > 0 && s >= rt.stats[i] {
		s -= rt.stats[i]
		i--
	}
	r := float64(i) + 0.5
	if rt.stats[i] > 1e-100 {
		r -= s / rt.stats[i]
	}
	srto := statScaleToTime(r) * 5 / 4
	atomic.StoreInt64((*int64)(&rt.peer.srto), int64(srto))
}

type statValueWeights [64]float64

func makeStatValueWeights(crossTime time.Duration) statValueWeights {
	var wt statValueWeights
	for i, _ := range w[:] {
		t := statScaleToTime(float64(i))
		w := 1 - float64(t)/float64(crossTime)
		if w < -1 {
			w = -1
		}
		wt[i] = w
	}
	return wt
}

func (rt *responseTimeStats) value(weights *statValueWeights) float64 {
	var v float64
	for i, s := range rt.stats[:] {
		v += s * weights[i]
	}
	if v > 0 {
		return v
	}
	return 0
}
