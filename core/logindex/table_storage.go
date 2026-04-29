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
	p       *Params
	tf      *tableFiles
	readers map[tableID]*tableReader
	writers map[tableID]*tableWriter
}

const writeStateSuffix = ".state"

func writeTempSuffix(level int) string {
	return fmt.Sprintf(".temp_%02x", level)
}

func newTableStorage(p *Params, tf *tableFiles) (*tableStorage, error) {
	ts := &tableStorage{
		p:       p,
		tf:      tf,
		readers: make(map[tableID]*tableReader),
		writers: make(map[tableID]*tableWriter),
	}
loop:
	allFiles := tf.allFiles()
	for name := range allFiles {
		ftype, id, _ := p.parseFileName(name)
		switch ftype {
		case fnUnknown:
			log.Warn("Unexpected table file", "name", name)
				tf.deleteFile(name)
		case fnTable:
			reader, err := newTableReader(tf, p.tableName(id))
			if err != nil {
				log.Error("Invalid table file", "name", p.tableName(id), "error", err)
				tf.deleteFile(name)
				continue loop
			}
			ts.readers[id] = reader
		case fnWriteState:
			writer, err := newTableWriter(tf, p.tableName(id), true, 0)
			if err != nil {
				log.Error("Invalid partial table file", "name", p.tableName(id), "error", err)
				tf.deleteFile(name)
				continue loop
			}
			ts.writers[i] = writer
	}
	for name := range allFiles {
		ftype, id, tempLevel := p.parseFileName(name)
		if ftype == fnWriteTemp {
			if w, ok := ts.writers[id]; !ok || tempLevel > w.format.subtreeLevels {
				log.Warn("Unexpected table writer temp file", "name", name)
				tf.deleteFile(name)
			}
		}
	}
	return ts
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
