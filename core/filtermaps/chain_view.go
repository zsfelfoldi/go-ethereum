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

package filtermaps

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

type chainView interface {
	headNumber() uint64
	getBlockHash(number uint64) common.Hash
	getBlockId(number uint64) common.Hash
	getReceipts(number uint64) types.Receipts
}

func equalViews(cv1, cv2 chainView) bool {
	if cv1 == nil || cv2 == nil {
		return false
	}
	head1, head2 := cv1.headNumber(), cv2.headNumber()
	return head1 == head2 && cv1.getBlockId(head1) == cv2.getBlockId(head2)
}

func matchViews(cv1, cv2 chainView, number uint64) bool {
	if cv1 == nil || cv2 == nil {
		return false
	}
	head1 := cv1.headNumber()
	if head1 < number {
		return false
	}
	head2 := cv2.headNumber()
	if head2 < number {
		return false
	}
	if number == head1 || number == head2 {
		return cv1.getBlockId(number) == cv2.getBlockId(number)
	}
	return cv1.getBlockHash(number) == cv2.getBlockHash(number)
}

// blockchain defines functions required by the FilterMaps log indexer.
type blockchain interface {
	GetHeader(hash common.Hash, number uint64) *types.Header
	GetCanonicalHash(number uint64) common.Hash
	GetReceiptsByHash(hash common.Hash) types.Receipts
}

type StoredChainView struct {
	chain  blockchain
	head   uint64
	hashes []common.Hash // block hashes starting backwards from headNumber until first canonical hash
}

func NewStoredChainView(chain blockchain, number uint64, hash common.Hash) *StoredChainView {
	cv := &StoredChainView{
		chain:  chain,
		head:   number,
		hashes: []common.Hash{hash},
	}
	cv.extendNonCanonical()
	return cv
}

func (cv *StoredChainView) headNumber() uint64 {
	return cv.head
}

func (cv *StoredChainView) getBlockHash(number uint64) common.Hash {
	if number >= cv.head {
		panic("invalid block number")
	}
	return cv.blockHash(number)
}

func (cv *StoredChainView) getBlockId(number uint64) common.Hash {
	if number > cv.head {
		panic("invalid block number")
	}
	return cv.blockHash(number)
}

func (cv *StoredChainView) getReceipts(number uint64) types.Receipts {
	if number > cv.head {
		panic("invalid block number")
	}
	return cv.chain.GetReceiptsByHash(cv.blockHash(number))
}

func (cv *StoredChainView) extendNonCanonical() bool {
	for {
		hash, number := cv.hashes[len(cv.hashes)-1], cv.head-uint64(len(cv.hashes)-1)
		if cv.chain.GetCanonicalHash(number) == hash {
			return true
		}
		if number == 0 {
			log.Error("Unknown genesis block hash found")
			return false
		}
		header := cv.chain.GetHeader(hash, number)
		if header == nil {
			log.Error("Header not found", "number", number, "hash", hash)
			return false
		}
		cv.hashes = append(cv.hashes, header.ParentHash)
	}
}

func (cv *StoredChainView) blockHash(number uint64) common.Hash {
	if number+uint64(len(cv.hashes)) <= cv.head {
		hash := cv.chain.GetCanonicalHash(number)
		if !cv.extendNonCanonical() {
			return common.Hash{}
		}
		if number+uint64(len(cv.hashes)) <= cv.head {
			return hash
		}
	}
	return cv.hashes[cv.head-number]
}

type limitedChainView struct {
	parent           chainView
	knownLimit, head uint64
}

func newLimitedChainView(parent chainView, knownLimit, headNumber uint64) *limitedChainView {
	return &limitedChainView{
		parent:     parent,
		knownLimit: knownLimit,
		head:       headNumber,
	}
}

func (cv *limitedChainView) headNumber() uint64 {
	return cv.head
}

func (cv *limitedChainView) getBlockHash(number uint64) common.Hash {
	if number >= cv.head {
		panic("invalid block number")
	}
	if number > cv.knownLimit {
		return common.Hash{}
	}
	return cv.parent.getBlockHash(number)
}

func (cv *limitedChainView) getBlockId(number uint64) common.Hash {
	if number > cv.head {
		panic("invalid block number")
	}
	if number > cv.knownLimit {
		return common.Hash{}
	}
	return cv.parent.getBlockId(number)
}

func (cv *limitedChainView) getReceipts(number uint64) types.Receipts {
	if number > cv.head {
		panic("invalid block number")
	}
	if number > cv.knownLimit {
		return nil
	}
	return cv.parent.getReceipts(number)
}
