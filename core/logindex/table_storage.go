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
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

const (
	maxOpenWriters   = 8 //TODO >= maxMergeThreads+1
	writeStateSuffix = ".state"
)

var errTableNotFound = errors.New("table not found")

type tableStorage struct {
	lock                       sync.Mutex
	params                     *Params
	tf                         *tableFiles
	readers                    map[tableID]*TableReader
	writers                    map[tableID]*tableWriter
	triggers                   map[tableID]chan struct{}
	complete, partial, preInit tableSet
	initRequest                bool
	initNumber                 uint64
}

func writeTempSuffix(level uint) string {
	return fmt.Sprintf(".temp_%02x", level)
}

func newTableStorage(params *Params, tf *tableFiles) (*tableStorage, error) {
	ts := &tableStorage{
		params:   params,
		tf:       tf,
		readers:  make(map[tableID]*TableReader),
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
			fmt.Println("table", reader.BlockRange(), "root", common.Hash(reader.TableRoot))
			ts.readers[id] = reader
		case fnWriteState:
			writer, err := newTableWriter(params, tf, params.tableName(id), true, 0, id.level == 0)
			if err != nil {
				log.Error("Invalid partial index table file", "name", params.tableName(id), "error", err)
				tf.deleteFile(name)
				continue loop
			}
			ts.writers[id] = writer
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
	for id := range ts.readers {
		ts.complete.add(id)
		ts.preInit.add(id)
	}
	for id := range ts.writers {
		ts.partial.add(id)
		ts.preInit.add(id)
	}
	if ts.preInit.isEmpty() {
		ts.preInit = nil
	}
	return ts, nil
}

func (ts *tableStorage) tables() (tableSet, tableSet, bool) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if ts.complete.count() != uint64(len(ts.readers)) {
		panic("ts.complete.count() != len(ts.readers)")
	}
	if ts.partial.count() != uint64(len(ts.writers)) {
		fmt.Println("partial:", ts.partial)
		fmt.Print("writers: ")
		for id := range ts.writers {
			fmt.Print(id, " ")
		}
		fmt.Println()
		panic("ts.partial.count() != len(ts.writers)")
	}
	complete := make(tableSet, len(ts.complete))
	for i, rs := range ts.complete {
		complete[i] = slices.Clone(rs)
	}
	partial := make(tableSet, len(ts.partial))
	for i, rs := range ts.partial {
		partial[i] = slices.Clone(rs)
	}
	return complete, partial, ts.preInit != nil
}

func (ts *tableStorage) getTableReader(id tableID) (*TableReader, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if tr, ok := ts.readers[id]; ok {
		return tr, nil
	}
	return nil, errTableNotFound
}

func (ts *tableStorage) waitForTableReader(id tableID) (*TableReader, error) {
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

func (ts *tableStorage) getRangeReaders(blockRange common.Range[uint64]) (readers []*TableReader) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	rs := common.SingleRangeSet[uint64](blockRange)
	for level := len(ts.params.tableLevels) - 1; level >= 0; level-- {
		avail := ts.complete[level].Intersection(shiftRangeSetLevel(rs, ts.params.tableLevels[0], ts.params.tableLevels[level], true))
		for index := range avail.Iter() {
			id := tableID{level: level, index: index}
			if tr, ok := ts.readers[id]; ok {
				readers = append(readers, tr)
			} else {
				panic("table reader not found for complete table")
			}
		}
		rs = rs.Difference(shiftRangeSetLevel(avail, ts.params.tableLevels[level], ts.params.tableLevels[0], false))
	}
	return
}

func (ts *tableStorage) addNewTableWriter(id tableID, entryCount uint64) (*tableWriter, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if ts.writers[id] != nil {
		return nil, errors.New("table already exists")
	}
	tw, err := newTableWriter(ts.params, ts.tf, ts.params.tableName(id), false, entryCount, id.level == 0)
	if err != nil {
		return nil, err
	}
	ts.writers[id] = tw
	//fmt.Println("+++ add tw 1", id)
	ts.partial.add(id)
	//fmt.Println("+++ add partial 1", id)
	return tw, nil
}

func (ts *tableStorage) getTableWriter(id tableID) (*tableWriter, error) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	tw, ok := ts.writers[id]
	if !ok {
		return nil, errTableNotFound
	}
	return tw, nil
}

func (ts *tableStorage) finalizeTableWriter(id tableID) error {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	tw, ok := ts.writers[id]
	if !ok {
		return errTableNotFound
	}
	//fmt.Println("+++ finalizeTableWriter", tw.name)
	if tw.getPhase() != wpFinalized {
		return errors.New("table writer not finalized yet")
	}
	delete(ts.writers, id)
	//fmt.Println("+++ delete tw 2", id)
	ts.partial.remove(id)
	//fmt.Println("+++ delete partial 2", id)
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
	fmt.Println("deliverInitBlockHash", number)
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
				if tr.Meta.LastBlockNumber != number || !ts.preInit.includes(id) {
					continue
				}
				if tr.Meta.LastBlockHash == hash {
					if tr.Meta.LastBlockNumber >= tr.Meta.BlockCount {
						hashes[tr.Meta.LastBlockNumber-tr.Meta.BlockCount] = tr.Meta.ParentHash
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
					delete(ts.writers, id)
					//fmt.Println("+++ delete tw 4", id)
					ts.partial.remove(id)
					//fmt.Println("+++ delete partial 4", id)
					tw.delete()
				}
				ts.preInit.remove(id)
			}
			delete(hashes, number)
		}
	}
	if ts.preInit.isEmpty() {
		ts.preInit = nil
	}
	fmt.Println(" done")
	return ts.preInit == nil
}

func (ts *tableStorage) close() {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	for _, ch := range ts.triggers {
		close(ch)
	}
	for _, tw := range ts.writers {
		if err := tw.close(); err != nil {
			log.Error("Failed to close index table writer", "error", err)
		}
	}
	fmt.Println("closed table writers")
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
