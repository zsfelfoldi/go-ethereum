// Copyright 2014 The go-ethereum Authors
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

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/access"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
)

var nullAddress = common.Address{}

type TrieAccess struct {
	ca      *access.ChainAccess
	trie    *trie.SecureTrie
	root    common.Hash
	address common.Address // if nullAddress, it's the account trie
}

func NewAccountTrieAccess(ca *access.ChainAccess, root common.Hash) *TrieAccess {
	return NewStateTrieAccess(ca, root, nullAddress) //
}

func NewStateTrieAccess(ca *access.ChainAccess, root common.Hash, address common.Address) *TrieAccess {
	return &TrieAccess{
		ca:      ca,
		trie:    trie.NewSecure(root[:], ca.Db()),
		root:    root,
		address: address,
	}
}

func (self *TrieAccess) Trie() *trie.SecureTrie {
	return self.trie
}

func (self *TrieAccess) Get(key []byte) ([]byte, error) {
	r := &TrieEntryAccess{trie: self.trie, key: key}
	err := self.ca.Retrieve(r, true)
	return r.value, err
}

type TrieEntryAccess struct {
	trie       *trie.SecureTrie
	address    common.Address // if nullAddress, it's the account trie
	key, value []byte
	proof      *trie.TrieProof
	skipLevels int // set by DbGet() if unsuccessful
}

func (self *TrieEntryAccess) Request(peer *access.Peer) error {
	if self.address == nullAddress {
		req := &access.AcctProofReq{
			RootHash:   common.BytesToHash(self.trie.Root()),
			Address:    common.BytesToAddress(self.key),
			SkipLevels: self.skipLevels,
		}
		return peer.GetAcctProof([]*access.AcctProofReq{req})
	} else {
		req := &access.StorageDataProofReq{
			RootHash:   common.BytesToHash(self.trie.Root()),
			Address:    self.address,
			Key:        common.BytesToHash(self.key),
			SkipLevels: self.skipLevels,
		}
		return peer.GetStorageDataProof([]*access.StorageDataProofReq{req})
	}
}

func (self *TrieEntryAccess) Valid(msg *access.Msg) bool {

	if msg.MsgType != access.MsgProof {
		return false
	}
	proof := msg.Obj.(*trie.TrieProof)
	valid := proof.Verify(self.key, proof.Value, self.trie.Root())
	if valid {
		self.proof = proof
		self.value = proof.Value
	}
	return valid
}

func (self *TrieEntryAccess) DbGet() bool {
	//self.value, self.skipLevels := self.trie.Get(self.key)
	self.value = self.trie.Get(self.key)
	return self.value != nil // distinguish no entry from unsuccessful retrieve
}

func (self *TrieEntryAccess) DbPut() {
	//recreate nodes from merkle proof, store
}

type NodeDataAccess struct {
	db   common.Database
	hash common.Hash
	data []byte
}

func (self *NodeDataAccess) Request(peer *access.Peer) error {
	return peer.GetNodeData([]common.Hash{self.hash})
}

func (self *NodeDataAccess) Valid(msg *access.Msg) bool {
	if msg.MsgType != access.MsgNodeData {
		return false
	}
	reply := msg.Obj.([][]byte)
	if len(reply) != 1 {
		return false
	}
	data := reply[0]
	hash := crypto.Sha3Hash(data)
	valid := bytes.Compare(self.hash[:], hash[:]) == 0
	if valid {
		self.data = data
	}
	return valid
}

func (self *NodeDataAccess) DbGet() bool {
	data, _ := self.db.Get(self.hash[:])
	if len(data) == 0 {
		return false
	}
	self.data = data
	return true
}

func (self *NodeDataAccess) DbPut() {
	self.db.Put(self.hash[:], self.data)
}

func RetrieveNodeData(ca *access.ChainAccess, hash common.Hash) []byte {
	r := &NodeDataAccess{db: ca.Db(), hash: hash}
	ca.Retrieve(r, true)
	return r.data
}
