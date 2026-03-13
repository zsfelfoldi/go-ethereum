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

package logindex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"slices"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/rlp"
)

type indexTable struct {
	tablePath              string
	memTable               []byte
	firstBlock, blockCount uint64
	refCount               int
}

func (it *indexTable) getReader() (*tableReader, error) {
	var ioReader io.ReadSeekCloser
	if it.memTable != nil {
		ioReader = bytes.NewReader(it.memTable)
	} else {
		var err error
		ioReader, err = os.Open(it.tablePath + ".table")
		if err != nil {
			return nil, err
		}
	}
	return newTableReader(ioReader)
}

func (it *indexTable) release() {
	it.refCount--
	if it.refCount == 0 && it.memTable == nil {
		os.Remove(it.tablePath + ".table")
	}
}

type tableStorage struct {
	path string
}

func (ts *tableStorage) newIndexTableFromBlock(block *types.Block) (*indexTable, error) {
	it, err := ts.newIndexTableFromTxReceipts(block.NumberU64(), block.Transactions(), block.Receipts())
	if err != nil {
		return nil, err
	}
	it.setBlockHash(block.Hash())
}

func (ts *tableStorage) newIndexTableFromTxReceipts(blockNumber uint64, txs types.Transactions, receipts types.Receipts) *indexTable {
	entries := txAndLogEntries(blockNumber, txs, receipts)
	entryCount := uint64(len(entries))
	ch := chunkHeights(entryCount)
	fileCount := len(ch)
	for fileCount > 0 && ch[fileCount-1] <= memoryTableLimit {
		fileCount--
	}
	files := make([]os.File, fileCount)
	memTableCount := len(ch) - fileCount
	writers := make([]io.Writer, len(ch))

	tw := newTableWriter(ch, entryCount)
}
