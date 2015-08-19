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

// Package access provides a layer to handle local blockchain database and
// on-demand network retrieval
package access

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
)

var (
	errNoPeer  = errors.New("no suitable peer found")
	errNotInDb = errors.New("object not found in database")
)

var requestTimeout = time.Second * 1

type ChainAccess struct {
	db          common.Database
	lock        sync.Mutex
	valFunc     validatorFunc
	deliverChan chan *Msg
	peers       *peerSet

	// p2p access objects
	// parameters (light/full/archive)
}

func NewDbChainAccess(db common.Database) *ChainAccess {
	return &ChainAccess{db: db, peers: newPeerSet()}
}

func (self *ChainAccess) Db() common.Database {
	return self.db
}

func (self *ChainAccess) RegisterPeer(id string, version int, head common.Hash, getBlockBodies getBlockBodiesFn, getNodeData getNodeDataFn, getReceipts getReceiptsFn, getAcctProof getAcctProofFn, getStorageDataProof getStorageDataProofFn) error {
	glog.V(logger.Detail).Infoln("Registering peer", id)
	if err := self.peers.Register(newPeer(id, version, head, getBlockBodies, getNodeData, getReceipts, getAcctProof, getStorageDataProof)); err != nil {
		glog.V(logger.Error).Infoln("Register failed:", err)
		return err
	}
	return nil
}

const (
	MsgBlockBodies = iota
	MsgNodeData
	MsgReceipts
	MsgProof
)

type Msg struct {
	MsgType int
	Obj     interface{}
}

type ObjectAccess interface {
	// database storage
	DbGet() bool
	DbPut()
	// network retrieval
	Request(*Peer) error
	Valid(*Msg) bool // if true, keeps the retrieved object
}

type requestFunc func(*Peer) error
type validatorFunc func(*Msg) bool

func (self *ChainAccess) Deliver(id string, msg *Msg) (processed bool) {
	self.lock.Lock()
	valFunc := self.valFunc
	chn := self.deliverChan
	self.lock.Unlock()
	if (valFunc != nil) && (chn != nil) && valFunc(msg) {
		chn <- msg
		return true
	}
	return false
}

func (self *ChainAccess) networkRequest(rqFunc requestFunc, valFunc validatorFunc) (*Msg, error) {
	self.lock.Lock()
	self.deliverChan = make(chan *Msg)
	self.valFunc = valFunc
	self.lock.Unlock()

	fmt.Println("networkRequest")

	var msg *Msg
	for {
		peer := self.peers.Select()
		if peer == nil {
			return nil, errNoPeer
		}
		rqFunc(peer)
		select {
		case msg = <-self.deliverChan:
			peer.Promote()
			fmt.Println("networkRequest success")
			break
		case <-time.After(requestTimeout):
			peer.Demote()
			fmt.Println("networkRequest timeout")
		}
	}

	self.lock.Lock()
	self.deliverChan = nil
	self.valFunc = nil
	self.lock.Unlock()

	return msg, nil
}

func (self *ChainAccess) Retrieve(obj ObjectAccess, odr bool) (err error) {
	// look in db
	if obj.DbGet() {
		return nil
	}
	if odr {
		// not found in db, trying the network
		_, err = self.networkRequest(obj.Request, obj.Valid)
		if err == nil {
			// retrieved from network, store in db
			obj.DbPut()
		}
		return
	} else {
		return errNotInDb
	}
}