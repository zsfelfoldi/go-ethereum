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
	"math/rand"
	"os"
	"sort"
	"testing"
)

func testValue(blockNumber uint64, txIndex, logIndex, entryType uint32) (result [32]byte) {
	var data [20]byte
	binary.LittleEndian.PutUint64(data[0:8], blockNumber)
	binary.LittleEndian.PutUint32(data[8:12], txIndex)
	binary.LittleEndian.PutUint32(data[12:16], logIndex)
	binary.LittleEndian.PutUint32(data[16:20], entryType)
	hasher := sha256.New()
	hasher.Write(data[:])
	hasher.Sum(result[:0])
	return
}

func TestIndexTable(t *testing.T) {
	var ies indexEntries
	for blockNumber := range uint64(1000) {
		for txIndex := range uint32(10) {
			for logIndex := range uint32(10) {
				for entryType := range uint32(10) {
					ies = append(ies, indexEntry{
						entryType:   entryType,
						indexValue:  testValue(blockNumber, txIndex, logIndex, entryType),
						blockNumber: blockNumber,
						txIndex:     txIndex,
						logIndex:    logIndex,
					})
				}
			}
		}
	}
	sort.Slice(ies, func(i, j int) bool {
		return ies[i].compare(&ies[j]) < 0
	})
	tw, _ := newTableWriter("testTable", uint64(len(ies)))
	for i := range ies {
		tw.addEntry(&ies[i])
	}
	tw.finished()
	f, _ := os.Open("testTable")
	tr, _ := newTableReader(f)
	/*for pos := range uint64(10000) {
		ie, _ := tr.getEntry(pos * 100)
		fmt.Println(pos*100, *ie)
	}*/
	for range 10000 {
		blockNumber := uint64(rand.Intn(1000))
		txIndex := uint32(rand.Intn(10))
		logIndex := uint32(rand.Intn(10))
		entryType := uint32(rand.Intn(10))
		target := &indexEntry{
			entryType:  entryType,
			indexValue: testValue(blockNumber, txIndex, logIndex, entryType),
		}
		pos, _, _ := tr.seekEntry(target)
		ie, _ := tr.getEntry(pos)
		if ie.blockNumber != blockNumber || ie.txIndex != txIndex || ie.logIndex != logIndex {
			t.Fatalf("Could not find entry position by type/value (expected: %d %d %d, got: %d %d %d)", blockNumber, txIndex, logIndex, ie.blockNumber, ie.txIndex, ie.logIndex)
		}
		_, found, _ := tr.seekEntry(ie)
		if !found {
			t.Fatalf("Could not find exact entry by type/value/position (%d %d %d)", ie.blockNumber, ie.txIndex, ie.logIndex)
		}
	}
	//fmt.Println("target", *target)
	//fmt.Println(pos, found, *ie)
}
