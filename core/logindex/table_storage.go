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
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type tableStorage struct {
	lock                       sync.Mutex
	params                     *Params
	tf                         *tableFiles
	readers                    map[tableID]*tableReader
	writers                    map[tableID]*tableWriter
	triggers                   map[tableID]chan struct{}
	complete, partial, preInit tableSet
	initRequest                bool
	initNumber                 uint64
}

const writeStateSuffix = ".state"

var errTableNotFound = errors.New("table not found")

func writeTempSuffix(level uint) string {
	return fmt.Sprintf(".temp_%02x", level)
}

func newTableStorage(params *Params, tf *tableFiles) (*tableStorage, error) {
	ts := &tableStorage{
		params:   params,
		tf:       tf,
		readers:  make(map[tableID]*tableReader),
		writers:  make(map[tableID]*tableWriter),
		triggers: make(map[tableID]chan struct{}),
		complete: params.newTableSet(),
		partial:  params.newTableSet(),
		preInit:  params.newTableSet(),
	}
	allFiles := tf.allFiles()
loop:
	for _, name := range allFiles {
		ftype, id, _ := params.parseFileName(name)
		switch ftype {
		case fnUnknown:
			log.Warn("Unexpected index table file", "name", name)
			tf.deleteFile(name)
		case fnTable:
			reader, err := newTableReader(params, tf, params.tableName(id))
			if err != nil {
				log.Error("Invalid index table file", "name", params.tableName(id), "error", err)
				tf.deleteFile(name)
				continue loop
			}
			ts.readers[id] = reader
			ts.complete.add(id)
			ts.preInit.add(id)
		case fnWriteState:
			writer, err := newTableWriter(params, tf, params.tableName(id), true, 0)
			if err != nil {
				log.Error("Invalid partial index table file", "name", params.tableName(id), "error", err)
				tf.deleteFile(name)
				continue loop
			}
			ts.writers[id] = writer
			ts.partial.add(id)
			ts.preInit.add(id)
		}
	}
	for id := range ts.writers {
		if _, ok := ts.readers[id]; ok {
			log.Warn("Both complete and partial index table found", "name", params.tableName(id))
			delete(ts.writers, id)
			tf.deleteFile(params.tableName(id) + writeStateSuffix)
		}
	}
	for _, name := range allFiles {
		ftype, id, tempLevel := params.parseFileName(name)
		if ftype == fnWriteTemp {
			if w, ok := ts.writers[id]; !ok || tempLevel > w.format.subtreeLevels {
				log.Warn("Unexpected index table writer temp file", "name", name)
				tf.deleteFile(name)
			}
		}
	}
	return ts, nil
}

func (ts *tableStorage) tables() (tableSet, tableSet) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	return ts.complete, ts.partial
}

func (ts *tableStorage) getTableReader(id tableID) (*tableReader, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if tr, ok := ts.readers[id]; ok {
		return tr, nil
	}
	return nil, errTableNotFound
}

func (ts *tableStorage) waitTableReader(id tableID) (*tableReader, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if tr, ok := ts.readers[id]; ok {
		return tr, nil
	}
	ch, ok := ts.triggers[id]
	if !ok {
		ch = make(chan struct{})
		ts.triggers[id] = ch
	}
	ts.lock.Unlock()
	<-ch
	ts.lock.Lock()
	if tr, ok := ts.readers[id]; ok {
		return tr, nil
	}
	return nil, errTableNotFound
}

func (ts *tableStorage) addNewTableWriter(id tableID, entryCount uint64) (*tableWriter, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if ts.writers[id] != nil {
		return nil, errors.New("table already exists")
	}
	tw, err := newTableWriter(ts.params, ts.tf, ts.params.tableName(id), false, entryCount)
	if err != nil {
		return nil, err
	}
	ts.writers[id] = tw
	return tw, nil
}

func (ts *tableStorage) getTableWriter(id tableID) (*tableWriter, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if tw, ok := ts.writers[id]; ok {
		return tw, nil
	}
	return nil, errTableNotFound
}

func (ts *tableStorage) finishedTableWriter(id tableID) error {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	tw, ok := ts.writers[id]
	if !ok {
		return errTableNotFound
	}
	if !tw.isFinalized() {
		return errors.New("table writer not finalized yet")
	}
	delete(ts.writers, id)
	ts.partial.remove(id)
	tr, err := newTableReader(ts.params, ts.tf, ts.params.tableName(id))
	if err != nil {
		return err
	}
	ts.readers[id] = tr
	ts.complete.add(id)
	if ch, ok := ts.triggers[id]; ok {
		close(ch)
		delete(ts.triggers, id)
	}
	return nil
}

func (ts *tableStorage) deleteTable(id tableID) error {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	return ts.deleteTableLocked(id)
}

func (ts *tableStorage) deleteTableLocked(id tableID) error {
	if _, ok := ts.readers[id]; ok {
		delete(ts.readers, id)
		ts.complete.remove(id)
		return ts.tf.deleteFile(ts.params.tableName(id))
	}
	if tw, ok := ts.writers[id]; ok {
		delete(ts.writers, id)
		ts.partial.remove(id)
		return tw.delete()
	}
	return errTableNotFound
}

func (ts *tableStorage) deleteTablesFromBlock(blockNumber uint64) error {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	for i := range ts.params.tableLevels {
		for {
			rs := ts.complete[i].Union(ts.partial[i])
			if rs.IsEmpty() {
				break
			}
			id := tableID{level: i, index: rs.Last()}
			if ts.params.blockRange(id).Last() >= blockNumber {
				if err := ts.deleteTableLocked(id); err != nil {
					return err
				}
			} else {
				break
			}
		}
	}
	return nil
}

func (ts *tableStorage) requestInitBlockHash() (number uint64, request bool) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	for i, rs := range ts.preInit {
		if !rs.IsEmpty() {
			last := ts.params.blockRange(tableID{level: i, index: rs.Last()}).Last()
			if !request || last > number {
				request, number = true, last
			}
		}
	}
	ts.initRequest, ts.initNumber = request, number
	return
}

func (ts *tableStorage) deliverInitBlockHash(number uint64, hash common.Hash) bool {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if ts.preInit == nil {
		return true
	}
	if !ts.initRequest || number != ts.initNumber {
		return false
	}
	ts.initRequest = false
	hashes := make(map[uint64]common.Hash)
	hashes[number] = hash
	for len(hashes) > 0 {
		for number, hash := range hashes {
			for id, tr := range ts.readers {
				if tr.meta.LastBlockNumber != number || !ts.preInit.includes(id) {
					continue
				}
				if tr.meta.LastBlockHash == hash {
					if tr.meta.LastBlockNumber >= tr.meta.BlockCount {
						hashes[tr.meta.LastBlockNumber-tr.meta.BlockCount] = tr.meta.ParentHash
					}
				} else {
					ts.complete.remove(id)
					ts.tf.deleteFile(ts.params.tableName(id))
				}
				ts.preInit.remove(id)
			}
			for id, tw := range ts.writers {
				if tw.meta.LastBlockNumber != number || !ts.preInit.includes(id) {
					continue
				}
				if tw.meta.LastBlockHash == hash {
					if tw.meta.LastBlockNumber >= tw.meta.BlockCount {
						hashes[tw.meta.LastBlockNumber-tw.meta.BlockCount] = tw.meta.ParentHash
					}
				} else {
					ts.partial.remove(id)
					tw.delete()
				}
				ts.preInit.remove(id)
			}
		}
	}
	if ts.preInit.isEmpty() {
		ts.preInit = nil
	}
	return ts.preInit == nil
}

func (ts *tableStorage) close() {
	for _, ch := range ts.triggers {
		close(ch)
	}
	ts.tf.close()
}

func (p *Params) tableName(id tableID) string {
	br := p.blockRange(id)
	return fmt.Sprintf("table_%016x_%016x", br.First(), br.Count())
}

const (
	fnUnknown = iota
	fnTable
	fnWriteState
	fnWriteTemp
)

func (p *Params) parseFileName(name string) (int, tableID, uint) {
	var (
		firstBlock, blockCount uint64
		suffix                 string
		tempLevel              uint
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
