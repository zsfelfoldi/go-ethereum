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

package core

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

const chainViewReorgCapacity = 256

type chainViewTracker struct {
	reorgCount uint64
	reorgs     [chainViewReorgCapacity]uint64
}

// Note that addReorg may be called concurrently with reorgSince but never with
// itself, therefore only the reorgCount update needs to be atomic.
func (ct *chainViewTracker) addReorg(reorgBlock uint64) {
	ct.reorgs[ct.reorgCount%chainViewReorgCapacity] = reorgBlock
	atomic.AddUint64(&ct.reorgCount, 1)
}

func (ct *chainViewTracker) reorgSince(prevReorgCount uint64) (reorgCount, oldestReorg uint64) {
	reorgCount = atomic.LoadUint64(&ct.reorgCount)
	oldestReorg = math.MaxUint64
	for reorgCount > prevReorgCount {
		reorgBlock := atomic.LoadUint64(&ct.reorgs[prevReorgCount])
		reorgCount = atomic.LoadUint64(&ct.reorgCount)
		if reorgCount >= prevReorgCount+chainViewReorgCapacity {
			return reorgCount, 0
		}
		oldestReorg = min(oldestReorg, reorgBlock)
		prevReorgCount++
	}
	return
}

// ChainView represents an immutable view of a chain with a block id and a set
// of receipts associated to each block number and a block hash associated with
// all block numbers except the head block. This is because in the future
// ChainView might represent a view where the head block is currently being
// created. Block id is a unique identifier that can also be calculated for the
// head block.
// Note that the view's head does not have to be the current canonical head
// of the underlying blockchain, it should only possess the block headers
// and receipts up until the expected chain view head.
type ChainView struct {
	lock                                           sync.Mutex
	chain                                          *BlockChain
	headNumber, lastProcessedReorg, canonicalUntil uint64
	hashes                                         []common.Hash // block hashes starting backwards from headNumber until first canonical hash
}

// NewChainView creates a new ChainView.
func (bc *BlockChain) NewChainView(hash common.Hash, number uint64) *ChainView {
	cv := &ChainView{
		chain:              bc,
		headNumber:         number,
		lastProcessedReorg: atomic.LoadUint64(&bc.chainViewTracker.reorgCount),
		hashes:             []common.Hash{hash},
	}
	for {
		if bc.GetCanonicalHash(number) == hash {
			reorgCount := atomic.LoadUint64(&bc.chainViewTracker.reorgCount)
			if reorgCount == cv.lastProcessedReorg {
				cv.canonicalUntil = number
				return cv
			}
			cv.lastProcessedReorg = reorgCount
			continue
		}
		if number == 0 {
			return nil
		}
		header := bc.GetHeader(hash, number)
		if header == nil {
			return nil
		}
		hash = header.ParentHash
		number--
		cv.hashes = append(cv.hashes, hash)
	}
}

// HeadNumber returns the head block number of the chain view.
func (cv *ChainView) HeadNumber() uint64 {
	return cv.headNumber
}

// BlockHash returns the block hash belonging to the given block number.
// Note that the hash of the head block is not returned because ChainView might
// represent a view where the head block is currently being created.
func (cv *ChainView) CanonicalHash(number uint64) common.Hash {
	cv.lock.Lock()
	defer cv.lock.Unlock()

	if number > cv.headNumber {
		return common.Hash{}
	}
	if number > cv.canonicalUntil {
		return cv.hashes[cv.headNumber-number]
	}
	if !cv.updateCanonical() {
		return common.Hash{}
	}
	if number > cv.canonicalUntil {
		return cv.hashes[cv.headNumber-number]
	}
	hash := cv.chain.GetCanonicalHash(number)
	if !cv.updateCanonical() {
		return common.Hash{}
	}
	if number > cv.canonicalUntil {
		return cv.hashes[cv.headNumber-number]
	}
	return hash
}

func (cv *ChainView) updateCanonical() bool {
	var oldestReorg uint64
	cv.lastProcessedReorg, oldestReorg = cv.chain.chainViewTracker.reorgSince(cv.lastProcessedReorg)
	hash := cv.hashes[cv.headNumber-cv.canonicalUntil]
	for oldestReorg < cv.canonicalUntil {
		header := bc.GetHeader(hash, cv.canonicalUntil)
		if header == nil {
			return nil
		}
		hash = header.ParentHash
		number--
		cv.hashes = append(cv.hashes, hash)
	}
}

// Header returns the block header at the given block number.
func (cv *ChainView) Header(number uint64) *types.Header {
	return rawdb.ReadHeader(cv.chain.db, cv.CanonicalHash(number), number)
}

func (cv *ChainView) Body(number uint64) *types.Body {
	return rawdb.ReadBody(cv.chain.db, cv.CanonicalHash(number), number)
}

// RawReceipts returns the set of receipts belonging to the block at the given
// block number. Does not derive the fields of the receipts, should only be
// used during creation of the filter maps, please use cv.Receipts during querying.
func (cv *ChainView) RawReceipts(number uint64) types.Receipts {
	return rawdb.ReadRawReceipts(cv.chain.db, cv.CanonicalHash(number), number)
}
