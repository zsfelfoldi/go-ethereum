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
	"io"
	"os"

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

func (ie *indexEntry) compare(i2 *indexEntry) int {
	if ie.entryType != i2.entryType {
		if i.entryType < i2.entryType {
			return -1
		}
		return 1
	}
	if c := bytes.Compare(ie.indexValue[:], i2.indexValue[:]); c != 0 {
		return c
	}
	if ie.blockNumber != i2.blockNumber {
		if ie.blockNumber < i2.blockNumber {
			return -1
		}
		return 1

	}
	if ie.txIndex != i2.txIndex {
		if ie.txIndex < i2.txIndex {
			return -1
		}
		return 1
	}
	if ie.logIndex > i2.logIndex {
		return 1
	}
	if ie.logIndex < i2.logIndex {
		return -1
	}
	return 0
}

type entriesForStorage []entryForStorage

type entryForStorage struct {
	TypeAndValueDiff []byte
	BlockDiff        uint64
	TxDiff, LogDiff  uint32
}

func (ies indexEntries) find(ie *indexEntry) (int, bool) {
	min, max := 0, len(ies)
	for min < max {
		mid := (min + max) / 2
		switch ies[mid].compare(ie) {
		case -1:
			min = mid + 1
		case 0:
			return mid, true
		case 1:
			max = mid
		}
	}
	return min, false
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
	BoundaryEntries entriesForStorage // N-1
	BoundaryFilePos []uint64          // N+1
	Hashes          []common.Hash     //N
}

type subtreeChunk struct {
	boundaryEntries indexEntries  // N-1
	boundaryFilePos []uint64      // N+1
	hashes          []common.Hash //N
}

type tableReader struct {
	reader            io.ReadSeeker
	entryChunkCache   *lru.Cache[uint64, indexEntries]
	subtreeChunkCache *lru.Cache[subtreePos, *subtreeChunk]
	count, filePos    uint64
	levelPointers     []uint64
	topLevel          uint
	tableRoot         common.Hash
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

func (tr *tableReader) newTableReader(reader io.ReadSeeker) (*tableReader, error) {
	pos, err := reader.Seek(-1, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	var headerSize [1]byte
	br, err := reader.Read(headerSize[:])
	if err != nil {
		return nil, err
	}
	if br != 1 {
		return nil, errors.New("unexpected end of file")
	}
	pos, err := reader.Seek(-1-int64(headerSize[0]), io.SeekEnd)

	tr := &tableReader{
		reader:            reader,
		entryChunkCache:   lru.NewCache[uint64, indexEntries](entryCacheSize),
		subtreeChunkCache: lru.NewCache[subtreePos, *subtreeChunk](subtreeCacheSize),
		count:             binary.LittleEndian.Uint64(end[8:]),
		rootStart:         binary.LittleEndian.Uint64(end[:8]),
		rootStop:          pos,
		filePos:           pos + 16,
	}
	c := (tr.count + entryChunkSize - 1) / entryChunkSize
	tr.topLevel = 1
	for c > 0 {
		c = (c + subtreeChunkSize - 1) / subtreeChunkSize
		tr.topLevel++
	}
	return tr
}

func (tr *tableReader) getSubtreeChunk(level uint, index uint64) (*subtreeChunk, error) {
	sp := subtreePos{level, index}
	if sc, ok := tr.subtreeChunkCache.Get(sp); ok {
		return sc, nil
	}
	var start, stop uint64
	if level == 0 {
		start, stop = tr.rootStart, tr.rootStop
	} else {
		sc, err := tr.getSubtreeChunk(level-1, index/subtreeChunkSize)
		if err != nil {
			return nil, err
		}
		i := index & subtreeChunkSize
		start, stop = sc.boundaryFilePos[i], sc.boundaryFilePos[i+1]
	}
	if tr.filePos != start {
		tr.reader.Seek(int64(start), io.SeekStart)
	}
	enc := make([]byte, stop-start)
	br, err := reader.Read(enc)
	if err != nil {
		tr.filePos = math.MaxUint64
		return nil, err
	}
	if br != len(enc) {
		tr.filePos = math.MaxUint64
		return nil, errors.New("unexpected end of file")
	}
	tr.filePos = stop
	var ss subtreesForStorage
	if err := rlp.DecodeBytes(enc, &ss); err != nil {
		return nil, err
	}
	sc := &subtreeChunk{
		boundaryEntries: ss.BoundaryEntries.toEntries(),
		boundaryFilePos: ss.BoundaryFilePos,
		hashes:          ss.Hashes,
	}
	tr.subtreeChunkCache.Add(sp, sc)
	return sc, nil
}

func (tr *tableReader) getEntryChunk(index uint64) (indexEntries, error) {
	if ec, ok := tr.entryChunkCache.Get(index); ok {
		return ec, nil
	}
	sc, err := tr.getSubtreeChunk(tr.topLevel-1, index/entryChunkSize)
	if err != nil {
		return nil, err
	}
	i := index & entryChunkSize
	start, stop := sc.boundaryFilePos[i], sc.boundaryFilePos[i+1]
	if tr.filePos != start {
		tr.reader.Seek(int64(start), io.SeekStart)
	}
	enc := make([]byte, stop-start)
	br, err := reader.Read(enc)
	if err != nil {
		tr.filePos = math.MaxUint64
		return nil, err
	}
	if br != len(enc) {
		tr.filePos = math.MaxUint64
		return nil, errors.New("unexpected end of file")
	}
	tr.filePos = stop
	var ess entriesForStorage
	if err := rlp.DecodeBytes(enc, &ess); err != nil {
		return nil, err
	}
	ec := ess.toEntries()
	tr.entryChunkCache.Add(index, ec)
	return ec, nil
}

func (tr *tableReader) getEntry(index uint64) (*indexEntry, error) {
	ec, err := tr.getEntryChunk(index / entryChunkSize)
	if err != nil {
		return nil, err
	}
	return ec[index%entryChunkSize], nil
}

func (tr *tableReader) seekEntry(target *indexEntry) (uint64, bool, error) {
	var (
		chunkLevel uint
		chunkIndex uint64
	)
	for chunkLevel < tr.topLevel {
		sc, err := tr.getSubtreeChunk(chunkLevel, chunkIndex)
		if err != nil {
			return false, err
		}
		subIndex, _ := sc.boundaryEntries.find(target)
		chunkLevel++
		chunkIndex = chunkIndex*subtreeChunkSize + subIndex
	}
	ec, err := tr.getEntryChunk(chunkIndex)
	if err != nil {
		return false, err
	}
	subIndex, found := ec.find(target)
	return chunkIndex*entryChunkSize + subIndex, found
}

type tableWriter struct {
	lastEntryChunk    indexEntries
	lastSubtreeChunks []*subtreeChunk
	files             []*os.File
	writers           []io.Writer
	writePointers     []uint64
	fileName          string
	topLevel          uint
	count, nextEntry  uint64
}

func newTableWriter(fileName string, count uint64) (*tableWriter, error) {
	c := (count + entryChunkSize - 1) / entryChunkSize
	topLevel := uint(1)
	for c > 0 {
		c = (c + subtreeChunkSize - 1) / subtreeChunkSize
		topLevel++
	}
	tw := &tableWriter{
		lastSubtreeChunks: make([]*subtreeChunk, topLevel),
		files:             make([]*os.File, topLevel+1),
		writers:           make([]io.Writer, topLevel+1),
		writePointers:     make([]uint64, topLevel+1),
		fileName:          fileName,
		topLevel:          topLevel,
		count:             count,
	}
	for i := range tw.files {
		f, err := os.Create(tw.getFileName(i))
		if err != nil {
			return nil, err
		}
		tw.files[i] = f
		tw.writers[i] = bufio.NewWriter(f)
	}
}

func (tw *tableWriter) getFileName(level uint) string {
	if level == tw.topLevel {
		return tw.fileName
	}
	return fmt.Sprintf("%s.temp.%d", tw.fileName, level)
}

func (tw *tableWriter) addEntry(ie *indexEntry) error {
	if tw.nextEntry >= tw.count {
		panic("too many entries")
	}
	tw.lastEntryChunk = append(tw.lastEntryChunk, *ie)
	tw.nextEntry++
	if len(tw.lastEntryChunk) == entryChunkSize || tw.nextEntry == tw.count {
		ess := tw.lastEntryChunk.toStorage()
		enc, err := rlp.EncodeToBytes(&ess)
		if err != nil {
			return err
		}
		bw, err := tw.tableWriter.Write(enc)
		if err != nil {
			return err
		}
		if bw != len(enc) {
			return errors.New("error writing table chunk")
		}
		beforePos := tw.twPosition
		tw.twPosition += uint64(bw)
		if err := tw.addSubtreeEntry(tw.topLevel-1, ie, beforePos, tw.twPosition); err != nil {
			return err
		}
		tw.lastEntryChunk = nil
	}
}

func (tw *tableWriter) addSubtreeEntry(level uint, boundaryEntry *indexEntry, beforePos, afterPos uint64, hash common.Hash) error {
	sc := tw.lastSubtreeChunks[level]
	l := len(sc.hashes)
	if l > 0 {
		sc.boundaryEntries = append(sc.boundaryEntries, *boundaryEntry)
	}
	if l == 0 {
		sc.boundaryFilePos = []uint64{beforePos}
	}
	sc.boundaryFilePos = append(sc.boundaryFilePos, afterPos)
	sc.hashes = append(sc.hashes, hash)
	l++
	if l == subtreeChunkSize || tw.nextEntry == tw.count {
		ss := subtreesForStorage{
			BoundaryEntries: sc.boundaryEntries.toStorage(),
			BoundaryFilePos: sc.boundaryFilePos,
			Hashes:          sc.hashes,
		}
		enc, err := rlp.EncodeToBytes(&ss)
		if err != nil {
			return err
		}
		bw, err := tw.subtreeWriters[level].Write(enc)
		if err != nil {
			return err
		}
		if bw != len(enc) {
			return errors.New("error writing subtree chunk")
		}
		beforePos := tw.swPositions[level]
		tw.swPositions[level] += uint64(bw)
		if level > 0 {
			if err := tw.addSubtreeEntry(level-1, ie, beforePos, tw.swPositions[level]); err != nil {
				return err
			}
		}
		tw.lastSubtreeChunks[level] = &subtreeChunk{}
	}
}

type tableHeader struct {
	LevelPointers []uint64
	Count         uint64
	TableRoot     common.Hash
}

func (tw *tableWriter) finished() error {
	if tw.nextEntry != tw.count {
		panic("not enough entries")
	}
	header := tableHeader{
		LevelPointers: make([]uint64, tw.topLevel+1),
		EntryCount:    tw.entryCount,
		TableRoot:     todo,
	}
	wp := tw.writePointers[tw.topLevel]
	header.LevelPointers[tw.topLevel] = wp
	for i := int(tw.topLevel - 1); i >= 0; i-- {
		tw.writers[i].Flush() //TODO error handling (every Flush, Seek, Close, etc)
		tw.files[i].Seek(0, io.SeekStart)
		bw, err := io.Copy(tw.writers[tw.topLevel], bufio.NewReader(tw.files[i]))
		if err != nil {
			return err
		}
		wp += uint64(bw)
		header.LevelPointers[i] = wp
		tw.files[i].Close()
		if err := os.Remove(tw.getFileName(i)); err != nil {
			return err
		}
	}
	enc, err := rlp.EncodeToBytes(&header)
	if err != nil {
		return err //TODO always guarantee file close
	}
	enc = append(enc, byte(len(enc)))
	bw, err := tw.writers[tw.topLevel].Write(enc)
	if err != nil {
		return err
	}
	if bw != len(enc) {
		return errors.New("error writing table header")
	}
	tw.writers[tw.topLevel].Flush()
	tw.files[tw.topLevel].Close()
	return nil
}
