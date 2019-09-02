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
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	retryDelay = time.Second * 30
	// maxNewEntries is the maximum number of newly discovered (never connected) nodes.
	// If the limit is reached, the least recently discovered one is thrown out.
	maxNewEntries = 1000
	// maxKnownEntries is the maximum number of known (already connected) nodes.
	// If the limit is reached, the least recently connected one is thrown out.
	// (not that unlike new entries, known entries are persistent)
	maxKnownEntries = 1000
	// after dialTimeout, consider the server unavailable and adjust statistics
	dialTimeout = time.Second * 30
	// new entry selection weight calculation based on most recent discovery time:
	// unity until discoverExpireStart, then exponential decay with discoverExpireConst
	discoverExpireStart = time.Minute * 20
	discoverExpireConst = time.Minute * 20
	delayScoreTC        = time.Second * 5

	statusPerTarget = 10
	connectedTarget = 3
)

const (
	psIdle = iota
	psStatusRequested
	psStatusSoftTimeout
	psPaymentConsidered
	psWaitConfirm
	psDialed
	psConnected
	psWaitRetry

	paymentRejected = iota
	paymentStarted
	paymentConfirmed
)

// serverPool implements a pool for storing and selecting newly discovered and already
// known light server nodes. It received discovered nodes, stores statistics about
// known nodes and takes care of always having enough good quality servers connected.
type serverPool struct {
	db     ethdb.Database
	dbKey  []byte
	server *p2p.Server
	connWg sync.WaitGroup

	topic discv5.Topic

	discSetPeriod chan time.Duration
	discNodes     chan *enode.Node
	discLookups   chan bool

	trustedNodes map[enode.ID]*enode.Node

	entries              map[enode.ID]*poolEntry
	knownQueue, newQueue poolEntryQueue
	fastDiscover         bool

	closeCh chan struct{}
	wg      sync.WaitGroup
}

// newServerPool creates a new serverPool instance
func newServerPool(db ethdb.Database, ulcServers []string) *serverPool {
	pool := &serverPool{
		db:           db,
		entries:      make(map[enode.ID]*poolEntry),
		timeout:      make(chan *poolEntry, 1),
		adjustStats:  make(chan poolStatAdjust, 100),
		enableRetry:  make(chan *poolEntry, 1),
		connCh:       make(chan *connReq),
		disconnCh:    make(chan *disconnReq),
		registerCh:   make(chan *registerReq),
		closeCh:      make(chan struct{}),
		knownSelect:  newWeightedRandomSelect(),
		newSelect:    newWeightedRandomSelect(),
		fastDiscover: true,
		trustedNodes: parseTrustedNodes(ulcServers),
	}

	pool.knownQueue = newPoolEntryQueue(maxKnownEntries, pool.removeEntry)
	pool.newQueue = newPoolEntryQueue(maxNewEntries, pool.removeEntry)
	return pool
}

func (pool *serverPool) start(server *p2p.Server, topic discv5.Topic) {
	pool.server = server
	pool.topic = topic
	pool.dbKey = append([]byte("serverPool/"), []byte(topic)...)
	pool.loadNodes()
	pool.connectToTrustedNodes()

	if pool.server.DiscV5 != nil {
		pool.discSetPeriod = make(chan time.Duration, 1)
		pool.discNodes = make(chan *enode.Node, 100)
		pool.discLookups = make(chan bool, 100)
		go pool.discoverNodes()
	}
	pool.checkDial()
	pool.wg.Add(1)
	go pool.eventLoop()
}

func (pool *serverPool) stop() {
	close(pool.closeCh)
	pool.wg.Wait()
}

type nodeStateUpdate struct {
	node                poolEntry
	lastState, newState int
}

type poolEntry interface {
	predictValue() float64
	tryConnect(chan nodeStateUpdate)
	setPaymentState(int)
}

// eventLoop handles pool events and mutex locking for all internal functions
func (pool *serverPool) eventLoop() {
	defer pool.wg.Done()

	var countStatusRequest, countDialConn, countPaymentConsidered int
	nodeStateCh := make(chan nodeStateUpdate, 16)

	for {
		select {
		case update := <-nodeStateCh:
			switch update.lastState {
			case psIdle:
			case psStatusRequested:
				countStatusRequest--
			case psStatusSoftTimeout:
			case psPaymentConsidered:
				countPaymentConsidered--
			case psWaitConfirm:
				countDialConn--
			case psDialed:
				countDialConn--
			case psConnected:
				countDialConn--
			case psWaitRetry:
			}

			switch update.newState {
			case psIdle:
				pool.insertNode(update.node)
			case psStatusRequested:
				// countStatusRequest is increased when connection is started
			case psStatusSoftTimeout:
			case psPaymentConsidered:
				countPaymentConsidered++
				node.setPaymentState(paymentRejected) // payments not supported yet
			case psWaitConfirm:
				countDialConn++
			case psDialed:
				countDialConn++
			case psConnected:
				countDialConn++
			case psWaitRetry:
			}
		case node := <-pool.discNodes:
			if pool.trustedNodes[node.ID()] == nil {
				entry := pool.findOrNewNode(node)
				pool.updateCheckDial(entry)
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

		case <-pool.closeCh:
			if pool.discSetPeriod != nil {
				close(pool.discSetPeriod)
			}

			// Spawn a goroutine to close the disconnCh after all connections are disconnected.
			go func() {
				pool.connWg.Wait()
				close(pool.disconnCh)
			}()

			// Handle all remaining disconnection requests before exit.
			for req := range pool.disconnCh {
				disconnect(req, true)
			}
			pool.saveNodes()
			return
		}
		// start new connection attempts if necessary
		targetStatusRequest := statusPerTarget * (connectedTarget - countDialConn)
		tp := paymentConsideredTarget - countPaymentConsidered
		tp += (tp + 3) / 4
		if tp < targetStatusRequest {
			targetStatusRequest = tp
		}
		for countStatusRequest < targetStatusRequest {
			node := pool.getBestNode()
			if node == nil {
				break
			}
			countStatusRequest++
			node.tryConnect(nodeStateCh)
		}
	}
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

// connect should be called upon any incoming connection. If the connection has been
// dialed by the server pool recently, the appropriate pool entry is returned.
// Otherwise, the connection should be rejected.
// Note that whenever a connection has been accepted and a pool entry has been returned,
// disconnect should also always be called.
func (pool *serverPool) connect(p *peer, node *enode.Node) *poolEntry {
	log.Debug("Connect new entry", "enode", p.id)
	req := &connReq{p: p, node: node, result: make(chan *poolEntry, 1)}
	select {
	case pool.connCh <- req:
	case <-pool.closeCh:
		return nil
	}
	return <-req.result
}

// registered should be called after a successful handshake
func (pool *serverPool) registered(entry *poolEntry) {
	log.Debug("Registered new entry", "enode", entry.node.ID())
	req := &registerReq{entry: entry, done: make(chan struct{})}
	select {
	case pool.registerCh <- req:
	case <-pool.closeCh:
		return
	}
	<-req.done
}

// disconnect should be called when ending a connection. Service quality statistics
// can be updated optionally (not updated if no registration happened, in this case
// only connection statistics are updated, just like in case of timeout)
func (pool *serverPool) disconnect(entry *poolEntry) {
	stopped := false
	select {
	case <-pool.closeCh:
		stopped = true
	default:
	}
	log.Debug("Disconnected old entry", "enode", entry.node.ID())
	req := &disconnReq{entry: entry, stopped: stopped, done: make(chan struct{})}

	// Block until disconnection request is served.
	pool.disconnCh <- req
	<-req.done
}

func (pool *serverPool) findOrNewNode(node *enode.Node) *poolEntry {
	now := mclock.Now()
	entry := pool.entries[node.ID()]
	if entry == nil {
		log.Debug("Discovered new entry", "id", node.ID())
		entry = &poolEntry{
			node:       node,
			addr:       make(map[string]*poolEntryAddress),
			addrSelect: *newWeightedRandomSelect(),
			shortRetry: shortRetryCnt,
		}
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

// loadNodes loads known nodes and their statistics from the database
func (pool *serverPool) loadNodes() {
	enc, err := pool.db.Get(pool.dbKey)
	if err != nil {
		return
	}
	var list []*poolEntry
	err = rlp.DecodeBytes(enc, &list)
	if err != nil {
		log.Debug("Failed to decode node list", "err", err)
		return
	}
	for _, e := range list {
		log.Debug("Loaded server stats", "id", e.node.ID(), "fails", e.lastConnected.fails,
			"conn", fmt.Sprintf("%v/%v", e.connectStats.avg, e.connectStats.weight),
			"delay", fmt.Sprintf("%v/%v", time.Duration(e.delayStats.avg), e.delayStats.weight),
			"response", fmt.Sprintf("%v/%v", time.Duration(e.responseStats.avg), e.responseStats.weight),
			"timeout", fmt.Sprintf("%v/%v", e.timeoutStats.avg, e.timeoutStats.weight))
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
		node, err := enode.Parse(enode.ValidSchemes, node)
		if err != nil {
			log.Warn("Trusted node URL invalid", "enode", node, "err", err)
			continue
		}
		nodes[node.ID()] = node
	}
	return nodes
}

// saveNodes saves known nodes and their statistics into the database. Nodes are
// ordered from least to most recently connected.
func (pool *serverPool) saveNodes() {
	list := make([]*poolEntry, len(pool.knownQueue.queue))
	for i := range list {
		list[i] = pool.knownQueue.fetchOldest()
	}
	enc, err := rlp.EncodeToBytes(list)
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
func (pool *serverPool) dial(entry *poolEntry) {
	if pool.server == nil || entry.state != psNotConnected {
		return
	}
	entry.state = psDialed
	entry.knownSelected = knownSelected
	if knownSelected {
		pool.knownSelected++
	} else {
		pool.newSelected++
	}
	addr := entry.addrSelect.choose().(*poolEntryAddress)
	log.Debug("Dialing new peer", "lesaddr", entry.node.ID().String()+"@"+addr.strKey(), "set", len(entry.addr), "known", knownSelected)
	entry.dialed = addr
	go func() {
		pool.server.AddPeer(entry.node)
		select {
		case <-pool.closeCh:
		case <-time.After(dialTimeout):
			select {
			case <-pool.closeCh:
			case pool.timeout <- entry:
			}
		}
	}()
}

// poolNodeInfo represents a server node and stores its current state and statistics.
type poolNodeInfo struct {
	pubkey         [64]byte // secp256k1 key of the node
	node           *enode.Node
	lastDiscovered mclock.AbsTime
	queueIdx       int
	removed        bool

	quit, connCh, paidCh, rejectedCh, confirmedCh chan struct{}
	disconnCh                                     chan float64
	statusCh                                      chan *remoteStatus
	wg                                            *sync.WaitGroup
}

func newPoolNodeInfo(p *peer, node *enode.Node) *poolNodeInfo {
}

func (p *poolNodeInfo) tryConnect(update chan nodeStateUpdate) {
	log.Debug("Trying to connect to peer", "nodeID", entry.node.ID().String())
	p.wg.Add(1)

	var (
		state         int
		peerAdded     bool
		value, weight float64
	)
	setState := func(newState int) {
		state = newState
		update <- nodeStateUpdate{p, state}
	}
	setState(psStatusRequested)
	if p.statusRequestSupported {
		p.server.RequestStatus(p.node)
		weight = 0.1
	} else {
		p.server.AddPeer(p.node)
		peerAdded = true
	}

	go func() {
		defer p.wg.Done()

		for {
			switch state {
			case psStatusSoftTimeout, psStatusRequested:
				var (
					timeout   time.Duration
					nextState int
				)
				if state == psStatusRequested {
					timeout = time.Second
					nextState = psStatusSoftTimeout
				} else {
					timeout = time.Second * 9
					nextState = psWaitRetry
				}
				if p.statusRequestSupported {
					select {
					case p.remoteStatus = <-p.statusCh:
						if p.remoteStatus.paymentRequested {
							setState(psPaymentConsidered)
						} else {
							setState(psDialed)
							p.server.AddPeer(p.node)
							peerAdded = true
						}
					case <-time.After(timeout):
						setState(nextState)
					case <-p.quit:
						return
					}
				} else {
					select {
					case <-p.connCh:
						setState(psConnected)
					case <-time.After(timeout):
						setState(nextState)
					case <-p.quit:
						return
					}
				}
			case psPaymentConsidered:
				exp := p.remoteStatus.expiration
				if exp > time.Minute {
					exp = time.Minute
				}
				select {
				case <-p.paidCh:
					setState(psWaitConfirm)
				case <-p.rejectedCh:
					setState(psWaitRetry)
				case <-time.After(exp):
					setState(psWaitRetry)
				case <-p.quit:
					return
				}
			case psWaitConfirm:
				select {
				case <-p.confirmedCh:
				case <-time.After(time.Second * 30):
				case <-p.quit:
					return
				}
				setState(psDialed)
				p.server.AddPeer(p.node)
				peerAdded = true
			case psDialed:
				select {
				case <-p.connCh:
					setState(psConnected)
				case value = <-p.disconnCh:
					setState(psWaitRetry)
				case <-time.After(time.Second * 30):
					setState(psWaitRetry)
				case <-p.quit:
					return
				}
			case psConnected:
				select {
				case value = <-p.disconnCh:
					setState(psWaitRetry)
					// value update
				case <-p.quit:
					return
				}
			case psWaitRetry:
				if peerAdded {
					weight = 1
					p.server.RemovePeer(p.node)
				}
				p.valuePredictor.update(value, weight)
				select {
				case <-time.After(time.Second * 30):
				case <-p.quit:
				}
				setState(psIdle)
				return
			}
		}
	}()
}

func (p *poolNodeInfo) connected(p *peer) bool {
	select {
	case p.connCh <- p:
		return true
	default:
		return false
	}
}

// poolEntryEnc is the RLP encoding of poolEntry.
type poolEntryEnc struct {
	Pubkey                     []byte
	IP                         net.IP
	Port                       uint16
	Fails                      uint
	CStat, DStat, RStat, TStat poolStats
}

func (e *poolEntry) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &poolEntryEnc{
		Pubkey: encodePubkey64(e.node.Pubkey()),
		IP:     e.lastConnected.ip,
		Port:   e.lastConnected.port,
		Fails:  e.lastConnected.fails,
		CStat:  e.connectStats,
		DStat:  e.delayStats,
		RStat:  e.responseStats,
		TStat:  e.timeoutStats,
	})
}

func (e *poolEntry) DecodeRLP(s *rlp.Stream) error {
	var entry poolEntryEnc
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
	e.connectStats = entry.CStat
	e.delayStats = entry.DStat
	e.responseStats = entry.RStat
	e.timeoutStats = entry.TStat
	e.shortRetry = shortRetryCnt
	e.known = true
	return nil
}

func encodePubkey64(pub *ecdsa.PublicKey) []byte {
	return crypto.FromECDSAPub(pub)[1:]
}

func decodePubkey64(b []byte) (*ecdsa.PublicKey, error) {
	return crypto.UnmarshalPubkey(append([]byte{0x04}, b...))
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
