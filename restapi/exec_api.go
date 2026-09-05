// Copyright 2025 The go-ethereum Authors
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

package restapi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/logindex/logquery"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/params/forks"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/mux"
)

type backend interface {
	HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error)
	HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error)
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error)
	BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error)
	GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error)
	GetCanonicalTransaction(txHash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64)
	GetPoolTransaction(txHash common.Hash) *types.Transaction
	ChainConfig() *params.ChainConfig
	LogQuery() *logquery.Matcher
	ChainView(hash common.Hash, number uint64) *core.ChainView
}

type execApiServer struct {
	apiBackend backend
}

func ExecutionAPI(server *Server, backend backend) API {
	api := execApiServer{apiBackend: backend}
	return func(router *mux.Router) {
		router.HandleFunc("/eth/v1/exec/headers/{blockid}", server.WrapHandler(api.handleHeaders, false, false, true)).Methods("GET")
		/*router.HandleFunc("/eth/v1/exec/blocks/{blockid}", server.WrapHandler(api.handleBlocks, false, false, true)).Methods("GET")
		router.HandleFunc("/eth/v1/exec/block_receipts/{blockid}", server.WrapHandler(api.handleBlockReceipts, false, false, true)).Methods("GET")
		router.HandleFunc("/eth/v1/exec/transaction/{txhash}", server.WrapHandler(api.handleTransaction, false, false, true)).Methods("GET")
		router.HandleFunc("/eth/v1/exec/transaction_by_index/{blockid}", server.WrapHandler(api.handleTxByIndex, false, false, true)).Methods("GET")
		router.HandleFunc("/eth/v1/exec/receipt_by_index/{blockid}", server.WrapHandler(api.handleReceiptByIndex, false, false, true)).Methods("GET")
		router.HandleFunc("/eth/v1/exec/state/{blockid}", server.WrapHandler(api.handleState, true, true, true)).Methods("POST")
		router.HandleFunc("/eth/v1/exec/call/{blockid}", server.WrapHandler(api.handleCall, true, true, true)).Methods("POST")
		router.HandleFunc("/eth/v1/exec/send_transaction", server.WrapHandler(api.handleSendTransaction, true, true, true)).Methods("POST")
		router.HandleFunc("/eth/v1/exec/history", server.WrapHandler(api.handleHistory, false, false, true)).Methods("GET")
		router.HandleFunc("/eth/v1/exec/transaction_position", server.WrapHandler(api.handleTxPosition, false, false, true)).Methods("GET")*/
		router.HandleFunc("/eth/v1/exec/logs", server.WrapHandler(api.handleLogs, false, false, true)).Methods("GET")
	}
}

type blockId struct {
	hash   common.Hash
	number uint64
}

func (b *blockId) isHash() bool {
	return b.hash != (common.Hash{})
}

func (s *execApiServer) getBlockId(ctx context.Context, id string) (blockId, bool) {
	switch id {
	case "genesis":
		return blockId{}, true
	case "finalized":
		header, err := s.apiBackend.HeaderByNumber(ctx, rpc.FinalizedBlockNumber)
		if err != nil {
			return blockId{}, false
		}
		return blockId{hash: header.Hash()}, true
	case "head":
		header, err := s.apiBackend.HeaderByNumber(ctx, rpc.LatestBlockNumber)
		if err != nil {
			return blockId{}, false
		}
		return blockId{hash: header.Hash()}, true
	}
	if hex, err := hexutil.Decode(id); err == nil {
		if len(hex) != common.HashLength {
			return blockId{}, false
		}
		var b blockId
		copy(b.hash[:], hex)
		return b, true
	}
	if number, err := strconv.ParseUint(id, 10, 64); err == nil {
		return blockId{number: number}, true
	}
	return blockId{}, false
}

// forkId returns the fork corresponding to the given header.
// Note that frontier thawing and difficulty bomb adjustments are ignored according
// to the API specification as they do not affect the interpretation of the
// returned data structures.
func (s *execApiServer) forkId(header *types.Header) forks.Fork {
	c := s.apiBackend.ChainConfig()
	switch {
	case header.Difficulty.Sign() == 0:
		return c.LatestFork(header.Time)
	case c.IsLondon(header.Number):
		return forks.London
	case c.IsBerlin(header.Number):
		return forks.Berlin
	case c.IsIstanbul(header.Number):
		return forks.Istanbul
	case c.IsPetersburg(header.Number):
		return forks.Petersburg
	case c.IsConstantinople(header.Number):
		return forks.Constantinople
	case c.IsByzantium(header.Number):
		return forks.Byzantium
	case c.IsEIP155(header.Number):
		return forks.SpuriousDragon
	case c.IsEIP150(header.Number):
		return forks.TangerineWhistle
	case c.IsDAOFork(header.Number):
		return forks.DAO
	case c.IsHomestead(header.Number):
		return forks.Homestead
	default:
		return forks.Frontier
	}
}

func (s *execApiServer) forkName(header *types.Header) string {
	return strings.ToLower(s.forkId(header).String())
}

func (s *execApiServer) handleHeaders(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	type headerResponse struct {
		Version string        `json:"version"`
		Data    *types.Header `json:"data"`
	}
	var (
		amount   int
		response []headerResponse
		err      error
	)
	id, ok := s.getBlockId(ctx, vars["blockid"])
	if !ok {
		return nil, "invalid block id", http.StatusBadRequest
	}
	if s := values.Get("amount"); s != "" {
		amount, err = strconv.Atoi(s)
		if err != nil || amount <= 0 {
			return nil, "invalid amount", http.StatusBadRequest
		}
	} else {
		amount = 1
	}

	response = make([]headerResponse, amount)
	for i := amount - 1; i >= 0; i-- {
		if id.isHash() {
			response[i].Data, err = s.apiBackend.HeaderByHash(ctx, id.hash)
		} else {
			response[i].Data, err = s.apiBackend.HeaderByNumber(ctx, rpc.BlockNumber(id.number))
		}
		if errors.Is(err, context.Canceled) {
			return nil, "request timeout", http.StatusRequestTimeout
		}
		if response[i].Data == nil || err != nil {
			return nil, "not available", http.StatusNotFound
		}
		response[i].Version = s.forkName(response[i].Data)
		if response[i].Data.Number.Uint64() == 0 {
			response = response[i:]
			break
		}
		id = blockId{hash: response[i].Data.ParentHash}
	}
	return response, "", 0
}

func (s *execApiServer) handleBlocks(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	type blockResponse struct {
		Version string       `json:"version"`
		Data    *types.Block `json:"data"`
	}
	var (
		amount   int
		response []blockResponse
		err      error
	)
	id, ok := s.getBlockId(ctx, vars["blockid"])
	if !ok {
		return nil, "invalid block id", http.StatusBadRequest
	}
	if s := values.Get("amount"); s != "" {
		amount, err = strconv.Atoi(s)
		if err != nil || amount <= 0 {
			return nil, "invalid amount", http.StatusBadRequest
		}
	} else {
		amount = 1
	}

	response = make([]blockResponse, amount)
	for i := amount - 1; i >= 0; i-- {
		if id.isHash() {
			response[i].Data, err = s.apiBackend.BlockByHash(ctx, id.hash)
		} else {
			response[i].Data, err = s.apiBackend.BlockByNumber(ctx, rpc.BlockNumber(id.number))
		}
		if errors.Is(err, context.Canceled) {
			return nil, "request timeout", http.StatusRequestTimeout
		}
		if response[i].Data == nil || err != nil {
			return nil, "not available", http.StatusNotFound
		}
		response[i].Version = s.forkName(response[i].Data.Header())
		if response[i].Data.NumberU64() == 0 {
			response = response[i:]
			break
		}
		id = blockId{hash: response[i].Data.ParentHash()}
	}
	return response, "", 0
}

func (s *execApiServer) handleBlockReceipts(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	type blockReceiptsResponse struct {
		Version string         `json:"version"`
		Data    types.Receipts `json:"data"`
	}
	var (
		amount   int
		response []blockReceiptsResponse
		err      error
	)
	id, ok := s.getBlockId(ctx, vars["blockid"])
	if !ok {
		return nil, "invalid block id", http.StatusBadRequest
	}
	if s := values.Get("amount"); s != "" {
		amount, err = strconv.Atoi(s)
		if err != nil || amount <= 0 {
			return nil, "invalid amount", http.StatusBadRequest
		}
	} else {
		amount = 1
	}

	response = make([]blockReceiptsResponse, amount)
	for i := amount - 1; i >= 0; i-- {
		var header *types.Header
		if id.isHash() {
			header, err = s.apiBackend.HeaderByHash(ctx, id.hash)
		} else {
			header, err = s.apiBackend.HeaderByNumber(ctx, rpc.BlockNumber(id.number))
		}
		if header != nil && err == nil {
			response[i].Data, err = s.apiBackend.GetReceipts(ctx, header.Hash())
		}
		if errors.Is(err, context.Canceled) {
			return nil, "request timeout", http.StatusRequestTimeout
		}
		if response[i].Data == nil || err != nil {
			return nil, "not available", http.StatusNotFound
		}
		response[i].Version = s.forkName(header)
		if header.Number.Uint64() == 0 {
			response = response[i:]
			break
		}
		id = blockId{hash: header.ParentHash}
	}
	return response, "", 0
}

func (s *execApiServer) handleTransaction(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	var txHash common.Hash
	if hex, err := hexutil.Decode(vars["txhash"]); err == nil {
		if len(hex) != common.HashLength {
			return nil, "invalid transaction hash", http.StatusBadRequest
		}
		copy(txHash[:], hex)
	}
	_, tx, _, _, _ := s.apiBackend.GetCanonicalTransaction(txHash)
	if tx == nil {
		tx = s.apiBackend.GetPoolTransaction(txHash)
	}
	if tx == nil {
		return nil, "not available", http.StatusNotFound
	}
	return tx, "", 0
}

func (s *execApiServer) handleTxByIndex(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	/*type txProof struct {
		Index       uint64             `json:"index"`
		Transaction *types.Transaction `json:"transaction" rlp:"-"`
		Proof       []hexutil.Bytes    `json:"proof"`
	}
	var response struct {
		Version string         `json:"version"`
		Data    types.Receipts `json:"data"`
	}*/

	_, ok := s.getBlockId(ctx, vars["blockid"])
	if !ok {
		return nil, "invalid block id", http.StatusBadRequest
	}
	/*indices := values["indices"]
	if len(indices) == 0 {
		return nil, "empty transaction index list", http.StatusBadRequest
	}
	response.Data = make([]txProof, len(indices))
	for i, str := range indicesStr {
		if number, err := strconv.ParseUint(str, 10, 64); err == nil {
			indices[i] = number
		} else {
			return nil, "invalid transaction index", http.StatusBadRequest
		}
	}
	var (
		block *types.Block
		err   error
	)
	if id.isHash() {
		block, err = s.apiBackend.BlockByHash(ctx, id.hash)
	} else {
		block, err = s.apiBackend.BlockByNumber(ctx, rpc.BlockNumber(id.number))
	}
	if errors.Is(err, context.Canceled) {
		return nil, "request timeout", http.StatusRequestTimeout
	}
	if block == nil || err != nil {
		return nil, "not available", http.StatusNotFound
	}
	response[i].Version = s.forkName(block.Header())
	t := trie.NewStackTrie(nil)
	if types.DeriveSha(block.Transactions(), t) != block.TransactionsRoot() {
		log.Error("")
		return nil, "transactions root mismatch", http.StatusInternalServerError
	}
	var indexBuf []byte
	for _, txIndex := range indices {
		indexBuf = rlp.AppendUint64(indexBuf[:0], txIndex)
		t.Prove(indexBuf, proofWriter)
	}*/
	panic("TODO")
}

func (s *execApiServer) handleReceiptByIndex(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	_, ok := s.getBlockId(ctx, vars["blockid"])
	if !ok {
		return nil, "invalid block id", http.StatusBadRequest
	}
	panic("TODO")
}

func (s *execApiServer) handleState(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	_, ok := s.getBlockId(ctx, vars["blockid"])
	if !ok {
		return nil, "invalid block id", http.StatusBadRequest
	}
	panic("TODO")
}

func (s *execApiServer) handleCall(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	_, ok := s.getBlockId(ctx, vars["blockid"])
	if !ok {
		return nil, "invalid block id", http.StatusBadRequest
	}
	panic("TODO")
}

func (s *execApiServer) handleHistory(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	panic("TODO")
} // Requires EIP-8304
func (s *execApiServer) handleTxPosition(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	panic("TODO")
} // Requires EIP-8304

// Requires EIP-8304
func (s *execApiServer) handleLogs(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	matcher := s.apiBackend.LogQuery()
	if matcher == nil {
		return nil, "log query backend unavailable", http.StatusInternalServerError
	}
	var (
		fromBlock, toBlock uint64
		limit              = uint64(math.MaxUint64)
		limitLatest        bool
		refHeader          *types.Header
		err                error
	)
	id, ok := s.getBlockId(ctx, values.Get("ref_head"))
	if !ok {
		return nil, "invalid reference block id", http.StatusBadRequest
	}
	if id.isHash() {
		refHeader, err = s.apiBackend.HeaderByHash(ctx, id.hash)
	} else {
		refHeader, err = s.apiBackend.HeaderByNumber(ctx, rpc.BlockNumber(id.number))
	}
	if errors.Is(err, context.Canceled) {
		return nil, "request timeout", http.StatusRequestTimeout
	}
	if refHeader == nil || err != nil {
		return nil, "reference block header not available", http.StatusNotFound
	}
	if s := values.Get("from_block"); s != "" {
		v, err := strconv.ParseInt(s, 10, 63)
		if err != nil || v < 0 {
			return nil, "invalid from_block", http.StatusBadRequest
		}
		fromBlock = uint64(v)
	}
	if s := values.Get("to_block"); s != "" {
		v, err := strconv.ParseInt(s, 10, 63)
		if err != nil || v < 0 {
			return nil, "invalid to_block", http.StatusBadRequest
		}
		toBlock = uint64(v)
	} else {
		toBlock = math.MaxUint64
	}
	if s := values.Get("limit_latest"); s != "" {
		v, err := strconv.ParseInt(s, 10, 63)
		if err != nil || v <= 0 {
			return nil, "invalid limit_latest", http.StatusBadRequest
		}
		limit, limitLatest = uint64(v), true
	}
	if s := values.Get("limit_oldest"); s != "" {
		if limitLatest {
			return nil, "both limit_oldest and limit_latest specified", http.StatusBadRequest
		}
		v, err := strconv.ParseInt(s, 10, 63)
		if err != nil || v <= 0 {
			return nil, "invalid limit_oldest", http.StatusBadRequest
		}
		limit = uint64(v)
	}
	patternMatcher, err := logquery.NewPatternMatcherFromJSON(json.RawMessage(values.Get("pattern")))
	if err != nil {
		return nil, "invalid search pattern: " + err.Error(), http.StatusInternalServerError
	}
	query := logquery.FilterQuery{
		Pattern:     patternMatcher,
		FirstBlock:  fromBlock,
		LastBlock:   toBlock,
		Limit:       limit,
		LimitLatest: limitLatest,
	}
	chainView := s.apiBackend.ChainView(refHeader.Hash(), refHeader.Number.Uint64())
	if chainView == nil {
		return nil, "could not create chain view for reference header", http.StatusInternalServerError
	}
	_, _, proof, err := matcher.GetMatches(ctx, chainView, query, true)
	if errors.Is(err, context.Canceled) {
		return nil, "request timeout", http.StatusRequestTimeout
	}
	if errors.Is(err, logquery.ErrMatchAll) {
		return nil, "request has no specified search values", http.StatusBadRequest
	}
	if err != nil {
		return nil, "log search failed: " + err.Error(), http.StatusInternalServerError
	}
	return proof, "", 0
}

func (s *execApiServer) handleSendTransaction(ctx context.Context, values url.Values, vars map[string]string, decodeBody func(*any) error) (any, string, int) {
	panic("TODO")
}
