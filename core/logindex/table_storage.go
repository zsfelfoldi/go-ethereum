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
	lock            sync.Mutex
	path            string
	closed          bool
	maxFiles        int
	lastUsedCounter uint64
	tableFiles      map[tableFileID]fileReader
	tableParts      map[tablePartID]*tablePart
}

const (
	tsFinal = iota
	tsTempLevel
	tsTempState
)

type tableID struct {
	level uint32
	index uint64
}

type tablePartID struct {
	tableID
	state, tempLevel int
}

type tableFileID struct {
	tablePartID
	fileIndex int
}

type fileReader struct {
	file     *os.File
	lastUsed uint64
}

func (id *tableFileID) fileName() string {
	switch id.state {
	case tsFinal:
		return fmt.Sprintf("table_%016x_%016x.table-%04x", firstBlock, blockCount, id.fileIndex)
	case tsTempLevel:
		return fmt.Sprintf("table_%016x_%016x.temp_level_%02x-%04x", firstBlock, blockCount, id.tempLevel, id.fileIndex)
	case tsTempState:
		return fmt.Sprintf("table_%016x_%016x.temp_state-%04x", firstBlock, blockCount, id.fileIndex)
	default:
		panic("invalid table file ID")
	}
}

func (id *tableFileID) parseName(name string) bool {
	var (
		firstBlock, blockCount uint64
		partID, ext            string
	)
	n, err := fmt.Sscanf(name, "%s-%04x", &partID, &id.fileIndex)
	if n != 2 || err != nil {
		return false
	}
	n, err := fmt.Sscanf(partID, "table_%016x_%016x.%s", &firstBlock, &blockCount, &ext)
	if n != 3 || err != nil {
		return false
	}
	id.level = bits.TrailingZeros64(blockCount)
	if blockCount != uint64(1)<<id.level {
		return false
	}
	id.index = firstBlock >> id.level
	if firstBlock != id.index<<id.level {
		return false
	}
	switch ext {
	case "table":
		id.state = tsFinal
	case "temp_state":
		id.state = tsTempState
	default:
		n, err := fmt.Sscanf(ext, "temp_level_%02x", &id.tempLevel)
		if n != 3 || err != nil {
			return false
		}
	}
	return name == id.fileName()
}

func (ts *tableStorage) getFileReader(id tableFileID) (*os.File, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	ts.lastUsedCounter++
	if fr, ok := ts.tableFiles[id]; ok {
		fr.lastUsed = ts.lastUsedCounter
		ts.tableFiles[id] = fr
		return fr.file, nil
	}
	for len(ts.tableFiles) >= maxFiles {
		oldestLu := math.MaxUint64
		var oldestId tableFileID
		for id, fr := range ts.tableFiles {
			if fr.lastUsed < oldestLu {
				oldestId, oldestLu = id, fr.lastUsed
			}
		}
		ts.tableFiles[oldestId].file.Close()
		delete(ts.tableFiles, oldestId)
	}
	file, err := os.Open(ts.getFileName(id))
	if err != nil {
		return nil, err
	}
	ts.tableFiles[oldestId] = fileReader{file: file, lastUsed: ts.lastUsedCounter}
	return file, nil
}

type tablePart struct {
	fileStorage bool // stored in individual file
	locked      bool // used by exclusive reader/writer
	readerCount int  // used by one or more concurrent readers
	memData     []byte
	memWriter   *bytes.Buffer
}

type memoryTablePart struct {
	Name string
	Data []byte
}

func newTableStorage(path string, maxFiles int) (*tableStorage, error) {
	if maxFiles < 1 {
		return nil, errors.New("invalid maxFiles parameter")
	}
	ts := &tableStorage{
		path:       path,
		maxFiles:   maxFiles,
		tableFiles: make(map[tableFileID]fileReader),
		tableParts: make(map[tablePartID]*tablePart),
	}
	var memTables []memoryTablePart
	fn := filepath.Join(path, "small_table_parts")
	f, err := os.Open(fn)
	if !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, err
		}
		err := rlp.Decode(f, &memTables)
		f.Close()
		if err != nil {
			return nil, err
		}
		if err := os.Remove(fn); err != nil {
			return nil, err
		}
	}
	for _, mt := range memTables {
		var id tablePartID
		if !id.parseName(mt.Name) {
			log.Warn("Invalid memory index table file name", "name", mt.Name)
			continue
		}
		ts.tableParts[id] = &tablePart{memData: mt.Data}
	}
	entries, err := os.ReadDir(path) //TODO create dir if not present?
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var id tableFileID
		if !id.parseName(entry.Name()) {
			log.Warn("Invalid index table file name", "name", entry.Name())
			continue
		}
		ts.tableParts[id.tablePartID] = &tablePart{fileStorage: true}
	}
	return ts, nil
}

func (ts *tableStorage) close() {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	var memTables []memoryTablePart
	for id, tp := range ts.tableParts {
		if tp.locked {
			log.Error("Index table is still locked while shutting down", "index", id.index, "level", id.level, "state", id.state, "tempLevel", id.tempLevel)
			continue
		}
		if tp.fileStorage {
			continue
		}
		memTables = append(memTables, memoryTablePart{
			Name: id.fileName(),
			Data: tp.memData,
		})
	}
	f, err := os.OpenFile(filepath.Join(ts.path, "small_table_parts"), os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		err = rlp.Encode(f, memTables)
		f.Close()
	}
	if err != nil {
		log.Error("Could not save small index table parts", "error", err)
	}
	ts.closed = true //TODO check everywhere
}

func (ts *tableStorage) getReadSeeker(id tablePartID) (io.ReadSeeker, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	tp := ts.tableParts[id]
	if tp == nil {
		return nil, errors.New("table part not found")
	}
	if tp.locked || tp.readerCount != 0 {
		return nil, errors.New("table part already in use")
	}
	if tp.storageType == stFile {
		f, err := os.Open(ts.getFileName(id))
		if err != nil {
			return nil, err
		}
		tp.locked = true
		tp.file = f
		return f, nil
	} else {
		tp.locked = true
		return bytes.NewReader(tp.memData), nil
	}
}

func (ts *tableStorage) getAppendWriter(id tablePartID, fileStorage bool) (io.Writer, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	tp := ts.tableParts[id]
	if tp == nil {
		tp = &tablePart{
			fileStorage: fileStorage,
		}
		ts.tableParts[id] = tp
	}
	if tp.locked || tp.readerCount != 0 {
		return nil, errors.New("table part already in use")
	}
	if tp.storageType == stFile {
		f, err := os.OpenFile(ts.getFileName(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		tp.locked = true
		tp.file = f
		return f, nil
	} else {
		tp.locked = true
		tp.memWriter = bytes.NewBuffer(tp.memData)
		return tp.memWriter, nil
	}
}

func (ts *tableStorage) getReaderAt(id tablePartID) (io.ReaderAt, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	tp := ts.tableParts[id]
	if tp == nil {
		return nil, errors.New("table part not found")
	}
	if tp.locked {
		return nil, errors.New("table part is locked")
	}
	if tp.storageType == stFile {
		if tp.readerCount == 0 {
			f, err := os.Open(ts.getFileName(id))
			if err != nil {
				return nil, err
			}
			tp.file = f
		}
		tp.readerCount++
		return tp.file, nil
	} else {
		tp.readerCount++
		return bytes.NewReader(tp.memData), nil
	}
}

func (ts *tableStorage) release(id tablePartID) error {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	tp := ts.tableParts[id]
	if tp == nil {
		return errors.New("table part not found")
	}
	switch {
	case tp.readLock:
		tp.readLock = false
		if tp.storageType == stFile {
			err := tp.file.Close()
			tp.file = nil
			return err
		} else {
			return nil
		}
	case tp.writeLock:
		tp.writeLockLock = false
		if tp.storageType == stFile {
			err := tp.file.Close()
			tp.file = nil
			return err
		} else {
			tp.memData = tp.memWriter.Bytes()
			tp.memWriter = nil
			return nil
		}
	case tp.readerCount != 0:
		tp.readerCount--
		if tp.readerCount == 0 && tp.storageType == stFile {
			err := tp.file.Close()
			tp.file = nil
			return err
		} else {
			return nil
		}
	default:
		return errors.New("table part already released")
	}
}
