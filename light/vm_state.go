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
	"github.com/ethereum/go-ethereum/core/vm"
	"golang.org/x/net/context"
)

//type errHandlerFn func(err error)

// VMState implements vm.Database
type VMState struct {
	ctx context.Context
	state *LightState
	env *VMEnv
}

func (s *VMState) errHandler(err error) {
	if err != nil && s.env.err == nil  {
		s.env.err = err
	}
}

func (s *VMState) GetAccount(addr common.Address) vm.Account {
	so, err := s.state.GetStateObject(s.ctx, addr)
	s.errHandler(err)
	return so
}

func (s *VMState) CreateAccount(addr common.Address) vm.Account {
	so, err := s.state.CreateStateObject(s.ctx, addr)
	s.errHandler(err)
	return so
}

func (s *VMState) AddBalance(addr common.Address, amount *big.Int) {
	err := s.state.AddBalance(s.ctx, addr, amount)
	s.errHandler(err)
}

func (s *VMState) GetBalance(addr common.Address) *big.Int {
	res, err := s.state.GetBalance(s.ctx, addr)
	s.errHandler(err)
	return res
}

func (s *VMState) GetNonce(addr common.Address) uint64 {
	res, err := s.state.GetNonce(s.ctx, addr)
	s.errHandler(err)
	return res
}

func (s *VMState) SetNonce(addr common.Address, nonce uint64) {
	err := s.state.SetNonce(s.ctx, addr, nonce)
	s.errHandler(err)
}

func (s *VMState) GetCode(addr common.Address) []byte {
	res, err := s.state.GetCode(s.ctx, addr)
	s.errHandler(err)
	return res
}

func (s *VMState) SetCode(addr common.Address, code []byte) {
	err := s.state.SetCode(s.ctx, addr, code)
	s.errHandler(err)
}

func (s *VMState) AddRefund(gas *big.Int) {
	s.state.AddRefund(gas)
}

func (s *VMState) GetRefund() *big.Int {
	return s.state.GetRefund()
}

func (s *VMState) GetState(a common.Address, b common.Hash) common.Hash {
	res, err := s.state.GetState(s.ctx, a, b)
	s.errHandler(err)
	return res
}

func (s *VMState) SetState(addr common.Address, key common.Hash, value common.Hash) {
	err := s.state.SetState(s.ctx, addr, key, value)
	s.errHandler(err)
}

func (s *VMState) Delete(addr common.Address) bool {
	res, err := s.state.Delete(s.ctx, addr)
	s.errHandler(err)
	return res
}

func (s *VMState) Exist(addr common.Address) bool {
	res, err := s.state.HasAccount(s.ctx, addr)
	s.errHandler(err)
	return res
}

func (s *VMState) IsDeleted(addr common.Address) bool {
	res, err := s.state.IsDeleted(s.ctx, addr)
	s.errHandler(err)
	return res
}