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
	"errors"
	"io"
	"math/bits"
	"slices"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	entryChunkSize      = 128
	logEntryChunkSize   = 7 //TODO params
	subtreeChunkSize    = 64
	logSubtreeChunkSize = 6
	entryCacheSize      = 100
	subtreeCacheSize    = 100

	maxCopyLength = 0x10000
)

const (
	ieBlock = iota
	ieTransaction
	ieAddress
	ieTopic0
)

type tableFormat struct {
	entryCount                                    uint64
	firstSubtreeHeight, subtreeLevels, leafHeight uint
	// memoryStorage + fileStorage = subtreeLevels + 1
	memoryStorage, fileStorage uint
}

func (p *Params) newTableFormat(entryCount uint64) (tf tableFormat) {
	tf.entryCount = entryCount
	tf.leafHeight = uint(64 - bits.LeadingZeros64(max(entryCount, 1)-1))
	if tf.leafHeight <= logEntryChunkSize {
		tf.subtreeLevels = 1
	} else {
		tf.subtreeLevels = (tf.leafHeight - logEntryChunkSize + logSubtreeChunkSize - 1) / logSubtreeChunkSize
		tf.firstSubtreeHeight = tf.leafHeight - logEntryChunkSize - tf.subtreeLevels*logSubtreeChunkSize
	}
	if tf.firstSubtreeHeight < p.fileStorageThresholdHeight {
		if tf.leafHeight < p.fileStorageThresholdHeight {
			tf.memoryStorage = tf.subtreeLevels + 1
		} else {
			tf.memoryStorage = (p.fileStorageThresholdHeight-tf.firstSubtreeHeight)/logSubtreeChunkSize + 1
		}
	}
	tf.fileStorage = tf.subtreeLevels + 1 - tf.memoryStorage
	return
}

func (tf *tableFormat) entryChunkHeight() uint {
	return tf.leafHeight - (tf.firstSubtreeHeight + (tf.subtreeLevels-1)*logSubtreeChunkSize)
}

func (tf *tableFormat) subtreeChunkHeight(level uint) uint {
	switch {
	case level == 0:
		return tf.firstSubtreeHeight
	case level > 0 && level < tf.subtreeLevels:
		return logSubtreeChunkSize
	default:
		panic("invalid table tree level")
	}
}

func (tf *tableFormat) baseHeight(level uint) uint {
	switch {
	case level == 0:
		return 0
	case level == 1:
		return tf.firstSubtreeHeight
	case level > 1 && level <= tf.subtreeLevels:
		return tf.firstSubtreeHeight + logSubtreeChunkSize*(level-1)
	default:
		panic("invalid table tree level")
	}
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

func txAndLogEntries(blockNumber uint64, txs types.Transactions, receipts types.Receipts) indexEntries {
	var entries indexEntries
	for txi, tx := range txs {
		entries = append(entries, indexEntry{
			indexValue: ([32]byte)(tx.Hash()),
			txIndex:    uint32(txi),
			entryType:  ieTransaction,
		})
	}
	for txi, receipt := range receipts {
		for li, log := range receipt.Logs {
			var addr32 [32]byte
			copy(addr32[32-common.AddressLength:], log.Address[:])
			entries = append(entries, indexEntry{
				indexValue:  addr32,
				blockNumber: blockNumber,
				txIndex:     uint32(txi),
				logIndex:    uint32(li),
				entryType:   ieAddress,
			})
			for ti, topic := range log.Topics {
				entries = append(entries, indexEntry{
					indexValue:  ([32]byte)(topic),
					blockNumber: blockNumber,
					txIndex:     uint32(txi),
					logIndex:    uint32(li),
					entryType:   ieTopic0 + uint32(ti),
				})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].compare(&entries[j]) < 0
	})
	return entries
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
	//fmt.Println("getHash gti", gti, "height", sc.height, "branches", sc.branches, "hasHash", sc.hasHash)
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
	reader            io.ReaderAt
	fileSize          int64
	entryChunkCache   *lru.Cache[uint64, indexEntries]
	subtreeChunkCache *lru.Cache[subtreePos, *subtreeChunk]
	entryCount        uint64
	levelPointers     []int64
	format            tableFormat
	tableRoot         merkle.Value
	meta              tableMeta
}

type subtreePos struct {
	level uint
	index uint64
}

func newTableReader(params *Params, tf *tableFiles, name string) (*tableReader, error) {
	ioReader, fileSize, err := tf.getReaderAt(name)
	if err != nil {
		return nil, err
	}
	var headerSizeByte [1]byte
	_, err = ioReader.ReadAt(headerSizeByte[:], fileSize-1)
	if err != nil {
		return nil, err
	}
	headerEnc := make([]byte, headerSizeByte[0])
	_, err = ioReader.ReadAt(headerEnc, fileSize-1-int64(headerSizeByte[0]))
	if err != nil {
		return nil, err
	}
	var header tableHeader
	if err := rlp.DecodeBytes(headerEnc, &header); err != nil {
		return nil, err
	}
	tr := &tableReader{
		reader:            ioReader,
		fileSize:          fileSize,
		entryChunkCache:   lru.NewCache[uint64, indexEntries](entryCacheSize),
		subtreeChunkCache: lru.NewCache[subtreePos, *subtreeChunk](subtreeCacheSize),
		format:            params.newTableFormat(header.EntryCount),
		entryCount:        header.EntryCount,
		levelPointers:     make([]int64, len(header.LevelPointers)),
		tableRoot:         header.TableRoot,
		meta:              header.tableMeta,
	}
	for i, p := range header.LevelPointers {
		tr.levelPointers[i] = int64(p)
	}
	return tr, nil
}

func (tr *tableReader) getSubtreeChunk(level uint, index uint64) (*subtreeChunk, error) {
	sp := subtreePos{level, index}
	if sc, ok := tr.subtreeChunkCache.Get(sp); ok {
		return sc, nil
	}
	var start, stop int64
	if level == 0 {
		start, stop = tr.levelPointers[1], tr.levelPointers[0]
	} else {
		sc, err := tr.getSubtreeChunk(level-1, index/subtreeChunkSize)
		if err != nil {
			return nil, err
		}
		i := index % subtreeChunkSize
		start, stop = tr.levelPointers[level+1]+int64(sc.boundaryFilePos[i]), tr.levelPointers[level+1]+int64(sc.boundaryFilePos[i+1])
	}
	enc := make([]byte, stop-start)
	_, err := tr.reader.ReadAt(enc, start)
	if err != nil {
		return nil, err
	}
	var ss subtreesForStorage
	if err := rlp.DecodeBytes(enc, &ss); err != nil {
		return nil, err
	}
	sc := ss.toSubtreeChunk(tr.format.subtreeChunkHeight(level), tr.format.leafHeight-tr.format.baseHeight(level+1))
	tr.subtreeChunkCache.Add(sp, sc)
	return sc, nil
}

func (tr *tableReader) getEntryChunk(index uint64) (indexEntries, error) {
	if ec, ok := tr.entryChunkCache.Get(index); ok {
		return ec, nil
	}
	sc, err := tr.getSubtreeChunk(tr.format.subtreeLevels-1, index/subtreeChunkSize)
	if err != nil {
		return nil, err
	}
	i := index % subtreeChunkSize
	start, stop := sc.boundaryFilePos[i], sc.boundaryFilePos[i+1]
	enc := make([]byte, stop-start)
	_, err = tr.reader.ReadAt(enc, int64(start))
	if err != nil {
		return nil, err
	}
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
	for chunkLevel < tr.format.subtreeLevels {
		sc, err := tr.getSubtreeChunk(chunkLevel, chunkIndex)
		if err != nil {
			return 0, false, err
		}
		subIndex, _ := sc.boundaryEntries.find(target)
		//fmt.Println("seek s", chunkLevel, chunkIndex, subIndex, sc.boundaryEntries[max(subIndex, 1)-1], sc.boundaryEntries[min(subIndex, len(sc.boundaryEntries)-1)])
		chunkLevel++
		chunkIndex = chunkIndex*subtreeChunkSize + uint64(subIndex)
	}
	ec, err := tr.getEntryChunk(chunkIndex)
	if err != nil {
		return 0, false, err
	}
	subIndex, found := ec.find(target)
	//fmt.Println("seek e", chunkLevel, chunkIndex, subIndex)
	return chunkIndex*entryChunkSize + uint64(subIndex), found, nil
}

type tableWriter struct {
	lock                                       sync.Mutex
	tf                                         *tableFiles
	name                                       string
	entryCount                                 uint64
	format                                     tableFormat
	meta                                       tableMeta
	isOpen, isDeleted, hasStoredState, hasMeta bool
	phase                                      uint
	tableRoot                                  merkle.Value // after all entries added
	// phase == wpWriteEntries
	lastEntryChunk    *entryChunk
	lastSubtreeChunks []*subtreeChunk
	writers           []io.WriteCloser
	writePointers     []int64
	nextEntry         uint64
	// phase == wpTempCopy
	copyLevel                     uint
	copyReader                    io.ReaderAt
	copyReadPointer, copyReadSize int64
	copyWriter                    io.WriteCloser
	copyWritePointer              int64
	levelPointers                 []int64
}

const (
	wpNone = iota // new table write, no partial state
	wpWriteEntries
	wpTempCopy
	wpFinalized
)

type writeState struct {
	Phase             uint
	NextEntry         uint64
	LastEntryChunk    entriesForStorage
	LastSubtreeChunks []subtreesForStorage
	CopyLevel         uint
	CopyReadPointer   uint64
	tableHeader
}

func newTableWriter(params *Params, tf *tableFiles, name string, storedState bool, entryCount uint64) (*tableWriter, error) {
	var state writeState
	if storedState {
		r, l, err := tf.getReaderAt(name + writeStateSuffix)
		if err != nil {
			tf.deleteFile(name + writeStateSuffix)
			return nil, err
		}
		enc := make([]byte, l)
		if _, err := r.ReadAt(enc, 0); err != nil {
			tf.deleteFile(name + writeStateSuffix)
			return nil, err
		}
		if err := rlp.DecodeBytes(enc, &state); err != nil {
			tf.deleteFile(name + writeStateSuffix)
			return nil, err
		}
		entryCount = state.EntryCount
	}
	format := params.newTableFormat(entryCount)
	tw := &tableWriter{
		tf:                tf,
		name:              name,
		hasStoredState:    storedState,
		hasMeta:           storedState,
		meta:              state.tableHeader.tableMeta,
		lastSubtreeChunks: make([]*subtreeChunk, format.subtreeLevels),
		writers:           make([]io.WriteCloser, format.subtreeLevels+1),
		writePointers:     make([]int64, format.subtreeLevels+1),
		entryCount:        entryCount,
		format:            format,
	}
	tw.phase = state.Phase
	tw.lastEntryChunk = tw.newEntryChunk()
	tw.nextEntry = state.NextEntry
	tw.tableRoot = state.TableRoot
	switch state.Phase {
	case wpNone:
		for i := range tw.lastSubtreeChunks {
			tw.lastSubtreeChunks[i] = tw.newSubtreeChunk(uint(i))
		}
		tw.phase = wpWriteEntries
	case wpWriteEntries:
		tw.lastEntryChunk.entries = state.LastEntryChunk.toEntries()
		for i := range tw.lastSubtreeChunks {
			tw.lastSubtreeChunks[i] = state.LastSubtreeChunks[i].toSubtreeChunk(tw.format.subtreeChunkHeight(uint(i)), tw.format.leafHeight-tw.format.baseHeight(uint(i+1)))
		}
		for i := range tw.format.subtreeLevels + 1 {
			_, l, err := tf.getReaderAt(name + writeTempSuffix(uint(i)))
			if err != nil {
				tw.delete()
				return nil, err
			}
			tw.writePointers[i] = l
		}
	case wpTempCopy:
		tw.copyLevel = state.CopyLevel
		tw.copyReadPointer = int64(state.CopyReadPointer)
		tw.levelPointers = make([]int64, len(state.LevelPointers))
		for i, p := range state.LevelPointers {
			tw.levelPointers[i] = int64(p)
		}
		var err error
		tw.copyReader, tw.copyReadSize, err = tf.getReaderAt(name + writeTempSuffix(tw.copyLevel))
		if err != nil {
			tw.delete()
			return nil, err
		}
		_, tw.copyWritePointer, err = tf.getReaderAt(name + writeTempSuffix(tw.format.subtreeLevels))
		if err != nil {
			tw.delete()
			return nil, err
		}
	default:
		tw.delete()
		return nil, errors.New("invalid table write phase")
	}
	return tw, nil
}

func (tw *tableWriter) delete() error {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	var finalErr error
	for _, w := range tw.writers {
		if w != nil {
			if err := w.Close(); err != nil {
				finalErr = err
			}
		}
	}
	if tw.copyWriter != nil {
		if err := tw.copyWriter.Close(); err != nil {
			finalErr = err
		}
	}
	if err := tw.tf.deleteFile(tw.name + writeStateSuffix); err != nil && err != errFileNotFound {
		finalErr = err
	}
	for i := range tw.format.subtreeLevels + 1 {
		if err := tw.tf.deleteFile(tw.name + writeTempSuffix(i)); err != nil && err != errFileNotFound {
			finalErr = err
		}
	}
	if err := tw.tf.deleteFile(tw.name); err != nil && err != errFileNotFound {
		finalErr = err
	}
	tw.isDeleted = true
	return finalErr
}

func (tw *tableWriter) open() error {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if tw.isDeleted {
		return ErrTableDeleted
	}
	if tw.isOpen {
		panic("table writer is already open")
	}
	if tw.hasStoredState {
		if err := tw.tf.deleteFile(tw.name + writeStateSuffix); err != nil {
			return err
		}
		tw.hasStoredState = false
	}
	var err error
	switch tw.phase {
	case wpWriteEntries:
		for i := range tw.format.subtreeLevels + 1 {
			tw.writers[i], err = tw.tf.getAppendWriter(tw.name+writeTempSuffix(i), i < tw.format.memoryStorage)
			if err != nil {
				return err
			}
		}
	case wpTempCopy:
		tw.copyWriter, err = tw.tf.getAppendWriter(tw.name+writeTempSuffix(tw.format.subtreeLevels), tw.format.subtreeLevels < tw.format.memoryStorage)
		if err != nil {
			return err
		}
	default:
		panic("invalid table write phase")
	}
	tw.isOpen = true
	return nil
}

func (tw *tableWriter) close() error {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if tw.isDeleted {
		return ErrTableDeleted
	}
	if !tw.isOpen {
		panic("table writer is not open")
	}
	if !tw.hasMeta {
		panic("tableMeta missing")
	}
	tw.isOpen = false
	state := writeState{
		Phase:     tw.phase,
		NextEntry: tw.nextEntry,
		tableHeader: tableHeader{
			EntryCount: tw.entryCount,
			TableRoot:  tw.tableRoot,
			tableMeta:  tw.meta,
		},
	}
	switch tw.phase {
	case wpWriteEntries:
		for i, w := range tw.writers {
			if err := w.Close(); err != nil {
				return err
			}
			tw.writers[i] = nil // signal for a potential delete() that this writer has been successfully closed
		}
		state.LastEntryChunk = tw.lastEntryChunk.entries.toStorage()
		state.LastSubtreeChunks = make([]subtreesForStorage, len(tw.lastSubtreeChunks))
		for i, sc := range tw.lastSubtreeChunks {
			state.LastSubtreeChunks[i] = sc.toStorage()
		}
	case wpTempCopy:
		if err := tw.copyWriter.Close(); err != nil {
			return err
		}
		tw.copyWriter = nil
		state.CopyLevel = tw.copyLevel
		state.CopyReadPointer = uint64(tw.copyReadPointer)
		state.LevelPointers = make([]uint64, len(tw.levelPointers))
		for i, p := range tw.levelPointers {
			state.LevelPointers[i] = uint64(p)
		}
	case wpFinalized:
		return nil
	default:
		panic("invalid table write phase")
	}
	sw, err := tw.tf.getAppendWriter(tw.name+writeStateSuffix, true)
	if err != nil {
		return err
	}
	if err := rlp.Encode(sw, &state); err != nil {
		sw.Close()
		return err
	}
	if err := sw.Close(); err != nil {
		return err
	}
	tw.hasStoredState = true
	return nil
}

func (tw *tableWriter) newEntryChunk() *entryChunk {
	return &entryChunk{
		height: tw.format.entryChunkHeight(),
	}
}

func (tw *tableWriter) newSubtreeChunk(level uint) *subtreeChunk {
	height := tw.format.subtreeChunkHeight(level)
	return &subtreeChunk{
		height:  height,
		above:   tw.format.leafHeight - tw.format.baseHeight(level+1),
		hashes:  make([]merkle.Value, 2<<height),
		hasHash: make([]bool, 2<<height),
	}
}

func (tw *tableWriter) addEntry(ie *indexEntry) error {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if tw.isDeleted {
		return ErrTableDeleted
	}
	if tw.phase != wpWriteEntries {
		panic("invalid table write phase")
	}
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
		n, err := tw.writers[tw.format.subtreeLevels].Write(enc)
		if err != nil {
			return err
		}
		if n != len(enc) {
			return errors.New("error writing table chunk")
		}
		beforePos := tw.writePointers[tw.format.subtreeLevels]
		tw.writePointers[tw.format.subtreeLevels] += int64(n)
		if err := tw.addSubtreeEntry(tw.format.subtreeLevels-1, ie, beforePos, tw.writePointers[tw.format.subtreeLevels], tw.lastEntryChunk.getHash(1)); err != nil {
			return err
		}
		tw.lastEntryChunk = tw.newEntryChunk()
	}
	return nil
}

func (tw *tableWriter) addSubtreeEntry(level uint, boundaryEntry *indexEntry, beforePos, afterPos int64, hash merkle.Value) error {
	sc := tw.lastSubtreeChunks[level]
	if sc.branches < subtreeChunkSize-1 {
		sc.boundaryEntries = append(sc.boundaryEntries, *boundaryEntry)
	}
	if sc.branches == 0 {
		sc.boundaryFilePos = []uint64{uint64(beforePos)}
	}
	sc.boundaryFilePos = append(sc.boundaryFilePos, uint64(afterPos))
	sc.hashes[1<<sc.height+sc.branches] = hash
	sc.hasHash[1<<sc.height+sc.branches] = true
	sc.branches++
	if sc.branches == subtreeChunkSize || tw.nextEntry == tw.entryCount {
		ss := sc.toStorage()
		enc, err := rlp.EncodeToBytes(&ss)
		if err != nil {
			return err
		}
		n, err := tw.writers[level].Write(enc)
		if err != nil {
			return err
		}
		if n != len(enc) {
			return errors.New("error writing subtree chunk")
		}
		beforePos := tw.writePointers[level]
		tw.writePointers[level] += int64(n)
		if level > 0 {
			if err := tw.addSubtreeEntry(level-1, boundaryEntry, beforePos, tw.writePointers[level], sc.getHash(1)); err != nil {
				return err
			}
		} else {
			tw.tableRoot = sc.getHash(1)
		}
		tw.lastSubtreeChunks[level] = tw.newSubtreeChunk(level)
	}
	return nil
}

func (tw *tableWriter) setMeta(meta tableMeta) {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if tw.hasMeta {
		panic("tableMeta already exists")
	}
	tw.meta, tw.hasMeta = meta, true
}

func (tw *tableWriter) getTableRoot() merkle.Value {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if tw.nextEntry != tw.entryCount {
		panic("not enough entries")
	}
	return tw.tableRoot
}

func (tw *tableWriter) finalize() (bool, error) {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if tw.isDeleted {
		return false, ErrTableDeleted
	}
	if tw.phase == wpWriteEntries {
		if tw.nextEntry != tw.entryCount {
			panic("not enough entries")
		}
		if !tw.hasMeta {
			panic("tableMeta missing")
		}
		for i := range tw.format.subtreeLevels {
			if err := tw.writers[i].Close(); err != nil {
				return false, err
			}
			tw.writers[i] = nil // signal for a potential delete() that this writer has been successfully closed
		}
		tw.copyWriter = tw.writers[tw.format.subtreeLevels]
		tw.copyWritePointer = tw.writePointers[tw.format.subtreeLevels]
		tw.writers = nil
		tw.copyLevel = tw.format.subtreeLevels
		tw.levelPointers = make([]int64, tw.format.subtreeLevels+1)
		tw.phase = wpTempCopy
	}
	if tw.phase != wpTempCopy {
		panic("invalid table write phase")
	}
	for tw.copyReadPointer == tw.copyReadSize {
		tw.levelPointers[tw.copyLevel] = tw.copyWritePointer
		if tw.copyLevel == 0 {
			// last temp level file copied; finalize by adding header
			header := tableHeader{
				LevelPointers: make([]uint64, len(tw.levelPointers)),
				EntryCount:    tw.entryCount,
				TableRoot:     tw.tableRoot,
				tableMeta:     tw.meta,
			}
			for i, p := range tw.levelPointers {
				header.LevelPointers[i] = uint64(p)
			}
			headerEnc, err := rlp.EncodeToBytes(&header)
			if err != nil {
				return false, err
			}
			headerEnc = append(headerEnc, byte(len(headerEnc)))
			if _, err := tw.copyWriter.Write(headerEnc); err != nil {
				return false, err
			}
			// close and rename top level temp file and return success
			if err := tw.copyWriter.Close(); err != nil {
				return false, err
			}
			tw.copyWriter = nil
			for i := range tw.format.subtreeLevels {
				if err := tw.tf.deleteFile(tw.name + writeTempSuffix(i)); err != nil {
					return false, err
				}
			}
			if err := tw.tf.renameFile(tw.name+writeTempSuffix(tw.format.subtreeLevels), tw.name); err != nil {
				return false, err
			}
			tw.phase = wpFinalized
			return true, nil
		}
		tw.copyLevel--
		tw.copyReadPointer = 0
		var err error
		if tw.copyReader, tw.copyReadSize, err = tw.tf.getReaderAt(tw.name + writeTempSuffix(tw.copyLevel)); err != nil {
			return false, err
		}
	}
	copyLength := min(tw.copyReadSize-tw.copyReadPointer, maxCopyLength)
	data := make([]byte, copyLength)
	n, err := tw.copyReader.ReadAt(data, tw.copyReadPointer)
	if err != nil {
		return false, err
	}
	if int64(n) != copyLength {
		return false, errors.New("could not read copy data chunk")
	}
	n, err = tw.copyWriter.Write(data)
	if err != nil {
		return false, err
	}
	if int64(n) != copyLength {
		return false, errors.New("could not write copy data chunk")
	}
	tw.copyReadPointer += copyLength
	tw.copyWritePointer += copyLength
	return false, nil
}

func (tw *tableWriter) isFinalized() bool {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	return tw.phase == wpFinalized
}

type tableHeader struct {
	LevelPointers []uint64
	EntryCount    uint64
	TableRoot     merkle.Value
	tableMeta
}

type tableMeta struct {
	LastBlockNumber, BlockCount uint64
	LastBlockHash, ParentHash   common.Hash
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
