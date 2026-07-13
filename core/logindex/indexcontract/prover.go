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
	"github.com/holiman/uint256"
)

type proverBackend interface {
	ChainConfig() *params.ChainConfig
	StateProverAt(header *types.Header, proofNodes, proofCodes map[common.Hash][]byte) (*state.StateDB, error)
}

type Prover struct {
	backend proverBackend
}

func NewProver(backend proverBackend) Prover {
	return Prover{backend: backend}
}

func (p Prover) ProveTableRoot(ctx context.Context, refHead *types.Header, contract common.Address, firstBlock, tableSize uint64, proofNodes, proofCodes map[common.Hash][]byte) (common.Hash, error) {
	fmt.Println("ProveTableRoot", firstBlock, tableSize)
	state, err := p.backend.StateProverAt(refHead, proofNodes, proofCodes)
	if err != nil {
		return common.Hash{}, err
	}
	//fmt.Println("header state root", head.Root, "intermediate root", state.IntermediateRoot(false))
	chainCtx := &chainContext{chainConfig: p.backend.ChainConfig(), head: refHead, engine: ethash.NewFaker()}
	witness, err := stateless.NewWitness(refHead, chainCtx, false)
	if err != nil {
		return common.Hash{}, err
	}
	state.SetWitness(witness)
	context := core.NewEVMBlockContext(refHead, chainCtx, nil)
	evm := vm.NewEVM(context, state, chainCtx.chainConfig, vm.Config{})

	var callData [64]byte
	binary.BigEndian.PutUint64(callData[24:32], firstBlock)
	binary.BigEndian.PutUint64(callData[56:64], tableSize)
	baseFee := uint256.MustFromBig(refHead.BaseFee)
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
	chainConfig *params.ChainConfig
	head        *types.Header
	engine      consensus.Engine
}

func (c *chainContext) Engine() consensus.Engine {
	return c.engine
}

func (c *chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	if hash == c.head.Hash() && number == c.head.Number.Uint64() {
		return c.head
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
	return nil
}

func (c *chainContext) GetHeaderByHash(hash common.Hash) *types.Header {
	if hash == c.head.Hash() {
		return c.head
	}
	return nil
}
