// Copyright 2026 The go-ethereum Authors
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

package indexcontract

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

type proverBackend interface {
	ChainConfig() *params.ChainConfig
	HeaderByHash(ctx context.Context, blockHash common.Hash) (*types.Header, error)
	StateByNumberOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error)
}

type Prover struct {
	backend proverBackend
}

func NewProver(backend proverBackend) Prover {
	return Prover{backend: backend}
}

func (p Prover) GetTableRoot(ctx context.Context, refHead common.Hash, contract common.Address, firstBlock, tableSize uint64) (common.Hash, error) {
	fmt.Println("GetTableRoot", firstBlock, tableSize)
	state, head, err := p.backend.StateByNumberOrHash(ctx, rpc.BlockNumberOrHashWithHash(refHead, false))
	//state, head, err := p.backend.StateByNumberOrHash(ctx, rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber))
	if err != nil {
		return common.Hash{}, err
	}
	//fmt.Println("header state root", head.Root, "intermediate root", state.IntermediateRoot(false))
	parent, err := p.backend.HeaderByHash(ctx, head.ParentHash)
	if err != nil {
		return common.Hash{}, err
	}
	chainCtx := &chainContext{chainConfig: p.backend.ChainConfig(), head: head, parent: parent, engine: ethash.NewFaker()}
	witness, err := stateless.NewWitness(head, chainCtx, false)
	if err != nil {
		return common.Hash{}, err
	}
	state.SetWitness(witness)
	context := core.NewEVMBlockContext(head, chainCtx, nil)
	evm := vm.NewEVM(context, state, chainCtx.chainConfig, vm.Config{})

	var callData [64]byte
	binary.BigEndian.PutUint64(callData[24:32], firstBlock)
	binary.BigEndian.PutUint64(callData[56:64], tableSize)
	baseFee := uint256.MustFromBig(head.BaseFee)
	msg := &core.Message{
		GasLimit:  10_000_000,
		GasPrice:  baseFee,
		GasFeeCap: baseFee,
		GasTipCap: baseFee,
		To:        &contract,
		Data:      callData[:],
	}
	result, err := core.ApplyMessage(evm, msg, core.NewGasPool(math.MaxUint64))
	if err != nil {
		return common.Hash{}, err
	}
	if result.Err != nil {
		return common.Hash{}, result.Err
	}
	if len(result.ReturnData) != common.HashLength {
		return common.Hash{}, errors.New("invalid return data size")
	}
	var tableRoot common.Hash
	copy(tableRoot[:], result.ReturnData)
	state.IntermediateRoot(false)
	for node := range witness.State {
		n := ([]byte)(node)
		hash := crypto.Keccak256(n)
		dec, err := rlp.SplitListValues(n)
		dlen := make([]int, len(dec))
		for i, d := range dec {
			dlen[i] = len(d)
		}
		fmt.Printf("state %x : %v %v\n", hash, dlen, err)
	}
	for code := range witness.Codes {
		fmt.Printf("code %x\n", ([]byte)(code))
	}
	return tableRoot, nil
}

type chainContext struct {
	chainConfig  *params.ChainConfig
	head, parent *types.Header
	engine       consensus.Engine
}

func (c *chainContext) Engine() consensus.Engine {
	return c.engine
}

func (c *chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	if hash == c.head.Hash() && number == c.head.Number.Uint64() {
		return c.head
	}
	if hash == c.parent.Hash() && number == c.parent.Number.Uint64() {
		return c.parent
	}
	return nil
}

func (c *chainContext) Config() *params.ChainConfig {
	return c.chainConfig
}

func (c *chainContext) CurrentHeader() *types.Header {
	return c.head
}

func (c *chainContext) GetHeaderByNumber(number uint64) *types.Header {
	if number == c.head.Number.Uint64() {
		return c.head
	}
	if number == c.parent.Number.Uint64() {
		return c.parent
	}
	return nil
}

func (c *chainContext) GetHeaderByHash(hash common.Hash) *types.Header {
	if hash == c.head.Hash() {
		return c.head
	}
	if hash == c.parent.Hash() {
		return c.parent
	}
	return nil
}
