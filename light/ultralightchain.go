// Copyright 2022 The go-ethereum Authors
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

// Package light implements on-demand retrieval capable state and chain objects
// for the Ethereum Light Client.
package light

import (
	"context"
	"errors"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	lru "github.com/hashicorp/golang-lru"
)

type UltraLightChain struct {
	odr           OdrBackend
	chainFeed     event.Feed
	chainSideFeed event.Feed
	chainHeadFeed event.Feed
	scope         event.SubscriptionScope
	config        *params.ChainConfig
	engine        consensus.Engine
	genesisBlock  *types.Block

	headerCache  *lru.Cache // Cache for the most recent block headers
	bodyCache    *lru.Cache // Cache for the most recent block bodies
	bodyRLPCache *lru.Cache // Cache for the most recent block bodies in RLP encoded format
	blockCache   *lru.Cache // Cache for the most recent entire blocks

	chainmu                                    sync.RWMutex // protects header inserts
	beaconHead                                 beacon.Header
	lastHeader, currentHeader, finalizedHeader *types.Header
	headerByNumberCache                        *lru.Cache // Cache for the most recent block headers

	//quit chan struct{}
	//wg   sync.WaitGroup
}

type UlcPeerInfo struct {
	lock                   sync.RWMutex
	announcedBeaconHeads   [4]common.Hash
	announcedBeaconHeadPtr int
}

func (u *UlcPeerInfo) AddBeaconHead(beaconHead common.Hash) {
	u.lock.Lock()
	defer u.lock.Unlock()

	for _, h := range u.announcedBeaconHeads {
		if h == beaconHead {
			return
		}
	}
	u.announcedBeaconHeads[u.announcedBeaconHeadPtr] = beaconHead
	u.announcedBeaconHeadPtr++
	if u.announcedBeaconHeadPtr == len(u.announcedBeaconHeads) {
		u.announcedBeaconHeadPtr = 0
	}
}

func (u *UlcPeerInfo) HasBeaconHead(beaconHead common.Hash) bool {
	u.lock.RLock()
	defer u.lock.RUnlock()

	for _, bh := range u.announcedBeaconHeads {
		if bh == beaconHead {
			return true
		}
	}
	return false
}

/*type beaconHeadInfo struct {
	headerByNumber map[uint64]*types.Header
	headerByHash   map[common.Hash]*types.Header
}*/

// NewUltraLightChain returns a fully initialised light chain using information
// available in the database. It initialises the default Ethereum header
// validator.
func NewUltraLightChain(odr OdrBackend, config *params.ChainConfig, engine consensus.Engine) (*UltraLightChain, error) {
	headerCache, _ := lru.New(headerCacheLimit)
	headerByNumberCache, _ := lru.New(headerByNumberCacheLimit)
	bodyCache, _ := lru.New(bodyCacheLimit)
	bodyRLPCache, _ := lru.New(bodyCacheLimit)
	blockCache, _ := lru.New(blockCacheLimit)

	bc := &UltraLightChain{
		//		chainDb:             odr.Database(),
		odr: odr,
		//		quit:                make(chan struct{}),
		config:              config,
		engine:              engine,
		headerCache:         headerCache,
		headerByNumberCache: headerByNumberCache,
		bodyCache:           bodyCache,
		bodyRLPCache:        bodyRLPCache,
		blockCache:          blockCache,
	}
	genesisHash := rawdb.ReadCanonicalHash(odr.Database(), 0)
	bc.genesisBlock = rawdb.ReadBlock(odr.Database(), genesisHash, 0)
	if bc.genesisBlock == nil {
		return nil, core.ErrNoGenesis
	}
	return bc, nil
}

// Odr returns the ODR backend of the chain
func (lc *UltraLightChain) Odr() OdrBackend {
	return lc.odr
}

// GasLimit returns the gas limit of the current HEAD block.
/*func (lc *UltraLightChain) GasLimit() uint64 {
	return lc.hc.CurrentHeader().GasLimit
}*/

// Config retrieves the header chain's chain configuration.
func (lc *UltraLightChain) Config() *params.ChainConfig { return lc.config }

// Engine retrieves the light chain's consensus engine.
func (lc *UltraLightChain) Engine() consensus.Engine { return lc.engine }

// Genesis returns the genesis block
func (lc *UltraLightChain) Genesis() *types.Block {
	return lc.genesisBlock
}

/*
// GetBody retrieves a block body (transactions and uncles) from the database
// or ODR service by hash, caching it if found.
func (lc *UltraLightChain) GetBody(ctx context.Context, hash common.Hash) (*types.Body, error) {
	// Short circuit if the body's already in the cache, retrieve otherwise
	if cached, ok := lc.bodyCache.Get(hash); ok {
		body := cached.(*types.Body)
		return body, nil
	}

	number := lc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil, errors.New("unknown block")
	}
	body, err := GetBody(ctx, lc.odr, header)
	if err != nil {
		return nil, err
	}
	// Cache the found body for next time and return
	lc.bodyCache.Add(hash, body)
	return body, nil
}

// GetBodyRLP retrieves a block body in RLP encoding from the database or
// ODR service by hash, caching it if found.
func (lc *UltraLightChain) GetBodyRLP(ctx context.Context, hash common.Hash) (rlp.RawValue, error) {
	// Short circuit if the body's already in the cache, retrieve otherwise
	if cached, ok := lc.bodyRLPCache.Get(hash); ok {
		return cached.(rlp.RawValue), nil
	}
	number := lc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil, errors.New("unknown block")
	}
	body, err := GetBodyRLP(ctx, lc.odr, hash, *number)
	if err != nil {
		return nil, err
	}
	// Cache the found body for next time and return
	lc.bodyRLPCache.Add(hash, body)
	return body, nil
}

// HasBlock checks if a block is fully present in the database or not, caching
// it if present.
func (lc *UltraLightChain) HasBlock(hash common.Hash, number uint64) bool {
	blk, _ := lc.GetBlock(NoOdr, hash, number)
	return blk != nil
}*/

// GetBlock retrieves a block from the database or ODR service by hash and number,
// caching it if found.
/*func (lc *UltraLightChain) GetBlock(ctx context.Context, hash common.Hash, number uint64) (*types.Block, error) {
	// Short circuit if the block's already in the cache, retrieve otherwise
	if block, ok := lc.blockCache.Get(hash); ok {
		return block.(*types.Block), nil
	}
	header, err := lc.GetHeaderOdr(ctx, lc.odr, hash, number)
	if err != nil {
		return nil, err
	}
	block, err := GetBlock(ctx, lc.odr, header)
	if err != nil {
		return nil, err
	}
	// Cache the found block for next time and return
	lc.blockCache.Add(hash, block)
	return block, nil
}*/

// GetBlockByHash retrieves a block from the database or ODR service by hash,
// caching it if found.
func (lc *UltraLightChain) GetBlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	// Short circuit if the block's already in the cache, retrieve otherwise
	if block, ok := lc.blockCache.Get(hash); ok {
		return block.(*types.Block), nil
	}
	header, err := lc.GetHeaderByHashOdr(ctx, hash)
	if err != nil {
		return nil, err
	}
	block, err2 := GetBlock(ctx, lc.odr, header)
	if err2 != nil {
		return nil, err2
	}
	// Cache the found block for next time and return
	lc.blockCache.Add(hash, block)
	return block, nil
}

// GetBlockByNumber retrieves a block from the database or ODR service by
// number, caching it (associated with its hash) if found.
func (lc *UltraLightChain) GetBlockByNumber(ctx context.Context, number uint64) (*types.Block, error) {
	// Retrieve the block header and body contents
	header, err := lc.GetHeaderByNumberOdr(ctx, number)
	if err != nil {
		return nil, err
	}
	hash := header.Hash()
	if block, ok := lc.blockCache.Get(hash); ok {
		return block.(*types.Block), nil
	}
	block, err := GetBlock(ctx, lc.odr, header)
	if err != nil {
		return nil, err
	}
	// Cache the found block for next time and return
	lc.blockCache.Add(hash, block)
	return block, nil
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
/*func (lc *UltraLightChain) Stop() {
	if !atomic.CompareAndSwapInt32(&lc.running, 0, 1) {
		return
	}
	close(lc.quit)
	lc.StopInsert()
	lc.wg.Wait()
	log.Info("Blockchain stopped")
}*/

func (lc *UltraLightChain) Stop() {}

func (lc *UltraLightChain) SetBeaconHead(head beacon.Header) {
	lc.chainmu.Lock()
	lc.headerByNumberCache.Purge()
	lc.beaconHead = head
	lc.currentHeader, lc.finalizedHeader = nil, nil
	lc.chainmu.Unlock()
	log.Info("Received new beacon head", "slot", head.Slot, "blockRoot", head.Hash())
}

// returns the last retrieved header or the genesis header
func (lc *UltraLightChain) CurrentHeader() *types.Header {
	lc.chainmu.RLock()
	currentHeader := lc.lastHeader
	lc.chainmu.RUnlock()

	return currentHeader
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (lc *UltraLightChain) CurrentHeaderOdr(ctx context.Context) (*types.Header, error) {
	lc.chainmu.RLock()
	currentHeader := lc.currentHeader
	beaconHead := lc.beaconHead
	lc.chainmu.RUnlock()

	if currentHeader != nil {
		return currentHeader, nil
	}

	if headers, err := GetExecHeaders(ctx, lc.odr, beaconHead, HeadMode, 0, 1); err == nil && len(headers) == 1 {
		currentHeader = headers[0]
		lc.chainmu.Lock()
		if lc.beaconHead == beaconHead {
			lc.currentHeader = currentHeader
			lc.lastHeader = currentHeader
			lc.headerByNumberCache.Add(currentHeader.Number.Uint64(), currentHeader)
		}
		lc.headerCache.Add(currentHeader.Hash(), currentHeader)
		lc.chainmu.Unlock()
		return currentHeader, nil
	} else {
		if err == nil {
			return nil, errors.New("Invalid number of headers retrieved") //TODO can this ever happen?
		}
		return nil, err
	}
}

func (lc *UltraLightChain) FinalizedHeaderOdr(ctx context.Context) (*types.Header, error) {
	lc.chainmu.RLock()
	finalizedHeader := lc.finalizedHeader
	beaconHead := lc.beaconHead
	lc.chainmu.RUnlock()

	if finalizedHeader != nil {
		return finalizedHeader, nil
	}

	if headers, err := GetExecHeaders(ctx, lc.odr, beaconHead, FinalizedMode, 0, 1); err == nil && len(headers) == 1 {
		finalizedHeader = headers[0]
		lc.chainmu.Lock()
		if lc.beaconHead == beaconHead {
			lc.finalizedHeader = finalizedHeader
			lc.headerByNumberCache.Add(finalizedHeader.Number.Uint64(), finalizedHeader)
		}
		lc.headerCache.Add(finalizedHeader.Hash(), finalizedHeader)
		lc.chainmu.Unlock()
		return finalizedHeader, nil
	} else {
		if err == nil {
			return nil, errors.New("Invalid number of headers retrieved") //TODO can this ever happen?
		}
		return nil, err
	}
}

func (lc *UltraLightChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	if h, ok := lc.headerCache.Get(hash); ok {
		return h.(*types.Header)
	}
	return nil
}

// GetHeader retrieves a block header from the database by hash and number,
// caching it if found.
/*func (lc *UltraLightChain) GetHeader(ctx context.Context, hash common.Hash, number uint64) (*types.Header, error) {
	return lc.GetHeaderByHash(ctx, lc.odr, hash)
}*/

// GetHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
func (lc *UltraLightChain) GetHeaderByHash(hash common.Hash) *types.Header {
	if header, ok := lc.headerCache.Get(hash); ok {
		return header.(*types.Header)
	}
	return nil
}

func (lc *UltraLightChain) GetHeaderByHashOdr(ctx context.Context, hash common.Hash) (*types.Header, error) {
	// Short circuit if the header's already in the cache, retrieve otherwise
	if header, ok := lc.headerCache.Get(hash); ok {
		return header.(*types.Header), nil
	}
	header, err := GetHeaderByHash(ctx, lc.odr, hash)
	if err != nil {
		return nil, err
	}
	// Cache the found header for next time and return
	lc.headerCache.Add(hash, header)
	return header, nil
}

// HasHeader checks if a block header is present in the database or not, caching
// it if present.
/*func (lc *UltraLightChain) HasHeader(hash common.Hash, number uint64) bool {
	return lc.hc.HasHeader(hash, number)
}*/

// GetCanonicalHash returns the canonical hash for a given block number
func (lc *UltraLightChain) GetCanonicalHashOdr(ctx context.Context, number uint64) (common.Hash, error) {
	if header, err := lc.GetHeaderByNumberOdr(ctx, number); header != nil { //TODO do this with zero header request? or not needed if the header is cached?
		return header.Hash(), nil
	} else {
		return common.Hash{}, err
	}
}

// GetAncestor retrieves the Nth ancestor of a given block. It assumes that either the given block or
// a close ancestor of it is canonical. maxNonCanonical points to a downwards counter limiting the
// number of blocks to be individually checked before we reach the canonical chain.
//
// Note: ancestor == 0 returns the same block, 1 returns its parent and so on.
/*func (lc *UltraLightChain) GetAncestor(hash common.Hash, number, ancestor uint64, maxNonCanonical *uint64) (common.Hash, uint64) {
	return lc.hc.GetAncestor(hash, number, ancestor, maxNonCanonical)
}*/

func (lc *UltraLightChain) GetHeaderByNumber(number uint64) *types.Header {
	if header, ok := lc.headerByNumberCache.Get(number); ok {
		return header.(*types.Header)
	}
	return nil
}

func (lc *UltraLightChain) GetHeaderByNumberOdr(ctx context.Context, number uint64) (*types.Header, error) {
	// Short circuit if the header's already in the cache, retrieve otherwise
	if header, ok := lc.headerByNumberCache.Get(number); ok {
		return header.(*types.Header), nil
	}
	lc.chainmu.RLock()
	beaconHead := lc.beaconHead
	lc.chainmu.RUnlock()
	header, err := GetHeaderByNumberV5(ctx, lc.odr, beaconHead, number)
	if err != nil {
		return nil, err
	}
	// Cache the found header for next time and return
	lc.chainmu.Lock()
	if lc.beaconHead == beaconHead {
		lc.headerByNumberCache.Add(header.Number.Uint64(), header)
	}
	lc.headerCache.Add(header.Hash(), header)
	lc.chainmu.Unlock()
	return header, nil
}

// SubscribeChainEvent registers a subscription of ChainEvent.
func (lc *UltraLightChain) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return lc.scope.Track(lc.chainFeed.Subscribe(ch))
}

// SubscribeChainHeadEvent registers a subscription of ChainHeadEvent.
func (lc *UltraLightChain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return lc.scope.Track(lc.chainHeadFeed.Subscribe(ch))
}

// SubscribeChainSideEvent registers a subscription of ChainSideEvent.
func (lc *UltraLightChain) SubscribeChainSideEvent(ch chan<- core.ChainSideEvent) event.Subscription {
	return lc.scope.Track(lc.chainSideFeed.Subscribe(ch))
}

// SubscribeLogsEvent implements the interface of filters.Backend
// UltraLightChain does not send logs events, so return an empty subscription.
func (lc *UltraLightChain) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return lc.scope.Track(new(event.Feed).Subscribe(ch))
}

// SubscribeRemovedLogsEvent implements the interface of filters.Backend
// UltraLightChain does not send core.RemovedLogsEvent, so return an empty subscription.
func (lc *UltraLightChain) SubscribeRemovedLogsEvent(ch chan<- core.RemovedLogsEvent) event.Subscription {
	return lc.scope.Track(new(event.Feed).Subscribe(ch))
}

func (lc *UltraLightChain) SetHead(head uint64) error {
	return errors.New("Not supported in ultra light mode")
}

func (lc *UltraLightChain) GetTd(hash common.Hash, number uint64) *big.Int {
	return common.Big0
}

func (lc *UltraLightChain) GetTdOdr(ctx context.Context, hash common.Hash, number uint64) *big.Int {
	return common.Big0
}
