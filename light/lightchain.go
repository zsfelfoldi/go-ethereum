// Copyright 2016 The go-ethereum Authors
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
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	lru "github.com/hashicorp/golang-lru"
)

var (
	bodyCacheLimit  = 256
	blockCacheLimit = 256
)

// LightChain represents a canonical chain that by default only handles block
// headers, downloading block bodies and receipts on demand through an ODR
// interface. It only does header validation during chain insertion.
type LightChain struct {
	hc            *core.HeaderChain
	indexerConfig *IndexerConfig
	chainDb       ethdb.Database
	engine        consensus.Engine
	odr           OdrBackend
	chainFeed     event.Feed
	chainSideFeed event.Feed
	chainHeadFeed event.Feed
	scope         event.SubscriptionScope
	genesisBlock  *types.Block
	forker        *core.ForkChoice

	bodyCache    *lru.Cache // Cache for the most recent block bodies
	bodyRLPCache *lru.Cache // Cache for the most recent block bodies in RLP encoded format
	blockCache   *lru.Cache // Cache for the most recent entire blocks

	chainmu                        sync.RWMutex // protects header inserts
	currentHeader, finalizedHeader *types.Header
	beaconHeader                   beacon.Header

	quit chan struct{}
	wg   sync.WaitGroup

	// Atomic boolean switches:
	running          int32 // whether LightChain is running or stopped
	procInterrupt    int32 // interrupts chain insert
	disableCheckFreq int32 // disables header verification
}

// NewLightChain returns a fully initialised light chain using information
// available in the database. It initialises the default Ethereum header
// validator.
func NewLightChain(odr OdrBackend, config *params.ChainConfig, engine consensus.Engine, checkpoint *params.TrustedCheckpoint) (*LightChain, error) {
	bodyCache, _ := lru.New(bodyCacheLimit)
	bodyRLPCache, _ := lru.New(bodyCacheLimit)
	blockCache, _ := lru.New(blockCacheLimit)

	bc := &LightChain{
		chainDb:       odr.Database(),
		indexerConfig: odr.IndexerConfig(),
		odr:           odr,
		quit:          make(chan struct{}),
		bodyCache:     bodyCache,
		bodyRLPCache:  bodyRLPCache,
		blockCache:    blockCache,
		engine:        engine,
	}
	bc.forker = core.NewForkChoice(bc, nil)
	var err error
	bc.hc, err = core.NewHeaderChain(odr.Database(), config, bc.engine, bc.getProcInterrupt)
	if err != nil {
		return nil, err
	}
	//bc.genesisBlock, _ = bc.GetBlockByNumber(NoOdr, 0)
	genesisHash := rawdb.ReadCanonicalHash(bc.chainDb, 0)
	bc.genesisBlock = rawdb.ReadBlock(bc.chainDb, genesisHash, 0)
	if bc.genesisBlock == nil {
		return nil, core.ErrNoGenesis
	}
	if checkpoint != nil {
		bc.AddTrustedCheckpoint(checkpoint)
	}
	if err := bc.loadLastState(); err != nil {
		return nil, err
	}
	// Check the current state of the block hashes and make sure that we do not have any of the bad blocks in our chain
	/*for hash := range core.BadHashes {
		if header := bc.GetHeaderByHash(hash); header != nil {
			log.Error("Found bad hash, rewinding chain", "number", header.Number, "hash", header.ParentHash)
			bc.SetHead(header.Number.Uint64() - 1)
			log.Info("Chain rewind was successful, resuming normal operation")
		}
	}*/
	return bc, nil
}

// AddTrustedCheckpoint adds a trusted checkpoint to the blockchain
func (lc *LightChain) AddTrustedCheckpoint(cp *params.TrustedCheckpoint) {
	if lc.odr.ChtIndexer() != nil {
		StoreChtRoot(lc.chainDb, cp.SectionIndex, cp.SectionHead, cp.CHTRoot)
		lc.odr.ChtIndexer().AddCheckpoint(cp.SectionIndex, cp.SectionHead)
	}
	if lc.odr.BloomTrieIndexer() != nil {
		StoreBloomTrieRoot(lc.chainDb, cp.SectionIndex, cp.SectionHead, cp.BloomRoot)
		lc.odr.BloomTrieIndexer().AddCheckpoint(cp.SectionIndex, cp.SectionHead)
	}
	if lc.odr.BloomIndexer() != nil {
		lc.odr.BloomIndexer().AddCheckpoint(cp.SectionIndex, cp.SectionHead)
	}
	log.Info("Added trusted checkpoint", "block", (cp.SectionIndex+1)*lc.indexerConfig.ChtSize-1, "hash", cp.SectionHead)
}

func (lc *LightChain) getProcInterrupt() bool {
	return atomic.LoadInt32(&lc.procInterrupt) == 1
}

// Odr returns the ODR backend of the chain
func (lc *LightChain) Odr() OdrBackend {
	return lc.odr
}

// HeaderChain returns the underlying header chain.
func (lc *LightChain) HeaderChain() *core.HeaderChain {
	return lc.hc
}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
func (lc *LightChain) loadLastState() error {
	/*if head := rawdb.ReadHeadHeaderHash(lc.chainDb); head == (common.Hash{}) {
		// Corrupt or empty database, init from scratch
		lc.Reset()
	} else {
		header := lc.GetHeaderByHash(head)
		if header == nil {
			// Corrupt or empty database, init from scratch
			lc.Reset()
		} else {
			lc.hc.SetCurrentHeader(header)
		}
	}
	// Issue a status log and return
	header := lc.hc.CurrentHeader()
	headerTd := lc.GetTd(header.Hash(), header.Number.Uint64())
	log.Info("Loaded most recent local header", "number", header.Number, "hash", header.Hash(), "td", headerTd, "age", common.PrettyAge(time.Unix(int64(header.Time), 0)))*/
	return nil
}

// SetHead rewinds the local chain to a new head. Everything above the new
// head will be deleted and the new one set.
func (lc *LightChain) SetHead(head uint64) error {
	lc.chainmu.Lock()
	defer lc.chainmu.Unlock()

	lc.hc.SetHead(head, nil, nil)
	return lc.loadLastState()
}

// GasLimit returns the gas limit of the current HEAD block.
func (lc *LightChain) GasLimit() uint64 {
	return lc.hc.CurrentHeader().GasLimit
}

// Reset purges the entire blockchain, restoring it to its genesis state.
func (lc *LightChain) Reset() {
	//lc.ResetWithGenesisBlock(lc.genesisBlock)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
func (lc *LightChain) ResetWithGenesisBlock(genesis *types.Block) {
	// Dump the entire block chain and purge the caches
	/*lc.SetHead(0)

	lc.chainmu.Lock()
	defer lc.chainmu.Unlock()

	// Prepare the genesis block and reinitialise the chain
	batch := lc.chainDb.NewBatch()
	rawdb.WriteTd(batch, genesis.Hash(), genesis.NumberU64(), genesis.Difficulty())
	rawdb.WriteBlock(batch, genesis)
	rawdb.WriteHeadHeaderHash(batch, genesis.Hash())
	if err := batch.Write(); err != nil {
		log.Crit("Failed to reset genesis block", "err", err)
	}
	lc.genesisBlock = genesis
	lc.hc.SetGenesis(lc.genesisBlock.Header())
	lc.hc.SetCurrentHeader(lc.genesisBlock.Header())*/
}

// Accessors

// Engine retrieves the light chain's consensus engine.
func (lc *LightChain) Engine() consensus.Engine { return lc.engine }

// Genesis returns the genesis block
func (lc *LightChain) Genesis() *types.Block {
	return lc.genesisBlock
}

func (lc *LightChain) StateCache() state.Database {
	panic("not implemented")
}

// GetBody retrieves a block body (transactions and uncles) from the database
// or ODR service by hash, caching it if found.
func (lc *LightChain) GetBody(ctx context.Context, hash common.Hash) (*types.Body, error) {
	// Short circuit if the body's already in the cache, retrieve otherwise
	if cached, ok := lc.bodyCache.Get(hash); ok {
		body := cached.(*types.Body)
		return body, nil
	}
	number := lc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil, errors.New("unknown block")
	}
	body, err := GetBody(ctx, lc.odr, hash, *number)
	if err != nil {
		return nil, err
	}
	// Cache the found body for next time and return
	lc.bodyCache.Add(hash, body)
	return body, nil
}

// GetBodyRLP retrieves a block body in RLP encoding from the database or
// ODR service by hash, caching it if found.
func (lc *LightChain) GetBodyRLP(ctx context.Context, hash common.Hash) (rlp.RawValue, error) {
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
func (lc *LightChain) HasBlock(hash common.Hash, number uint64) bool {
	blk, _ := lc.GetBlock(NoOdr, hash, number)
	return blk != nil
}

// GetBlock retrieves a block from the database or ODR service by hash and number,
// caching it if found.
func (lc *LightChain) GetBlock(ctx context.Context, hash common.Hash, number uint64) (*types.Block, error) {
	// Short circuit if the block's already in the cache, retrieve otherwise
	if block, ok := lc.blockCache.Get(hash); ok {
		return block.(*types.Block), nil
	}
	block, err := GetBlock(ctx, lc.odr, hash, number)
	if err != nil {
		return nil, err
	}
	// Cache the found block for next time and return
	lc.blockCache.Add(block.Hash(), block)
	return block, nil
}

// GetBlockByHash retrieves a block from the database or ODR service by hash,
// caching it if found.
func (lc *LightChain) GetBlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	// Retrieve the block header and body contents
	header, err := GetHeaderByHash(ctx, lc.odr, hash)
	if err != nil {
		return nil, err
	}
	body, err := GetBody(ctx, lc.odr, hash, header.Number.Uint64())
	if err != nil {
		return nil, err
	}
	// Reassemble the block and return
	return types.NewBlockWithHeader(header).WithBody(body.Transactions, body.Uncles), nil
}

// GetBlockByNumber retrieves a block from the database or ODR service by
// number, caching it (associated with its hash) if found.
func (lc *LightChain) GetBlockByNumber(ctx context.Context, number uint64) (*types.Block, error) {
	lc.chainmu.RLock()
	beaconHeader := lc.beaconHeader
	lc.chainmu.RUnlock()

	// Retrieve the block header and body contents
	header, err := GetHeaderByNumber(ctx, lc.odr, beaconHeader, number)
	if err != nil {
		return nil, err
	}
	body, err := GetBody(ctx, lc.odr, header.Hash(), number)
	if err != nil {
		return nil, err
	}
	// Reassemble the block and return
	return types.NewBlockWithHeader(header).WithBody(body.Transactions, body.Uncles), nil
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
func (lc *LightChain) Stop() {
	if !atomic.CompareAndSwapInt32(&lc.running, 0, 1) {
		return
	}
	close(lc.quit)
	lc.StopInsert()
	lc.wg.Wait()
	log.Info("Blockchain stopped")
}

// StopInsert interrupts all insertion methods, causing them to return
// errInsertionInterrupted as soon as possible. Insertion is permanently disabled after
// calling this method.
func (lc *LightChain) StopInsert() {
	atomic.StoreInt32(&lc.procInterrupt, 1)
}

// Rollback is designed to remove a chain of links from the database that aren't
// certain enough to be valid.
func (lc *LightChain) Rollback(chain []common.Hash) {
	/*lc.chainmu.Lock()
	defer lc.chainmu.Unlock()

	batch := lc.chainDb.NewBatch()
	for i := len(chain) - 1; i >= 0; i-- {
		hash := chain[i]

		// Degrade the chain markers if they are explicitly reverted.
		// In theory we should update all in-memory markers in the
		// last step, however the direction of rollback is from high
		// to low, so it's safe the update in-memory markers directly.
		if head := lc.hc.CurrentHeader(); head.Hash() == hash {
			rawdb.WriteHeadHeaderHash(batch, head.ParentHash)
			lc.hc.SetCurrentHeader(lc.GetHeader(head.ParentHash, head.Number.Uint64()-1))
		}
	}
	if err := batch.Write(); err != nil {
		log.Crit("Failed to rollback light chain", "error", err)
	}*/
}

func (lc *LightChain) InsertHeader(header *types.Header) error {
	// Verify the header first before obtaining the lock
	headers := []*types.Header{header}
	if _, err := lc.hc.ValidateHeaderChain(headers, 100); err != nil {
		return err
	}
	// Make sure only one thread manipulates the chain at once
	lc.chainmu.Lock()
	defer lc.chainmu.Unlock()

	lc.wg.Add(1)
	defer lc.wg.Done()

	_, err := lc.hc.WriteHeaders(headers)
	log.Info("Inserted header", "number", header.Number, "hash", header.Hash())
	return err
}

func (lc *LightChain) SetCanonical(header *types.Header) error {
	lc.chainmu.Lock()
	defer lc.chainmu.Unlock()

	lc.wg.Add(1)
	defer lc.wg.Done()

	if err := lc.hc.Reorg([]*types.Header{header}); err != nil {
		return err
	}
	// Emit events
	block := types.NewBlockWithHeader(header)
	lc.chainFeed.Send(core.ChainEvent{Block: block, Hash: block.Hash()})
	lc.chainHeadFeed.Send(core.ChainHeadEvent{Block: block})
	log.Info("Set the chain head", "number", block.Number(), "hash", block.Hash())
	return nil
}

// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
//
// The verify parameter can be used to fine tune whether nonce verification
// should be done or not. The reason behind the optional check is because some
// of the header retrieval mechanisms already need to verfy nonces, as well as
// because nonces can be verified sparsely, not needing to check each.
//
// In the case of a light chain, InsertHeaderChain also creates and posts light
// chain events when necessary.
func (lc *LightChain) InsertHeaderChain(chain []*types.Header, checkFreq int) (int, error) {
	if len(chain) == 0 {
		return 0, nil
	}
	if atomic.LoadInt32(&lc.disableCheckFreq) == 1 {
		checkFreq = 0
	}
	start := time.Now()
	if i, err := lc.hc.ValidateHeaderChain(chain, checkFreq); err != nil {
		return i, err
	}

	// Make sure only one thread manipulates the chain at once
	lc.chainmu.Lock()
	defer lc.chainmu.Unlock()

	lc.wg.Add(1)
	defer lc.wg.Done()

	status, err := lc.hc.InsertHeaderChain(chain, start, lc.forker)
	if err != nil || len(chain) == 0 {
		return 0, err
	}

	// Create chain event for the new head block of this insertion.
	var (
		lastHeader = chain[len(chain)-1]
		block      = types.NewBlockWithHeader(lastHeader)
	)
	switch status {
	case core.CanonStatTy:
		lc.chainFeed.Send(core.ChainEvent{Block: block, Hash: block.Hash()})
		lc.chainHeadFeed.Send(core.ChainHeadEvent{Block: block})
	case core.SideStatTy:
		lc.chainSideFeed.Send(core.ChainSideEvent{Block: block})
	}
	return 0, err
}

func (lc *LightChain) SetBeaconHead(head beacon.Header) {
	lc.chainmu.Lock()
	lc.currentHeader, lc.finalizedHeader = nil, nil
	lc.beaconHeader = head
	lc.chainmu.Unlock()
	log.Info("Received new beacon head", "slot", head.Slot, "blockRoot", head.Hash())
}

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
func (lc *LightChain) CurrentHeader(ctx context.Context) (*types.Header, error) {
	lc.chainmu.RLock()
	currentHeader, beaconHeader := lc.currentHeader, lc.beaconHeader
	lc.chainmu.RUnlock()

	if currentHeader != nil {
		return currentHeader, nil
	}

	for {
		if headers, err := GetExecHeaders(ctx, lc.odr, beaconHeader, HeadMode, 0, 1); err == nil && len(headers) == 1 {
			currentHeader = headers[0]
			lc.chainmu.Lock()
			if lc.beaconHeader == beaconHeader {
				lc.currentHeader = currentHeader
			} else {
				currentHeader, beaconHeader = nil, lc.beaconHeader
			}
			lc.chainmu.Unlock()
			if currentHeader != nil {
				return currentHeader, nil
			}
		} else {
			if err == nil {
				return nil, errors.New("Invalid number of headers retrieved") //TODO can this ever happen?
			}
			return nil, err
		}
	}

	//	return lc.hc.CurrentHeader()
}

func (lc *LightChain) FinalizedHeader(ctx context.Context) (*types.Header, error) {
	lc.chainmu.RLock()
	finalizedHeader, beaconHeader := lc.finalizedHeader, lc.beaconHeader
	lc.chainmu.RUnlock()

	if finalizedHeader != nil {
		return finalizedHeader, nil
	}

	for {
		if headers, err := GetExecHeaders(ctx, lc.odr, beaconHeader, FinalizedMode, 0, 1); err == nil && len(headers) == 1 {
			finalizedHeader = headers[0]
			lc.chainmu.Lock()
			if lc.beaconHeader == beaconHeader {
				lc.finalizedHeader = finalizedHeader
			} else {
				finalizedHeader, beaconHeader = nil, lc.beaconHeader
			}
			lc.chainmu.Unlock()
			if finalizedHeader != nil {
				return finalizedHeader, nil
			}
		} else {
			if err == nil {
				return nil, errors.New("Invalid number of headers retrieved") //TODO can this ever happen?
			}
			return nil, err
		}
	}
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash and number, caching it if found.
func (lc *LightChain) GetTd(hash common.Hash, number uint64) *big.Int {
	return lc.hc.GetTd(hash, number)
}

// GetHeaderByNumberOdr retrieves the total difficult from the database or
// network by hash and number, caching it (associated with its hash) if found.
func (lc *LightChain) GetTdOdr(ctx context.Context, hash common.Hash, number uint64) *big.Int {
	td := lc.GetTd(hash, number)
	if td != nil {
		return td
	}
	td, _ = GetTd(ctx, lc.odr, hash, number)
	return td
}

// GetHeader retrieves a block header from the database by hash and number,
// caching it if found.
func (lc *LightChain) GetHeader(ctx context.Context, hash common.Hash, number uint64) (*types.Header, error) {
	//TODO cache, lookup/store in database (by number and hash)
	return GetHeaderByHash(ctx, lc.odr, hash)
}

// GetHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
func (lc *LightChain) GetHeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	//TODO cache, lookup/store in database (by hash)
	return GetHeaderByHash(ctx, lc.odr, hash)
}

// HasHeader checks if a block header is present in the database or not, caching
// it if present.
func (lc *LightChain) HasHeader(hash common.Hash, number uint64) bool {
	return lc.hc.HasHeader(hash, number)
}

// GetCanonicalHash returns the canonical hash for a given block number
func (bc *LightChain) GetCanonicalHash(ctx context.Context, number uint64) (common.Hash, error) {
	if header, err := bc.GetHeaderByNumber(ctx, number); header != nil { //TODO do this with zero header request? or not needed if the header is cached?
		return header.Hash(), nil
	} else {
		return common.Hash{}, err
	}
	//return bc.hc.GetCanonicalHash(number)
}

// GetAncestor retrieves the Nth ancestor of a given block. It assumes that either the given block or
// a close ancestor of it is canonical. maxNonCanonical points to a downwards counter limiting the
// number of blocks to be individually checked before we reach the canonical chain.
//
// Note: ancestor == 0 returns the same block, 1 returns its parent and so on.
func (lc *LightChain) GetAncestor(hash common.Hash, number, ancestor uint64, maxNonCanonical *uint64) (common.Hash, uint64) {
	return lc.hc.GetAncestor(hash, number, ancestor, maxNonCanonical)
}

// GetHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
/*func (lc *LightChain) GetHeaderByNumber(number uint64) *types.Header {
	return lc.hc.GetHeaderByNumber(number)
}*/

// GetHeaderByNumberOdr retrieves a block header from the database or network
// by number, caching it (associated with its hash) if found.
/*func (lc *LightChain) GetHeaderByNumberOdr(ctx context.Context, number uint64) (*types.Header, error) {
	if header := lc.hc.GetHeaderByNumber(number); header != nil {
		return header, nil
	}
	return GetHeaderByNumber(ctx, lc.odr, number)
}*/

func (lc *LightChain) GetHeaderByNumber(ctx context.Context, number uint64) (*types.Header, error) {
	lc.chainmu.RLock()
	beaconHeader := lc.beaconHeader
	lc.chainmu.RUnlock()

	//TODO cache header by number (clear cache when beacon head is updated)
	return GetHeaderByNumber(ctx, lc.odr, beaconHeader, number)
}

// Config retrieves the header chain's chain configuration.
func (lc *LightChain) Config() *params.ChainConfig { return lc.hc.Config() }

// SyncCheckpoint fetches the checkpoint point block header according to
// the checkpoint provided by the remote peer.
//
// Note if we are running the clique, fetches the last epoch snapshot header
// which covered by checkpoint.
func (lc *LightChain) SyncCheckpoint(ctx context.Context, checkpoint *params.TrustedCheckpoint) bool {
	// Ensure the remote checkpoint head is ahead of us
	/*head := lc.CurrentHeader().Number.Uint64()

	latest := (checkpoint.SectionIndex+1)*lc.indexerConfig.ChtSize - 1
	if clique := lc.hc.Config().Clique; clique != nil {
		latest -= latest % clique.Epoch // epoch snapshot for clique
	}
	if head >= latest {
		return true
	}
	// Retrieve the latest useful header and update to it
	if header, err := GetHeaderByNumber(ctx, lc.odr, latest); header != nil && err == nil {
		lc.chainmu.Lock()
		defer lc.chainmu.Unlock()

		// Ensure the chain didn't move past the latest block while retrieving it
		if lc.hc.CurrentHeader().Number.Uint64() < header.Number.Uint64() {
			log.Info("Updated latest header based on CHT", "number", header.Number, "hash", header.Hash(), "age", common.PrettyAge(time.Unix(int64(header.Time), 0)))
			rawdb.WriteHeadHeaderHash(lc.chainDb, header.Hash())
			lc.hc.SetCurrentHeader(header)
		}
		return true
	}*/ //TODO
	return false
}

// LockChain locks the chain mutex for reading so that multiple canonical hashes can be
// retrieved while it is guaranteed that they belong to the same version of the chain
func (lc *LightChain) LockChain() {
	lc.chainmu.RLock()
}

// UnlockChain unlocks the chain mutex
func (lc *LightChain) UnlockChain() {
	lc.chainmu.RUnlock()
}

// SubscribeChainEvent registers a subscription of ChainEvent.
func (lc *LightChain) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return lc.scope.Track(lc.chainFeed.Subscribe(ch))
}

// SubscribeChainHeadEvent registers a subscription of ChainHeadEvent.
func (lc *LightChain) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return lc.scope.Track(lc.chainHeadFeed.Subscribe(ch))
}

// SubscribeChainSideEvent registers a subscription of ChainSideEvent.
func (lc *LightChain) SubscribeChainSideEvent(ch chan<- core.ChainSideEvent) event.Subscription {
	return lc.scope.Track(lc.chainSideFeed.Subscribe(ch))
}

// SubscribeLogsEvent implements the interface of filters.Backend
// LightChain does not send logs events, so return an empty subscription.
func (lc *LightChain) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return lc.scope.Track(new(event.Feed).Subscribe(ch))
}

// SubscribeRemovedLogsEvent implements the interface of filters.Backend
// LightChain does not send core.RemovedLogsEvent, so return an empty subscription.
func (lc *LightChain) SubscribeRemovedLogsEvent(ch chan<- core.RemovedLogsEvent) event.Subscription {
	return lc.scope.Track(new(event.Feed).Subscribe(ch))
}

// DisableCheckFreq disables header validation. This is used for ultralight mode.
func (lc *LightChain) DisableCheckFreq() {
	atomic.StoreInt32(&lc.disableCheckFreq, 1)
}

// EnableCheckFreq enables header validation.
func (lc *LightChain) EnableCheckFreq() {
	atomic.StoreInt32(&lc.disableCheckFreq, 0)
}
