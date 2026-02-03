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
	"bytes"
	"crypto/sha256"
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common/lru"
)

type indexEntry struct {
	indexValue                   [32]byte
	blockNumber                  uint64
	txIndex, logIndex, entryType uint32
}

type indexEntries []indexEntry

func (ie *indexEntry) hash() (result merkle.Value) {
	var enc [64]byte
	binary.BigEndian.PutUint64(enc[0:8], uint64(ie.entryType))
	copy(enc[8:40], ie.indexValue[:])
	binary.BigEndian.PutUint64(enc[40:48], ie.blockNumber)
	binary.BigEndian.PutUint64(enc[48:56], uint64(ie.txIndex))
	binary.BigEndian.PutUint64(enc[56:64], uint64(ie.logIndex))
	hasher := sha256.New()
	hasher.Write(enc[:])
	hasher.Sum(result[:0])
	return
}

func (ie *indexEntry) lessThan(i2 *indexEntry) bool {
	if ie.entryType != i2.entryType {
		return i.entryType < i2.entryType
	}
	if c := bytes.Compare(ie.indexValue[:], i2.indexValue[:]); c != 0 {
		return c < 0
	}
	if ie.blockNumber != i2.blockNumber {
		return ie.blockNumber < i2.blockNumber
	}
	if ie.txIndex != i2.txIndex {
		return ie.txIndex < i2.txIndex
	}
	return ie.logIndex < i2.logIndex
}

type entriesForStorage []entryForStorage

type entryForStorage struct {
	TypeAndValueDiff []byte
	BlockDiff        uint64
	TxDiff, LogDiff  uint32
}

func (ies indexEntries) find(ie *indexEntry) (int, bool) {
	if len(ies) == 0 {
		return 0, false
	}
	min, max := 0, len(ies)-1
	for 
}

func (ies indexEntries) toStorage() entriesForStorage {
	ess := make(entriesForStorage, 0, len(ies))
	var (
		typeAndValue, lastTypeAndValue [36]byte
		lastBlockNumber                uint64
		lastTxIndex, lastLogIndex      uint32
	)
	for i, ie := range ies {
		binary.BigEndian.PutUint32(typeAndValue[0:4], ie.entryType)
		copy(typeAndValue[4:], ie.indexValue[:])
		var es entryForStorage
		p := 0
		for ; p < 36; p++ {
			if typeAndValue[p] != lastTypeAndValue[p] {
				break
			}
		}
		if p < 36 {
			es.TypeAndValueDiff = slices.Clone(typeAndValue[p:])
			lastTypeAndValue = typeAndValue
			lastBlockNumber, lastTxIndex, lastLogIndex = 0, 0, 0
		}
		if ie.blockNumber != lastBlockNumber {
			es.BlockDiff = ie.blockNumber - lastBlockNumber
			lastBlockNumber = ie.blockNumber
			lastTxIndex, lastLogIndex = 0, 0
		}
		if ie.txIndex != lastTxIndex {
			es.TxDiff = ie.txIndex - lastTxIndex
			lastTxIndex = ie.txIndex
			lastLogIndex = 0
		}
		es.LogDiff = ie.logIndex - lastLogIndex
		lastLogIndex = ie.logIndex
		ess = append(ess, es)
	}
	return ess
}

func (ess entriesForStorage) toEntries() indexEntries {
	ies := make(indexEntries, 0, len(ess))
	var (
		lastTypeAndValue          [36]byte
		lastBlockNumber           uint64
		lastTxIndex, lastLogIndex uint32
	)
	for i, es := range ess {
		if len(es.TypeAndValueDiff) != 0 {
			copy(lastTypeAndValue[36-len(es.TypeAndValueDiff):], es.TypeAndValueDiff)
			lastBlockNumber, lastTxIndex, lastLogIndex = 0, 0, 0
		}
		if es.BlockDiff != 0 {
			lastBlockNumber += es.BlockDiff
			lastTxIndex, lastLogIndex = 0, 0
		}
		if es.TxDiff != 0 {
			lastTxIndex += es.TxDiff
			lastLogIndex = 0
		}
		lastLogIndex += es.LogDiff
		ie := indexEntry{
			entryType:   binary.BigEndian.Uint32(lastTypeAndValue[:4]),
			blockNumber: lastBlockNumber,
			txIndex:     lastTxIndex,
			logIndex:    lastLogIndex,
		}
		copy(ie.indexValue[:], lastTypeAndValue[4:])
		ies = append(ies, ie)
	}
	return ies
}

type subtreesForStorage struct {
	BoundEntries entriesForStorage // N+1
	BoundFilePos []uint64          // N+1
	Hashes       []common.Hash     //N
}

type subtreeChunk struct {
	boundEntries indexEntries  // N+1
	boundFilePos []uint64      // N+1
	hashes       []common.Hash //N
}

type chunkReader struct {
	reader                              io.ReadSeeker
	entryChunkCache                     *lru.Cache[uint64, indexEntries]
	subtreeChunkCache                   *lru.Cache[subtreePos, *subtreeChunk]
	count, rootStart, rootStop, filePos uint64
	topLevel                            uint
}

type subtreePos struct {
	level uint
	index uint64
}

const (
	entryChunkSize   = 128
	subtreeChunkSize = 64
	entryCacheSize   = 100
	subtreeCacheSize = 100
)

func (cr *chunkReader) newChunkReader(reader io.ReadSeeker) (*chunkReader, error) {
	pos, err := reader.Seek(-16, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	var end [16]byte
	br, err := reader.Read(end[:])
	if err != nil {
		return nil, err
	}
	if br != 16 {
		return nil, errors.New("unexpected end of file")
	}
	cr := &chunkReader{
		reader:            reader,
		entryChunkCache:   lru.NewCache[uint64, indexEntries](entryCacheSize),
		subtreeChunkCache: lru.NewCache[subtreePos, *subtreeChunk](subtreeCacheSize),
		count:             binary.LittleEndian.Uint64(end[8:]),
		rootStart:         binary.LittleEndian.Uint64(end[:8]),
		rootStop:          pos,
		filePos:           pos + 16,
	}
	c := (cr.count + entryChunkSize - 1) / entryChunkSize
	cr.topLevel = 1
	for c > 0 {
		c = (c + subtreeChunkSize - 1) / subtreeChunkSize
		cr.topLevel++
	}
	return cr
}

func (cr *chunkReader) getSubtreeChunk(level uint, index uint64) (*subtreeChunk, error) {
	sp := subtreePos{level, index}
	if sc, ok := cr.subtreeChunkCache.Get(sp); ok {
		return sc, nil
	}
	var start, stop uint64
	if level == 0 {
		start, stop = cr.rootStart, cr.rootStop
	} else {
		sc, err := cr.getSubtreeChunk(level-1, index/subtreeChunkSize)
		if err != nil {
			return nil, err
		}
		i := index & subtreeChunkSize
		start, stop = sc.boundFilePos[i], sc.boundFilePos[i+1]
	}
	if cr.filePos != start {
		cr.reader.Seek(int64(start), io.SeekStart)
	}
	enc := make([]byte, stop-start)
	br, err := reader.Read(enc)
	if err != nil {
		cr.filePos = math.MaxUint64
		return nil, err
	}
	if br != len(enc) {
		cr.filePos = math.MaxUint64
		return nil, errors.New("unexpected end of file")
	}
	cr.filePos = stop
	var ss subtreesForStorage
	if err := rlp.DecodeBytes(enc, &ss); err != nil {
		return nil, err
	}
	sc := &subtreeChunk{
		boundEntries: ss.BoundEntries.toEntries(),
		boundFilePos: ss.BoundFilePos,
		hashes:       ss.Hashes,
	}
	cr.subtreeChunkCache.Add(sp, sc)
	return sc, nil
}

func (cr *chunkReader) getEntryChunk(index uint64) (indexEntries, error) {
	if ec, ok := cr.entryChunkCache.Get(index); ok {
		return ec, nil
	}
	sc, err := cr.getSubtreeChunk(cr.topLevel-1, index/entryChunkSize)
	if err != nil {
		return nil, err
	}
	i := index & entryChunkSize
	start, stop := sc.boundFilePos[i], sc.boundFilePos[i+1]
	if cr.filePos != start {
		cr.reader.Seek(int64(start), io.SeekStart)
	}
	enc := make([]byte, stop-start)
	br, err := reader.Read(enc)
	if err != nil {
		cr.filePos = math.MaxUint64
		return nil, err
	}
	if br != len(enc) {
		cr.filePos = math.MaxUint64
		return nil, errors.New("unexpected end of file")
	}
	cr.filePos = stop
	var ess entriesForStorage
	if err := rlp.DecodeBytes(enc, &ess); err != nil {
		return nil, err
	}
	ec := ess.toEntries()
	cr.entryChunkCache.Add(index, ec)
	return ec, nil
}

type tableReader struct {
	cr        *chunkReader
	nextEntry uint64
}

func newTableReader(cr *chunkReader) *tableReader {
	return &tableReader{
		cr: cr,
	}
}

func (tr *tableReader) seekEntry(target *indexEntry) (bool, error) {
	sc, err := tr.cr.getSubtreeChunk(0, 0)
	if err != nil {
		return false, err
	}

}
