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

type tableStorage struct {
	lock                    sync.Mutex
	params                  *Params
	tf                      *tableFiles
	readers, unknownReaders map[tableID]*tableReader
	writers, unknownWriters map[tableID]*tableWriter
	initRequest             bool
	initNumber              uint64
}

const writeStateSuffix = ".state"

func writeTempSuffix(level int) string {
	return fmt.Sprintf(".temp_%02x", level)
}

func newTableStorage(p *Params, tf *tableFiles) (*tableStorage, error) {
	ts := &tableStorage{
		params:         p,
		tf:             tf,
		readers:        make(map[tableID]*tableReader),
		unknownReaders: make(map[tableID]*tableReader),
		writers:        make(map[tableID]*tableWriter),
		unknownWriters: make(map[tableID]*tableWriter),
	}
loop:
	allFiles := tf.allFiles()
	for name := range allFiles {
		ftype, id, _ := p.parseFileName(name)
		switch ftype {
		case fnUnknown:
			log.Warn("Unexpected index table file", "name", name)
			tf.deleteFile(name)
		case fnTable:
			reader, err := newTableReader(tf, p.tableName(id))
			if err != nil {
				log.Error("Invalid index table file", "name", p.tableName(id), "error", err)
				tf.deleteFile(name)
				continue loop
			}
			ts.unknownReaders[id] = reader
		case fnWriteState:
			writer, err := newTableWriter(tf, p.tableName(id), true, 0)
			if err != nil {
				log.Error("Invalid partial index table file", "name", p.tableName(id), "error", err)
				tf.deleteFile(name)
				continue loop
			}
			ts.unknownWriters[i] = writer
		}
	}
	for id := range ts.unknownWriters {
		if _, ok := ts.unknownReaders[id]; ok {
			log.Warn("Both complete and partial index table found", "name", p.tableName(id))
			delete(ts.unknownWriters, id)
			tf.deleteFile(p.tableName(id) + writeStateSuffix)
		}
	}
	for name := range allFiles {
		ftype, id, tempLevel := p.parseFileName(name)
		if ftype == fnWriteTemp {
			if w, ok := ts.unknownWriters[id]; !ok || tempLevel > w.format.subtreeLevels {
				log.Warn("Unexpected index table writer temp file", "name", name)
				tf.deleteFile(name)
			}
		}
	}
	return ts
}

func (ts *tableStorage) requestInitBlockHash() (number uint64, request bool) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	for _, tr := range ts.unknownReaders {
		if !request || tr.meta.LastBlockNumber > number {
			request, number = true, tr.meta.LastBlockNumber
		}
	}
	for _, tw := range ts.unknownWriters {
		if !request || tw.meta.LastBlockNumber > number {
			request, number = true, tw.meta.LastBlockNumber
		}
	}
	ts.initRequest, ts.initNumber = request, number
	return
}

func (ts *tableStorage) deliverInitBlockHash(number uint64, hash common.Hash) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if !ts.initRequest || number != ts.initNumber {
		return
	}
	ts.initRequest = false
	hashes := make(map[uint64]common.Hash)
	hashes[number] = hash
	for len(hashes) > 0 {
		for number, hash := range hashes {
			for id, tr := range ts.unknownReaders {
				if tr.meta.LastBlockNumber != number {
					continue
				}
				if tr.meta.LastBlockHash == hash {
					ts.readers[id] = tr
					if tr.meta.LastBlockNumber >= tr.meta.BlockCount {
						hashes[tr.meta.LastBlockNumber-tr.meta.BlockCount] = tr.meta.ParentHash
					}
				} else {
					ts.tf.deleteFile(ts.p.tableName(id))
				}
				delete(ts.unknownReaders, id)
			}
			for id, tw := range ts.unknownWriters {
				if tw.meta.LastBlockNumber != number {
					continue
				}
				if tw.meta.LastBlockHash == hash {
					ts.writers[id] = tw
					if tw.meta.LastBlockNumber >= tw.meta.BlockCount {
						hashes[tw.meta.LastBlockNumber-tw.meta.BlockCount] = tw.meta.ParentHash
					}
				} else {
					tw.delete()
				}
				delete(ts.unknownWriters, id)
			}
		}
	}
}

func (ts *tableStorage) close() {
	ts.tf.close()
}

func (p *Params) tableName(id tableID) string {
	br := p.tableLevels[id.level].blockRange(id.index)
	return fmt.Sprintf("table_%016x_%016x", br.First(), br.Count())
}

const (
	fnUnknown = iota
	fnTable
	fnWriteState
	fnWriteTemp
)

func (p *Params) parseFileName(name string) (int, tableID, int) {
	var (
		firstBlock, blockCount uint64
		suffix                 string
		tempLevel              int
	)
	n, _ := fmt.Sscanf(name, "table_%016x_%016x%s", &firstBlock, &blockCount, &suffix)
	if n != 2 && n != 3 {
		return fnUnknown, tableID{}, 0
	}
	id, ok := p.rangeID(common.NewRange[uint64](firstBlock, blockCount))
	if !ok {
		return fnUnknown, tableID{}, 0
	}
	switch suffix {
	case "":
		return fnTable, id, 0
	case writeStateSuffix:
		return fnWriteState, id, 0
	default:
		if n, _ := fmt.Sscanf(suffix, ".temp_%02x", &tempLevel); n == 1 {
			return fnWriteTemp, id, tempLevel
		}
	}
	return fnUnknown, tableID{}, 0
}
