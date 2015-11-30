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

package light

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"golang.org/x/net/context"
)

// The starting nonce determines the default nonce when new accounts are being
// created.
var StartingNonce uint64

// StateDBs within the ethereum protocol are used to store anything
// within the merkle trie. LightStates take care of caching and storing
// nested states. It's the general query interface to retrieve:
// * Contracts
// * Accounts
type LightState struct {
	odr  OdrBackend
	trie *LightTrie

	stateObjects map[string]*StateObject
}

// Create a new state from a given trie
func NewLightState(root common.Hash, odr OdrBackend) *LightState {
	tr := NewLightTrie(root, odr, true)
	return &LightState{
		odr:          odr,
		trie:         tr,
		stateObjects: make(map[string]*StateObject),
	}
}

func (self *LightState) HasAccount(ctx context.Context, addr common.Address) (bool, error) {
	so, err := self.GetStateObject(ctx, addr)
	return so != nil, err
}

// Retrieve the balance from the given address or 0 if object not found
func (self *LightState) GetBalance(ctx context.Context, addr common.Address) (*big.Int, error) {
	stateObject, err := self.GetStateObject(ctx, addr)
	if err != nil {
		return common.Big0, err
	}
	if stateObject != nil {
		return stateObject.balance, nil
	}

	return common.Big0, nil
}

func (self *LightState) GetNonce(ctx context.Context, addr common.Address) (uint64, error) {
	stateObject, err := self.GetStateObject(ctx, addr)
	if err != nil {
		return 0, err
	}
	if stateObject != nil {
		return stateObject.nonce, nil
	}
	return 0, nil
}

func (self *LightState) GetCode(ctx context.Context, addr common.Address) ([]byte, error) {
	stateObject, err := self.GetStateObject(ctx, addr)
	if err != nil {
		return nil, err
	}
	if stateObject != nil {
		return stateObject.code, nil
	}
	return nil, nil
}

func (self *LightState) GetState(ctx context.Context, a common.Address, b common.Hash) (common.Hash, error) {
	stateObject, err := self.GetStateObject(ctx, a)
	if err == nil && stateObject != nil {
		return stateObject.GetState(ctx, b)
	}
	return common.Hash{}, err
}

func (self *LightState) IsDeleted(ctx context.Context, addr common.Address) (bool, error) {
	stateObject, err := self.GetStateObject(ctx, addr)
	if err == nil && stateObject != nil {
		return stateObject.remove, nil
	}
	return false, err
}

/*
 * SETTERS
 */

func (self *LightState) AddBalance(ctx context.Context, addr common.Address, amount *big.Int) error {
	stateObject, err := self.GetOrNewStateObject(ctx, addr)
	if err == nil && stateObject != nil {
		stateObject.AddBalance(amount)
	}
	return err
}

func (self *LightState) SetNonce(ctx context.Context, addr common.Address, nonce uint64) error {
	stateObject, err := self.GetOrNewStateObject(ctx, addr)
	if err == nil && stateObject != nil {
		stateObject.SetNonce(nonce)
	}
	return err
}

func (self *LightState) SetCode(ctx context.Context, addr common.Address, code []byte) error {
	stateObject, err := self.GetOrNewStateObject(ctx, addr)
	if err == nil && stateObject != nil {
		stateObject.SetCode(code)
	}
	return err
}

func (self *LightState) SetState(ctx context.Context, addr common.Address, key common.Hash, value common.Hash) error {
	stateObject, err := self.GetOrNewStateObject(ctx, addr)
	if err == nil && stateObject != nil {
		stateObject.SetState(key, value)
	}
	return err
}

func (self *LightState) Delete(ctx context.Context, addr common.Address) (bool, error) {
	stateObject, err := self.GetOrNewStateObject(ctx, addr)
	if err == nil && stateObject != nil {
		stateObject.MarkForDeletion()
		stateObject.balance = new(big.Int)

		return true, nil
	}

	return false, err
}

//
// Get, set, new state object methods
//

// Retrieve a state object given my the address. Nil if not found
func (self *LightState) GetStateObject(ctx context.Context, addr common.Address) (stateObject *StateObject, err error) {
	stateObject = self.stateObjects[addr.Str()]
	if stateObject != nil {
		if stateObject.deleted {
			stateObject = nil
		}
		return stateObject, nil
	}
	data, err := self.trie.Get(ctx, addr[:])
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	stateObject, err = NewStateObjectFromBytes(ctx, addr, []byte(data), self.odr)
	if err != nil {
		return nil, err
	}
	self.SetStateObject(stateObject)

	return stateObject, nil
}

func (self *LightState) SetStateObject(object *StateObject) {
	self.stateObjects[object.Address().Str()] = object
}

// Retrieve a state object or create a new state object if nil
func (self *LightState) GetOrNewStateObject(ctx context.Context, addr common.Address) (*StateObject, error) {
	stateObject, err := self.GetStateObject(ctx, addr)
	if err == nil && (stateObject == nil || stateObject.deleted) {
		stateObject, err = self.CreateStateObject(ctx, addr)
	}
	return stateObject, err
}

// NewStateObject create a state object whether it exist in the trie or not
func (self *LightState) newStateObject(addr common.Address) *StateObject {
	if glog.V(logger.Core) {
		glog.Infof("(+) %x\n", addr)
	}

	stateObject := NewStateObject(addr, self.odr)
	stateObject.SetNonce(StartingNonce)
	self.stateObjects[addr.Str()] = stateObject

	return stateObject
}

// Creates creates a new state object and takes ownership. This is different from "NewStateObject"
func (self *LightState) CreateStateObject(ctx context.Context, addr common.Address) (*StateObject, error) {
	// Get previous (if any)
	so, err := self.GetStateObject(ctx, addr)
	if err != nil {
		return nil, err
	}
	// Create a new one
	newSo := self.newStateObject(addr)

	// If it existed set the balance to the new account
	if so != nil {
		newSo.balance = so.balance
	}

	return newSo, nil
}

//
// Setting, copying of the state methods
//

func (self *LightState) Copy() *LightState {
	// ignore error - we assume state-to-be-copied always exists
	state := NewLightState(common.Hash{}, self.odr)
	state.trie = self.trie
	for k, stateObject := range self.stateObjects {
		state.stateObjects[k] = stateObject.Copy()
	}

	return state
}

func (self *LightState) Set(state *LightState) {
	self.trie = state.trie
	self.stateObjects = state.stateObjects
}
