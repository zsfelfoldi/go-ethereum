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
	"context"
	"math/rand"
	"sort"

	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	//"github.com/ethereum/go-ethereum/log"
)

/*type beaconHeadInfo struct {
	lock         sync.RWMutex
	beaconHeader beacon.Header
	//execHead   *types.Header
	execRoots map[common.Hash]struct{}
}

func NewBeaconHeadInfo(beaconHead common.Hash) *beaconHeadInfo {
	return *beaconHeadInfo{beaconHead: beaconHead, execRoots: make(map[common.Hash]struct{})}
}

func (bh *beaconHeadInfo) addExecRoots(headers []*types.Header) {
	bh.lock.Lock()
	for _, h := range headers {
		bh.execRoots[h.Hash()] = struct{}{}
	}
	bh.lock.Unlock()
}

func (bh *beaconHeadInfo) hasExecRoot(execRoot common.Hash) bool {
	bh.lock.RLock()
	_, ok := bh.execRoots[execRoot]
	bh.lock.RUnlock()
	return ok
}*/

// LesOdr implements light.OdrBackend
type LesOdr struct {
	db                                         ethdb.Database
	indexerConfig                              *light.IndexerConfig
	chtIndexer, bloomTrieIndexer, bloomIndexer *core.ChainIndexer
	peers                                      *serverPeerSet
	retriever                                  *retrieveManager
	stop                                       chan struct{}

	beaconHeaderLock sync.RWMutex
	beaconHeader     beacon.Header
}

func NewLesOdr(db ethdb.Database, config *light.IndexerConfig, peers *serverPeerSet, retriever *retrieveManager) *LesOdr {
	return &LesOdr{
		db:            db,
		indexerConfig: config,
		peers:         peers,
		retriever:     retriever,
		stop:          make(chan struct{}),
		//beaconHeadMap: make(map[common.Hash]*beaconHeadInfo),
	}
}

// Stop cancels all pending retrievals
func (odr *LesOdr) Stop() {
	close(odr.stop)
}

// Database returns the backing database
func (odr *LesOdr) Database() ethdb.Database {
	return odr.db
}

// SetIndexers adds the necessary chain indexers to the ODR backend
func (odr *LesOdr) SetIndexers(chtIndexer, bloomTrieIndexer, bloomIndexer *core.ChainIndexer) {
	odr.chtIndexer = chtIndexer
	odr.bloomTrieIndexer = bloomTrieIndexer
	odr.bloomIndexer = bloomIndexer
}

// ChtIndexer returns the CHT chain indexer
func (odr *LesOdr) ChtIndexer() *core.ChainIndexer {
	return odr.chtIndexer
}

// BloomTrieIndexer returns the bloom trie chain indexer
func (odr *LesOdr) BloomTrieIndexer() *core.ChainIndexer {
	return odr.bloomTrieIndexer
}

// BloomIndexer returns the bloombits chain indexer
func (odr *LesOdr) BloomIndexer() *core.ChainIndexer {
	return odr.bloomIndexer
}

// IndexerConfig returns the indexer config.
func (odr *LesOdr) IndexerConfig() *light.IndexerConfig {
	return odr.indexerConfig
}

/*func (odr *LesOdr) SetBeaconHead(head beacon.Header) {
	odr.beaconHeadLock.Lock()
	odr.beaconHead = head
	odr.beaconHeadLock.Unlock()
	log.Info("Received new beacon head", "slot", head.Slot, "blockRoot", head.Hash())
}

func (odr *LesOdr) GetBeaconHead() beacon.Header {
	odr.beaconHeadLock.RLock()
	head := odr.beaconHead
	odr.beaconHeadLock.RUnlock()
	return head
}*/

const (
	MsgBlockHeaders = iota
	MsgBlockBodies
	MsgCode
	MsgReceipts
	MsgProofsV2
	MsgHelperTrieProofs
	MsgTxStatus
	MsgBeaconInit
	MsgBeaconData
	MsgExecHeaders
	MsgCommitteeProofs
)

// Msg encodes a LES message that delivers reply data for a request
type Msg struct {
	MsgType int
	ReqID   uint64
	Obj     interface{}
}

// peerByTxHistory is a heap.Interface implementation which can sort
// the peerset by transaction history.
type peerByTxHistory []*peer

func (h peerByTxHistory) Len() int { return len(h) }
func (h peerByTxHistory) Less(i, j int) bool {
	if h[i].txHistory == txIndexUnlimited {
		return false
	}
	if h[j].txHistory == txIndexUnlimited {
		return true
	}
	return h[i].txHistory < h[j].txHistory
}
func (h peerByTxHistory) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

const (
	maxTxStatusRetry      = 3 // The maximum retrys will be made for tx status request.
	maxTxStatusCandidates = 5 // The maximum les servers the tx status requests will be sent to.
)

// RetrieveTxStatus retrieves the transaction status from the LES network.
// There is no guarantee in the LES protocol that the mined transaction will
// be retrieved back for sure because of different reasons(the transaction
// is unindexed, the malicous server doesn't reply it deliberately, etc).
// Therefore, unretrieved transactions(UNKNOWN) will receive a certain number
// of retries, thus giving a weak guarantee.
func (odr *LesOdr) RetrieveTxStatus(ctx context.Context, req *light.TxStatusRequest) error {
	// Sort according to the transaction history supported by the peer and
	// select the peers with longest history.
	var (
		retries int
		peers   []*peer
		missing = len(req.Hashes)
		result  = make([]light.TxStatus, len(req.Hashes))
		canSend = make(map[string]bool)
	)
	for _, peer := range odr.peers.allPeers() {
		if peer.txHistory == txIndexDisabled {
			continue
		}
		peers = append(peers, peer)
	}
	sort.Sort(sort.Reverse(peerByTxHistory(peers)))
	for i := 0; i < maxTxStatusCandidates && i < len(peers); i++ {
		canSend[peers[i].id] = true
	}
	// Send out the request and assemble the result.
	for {
		if retries >= maxTxStatusRetry || len(canSend) == 0 {
			break
		}
		var (
			// Deep copy the request, so that the partial result won't be mixed.
			req     = &TxStatusRequest{Hashes: req.Hashes}
			id      = rand.Uint64()
			distreq = &distReq{
				getCost: func(dp distPeer) uint64 { return req.GetCost(dp.(*peer)) },
				canSend: func(dp distPeer) bool { return canSend[dp.(*peer).id] },
				request: func(dp distPeer) func() {
					p := dp.(*peer)
					p.fcServer.QueuedRequest(id, req.GetCost(p))
					delete(canSend, p.id)
					return func() { req.Request(id, beacon.Header{}, p) }
				},
			}
		)
		if err := odr.retriever.retrieve(ctx, id, distreq, func(p distPeer, msg *Msg) error { return req.Validate(odr.db, beacon.Header{}, msg) }, odr.stop); err != nil {
			return err
		}
		// Collect the response and assemble them to the final result.
		// All the response is not verifiable, so always pick the first
		// one we get.
		for index, status := range req.Status {
			if result[index].Status != core.TxStatusUnknown {
				continue
			}
			if status.Status == core.TxStatusUnknown {
				continue
			}
			result[index], missing = status, missing-1
		}
		// Abort the procedure if all the status are retrieved
		if missing == 0 {
			break
		}
		retries += 1
	}
	req.Status = result
	return nil
}

func (odr *LesOdr) SetBeaconHead(head beacon.Header) {
	odr.beaconHeaderLock.Lock()
	odr.beaconHeader = head
	odr.beaconHeaderLock.Unlock()
}

// Retrieve tries to fetch an object from the LES network. It's a common API
// for most of the LES requests except for the TxStatusRequest which needs
// the additional retry mechanism.
// If the network retrieval was successful, it stores the object in local db.
func (odr *LesOdr) Retrieve(ctx context.Context, req light.OdrRequest) (err error) {
	return odr.RetrieveWithBeaconHeader(ctx, beacon.Header{}, req)
}

func (odr *LesOdr) RetrieveWithBeaconHeader(ctx context.Context, beaconHeader beacon.Header, req light.OdrRequest) (err error) {
	if beaconHeader == (beacon.Header{}) {
		odr.beaconHeaderLock.RLock()
		beaconHeader = odr.beaconHeader
		odr.beaconHeaderLock.RUnlock()
	}

	lreq := LesRequest(req)
	reqID := rand.Uint64()
	rq := &distReq{
		getCost: func(dp distPeer) uint64 {
			return lreq.GetCost(dp.(*peer))
		},
		canSend: func(dp distPeer) bool {
			p := dp.(*peer)
			//p.setBestBeaconHeader(beaconHeader)
			return lreq.CanSend(beaconHeader, p)
		},
		request: func(dp distPeer) func() {
			p := dp.(*peer)
			cost := lreq.GetCost(p)
			p.fcServer.QueuedRequest(reqID, cost)
			return func() { lreq.Request(reqID, beaconHeader, p) }
		},
	}

	defer func(sent mclock.AbsTime) {
		if err != nil {
			return
		}
		requestRTT.Update(time.Duration(mclock.Now() - sent))
	}(mclock.Now())

	if err := odr.retriever.retrieve(ctx, reqID, rq, func(p distPeer, msg *Msg) error { return lreq.Validate(odr.db, beaconHeader, msg) }, odr.stop); err != nil {
		return err
	}
	/*switch r := req.(type) {
	case *light.ExecHeadersRequest:
		odr.execHeadersRetrieved(r.Header, r.ExecHeaders)
	case *light.HeadersByHashRequest:
		odr.execHeadersRetrieved(r.BeaconHeader, r.Headers)
	default:
	}*/
	req.StoreResult(odr.db)
	return nil
}

/*func (odr *LesOdr) execHeadersRetrieved(beaconHeader beacon.Header, execHeaders []*types.Header) {
	//execNumber, execHash := header.Number.Uint64(), header.Hash()
	beaconRoot := beaconHeader.Hash()
	odr.beaconHeadLock.Lock()
	headInfo := odr.beaconHeadMap[beaconRoot]
	if headInfo == nil {
		var bestSlot uint64
		for _, bh := range odr.beaconHeadMap {
			if bh.beaconHeader.Slot > bestSlot {
				bestSlot = bh.beaconHeader.Slot
			}
		}
		// if current beacon header is not very old then add new headInfo and remove old entries
		if beaconHeader.Slot+16 >= bestSlot {
			headInfo = newBeaconHeadInfo(beaconHeader)
			for br, bh := range odr.beaconHeadMap {
				if bh.beaconHeader.Slot+16 < bestSlot {
					delete(odr.beaconHeadMap, br)
				}
			}
			odr.beaconHeadMap[beaconRoot] = headInfo
		}
	}
}*/

/*func (odr *LesOdr) getExecHeader(beaconHead common.Hash) *types.Header {
	odr.beaconHeadLock.RLock()
	header := odr.beaconHeadMap[beaconHead]
	odr.beaconHeadLock.RUnlock()
	return header
}*/
