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
	"encoding/binary"
	"errors"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
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

func (p Prover) ProveTableRoot(refHead *types.Header, contract common.Address, firstBlock, tableSize uint64, proofNodes, proofCodes map[common.Hash][]byte) (common.Hash, error) {
	//fmt.Println("ProveTableRoot", firstBlock, tableSize)
	state, err := p.backend.StateProverAt(refHead, proofNodes, proofCodes)
	if err != nil {
		return common.Hash{}, err
	}
	return getTableRoot(state, p.backend.ChainConfig(), refHead, contract, firstBlock, tableSize)
}

func getTableRoot(state *state.StateDB, chainConfig *params.ChainConfig, refHead *types.Header, contract common.Address, firstBlock, tableSize uint64) (common.Hash, error) {
	chainCtx := &chainContext{chainConfig: chainConfig, head: refHead, engine: ethash.NewFaker()}
	context := core.NewEVMBlockContext(refHead, chainCtx, new(common.Address))
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

type Verifier struct {
	chainConfig *params.ChainConfig
	trieConfig  *triedb.Config
}

func NewVerifier(chainConfig *params.ChainConfig, trieConfig *triedb.Config) Verifier {
	return Verifier{
		chainConfig: chainConfig,
		trieConfig:  trieConfig,
	}
}

func (v Verifier) GetProvenTableRoot(refHead *types.Header, contract common.Address, firstBlock, tableSize uint64, proofNodes, proofCodes map[common.Hash][]byte) (common.Hash, error) {
	proofDb := triedb.NewProofReader(proofNodes, v.trieConfig)
	codeDb := state.NewProofCodeReader(proofCodes)
	state, err := state.New(refHead.Root, state.NewMPTDatabase(proofDb, codeDb)) //TODO UBT
	if err != nil {
		return common.Hash{}, err
	}
	return getTableRoot(state, v.chainConfig, refHead, contract, firstBlock, tableSize)
}
