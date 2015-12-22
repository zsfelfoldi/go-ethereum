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
package les

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"golang.org/x/net/context"
)

var (
	requestTimeout = time.Millisecond * 300
	retryPeers     = time.Second * 1
)

type LesOdr struct {
	light.OdrBackend
	db         ethdb.Database
	lock       sync.Mutex
	sentReqs   map[uint64]*sentReq
	sentReqCnt uint64
	peers      *odrPeerSet
}

func NewLesOdr(db ethdb.Database) *LesOdr {
	return &LesOdr{
		db:       db,
		peers:    newOdrPeerSet(),
		sentReqs: make(map[uint64]*sentReq),
	}
}

func (odr *LesOdr) Database() ethdb.Database {
	return odr.db
}

// requestFunc is a function that requests some data from a peer
type requestFunc func(*odrPeer) error

// validatorFunc is a function that processes a message and returns true if
// it was a meaningful answer to a given request
type validatorFunc func(ethdb.Database, *Msg) bool

// sentReq is a request waiting for an answer that satisfies its valFunc
type sentReq struct {
	valFunc     validatorFunc
	deliverChan chan *Msg
}

// RegisterPeer registers a new LES peer to the ODR capable peer set
func (self *LesOdr) RegisterPeer(id string, version int, head common.Hash, getBlockBodies getBlockBodiesFn, getNodeData getNodeDataFn, getReceipts getReceiptsFn, getProofs getProofsFn) error {
	glog.V(logger.Detail).Infoln("Registering peer", id)
	if err := self.peers.Register(newOdrPeer(id, version, head, getBlockBodies, getNodeData, getReceipts, getProofs)); err != nil {
		glog.V(logger.Error).Infoln("Register failed:", err)
		return err
	}
	return nil
}

// UnregisterPeer removes a peer from the ODR capable peer set
func (self *LesOdr) UnregisterPeer(id string) {
	self.peers.Unregister(id)
}

const (
	MsgBlockBodies = iota
	MsgNodeData
	MsgReceipts
	MsgProofs
)

// Msg encodes a LES message that delivers reply data for a request
type Msg struct {
	MsgType int
	Obj     interface{}
}

// Deliver is called by the LES protocol manager to deliver ODR reply messages to waiting requests
func (self *LesOdr) Deliver(id string, msg *Msg) (processed bool) {
	self.lock.Lock()
	defer self.lock.Unlock()

	for i, req := range self.sentReqs {
		if req.valFunc(self.db, msg) {
			req.deliverChan <- msg
			delete(self.sentReqs, i)
			return true
		}
	}
	return false
}

// networkRequest sends a request to known peers until an answer is received
// or the context is cancelled
func (self *LesOdr) networkRequest(ctx context.Context, rqFunc requestFunc, valFunc validatorFunc) (*Msg, error) {
	req := &sentReq{
		deliverChan: make(chan *Msg),
		valFunc:     valFunc,
	}
	self.lock.Lock()
	reqCnt := self.sentReqCnt
	self.sentReqCnt++
	self.sentReqs[reqCnt] = req
	self.lock.Unlock()

	defer func() {
		self.lock.Lock()
		delete(self.sentReqs, reqCnt)
		self.lock.Unlock()
	}()

	var msg *Msg

	for {
		peers := self.peers.BestPeers()
		if len(peers) == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryPeers):
			}
		}
		for _, peer := range peers {
			rqFunc(peer)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case msg = <-req.deliverChan:
				peer.Promote()
				glog.V(logger.Debug).Infof("networkRequest success")
				return msg, nil
			case <-time.After(requestTimeout):
				peer.Demote()
				glog.V(logger.Debug).Infof("networkRequest timeout")
			}
		}
	}
}

// Retrieve tries to fetch an object from the local db, then from the LES network.
// If the network retrieval was successful, it stores the object in local db.
func (self *LesOdr) Retrieve(ctx context.Context, req light.OdrRequest) (err error) {
	tctx, _ := context.WithTimeout(ctx, time.Second) // temp solution
	lreq := LesRequest(req)
	_, err = self.networkRequest(tctx, lreq.Request, lreq.Valid)
	if err == nil {
		// retrieved from network, store in db
		req.StoreResult(self.db)
	} else {
		glog.V(logger.Debug).Infof("networkRequest  err = %v", err)
	}
	return
}
