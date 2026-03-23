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

func (ix *Indexer) mergeLoop() {
	var (
		source []tableID
		target tableID
	)
	for {
		select {
		case <-ix.updateMergeCh:
		mergeLoop:
			var closed bool
			for !closed {
				ix.lock.Lock()
				source = ix.mergeSource
				target = ix.mergeTarget
				ix.lock.Unlock()
				if source == nil {
					break mergeLoop
				}
				done, err := ix.mergeTable(source, target, func() bool {
					select {
					case <-ix.updateMergeCh:
						ix.lock.Lock()
						changed := source != ix.mergeSource || target != ix.mergeTarget
						ix.lock.Unlock()
						return changed
					case <-ix.closeMergeCh:
						closed = true
						return true
					default:
						return false
					}
				})
				//TODO delete source/target on err, handle next target on done / target update
			}
		case <-ix.closeMergeCh:
			ix.mergeWg.Done()
			return
		}
	}
}

const (
	mpNone = iota // new table merge, no partial state
	mpWriteEntries
	mpTempCopy
	mpFinalize
)

type mergeState struct {
	Phase                 uint
	SourcePtrs            []uint64
	EntryCount            uint64
	TempLevels, CopyLevel uint
	CopyPointer           uint64
}

func (ix *Indexer) mergeTable(source []tableID, target tableID, stopFn func() bool) (bool, error) {
	// read partial merge state if present
	var ms mergeState
	tsID := tablePartID{tableID: target, state: tsTempState}
	if ix.storage.exists(tsID) {
		tsReader, err := ix.storage.getReadSeeker(tsID)
		if err != nil {
			return false, fmt.Errorf("could not open reader for index table temp state: %v", err)
		} else {
			if err := rlp.Decode(tsReader, &mergeState); err != nil {
				return false, fmt.Errorf("could not decode index table temp state: %v", err)
			}
			if err := ix.storage.release(tsID); err != nil {
				return false, fmt.Errorf("could not release index table temp state reader: %v", err)
			}
		}
	}
	if ms.Phase == mpNone {
		ms.SourcePtrs = make([]uint64, len(source))
		ms.Phase = mpWriteEntries
	}
	var topLevelWriter io.Writer
	// start/resume writing merged entries if not finished yet
	if ms.Phase == mpWriteEntries {
		if len(ms.SourcePtrs) != len(source) {
			return false, fmt.Errorf("partial index table merge with incorrect source count found")
		}
		if done, err := mergeWritePhase(ms, source, target, stopFn); !done {
			return done, err
		}
		if ms.TempLevels >= 2 {
			ms.Phase = mpTempCopy
			ms.CopyLevel = ms.TempLevels - 2
		} else {
			ms.Phase = mpFinalize
		}
	}
	if ms.Phase == mpTempCopy {

	}
	if err := ix.storage.move(tablePartID{tableID: target, state: tsTempLevel, tempLevel: ms.TempLevels - 1}, tablePartID{tableID: target, state: tsFinal}); err != nil {
		return false, fmt.Errorf("could not rename finalized index table: %v", err)
	}
	return true, nil
}

func (ix *Indexer) mergeWritePhase(ms *mergeState, source []tableID, target tableID, stopFn func() bool) (done bool, finalErr error) {
	sourceReaders := make([]*tableReader, len(source))
	ms.EntryCount = 0
	var openReaders, openWriters int
	defer func() {
		for i := range openReaders {
			if err := ix.storage.release(tablePartID{tableID: source[i], state: tsFinal}); err != nil {
				done, finalErr = false, fmt.Errorf("could not release merge source index table reader: %v", err)
			}
		}
		for i := range openWriters {
			if err := ix.storage.release(tablePartID{tableID: target, state: tsTempLevel, tempLevel: i}); err != nil {
				done, finalErr = false, fmt.Errorf("could not release merge targer index table writer: %v", err)
			}
		}
	}()
	for i, s := range source {
		sourceID := tablePartID{tableID: s}
		sr, size, err := ix.storage.getReaderAt(sourceID)
		if err == nil {
			sourceReaders[i], err = newTableReader(sr, size)
		}
		if err != nil {
			return false, fmt.Errorf("could not open reader for merge source index table: %v", err)
		}
		openReaders++
		ms.EntryCount += sourceReaders[i].entryCount
	}
	targetFormat := newTableFormat(ms.EntryCount)
	ms.TempLevels = targetFormat.memoryStorage + targetFormat.fileStorage
	tw := make([]*bufio.Writer, ms.TempLevels)
	for i := range tw {
		w, err := ix.storage.getAppendWriter(tablePartID{tableID: target, state: tsTempLevel, tempLevel: i}, i >= targetFormat.memoryStorage)
		if err != nil {
			return false, fmt.Errorf("could not create merge target index table writer: %v", err)
		}
		tw[i] = bufio.NewWriter(w)
		openWriters++
	}
	targetWriter, err := newTableWriter(tw, ms.EntryCount)
	if err != nil {
		return false, fmt.Errorf("could not create merge target index table writer: %v", err)
	}
	var cbCounter int
	for {
		var (
			bestEntry  *indexEntry
			bestReader int
		)
		for i, tr := range sourceReaders {
			if ms.SourcePtrs[i] < tr.entryCount {
				ie, err := tr.getEntry(ms.SourcePtrs[i])
				if err != nil {
					return err
				}
				if bestEntry == nil || ie.compare(bestEntry) < 0 {
					bestEntry, bestReader = ie, i
				}
			}
		}
		if bestEntry == nil {
			break
		}
		if err := targetWriter.addEntry(bestEntry); err != nil {
			return err
		}
		ms.SourcePtrs[bestReader]++
		cbCounter++
		if cbCounter >= 10000 {
			if stopFn() {
				return false, nil
			}
			cbCounter = 0
		}
	}
	for _, w := range tw {
		w.Flush()
	}
	return true, nil
}

func (ix *Indexer) mergeCopyPhase(ms *mergeState, target tableID, stopFn func() bool) (done bool, finalErr error) {
	writer, err := ix.storage.getAppendWriter(tablePartID{tableID: target, state: tsTempLevel, tempLevel: ms.TempLevels - 1}, false)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := ix.storage.release(tablePartID{tableID: target, state: tsTempLevel, tempLevel: ms.TempLevels - 1}); err != nil {
			done, finalErr = false, fmt.Errorf("could not release table after temp level copy write: %v", err)
		}
	}()

	writer := bufio.NewWriter(writer)
	buffer := make([]byte, 0x40000)
	for {
		reader, err := ix.storage.getReadSeeker(tablePartID{tableID: target, state: tsTempLevel, tempLevel: ms.CopyLevel})
		reader.Seek(int64(ms.CopyPointer), io.SeekStart)
	levelCopyLoop:
		for {
			n, err := reader.Read(buffer)
			if err != nil {
				return false, fmt.Errorf("error reading temp level copy buffer: %v", err)
			}
			_, err := writer.Write(buffer[:n])
			if err != nil {
				return false, fmt.Errorf("error writing temp level copy buffer: %v", err)
			}
			ms.CopyPointer += uint64(n)
			if stopFn() {
				return false, nil
			}
			if n < len(buffer) {
				err := ix.storage.release(tablePartID{tableID: target, state: tsTempLevel, tempLevel: ms.CopyLevel})
				if err != nil {
					return false, fmt.Errorf("could not release table after temp level copy read: %v", err)
				}
				if ms.CopyLevel == 0 {
					writer.Flush()
					return true, nil
				}
				ms.CopyLevel--
				ms.CopyPointer = 0
				break levelCopyLoop
			}
		}
	}
}
