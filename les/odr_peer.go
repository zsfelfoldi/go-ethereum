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
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
)

type ProofReq struct {
	Root common.Hash
	Key  []byte
}

type getBlockBodiesFn func([]common.Hash) error
type getNodeDataFn func([]common.Hash) error
type getReceiptsFn func([]common.Hash) error
type getProofsFn func([]*ProofReq) error

// odrPeer stores ODR-specific information about LES peers that are able to serve requests
type odrPeer struct {
	id   string      // Unique identifier of the peer
	head common.Hash // Hash of the peers latest known block

	rep int32 // Simple peer reputation

	GetBlockBodies getBlockBodiesFn
	GetNodeData    getNodeDataFn
	GetReceipts    getReceiptsFn
	GetProofs      getProofsFn

	version int // LES protocol version number
}

// newPeer creates a new ODR peer
func newOdrPeer(id string, version int, head common.Hash, getBlockBodies getBlockBodiesFn, getNodeData getNodeDataFn, getReceipts getReceiptsFn, getProofs getProofsFn) *odrPeer {
	return &odrPeer{
		id:             id,
		head:           head,
		GetBlockBodies: getBlockBodies,
		GetNodeData:    getNodeData,
		GetReceipts:    getReceipts,
		GetProofs:      getProofs,
		version:        version,
	}
}

// Id returns a peer's ID string
func (p *odrPeer) Id() string {
	return p.id
}

// Promote increases the peer's reputation
func (p *odrPeer) Promote() {
	atomic.AddInt32(&p.rep, 1)
}

// Demote decreases the peer's reputation or leaves it at 0.
func (p *odrPeer) Demote() {
	for {
		// Calculate the new reputation value
		prev := atomic.LoadInt32(&p.rep)
		next := prev / 2

		// Try to update the old value
		if atomic.CompareAndSwapInt32(&p.rep, prev, next) {
			return
		}
	}
}

// odrPeerSet represents the collection of active peer participating in the block
// download procedure.
type odrPeerSet struct {
	peers map[string]*odrPeer
	lock  sync.RWMutex
}

// newPeerSet creates a new peer set top track the active download sources.
func newOdrPeerSet() *odrPeerSet {
	return &odrPeerSet{
		peers: make(map[string]*odrPeer),
	}
}

// Register injects a new peer into the working set, or returns an error if the
// peer is already known.
func (ps *odrPeerSet) Register(p *odrPeer) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if _, ok := ps.peers[p.id]; ok {
		return errAlreadyRegistered
	}
	ps.peers[p.id] = p
	return nil
}

// Unregister removes a remote peer from the active set, disabling any further
// actions to/from that particular entity.
func (ps *odrPeerSet) Unregister(id string) error {
	ps.lock.Lock()
	defer ps.lock.Unlock()

	if _, ok := ps.peers[id]; !ok {
		return errNotRegistered
	}
	delete(ps.peers, id)
	return nil
}

// odrPeer retrieves the registered peer with the given id.
func (ps *odrPeerSet) odrPeer(id string) *odrPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.peers[id]
}

// Len returns if the current number of peers in the set.
func (ps *odrPeerSet) Len() int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return len(ps.peers)
}

// AllPeers retrieves a flat list of all the peers within the set.
func (ps *odrPeerSet) AllPeers() []*odrPeer {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	list := make([]*odrPeer, 0, len(ps.peers))
	for _, p := range ps.peers {
		list = append(list, p)
	}
	return list
}

// BestPeers returns an ordered list of available peers, starting with the
// highest reputation
func (ps *odrPeerSet) BestPeers() []*odrPeer {
	list := ps.AllPeers()
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if atomic.LoadInt32(&list[i].rep) < atomic.LoadInt32(&list[j].rep) {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	return list
}
