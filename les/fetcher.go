// Copyright 2015 The go-ethereum Authors
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

// Package les implements the Light Ethereum Subprotocol.
package les

import (
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
)

type lightFetcher struct {
	pm               *ProtocolManager
	odr              *LesOdr
	chain            BlockChain

	headAnnouncedMu  sync.Mutex
	headAnnouncedBy  map[common.Hash{}][]*peer	
	currentTd		*big.Int
	deliverChn		chan fetchResponse
	reqMu            sync.RWMutex
	requested        map[uint64]fetchRequest
	timeoutChn		chan uint64
	notifyChn		chan bool   // true if initiated from outside
}

type fetchRequest struct {
	hash common.Hash
	amount uint64
	peer *peer
}

type fetchResponse struct {
	reqID uint64
	headers []*types.Header
}

func newLightFetcher(pm *ProtocolManager) *lightFetcher {
	f := &lightFetcher{
		pm:             pm,
		chain:          pm.blockchain,
		odr:            pm.odr,
		requested:      make(map[uint64]chan *types.Header),
		headAnnouncedBy: make(map[common.Hash{}][]*peer),
		deliverChn:		make(chan fetchResponse, 100),
		requested:       make(map[uint64]*newBlockHashData),
		timeoutChn:		make(chan uint64),
		notifyChn:		make(chan bool, 100),
	}
	go f.syncLoop()
	return f
}

func (f *lightFetcher) notify(p *peer, head *newBlockHashData) {
	if !p.addNotify(head) {
		f.pm.removePeer(p.id)
	}
	f.headAnnouncedMu.Lock()
	f.headAnnouncedBy[head.Hash] = append(f.headAnnouncedBy[head.Hash], p)
	f.headAnnouncedMu.Unlock()
	f.notifyChn <- true
}

func (f *lightFetcher) gotHeader(header *types.Header) {
	f.headAnnouncedMu.Lock()
	defer f.headAnnouncedMu.Unlock()

	hash := header.Hash()
	peerList := f.headAnnouncedBy[hash]
	if peerList == nil {
		return
	}
	number := header.GetNumberU64()
	td := core.GetTd(f.pm.chainDb, hash, number)
	for _, peer := range peerList {
		if !peer.gotHeader(hash, number, td) {
			f.pm.removePeer(p.id)
		}
	}	
	delete(f.headAnnouncedBy, hash)
}

func (f *lightFetcher) nextRequest() (*peer, *newBlockHashData) {
	var bestPeer *peer
	bestTd := f.currentTd
	for _, peer := range f.pm.peers.AllPeers() {
		peer.lock.RLock()
		if !peer.headInfo.requested && (td == nil || peer.headInfo.Td.Cmp(bestTd) > 0 ||
		(bestPeer != nil && peer.headInfo.Td.Cmp(bestTd) == 0 && peer.headInfo.haveHeaders > bestPeer.headInfo.haveHeaders)) {
			bestPeer = peer
			bestTd = peer.headInfo.Td
		}
		peer.lock.RUnlock()
	}
	if bestPeer == nil {
		return nil, nil
	}
	bestPeer.lock.Lock()
	res := bestPeer.headInfo
	res.requested = true
	bestPeer.lock.Unlock()
	return bestPeer, res
}

func (f *lightFetcher) deliverHeaders(reqID uint64, headers []*types.Header) {
	f.deliverChn <- fetchResponse{reqID: reqID, headers: headers}
}

func (f *lightFetcher) requestedID(reqID uint64) bool {
	f.reqMu.RLock()
	_, ok := f.requested[reqID]
	f.reqMu.RUnlock()
	return ok
}

func (f *lightFetcher) request(p *peer, block *newBlockHashData) {
	amount := block.Number-block.haveHeaders
	if amount == 0 {
		return
	}
	reqID := f.odr.getNextReqID()
	f.reqMu.Lock()
	f.requested[reqID] = fetchRequest{hash: block.Hash, amount: amount, peer: p}
	f.reqMu.Unlock()
	cost := p.GetRequestCost(GetBlockHeadersMsg, amount)
	p.fcServer.SendRequest(reqID, cost)
	go p.RequestHeadersByHash(reqID, cost, block.Hash, amount, 0, true)
	go func() {
		time.Sleep(hardRequestTimeout)
		f.timeoutChn <- reqID
	}()
}

func (f *lightFetcher) processResponse(req fetchRequest, resp fetchResponse) bool {
	if len(resp.headers) != req.amount || req.headers[0].Hash() != req.hash {
		return false
	}
	headers := make([]*types.Header, req.amount)
	for i, header := range resp.headers {
		headers[req.amount-1-i] = header
	}
	if _, err := f.chain.InsertHeaderChain(headers, 1); err != nil {
		return false
	}
	for _, header := range headers {
		td := core.GetTd(f.pm.chainDb, header.Hash(), header.GetNumberU64())
		if td == nil {
			return false
		}
		f.gotHeader(header)
	}
	return true
}

func (f *lightFetcher) syncLoop() {
	f.pm.wg.Add(1)
	defer f.pm.wg.Done()

	srtoNotify := false
	for {
		select {
		case <-f.pm.quitSync:
			return
		case ext := <-f.notifyChn:
			if !(ext && srtoNotify) {
				if p, r := f.nextRequest(); r != nil {
					srtoNotify = true
					go func() {
						time.Sleep(softRequestTimeout)
						f.notifyChn <- false
					}()
					f.request(p, r)
				} else {
					srtoNotify = false
				}
			}
		case reqID := <-f.timeoutChn:
			f.reqMu.Lock()
			req, ok := f.requested[reqID]
			if ok {
				delete(f.requested, reqID)
			}
			f.reqMu.Unlock()
			if ok {
				f.pm.removePeer(req.peer.id)
			}
		case resp := <-f.deliverChn:
			f.reqMu.Lock()
			req, ok := f.requested[resp.reqID]
			delete(f.requested, resp.reqID)
			f.reqMu.Unlock()
			if !ok || !f.processResponse(req, resp) {
				f.pm.removePeer(req.peer.id)
			}
		}
	}
}
