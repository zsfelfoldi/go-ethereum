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

const (
	entryChunkSize      = 128
	logEntryChunkSize   = 7 //TODO params
	subtreeChunkSize    = 64
	logSubtreeChunkSize = 6
	entryCacheSize      = 100
	subtreeCacheSize    = 100
)

func chunkHeights(entryCount uint64) []uint {
	totalHeight := uint(64 - bits.LeadingZeros64(max(entryCount, 1)-1))
	if totalHeight <= logEntryChunkSize {
		return []uint{0, totalHeight}
	}
	subtreesHeight := totalHeight - logEntryChunkSize
	subtreeLevels := (subtreesHeight + logSubtreeChunkSize - 1) / logSubtreeChunkSize
	heights := make([]uint, subtreeLevels+2)
	for i := range subtreeLevels + 1 {
		heights[i] = subtreesHeight - (subtreeLevels-i)*logSubtreeChunkSize
	}
	heights[subtreeLevels+1] = totalHeight
	return heights
}

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
		if ie.entryType < i2.entryType {
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
	for _, ie := range ies {
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

type entryChunk struct {
	entries indexEntries
	height  uint
	hashes  []merkle.Value // 2^(height+1)
	hasHash []bool         // 2^(height+1)
}

func (ec *entryChunk) getHash(gti uint64) (result merkle.Value) {
	if ec.hashes == nil {
		ec.hashes = make([]merkle.Value, 2<<ec.height)
		ec.hasHash = make([]bool, 2<<ec.height)
	}
	if ec.hasHash[gti] {
		return ec.hashes[gti]
	}
	gtiHeight := uint(63 - bits.LeadingZeros64(gti))
	if gtiHeight > ec.height {
		panic("invalid entry tree index")
	}
	if gti<<(ec.height-gtiHeight) >= uint64(1)<<ec.height+uint64(len(ec.entries)) {
		result = zeroValues[ec.height-gtiHeight]
	} else if gti >= uint64(1)<<ec.height {
		result = ec.entries[gti-uint64(1)<<ec.height].hash()
	} else {
		hasher := sha256.New()
		left := ec.getHash(gti * 2)
		right := ec.getHash(gti*2 + 1)
		hasher.Write(left[:])
		hasher.Write(right[:])
		hasher.Sum(result[:0])
	}
	ec.hashes[gti] = result
	ec.hasHash[gti] = true
	return
}

func (ess entriesForStorage) toEntries() indexEntries {
	ies := make(indexEntries, 0, len(ess))
	var (
		lastTypeAndValue          [36]byte
		lastBlockNumber           uint64
		lastTxIndex, lastLogIndex uint32
	)
	for _, es := range ess {
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
	Hashes          []merkle.Value    //N
}

type subtreeChunk struct {
	boundaryEntries         indexEntries // N-1
	boundaryFilePos         []uint64     // N+1
	height, above, branches uint
	hashes                  []merkle.Value // 2^(height+1)
	hasHash                 []bool         // 2^(height+1)
}

func (ss *subtreesForStorage) toSubtreeChunk(height, above uint) *subtreeChunk {
	sc := &subtreeChunk{
		boundaryEntries: ss.BoundaryEntries.toEntries(),
		boundaryFilePos: ss.BoundaryFilePos,
		height:          height,
		above:           above,
		branches:        uint(len(ss.Hashes)),
		hashes:          make([]merkle.Value, 2<<height),
		hasHash:         make([]bool, 2<<height),
	}
	for i, hash := range ss.Hashes {
		j := 1<<height + i
		sc.hashes[j] = hash
		sc.hasHash[j] = true
	}
	return sc
}

func (sc *subtreeChunk) toStorage() subtreesForStorage {
	return subtreesForStorage{
		BoundaryEntries: sc.boundaryEntries.toStorage(),
		BoundaryFilePos: sc.boundaryFilePos,
		Hashes:          sc.hashes[1<<sc.height : 1<<sc.height+sc.branches],
	}
}

func (sc *subtreeChunk) getHash(gti uint64) (result merkle.Value) {
	if sc.hasHash[gti] {
		return sc.hashes[gti]
	}
	gtiHeight := uint(63 - bits.LeadingZeros64(gti))
	if gtiHeight > sc.height {
		panic("invalid subtree index")
	}
	if gti<<(sc.height-gtiHeight) >= uint64(1)<<sc.height+uint64(sc.branches) {
		result = zeroValues[sc.above+sc.height-gtiHeight]
	} else {
		left := sc.getHash(gti * 2)
		right := sc.getHash(gti*2 + 1)
		hasher := sha256.New()
		hasher.Write(left[:])
		hasher.Write(right[:])
		hasher.Sum(result[:0])
	}
	sc.hashes[gti] = result
	sc.hasHash[gti] = true
	return
}

type tableReader struct {
	reader              io.ReadSeeker
	entryChunkCache     *lru.Cache[uint64, indexEntries]
	subtreeChunkCache   *lru.Cache[subtreePos, *subtreeChunk]
	entryCount, filePos uint64
	levelPointers       []uint64
	chunkHeights        []uint
	topLevel            uint
	tableRoot           merkle.Value
}

type subtreePos struct {
	level uint
	index uint64
}

func newTableReader(reader io.ReadSeeker) (*tableReader, error) {
	pos, err := reader.Seek(-1, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	var headerSizeByte [1]byte
	br, err := reader.Read(headerSizeByte[:])
	if err != nil {
		return nil, err
	}
	if br != 1 {
		return nil, errors.New("unexpected end of file")
	}
	headerSize := int(headerSizeByte[0])
	_, err = reader.Seek(-1-int64(headerSize), io.SeekEnd)
	if err != nil {
		return nil, err
	}
	headerEnc := make([]byte, headerSize)
	br, err = reader.Read(headerEnc)
	if err != nil {
		return nil, err
	}
	if br != headerSize {
		return nil, errors.New("could not read table header")
	}
	var header tableHeader
	if err := rlp.DecodeBytes(headerEnc, &header); err != nil {
		return nil, err
	}
	tr := &tableReader{
		reader:            reader,
		entryChunkCache:   lru.NewCache[uint64, indexEntries](entryCacheSize),
		subtreeChunkCache: lru.NewCache[subtreePos, *subtreeChunk](subtreeCacheSize),
		chunkHeights:      chunkHeights(header.EntryCount),
		entryCount:        header.EntryCount,
		filePos:           uint64(pos),
		levelPointers:     header.LevelPointers,
		tableRoot:         header.TableRoot,
	}
	tr.topLevel = uint(len(tr.chunkHeights) - 2)
	return tr, nil
}

func (tr *tableReader) getSubtreeChunk(level uint, index uint64) (*subtreeChunk, error) {
	sp := subtreePos{level, index}
	if sc, ok := tr.subtreeChunkCache.Get(sp); ok {
		return sc, nil
	}
	var start, stop uint64
	if level == 0 {
		start, stop = tr.levelPointers[1], tr.levelPointers[0]
	} else {
		sc, err := tr.getSubtreeChunk(level-1, index/subtreeChunkSize)
		if err != nil {
			return nil, err
		}
		i := index & subtreeChunkSize
		start, stop = tr.levelPointers[level+1]+sc.boundaryFilePos[i], tr.levelPointers[level+1]+sc.boundaryFilePos[i+1]
	}
	if tr.filePos != start {
		tr.reader.Seek(int64(start), io.SeekStart)
	}
	enc := make([]byte, stop-start)
	br, err := tr.reader.Read(enc)
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
	sc := ss.toSubtreeChunk(tr.chunkHeights[level+1]-tr.chunkHeights[level], tr.chunkHeights[tr.topLevel+1]-tr.chunkHeights[level+1])
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
	br, err := tr.reader.Read(enc)
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
	return &ec[index%entryChunkSize], nil
}

func (tr *tableReader) seekEntry(target *indexEntry) (uint64, bool, error) {
	var (
		chunkLevel uint
		chunkIndex uint64
	)
	for chunkLevel < tr.topLevel {
		sc, err := tr.getSubtreeChunk(chunkLevel, chunkIndex)
		if err != nil {
			return 0, false, err
		}
		subIndex, _ := sc.boundaryEntries.find(target)
		chunkLevel++
		chunkIndex = chunkIndex*subtreeChunkSize + uint64(subIndex)
	}
	ec, err := tr.getEntryChunk(chunkIndex)
	if err != nil {
		return 0, false, err
	}
	subIndex, found := ec.find(target)
	return chunkIndex*entryChunkSize + uint64(subIndex), found, nil
}

type tableWriter struct {
	lastEntryChunk        *entryChunk
	lastSubtreeChunks     []*subtreeChunk
	files                 []*os.File
	writers               []*bufio.Writer
	writePointers         []uint64
	fileName              string
	chunkHeights          []uint
	topLevel              uint
	entryCount, nextEntry uint64
}

func newTableWriter(fileName string, entryCount uint64) (*tableWriter, error) {
	c := chunkHeights(entryCount)
	topLevel := uint(len(c) - 2)
	tw := &tableWriter{
		lastEntryChunk:    &entryChunk{},
		lastSubtreeChunks: make([]*subtreeChunk, topLevel),
		files:             make([]*os.File, topLevel+1),
		writers:           make([]*bufio.Writer, topLevel+1),
		writePointers:     make([]uint64, topLevel+1),
		fileName:          fileName,
		entryCount:        entryCount,
		chunkHeights:      c,
		topLevel:          topLevel,
	}
	for i := range tw.lastSubtreeChunks {
		tw.lastSubtreeChunks[i] = tw.newSubtreeChunk(uint(i))
	}
	for i := range tw.files {
		f, err := os.Create(tw.getFileName(uint(i)))
		if err != nil {
			return nil, err
		}
		tw.files[i] = f
		tw.writers[i] = bufio.NewWriter(f)
	}
	return tw, nil
}

func (tw *tableWriter) newSubtreeChunk(level uint) *subtreeChunk {
	height := tw.chunkHeights[level+1] - tw.chunkHeights[level]
	return &subtreeChunk{
		height:  height,
		above:   tw.chunkHeights[tw.topLevel+1] - tw.chunkHeights[level+1],
		hashes:  make([]merkle.Value, 2<<height),
		hasHash: make([]bool, 2<<height),
	}
}

func (tw *tableWriter) getFileName(level uint) string {
	if level == tw.topLevel {
		return tw.fileName
	}
	return fmt.Sprintf("%s.temp.%d", tw.fileName, level)
}

func (tw *tableWriter) addEntry(ie *indexEntry) error {
	if tw.nextEntry >= tw.entryCount {
		panic("too many entries")
	}
	tw.lastEntryChunk.entries = append(tw.lastEntryChunk.entries, *ie)
	tw.nextEntry++
	if len(tw.lastEntryChunk.entries) == entryChunkSize || tw.nextEntry == tw.entryCount {
		ess := tw.lastEntryChunk.entries.toStorage()
		enc, err := rlp.EncodeToBytes(&ess)
		if err != nil {
			return err
		}
		bw, err := tw.writers[tw.topLevel].Write(enc)
		if err != nil {
			return err
		}
		if bw != len(enc) {
			return errors.New("error writing table chunk")
		}
		beforePos := tw.writePointers[tw.topLevel]
		tw.writePointers[tw.topLevel] += uint64(bw)
		if err := tw.addSubtreeEntry(tw.topLevel-1, ie, beforePos, tw.writePointers[tw.topLevel], tw.lastEntryChunk.getHash(1)); err != nil {
			return err
		}
		tw.lastEntryChunk = &entryChunk{}
	}
	return nil
}

func (tw *tableWriter) addSubtreeEntry(level uint, boundaryEntry *indexEntry, beforePos, afterPos uint64, hash merkle.Value) error {
	sc := tw.lastSubtreeChunks[level]
	if sc.branches > 0 {
		sc.boundaryEntries = append(sc.boundaryEntries, *boundaryEntry)
	}
	if sc.branches == 0 {
		sc.boundaryFilePos = []uint64{beforePos}
	}
	sc.boundaryFilePos = append(sc.boundaryFilePos, afterPos)
	sc.hashes[1<<sc.height+sc.branches] = hash
	sc.branches++
	if sc.branches == subtreeChunkSize || tw.nextEntry == tw.entryCount {
		ss := sc.toStorage()
		enc, err := rlp.EncodeToBytes(&ss)
		if err != nil {
			return err
		}
		bw, err := tw.writers[level].Write(enc)
		if err != nil {
			return err
		}
		if bw != len(enc) {
			return errors.New("error writing subtree chunk")
		}
		beforePos := tw.writePointers[level]
		tw.writePointers[level] += uint64(bw)
		if level > 0 {
			if err := tw.addSubtreeEntry(level-1, boundaryEntry, beforePos, tw.writePointers[level], sc.getHash(1)); err != nil {
				return err
			}
		}
		tw.lastSubtreeChunks[level] = tw.newSubtreeChunk(level)
	}
	return nil
}

type tableHeader struct {
	LevelPointers []uint64
	EntryCount    uint64
	TableRoot     merkle.Value
}

func (tw *tableWriter) finished() error {
	if tw.nextEntry != tw.entryCount {
		panic("not enough entries")
	}
	header := tableHeader{
		LevelPointers: make([]uint64, tw.topLevel+1),
		EntryCount:    tw.entryCount,
		TableRoot:     tw.lastSubtreeChunks[0].getHash(1),
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
		if err := os.Remove(tw.getFileName(uint(i))); err != nil {
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

var zeroValues = func() []merkle.Value {
	zv := make([]merkle.Value, 256)
	for i := 1; i < 256; i++ {
		hasher := sha256.New()
		hasher.Write(zv[i-1][:])
		hasher.Write(zv[i-1][:])
		hasher.Sum(zv[i][:0])
	}
	return zv
}()
