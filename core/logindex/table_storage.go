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
	lock    sync.Mutex
	path    string
	tables  map[tableID]*indexTable
	partial map[tableID]*partialTable
}

type tableID struct {
	level uint32
	index uint64
}

type indexTable struct {
	persistent bool
	refCount   int
	file       *os.File
	memTable   []byte
	reader     *tableReader
}

type partialTable struct {
	format     tableFormat
	open       bool
	files      []*os.File
	memLevels  [][]byte
	buffers    []*bytes.Buffer
	writeState []byte
}

func newTableStorage(path string) (*tableStorage, tableSet, error) {

}

func (ts *tableStorage) getReader(id tableID) (*tableReader, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	it := ts.tables[id]
	if it == nil {
		return nil, errors.New("table not found")
	}
	if it.refCount == 0 {
		if it.persistent {
			f, err := os.Open(ts.tableFileName(id))
			if err != nil {
				return nil, err
			}
			fi, err := f.Stat()
			if err != nil {
				return nil, err
			}
			reader, err := newTableReader(f, fi.Size())
			if err != nil {
				return nil, err
			}
			it.file, it.reader = f, reader
		} else {
			tr, err := bytes.NewReader(it.memTable, len(it.memTable))
			if err != nil {
				return nil, err
			}
			it.reader = tr
		}
	}
	it.refCount++
	return it.reader
}

func (ts *tableStorage) releaseReader(id tableID) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	it := ts.tables[id]
	if it.refCount <= 0 {
		panic("table reader refCount <= 0")
	}
	it.refCount--
	if it.refCount == 0 {
		if it.file != nil {
			it.file.Close()
		}
		it.file, it.reader = nil, nil
	}
}

func (ts *tableStorage) getWriter(id tableID, format tableFormat) (*tableWriter, []byte, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if it := ts.tables[id]; it != nil {
		return nil, nil, errors.New("finalized table already exists")
	}
	pt := ts.partial[id]
	if pt == nil {
		pt = &partialTable{
			format:    format,
			files:     make([]*os.File, format.fileStorage),
			memLevels: make([][]byte, format.memoryStorage),
			buffers:   make([]*bytes.Buffer, format.memoryStorage),
		}
		ts.partial[id] = pt
	}
	for i := range pt.files {
		f, err := os.OpenFile(ts.tempFileName(id, format.memoryStorage+i), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			for j := range i {
				pt.files[j].Close()
			}
			return nil, nil, err
		}
		pt.files[i] = f
	}
	for i, ml := range pt.memLevels {
		pt.buffers[i] = bytes.NewBuffer(ml)
	}
	pt.open = true
	writers := make([]io.Writer, format.fileStorage+format.memoryStorage)
	for i, buf := range pt.buffers {
		writers[i] = buf
	}
	for i, file := range pt.files {
		writers[format.memoryStorage+i] = file
	}
	tw := newTableWriter(writers, format)
	return tw, pt.writeState, nil
}
