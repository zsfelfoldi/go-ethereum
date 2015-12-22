// Copyright 2015 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package les

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/compiler"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	rpc "github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/net/context"
)

// PublicLightEthereumApi provides an API to access LightEthereum related information.
// It offers only methods that operate on public data that is freely available to anyone.
type PublicLightEthereumApi struct {
	e *LightEthereum
}

// NewPublicEthereumApi creates a new Etheruem protocol API.
func NewPublicLightEthereumAPI(e *LightEthereum) *PublicLightEthereumApi {
	return &PublicLightEthereumApi{e}
}

// GetCompilers returns the collection of available smart contract compilers
func (s *PublicLightEthereumApi) GetCompilers() ([]string, error) {
	solc, err := s.e.Solc()
	if err != nil {
		return nil, err
	}

	if solc != nil {
		return []string{"Solidity"}, nil
	}

	return nil, nil
}

// CompileSolidity compiles the given solidity source
func (s *PublicLightEthereumApi) CompileSolidity(source string) (map[string]*compiler.Contract, error) {
	solc, err := s.e.Solc()
	if err != nil {
		return nil, err
	}

	if solc == nil {
		return nil, errors.New("solc (solidity compiler) not found")
	}

	return solc.Compile(source)
}

// ProtocolVersion returns the current Ethereum protocol version this node supports
func (s *PublicLightEthereumApi) ProtocolVersion() *rpc.HexNumber {
	return rpc.NewHexNumber(s.e.LesVersion())
}

// Syncing returns false in case the node is currently not synching with the network. It can be up to date or has not
// yet received the latest block headers from its pears. In case it is synchronizing an object with 3 properties is
// returned:
// - startingBlock: block number this node started to synchronise from
// - currentBlock: block number this node is currently importing
// - highestBlock: block number of the highest block header this node has received from peers
func (s *PublicLightEthereumApi) Syncing() (interface{}, error) {
	origin, current, height := s.e.Downloader().Progress()
	if current < height {
		return map[string]interface{}{
			"startingBlock": rpc.NewHexNumber(origin),
			"currentBlock":  rpc.NewHexNumber(current),
			"highestBlock":  rpc.NewHexNumber(height),
		}, nil
	}
	return false, nil
}

// PublicLightChainAPI provides an API to access the Ethereum blockchain.
// It offers only methods that operate on public data that is freely available to anyone.
type PublicLightChainAPI struct {
	bc       *light.LightChain
	odr      light.OdrBackend
	eventMux *event.TypeMux
	am       *accounts.Manager
}

// NewPublicLightChainAPI creates a new Etheruem blockchain API.
func NewPublicLightChainAPI(bc *light.LightChain, odr light.OdrBackend, eventMux *event.TypeMux, am *accounts.Manager) *PublicLightChainAPI {
	return &PublicLightChainAPI{bc: bc, odr: odr, eventMux: eventMux, am: am}
}

// BlockNumber returns the block number of the chain head.
func (s *PublicLightChainAPI) BlockNumber() *big.Int {
	return s.bc.CurrentHeader().Number
}

// GetBalance returns the amount of wei for the given address in the state of the given block number.
// When block number equals rpc.LatestBlockNumber the current block is used.
func (s *PublicLightChainAPI) GetBalance(ctx context.Context, address common.Address, blockNr rpc.BlockNumber) (*big.Int, error) {
	header := headerByNumber(s.bc, blockNr)
	if header == nil {
		return nil, nil
	}

	state := light.NewLightState(header.Root, s.odr)
	return state.GetBalance(ctx, address)
}

// headerByNumber is a commonly used helper function which retrieves and returns the header for the given block number. It
// returns nil when no block could be found.
func headerByNumber(bc *light.LightChain, blockNr rpc.BlockNumber) *types.Header {
	if blockNr == rpc.LatestBlockNumber {
		return bc.CurrentHeader()
	}

	return bc.GetHeaderByNumber(uint64(blockNr))
}

// blockByNumber is a commonly used helper function which retrieves and returns the block for the given block number. It
// returns nil when no block could be found.
func blockByNumber(ctx context.Context, bc *light.LightChain, blockNr rpc.BlockNumber) (*types.Block, error) {
	if blockNr == rpc.LatestBlockNumber {
		return bc.GetBlock(ctx, bc.CurrentHeader().Hash())
	}

	return bc.GetBlockByNumber(ctx, uint64(blockNr))
}

// GetBlockByNumber returns the requested block. When blockNr is -1 the chain head is returned. When fullTx is true all
// transactions in the block are returned in full detail, otherwise only the transaction hash is returned.
func (s *PublicLightChainAPI) GetBlockByNumber(ctx context.Context, blockNr rpc.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	block, err := blockByNumber(ctx, s.bc, blockNr)
	if block != nil {
		return s.rpcOutputBlock(block, true, fullTx)
	}
	return nil, err
}

// GetBlockByHash returns the requested block. When fullTx is true all transactions in the block are returned in full
// detail, otherwise only the transaction hash is returned.
func (s *PublicLightChainAPI) GetBlockByHash(ctx context.Context, blockHash common.Hash, fullTx bool) (map[string]interface{}, error) {
	block, err := s.bc.GetBlock(ctx, blockHash)
	if block != nil {
		return s.rpcOutputBlock(block, true, fullTx)
	}
	return nil, err
}

// GetUncleByBlockNumberAndIndex returns the uncle block for the given block hash and index. When fullTx is true
// all transactions in the block are returned in full detail, otherwise only the transaction hash is returned.
func (s *PublicLightChainAPI) GetUncleByBlockNumberAndIndex(ctx context.Context, blockNr rpc.BlockNumber, index rpc.HexNumber) (map[string]interface{}, error) {
	if blockNr == rpc.PendingBlockNumber {
		return nil, nil
	}

	block, err := blockByNumber(ctx, s.bc, blockNr)
	if block != nil {
		uncles := block.Uncles()
		if index.Int() < 0 || index.Int() >= len(uncles) {
			glog.V(logger.Debug).Infof("uncle block on index %d not found for block #%d", index.Int(), blockNr)
			return nil, nil
		}
		block = types.NewBlockWithHeader(uncles[index.Int()])
		return s.rpcOutputBlock(block, false, false)
	}
	return nil, err
}

// GetUncleByBlockHashAndIndex returns the uncle block for the given block hash and index. When fullTx is true
// all transactions in the block are returned in full detail, otherwise only the transaction hash is returned.
func (s *PublicLightChainAPI) GetUncleByBlockHashAndIndex(ctx context.Context, blockHash common.Hash, index rpc.HexNumber) (map[string]interface{}, error) {
	block, err := s.bc.GetBlock(ctx, blockHash)
	if block != nil {
		uncles := block.Uncles()
		if index.Int() < 0 || index.Int() >= len(uncles) {
			glog.V(logger.Debug).Infof("uncle block on index %d not found for block %s", index.Int(), blockHash.Hex())
			return nil, nil
		}
		block = types.NewBlockWithHeader(uncles[index.Int()])
		return s.rpcOutputBlock(block, false, false)
	}
	return nil, err
}

// GetUncleCountByBlockNumber returns number of uncles in the block for the given block number
func (s *PublicLightChainAPI) GetUncleCountByBlockNumber(ctx context.Context, blockNr rpc.BlockNumber) *rpc.HexNumber {
	if blockNr == rpc.PendingBlockNumber {
		return rpc.NewHexNumber(0)
	}

	if block, _ := blockByNumber(ctx, s.bc, blockNr); block != nil {
		return rpc.NewHexNumber(len(block.Uncles()))
	}
	return nil
}

// GetUncleCountByBlockHash returns number of uncles in the block for the given block hash
func (s *PublicLightChainAPI) GetUncleCountByBlockHash(ctx context.Context, blockHash common.Hash) *rpc.HexNumber {
	if block, _ := s.bc.GetBlock(ctx, blockHash); block != nil {
		return rpc.NewHexNumber(len(block.Uncles()))
	}
	return nil
}

// NewBlocksArgs allows the user to specify if the returned block should include transactions and in which format.
type NewBlocksArgs struct {
	IncludeTransactions bool `json:"includeTransactions"`
	TransactionDetails  bool `json:"transactionDetails"`
}

/*
// NewBlocks triggers a new block event each time a block is appended to the chain. It accepts an argument which allows
// the caller to specify whether the output should contain transactions and in what format.
func (s *PublicLightChainAPI) NewBlocks(args NewBlocksArgs) (rpc.Subscription, error) {
	sub := s.eventMux.Subscribe(core.ChainEvent{})

	output := func(rawBlock interface{}) interface{} {
		if event, ok := rawBlock.(core.ChainEvent); ok {
			notification, err := s.rpcOutputBlock(event.Block, args.IncludeTransactions, args.TransactionDetails)
			if err == nil {
				return notification
			}
		}
		return rawBlock
	}

	return rpc.NewSubscriptionWithOutputFormat(sub, output), nil
}*/

// GetCode returns the code stored at the given address in the state for the given block number.
func (s *PublicLightChainAPI) GetCode(ctx context.Context, address common.Address, blockNr rpc.BlockNumber) (string, error) {
	return s.GetData(ctx, address, blockNr)
}

// GetData returns the data stored at the given address in the state for the given block number.
func (s *PublicLightChainAPI) GetData(ctx context.Context, address common.Address, blockNr rpc.BlockNumber) (string, error) {
	if header := headerByNumber(s.bc, blockNr); header != nil {
		state := light.NewLightState(header.Root, s.odr)
		res, err := state.GetCode(ctx, address)
		if err != nil {
			return "", err
		}
		if len(res) == 0 { // backwards compatibility
			return "0x", nil
		}
		return common.ToHex(res), nil
	}

	return "0x", nil
}

// GetStorageAt returns the storage from the state at the given address, key and block number.
func (s *PublicLightChainAPI) GetStorageAt(ctx context.Context, address common.Address, key string, blockNr rpc.BlockNumber) (string, error) {
	if header := headerByNumber(s.bc, blockNr); header != nil {
		state := light.NewLightState(header.Root, s.odr)
		res, err := state.GetState(ctx, address, common.HexToHash(key))
		if err != nil {
			return "", err
		}

		return res.Hex(), nil
	}

	return "0x", nil
}

// callmsg is the message type used for call transations.
type callmsg struct {
	from          *light.StateObject
	to            *common.Address
	gas, gasPrice *big.Int
	value         *big.Int
	data          []byte
}

// accessor boilerplate to implement core.Message
func (m callmsg) From() (common.Address, error) { return m.from.Address(), nil }
func (m callmsg) Nonce() uint64                 { return m.from.Nonce() }
func (m callmsg) To() *common.Address           { return m.to }
func (m callmsg) GasPrice() *big.Int            { return m.gasPrice }
func (m callmsg) Gas() *big.Int                 { return m.gas }
func (m callmsg) Value() *big.Int               { return m.value }
func (m callmsg) Data() []byte                  { return m.data }

type CallArgs struct {
	From     common.Address `json:"from"`
	To       common.Address `json:"to"`
	Gas      rpc.HexNumber  `json:"gas"`
	GasPrice rpc.HexNumber  `json:"gasPrice"`
	Value    rpc.HexNumber  `json:"value"`
	Data     string         `json:"data"`
}

func (s *PublicLightChainAPI) doCall(ctx context.Context, args CallArgs, blockNr rpc.BlockNumber) (string, *big.Int, error) {
	if header := headerByNumber(s.bc, blockNr); header != nil {
		stateDb := light.NewLightState(header.Root, s.odr)

		stateDb = stateDb.Copy()
		var addr common.Address
		if args.From == (common.Address{}) {
			accounts, err := s.am.Accounts()
			if err != nil || len(accounts) == 0 {
				addr = common.Address{}
			} else {
				addr = accounts[0].Address
			}
		} else {
			addr = args.From
		}

		from, err := stateDb.GetOrNewStateObject(ctx, addr)
		if err != nil {
			return "0x", nil, err
		}

		from.SetBalance(common.MaxBig)

		msg := callmsg{
			from:     from,
			to:       &args.To,
			gas:      args.Gas.BigInt(),
			gasPrice: args.GasPrice.BigInt(),
			value:    args.Value.BigInt(),
			data:     common.FromHex(args.Data),
		}

		if msg.gas.Cmp(common.Big0) == 0 {
			msg.gas = big.NewInt(50000000)
		}

		if msg.gasPrice.Cmp(common.Big0) == 0 {
			msg.gasPrice = new(big.Int).Mul(big.NewInt(50), common.Shannon)
		}

		header := s.bc.CurrentHeader()
		vmenv := light.NewEnv(ctx, stateDb, s.bc, msg, header)
		gp := new(core.GasPool).AddGas(common.MaxBig)
		res, gas, err := core.ApplyMessage(vmenv, msg, gp)
		if vmenv.Error() != nil {
			return "0x", common.Big0, vmenv.Error()
		}
		if len(res) == 0 { // backwards compatability
			return "0x", gas, err
		}
		return common.ToHex(res), gas, err
	}

	return "0x", common.Big0, nil
}

// Call executes the given transaction on the state for the given block number.
// It doesn't make and changes in the state/blockchain and is usefull to execute and retrieve values.
func (s *PublicLightChainAPI) Call(ctx context.Context, args CallArgs, blockNr rpc.BlockNumber) (string, error) {
	result, _, err := s.doCall(ctx, args, blockNr)
	return result, err
}

// EstimateGas returns an estimate of the amount of gas needed to execute the given transaction.
func (s *PublicLightChainAPI) EstimateGas(ctx context.Context, args CallArgs) (*rpc.HexNumber, error) {
	_, gas, err := s.doCall(ctx, args, rpc.LatestBlockNumber)
	return rpc.NewHexNumber(gas), err
}

// rpcOutputBlock converts the given block to the RPC output which depends on fullTx. If inclTx is true transactions are
// returned. When fullTx is true the returned block contains full transaction details, otherwise it will only contain
// transaction hashes.
func (s *PublicLightChainAPI) rpcOutputBlock(b *types.Block, inclTx bool, fullTx bool) (map[string]interface{}, error) {
	return eth.RpcOutputBlock(b, inclTx, fullTx, s.bc.GetTd(b.Hash()))
}
