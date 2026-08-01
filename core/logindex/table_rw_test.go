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
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func testValue(blockNumber uint64, txIndex, logIndex, EntryType uint32) (result [32]byte) {
	var data [20]byte
	binary.LittleEndian.PutUint64(data[0:8], blockNumber)
	binary.LittleEndian.PutUint32(data[8:12], txIndex)
	binary.LittleEndian.PutUint32(data[12:16], logIndex)
	binary.LittleEndian.PutUint32(data[16:20], EntryType)
	hasher := sha256.New()
	hasher.Write(data[:])
	hasher.Sum(result[:0])
	return
}

func makeIndexEntries(blockCount uint64) IndexEntries {
	var ies IndexEntries
	for blockNumber := range blockCount {
		for txIndex := range uint32(10) {
			for logIndex := range uint32(10) {
				for entryType := range uint32(10) {
					ies = append(ies, IndexEntry{
						IndexValue: IndexValue{
							EntryType: entryType,
							Value:     testValue(blockNumber, txIndex, logIndex, entryType),
						},
						IndexPosition: IndexPosition{
							BlockNumber: blockNumber,
							TxIndex:     txIndex,
							LogIndex:    logIndex,
						},
					})
				}
			}
		}
	}
	sort.Slice(ies, func(i, j int) bool {
		return ies[i].Compare(&ies[j]) < 0
	})
	return ies
}

func TestTableRW(t *testing.T) {
	testTableRW(t, 1000, 2000000000)
}

func testTableRW(t *testing.T, blockCount uint64, maxFileSize int64) {
	path, _ := os.MkdirTemp("", "index_table_test")
	defer os.RemoveAll(path)

	files, err := newTableFiles(path, maxFileSize, 4)
	if err != nil {
		t.Fatalf("Error during newTableFiles: %v", err)
	}
	defer func() {
		files.close()
	}()

	ies := makeIndexEntries(blockCount)
	tw, err := newTableWriter(DefaultParams, files, "test_table", false, uint64(len(ies)), false)
	if err != nil {
		t.Fatalf("Error during newTableWriter: %v", err)
	}
	err = tw.open()
	if err != nil {
		t.Fatalf("Error during tableWriter.open: %v", err)
	}
	tw.setMeta(TableMeta{LastBlockHash: common.Hash{1}})
	for i := range ies {
		err := tw.addEntry(&ies[i])
		if err != nil {
			t.Fatalf("Error during tableWriter.addEntry: %v", err)
		}
		lastEntry, nextEntry := tw.lastAndNextEntry()
		expLastEntry := &ies[i]
		if i == len(ies)-1 {
			expLastEntry = nil
		}
		if (lastEntry == nil) != (expLastEntry == nil) || (lastEntry != nil && expLastEntry != nil && *lastEntry != *expLastEntry) {
			t.Fatalf("Invalid lastEntry from tableWriter.lastAndNextEntry after adding entry %d (expected %v, got %v)", i, expLastEntry, lastEntry)
		}
		if nextEntry != uint64(i+1) {
			t.Fatalf("Invalid nextEntry from tableWriter.lastAndNextEntry after adding entry %d (expected %d, got %d)", i, i+1, nextEntry)
		}
		if rand.Intn(len(ies)) < 100 {
			err := tw.close()
			if err != nil {
				t.Fatalf("Error during tableWriter.close: %v", err)
			}
			if /*restart && */ rand.Intn(2) == 0 {
				files.close()
				files, err = newTableFiles(path, maxFileSize, 4)
				if err != nil {
					t.Fatalf("Error during newTableFiles: %v", err)
				}
				tw, err = newTableWriter(DefaultParams, files, "test_table", true, uint64(len(ies)), false)
				if err != nil {
					t.Fatalf("Error during newTableWriter: %v", err)
				}
				fmt.Println("/// restart", tw.entryCount, len(ies))
			}
			err = tw.open()
			if err != nil {
				t.Fatalf("Error during tableWriter.open: %v", err)
			}
		}
	}
	for {
		done, err := tw.finalize()
		if err != nil {
			t.Fatalf("Error during tableWriter.finalize: %v", err)
		}
		if done {
			break
		}
	}
	if tw.getPhase() != wpFinalized {
		t.Fatalf("Invalid table writer phase after finalization (expected %d, got %d)", wpFinalized, tw.getPhase())
	}
	tr, err := newTableReader(DefaultParams, files, "test_table")
	if err != nil {
		t.Fatalf("Error during newTableReader: %v", err)
	}
	for range 10000 {
		blockNumber := uint64(rand.Int63n(int64(blockCount)))
		txIndex := uint32(rand.Intn(10))
		logIndex := uint32(rand.Intn(10))
		entryType := uint32(rand.Intn(10))
		target := &IndexEntry{
			IndexValue: IndexValue{
				EntryType: entryType,
				Value:     testValue(blockNumber, txIndex, logIndex, entryType),
			},
		}
		pos, _, err := tr.SeekEntry(target)
		if err != nil {
			t.Fatalf("Error during tableReader.SeekEntry: %v", err)
		}
		ie, err := tr.GetEntry(pos)
		if err != nil {
			t.Fatalf("Error during tableReader.GetEntry: %v", err)
		}
		if ie.BlockNumber != blockNumber || ie.TxIndex != txIndex || ie.LogIndex != logIndex {
			t.Fatalf("Could not find entry position by type/value (expected: %d %d %d, got: %d %d %d)", blockNumber, txIndex, logIndex, ie.BlockNumber, ie.TxIndex, ie.LogIndex)
		}
		_, found, err := tr.SeekEntry(ie)
		if err != nil {
			t.Fatalf("Error during tableReader.SeekEntry: %v", err)
		}
		if !found {
			t.Fatalf("Could not find exact entry by type/value/position (%d %d %d)", ie.BlockNumber, ie.TxIndex, ie.LogIndex)
		}
	}
}
