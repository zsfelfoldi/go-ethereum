// Copyright 2024 The go-ethereum Authors
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
	"crypto/sha256"
	"encoding/binary"
	"math/bits"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type treeHasher struct {
	*FilterMaps
	blockNumber                   uint64
	blockHash                     common.Hash
	receipts                      types.Receipts
	txIndex, logIndex, valueIndex int
	lvIndex                       uint64
}

func (f *FilterMaps) newTreeHasher() *treeHasher {
	return &treeHasher{
		FilterMaps: f,
		hc:         newHeaderChain(f.chain, f.headBlockNumber, f.headBlockHash),
	}
}

func (h *treeHasher) getNode(index uint64) (common.Hash, error) {
	level := 63 - bits.LeadingZeros(index)
	if level <= h.logMaxEpochs {
		return h.hashNode(index)
	}
	epochIndex := uint32(index>>(level-h.logMaxEpochs)) - uint32(1)<<h.logMaxEpochs
	subtreeRootOffset := uint64(1) << (level - h.logMaxEpochs)
	subIndex := subtreeRootOffset + index&(subtreeRootOffset-1)
	if index&(uint64(1)<<(level-h.logMaxEpochs-1)) == 0 {
		// filter map subtree
		leafOffset := uint64(1) << (h.logMapHeight + h.logMapsPerEpoch)
		if subIndex < leafOffset {
			return h.hashNode(index)
		}
		if subIndex >= leafOffset*2 {
			return common.Hash{}, errors.New("invalid hash tree index")
		}
		return h.mapRowHash(epochIndex, subIndex%h.mapsPerEpoch, (subIndex-leafOffset)>>h.logMapsPerEpoch)
	} else {
		// log index subtree
		leafOffset := uint64(1) << (h.logMapsPerEpoch + h.logValuesPerMap + 1)
		if subIndex < leafOffset {
			return h.hashNode(index)
		}
		if subIndex >= leafOffset*2 {
			return common.Hash{}, errors.New("invalid hash tree index")
		}
		return h.logIndexHash(epochIndex, (subIndex-leafOffset)>>1, subIndex&1)
	}
}

func (h *treeHasher) hashNode(index uint64) (common.Hash, error) {
	left, err := h.getNode(index * 2)
	if err != nil {
		return common.Hash{}, err
	}
	right, err := h.getNode(index*2 + 1)
	if err != nil {
		return common.Hash{}, err
	}
	hasher := sha256.New()
	hasher.Write(left[:])
	hasher.Write(right[:])
	var result common.Hash
	hasher.Sum(result[:0])
	return result, nil
}

func (h *treeHasher) mapRowHash(mapIndex, rowIndex uint32) (common.Hash, error) {
	row, err := h.getFilterMapRow(mapIndex, rowIndex)
	if err != nil {
		return common.Hash{}, err
	}
	encRow := make([]byte, len(row)*4)
	for i, c := range row {
		binary.LittleEndian.PutUint32(encRow[i*4:(i+1)*4], c)
	}
	hasher := sha256.New()
	hasher.Write(encRow)
	var result common.Hash
	hasher.Sum(result[:0])
	return result, nil
}

func (h *treeHasher) logIndexNode(lvIndex uint64, refSubIndex uint32) (common.Hash, error) {
	if !h.iterateTo(lvIndex) {
		if lvIndex < f.tailBlockLvPointer {
			return common.Hash{}, errors.New("not indexed")
		}
		if lvIndex >= f.headLvPointer {
			return common.Hash{}, nil
		}
		blockNumber, lvPointer, err := h.getBlockByLvIndex(lvIndex)
		if err != nil {
			return common.Hash{}, err
		}
		blockHash := h.hc.getBlockHash(blockNumber)
		receipts := h.chain.GetReceiptsByHash(blockHash)
		if receipts == nil {
			return common.Hash{}, errors.New("receipts not found")
		}
		h.blockNumber, h.blockHash, h.receipts, h.lvIndex = blockNumber, blockHash, receipts, lvPointer
		h.txIndex, h.logIndex, h.valueIndex = 0, 0, 0
		if !h.iterateTo(lvIndex) {
			log.Error("Could not iterate to log value index")
			return common.Hash{}, errors.New("could not iterate to log value index")
		}
	}
	switch refSubIndex {
	case 0:
		return h.blockHash, nil
	case 1:
		var result common.Hash
		binary.LittleEndian.PutUint64(result[0:8], h.blockNumber)
		binary.LittleEndian.PutUint32(result[8:12], uint32(h.txIndex))
		binary.LittleEndian.PutUint32(result[12:16], uint32(h.logIndex))
		binary.LittleEndian.PutUint32(result[16:20], uint32(h.valueIndex))
		return result, nil
	default:
		panic("invalid refSubIndex")
	}
}

func (h *treeHasher) iterateTo(lvTarget uint64) bool {
	if l.receipts == nil {
		return false
	}
	for ; l.txIndex < len(l.receipts); l.txIndex++ {
		receipt := l.receipts[l.txIndex]
		for ; l.logIndex < len(receipt.Logs); l.logIndex++ {
			log := receipt.Logs[l.logIndex]
			for ; l.valueIndex <= len(log.Topics); l.valueIndex++ {
				if l.lvIndex == lvTarget {
					return true
				}
				l.lvIndex++
			}
		}
	}
	return false
}
