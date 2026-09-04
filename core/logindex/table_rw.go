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

	//"fmt"
	"io"
	"math"
	"math/bits"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/params"
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
	IeBlock = iota
	IeTransaction
	IeAddress
	IeTopic0
	MaxTopicCount = 4
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
		tf.firstSubtreeHeight = tf.leafHeight - logEntryChunkSize - (tf.subtreeLevels-1)*logSubtreeChunkSize
	}
	if tf.firstSubtreeHeight < p.fileStorageThresholdHeight {
		if tf.leafHeight < p.fileStorageThresholdHeight {
			tf.memoryStorage = tf.subtreeLevels + 1
		} else {
			tf.memoryStorage = (p.fileStorageThresholdHeight-tf.firstSubtreeHeight)/logSubtreeChunkSize + 1 //TODO inkabb fix valahany szint legyen memory?
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

func (tf *tableFormat) getChunkLevel(height uint) uint {
	if height <= tf.firstSubtreeHeight {
		return 0
	}
	//if height >= tf.firstSubtreeHeight+(tf.subtreeLevels-1)*logSubtreeChunkSize {
	if height > tf.firstSubtreeHeight+(tf.subtreeLevels-1)*logSubtreeChunkSize {
		return tf.subtreeLevels
	}
	return (height - tf.firstSubtreeHeight + logSubtreeChunkSize - 1) / logSubtreeChunkSize
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

type IndexEntry struct {
	IndexValue
	IndexPosition
}

type IndexValue struct {
	EntryType uint32      `json:"type"`
	Value     common.Hash `json:"value"`
}

type IndexPosition struct {
	BlockNumber uint64 `json:"block"`
	TxIndex     uint32 `json:"tx"`
	LogIndex    uint32 `json:"log"`
}

func (ip *IndexPosition) Decrease() {
	if ip.LogIndex > 0 {
		ip.LogIndex--
		return
	}
	ip.LogIndex = math.MaxUint32
	if ip.TxIndex > 0 {
		ip.TxIndex--
		return
	}
	ip.TxIndex = math.MaxUint32
	if ip.BlockNumber > 0 {
		ip.BlockNumber--
		return
	}
	panic("cannot decrease null index position")
}

type IndexEntries []IndexEntry

// DecodeRLP implements rlp.Decoder.
func (ies *IndexEntries) DecodeRLP(s *rlp.Stream) error {
	var ess entriesForStorage
	if err := s.Decode(&ess); err != nil {
		return err
	}
	*ies = ess.toEntries()
	return nil
}

// EncodeRLP implements rlp.Encoder
func (ies IndexEntries) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, ies.toStorage())
}

func (ie *IndexEntry) Hash() (result merkle.Value) {
	var (
		enc    [50]byte
		encLen int
	)
	binary.BigEndian.PutUint16(enc[0:2], uint16(ie.EntryType))
	switch ie.EntryType {
	case IeBlock:
		copy(enc[2:34], ie.Value[:])
		binary.BigEndian.PutUint64(enc[34:42], ie.BlockNumber)
		encLen = 42
	case IeAddress:
		copy(enc[2:22], ie.Value[12:])
		binary.BigEndian.PutUint64(enc[22:30], ie.BlockNumber)
		binary.BigEndian.PutUint32(enc[30:34], ie.TxIndex)
		binary.BigEndian.PutUint32(enc[34:38], ie.LogIndex)
		encLen = 38
	default: // transaction or topic
		copy(enc[2:34], ie.Value[:])
		binary.BigEndian.PutUint64(enc[34:42], ie.BlockNumber)
		binary.BigEndian.PutUint32(enc[42:46], ie.TxIndex)
		binary.BigEndian.PutUint32(enc[46:50], ie.LogIndex)
		encLen = 50
	}
	hasher := sha256.New()
	hasher.Write(enc[:encLen])
	hasher.Sum(result[:0])
	return
}

func (ie *IndexEntry) Compare(i2 *IndexEntry) int {
	if c := ie.IndexValue.Compare(&i2.IndexValue); c != 0 {
		return c
	}
	return ie.IndexPosition.Compare(&i2.IndexPosition)
}

func (iv *IndexValue) Compare(i2 *IndexValue) int {
	if iv.EntryType != i2.EntryType {
		if iv.EntryType < i2.EntryType {
			return -1
		}
		return 1
	}
	return bytes.Compare(iv.Value[:], i2.Value[:])
}

func (ip *IndexPosition) Compare(i2 *IndexPosition) int {
	if ip.BlockNumber != i2.BlockNumber {
		if ip.BlockNumber < i2.BlockNumber {
			return -1
		}
		return 1

	}
	if ip.TxIndex != i2.TxIndex {
		if ip.TxIndex < i2.TxIndex {
			return -1
		}
		return 1
	}
	if ip.LogIndex != i2.LogIndex {
		if ip.LogIndex < i2.LogIndex {
			return -1
		}
		return 1
	}
	return 0
}

type entriesForStorage []entryForStorage

type entryForStorage struct {
	TypeAndValueDiff []byte
	BlockDiff        uint64
	TxDiff, LogDiff  uint32
}

// ies[pos] >= ie; == if found is true
func (ies IndexEntries) Find(ie *IndexEntry) (int, bool) {
	min, max := 0, len(ies)
	for min < max {
		mid := (min + max) / 2
		switch ies[mid].Compare(ie) {
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

func (ies IndexEntries) toStorage() entriesForStorage {
	ess := make(entriesForStorage, 0, len(ies))
	var (
		typeAndValue, lastTypeAndValue [36]byte
		lastBlockNumber                uint64
		lastTxIndex, lastLogIndex      uint32
	)
	for _, ie := range ies {
		binary.BigEndian.PutUint32(typeAndValue[0:4], ie.EntryType)
		copy(typeAndValue[4:], ie.Value[:])
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
		if ie.BlockNumber != lastBlockNumber {
			es.BlockDiff = ie.BlockNumber - lastBlockNumber
			lastBlockNumber = ie.BlockNumber
			lastTxIndex, lastLogIndex = 0, 0
		}
		if ie.TxIndex != lastTxIndex {
			es.TxDiff = ie.TxIndex - lastTxIndex
			lastTxIndex = ie.TxIndex
			lastLogIndex = 0
		}
		es.LogDiff = ie.LogIndex - lastLogIndex
		lastLogIndex = ie.LogIndex
		ess = append(ess, es)
	}
	return ess
}

type entryChunk struct {
	entries IndexEntries
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
		result = ec.entries[gti-uint64(1)<<ec.height].Hash()
	} else {
		hasher := sha256.New()
		left := ec.getHash(gti * 2)
		right := ec.getHash(gti*2 + 1)
		hasher.Write(left[:])
		hasher.Write(right[:])
		hasher.Sum(result[:0])
	} //TODO hashing
	ec.hashes[gti] = result
	ec.hasHash[gti] = true
	return
}

func (ess entriesForStorage) toEntries() IndexEntries {
	ies := make(IndexEntries, 0, len(ess))
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
		ie := IndexEntry{
			IndexValue: IndexValue{
				EntryType: binary.BigEndian.Uint32(lastTypeAndValue[:4]),
			},
			IndexPosition: IndexPosition{
				BlockNumber: lastBlockNumber,
				TxIndex:     lastTxIndex,
				LogIndex:    lastLogIndex,
			},
		}
		copy(ie.Value[:], lastTypeAndValue[4:])
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
	boundaryEntries         IndexEntries // N-1
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
	} //TODO hashing
	sc.hashes[gti] = result
	sc.hasHash[gti] = true
	return
}

type TableReader struct {
	reader            io.ReaderAt
	fileSize          int64
	entryChunkCache   *lru.Cache[uint64, *entryChunk]
	subtreeChunkCache *lru.Cache[subtreePos, *subtreeChunk]
	EntryCount        uint64
	levelPointers     []int64
	format            tableFormat
	TableRoot         merkle.Value
	IndexContract     common.Address //TODO
	Meta              TableMeta
}

type subtreePos struct {
	level uint
	index uint64
}

func newTableReader(p *Params, tf *tableFiles, name string) (*TableReader, error) {
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
	tr := &TableReader{
		reader:            ioReader,
		fileSize:          fileSize,
		entryChunkCache:   lru.NewCache[uint64, *entryChunk](entryCacheSize),
		subtreeChunkCache: lru.NewCache[subtreePos, *subtreeChunk](subtreeCacheSize),
		format:            p.newTableFormat(header.EntryCount),
		EntryCount:        header.EntryCount,
		levelPointers:     make([]int64, len(header.LevelPointers)),
		TableRoot:         header.TableRoot,
		IndexContract:     params.IndexContractAddress, //TODO
		Meta:              header.Meta,
	}
	for i, p := range header.LevelPointers {
		tr.levelPointers[i] = int64(p)
	}
	return tr, nil
}

func (tr *TableReader) BlockRange() common.Range[uint64] {
	return common.NewRange[uint64](tr.Meta.LastBlockNumber+1-tr.Meta.BlockCount, tr.Meta.BlockCount)
}

func (tr *TableReader) getSubtreeChunk(level uint, index uint64) (*subtreeChunk, error) {
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

func (tr *TableReader) GetEntryChunk(index uint64) (*entryChunk, error) {
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
	ec := &entryChunk{
		height: tr.format.entryChunkHeight(),
	}
	if err := rlp.DecodeBytes(enc, &ec.entries); err != nil {
		return nil, err
	}
	tr.entryChunkCache.Add(index, ec)
	return ec, nil
}

func (tr *TableReader) GetHash(gti uint64) (merkle.Value, error) {
	gtiHeight := uint(63 - bits.LeadingZeros64(gti))
	chunkLevel := tr.format.getChunkLevel(gtiHeight)
	chunkBaseHeight := tr.format.baseHeight(chunkLevel)
	chunkIndex := (gti - uint64(1)<<gtiHeight) >> (gtiHeight - chunkBaseHeight)
	m := uint64(1) << (gtiHeight - chunkBaseHeight)
	chunkGti := m + gti&(m-1)
	if chunkLevel == tr.format.subtreeLevels {
		ec, err := tr.GetEntryChunk(chunkIndex)
		if err != nil {
			return merkle.Value{}, err
		}
		return ec.getHash(chunkGti), nil
	}
	sc, err := tr.getSubtreeChunk(chunkLevel, chunkIndex)
	if err != nil {
		return merkle.Value{}, err
	}
	return sc.getHash(chunkGti), nil
}

func (tr *TableReader) GetEntry(index uint64) (*IndexEntry, error) {
	ec, err := tr.GetEntryChunk(index / entryChunkSize)
	if err != nil {
		return nil, err
	}
	return &ec.entries[index%entryChunkSize], nil
}

// batch read entry chunk (not cached, optimized for table merge linear read)
func (tr *TableReader) getEntries(indexRange common.Range[uint64]) (IndexEntries, error) {
	if indexRange.IsEmpty() {
		return nil, nil
	}
	firstEC, lastEC := indexRange.First()/entryChunkSize, indexRange.Last()/entryChunkSize
	firstSC, lastSC := firstEC/subtreeChunkSize, lastEC/subtreeChunkSize
	/*fmt.Println("getEntries", indexRange.First(), indexRange.Last(), tr.EntryCount)
	fmt.Println(" EC", firstEC, lastEC)
	fmt.Println(" SC", firstSC, lastSC)*/
	scs := make([]*subtreeChunk, lastSC+1-firstSC)
	for i := range scs {
		sc, err := tr.getSubtreeChunk(tr.format.subtreeLevels-1, firstSC+uint64(i))
		if err != nil {
			return nil, err
		}
		//fmt.Println("scs", i, "bfp", sc.boundaryFilePos)
		scs[i] = sc
	}
	start, stop := scs[0].boundaryFilePos[firstEC%subtreeChunkSize], scs[lastSC-firstSC].boundaryFilePos[lastEC%subtreeChunkSize+1]
	//fmt.Println(" read", start, stop, stop-start)
	enc := make([]byte, stop-start)
	_, err := tr.reader.ReadAt(enc, int64(start))
	if err != nil {
		return nil, err
	}
	entries := make(IndexEntries, 0, (lastEC+1-firstEC)*entryChunkSize)
	for ec := firstEC; ec <= lastEC; ec++ {
		sc := scs[ec/subtreeChunkSize-firstSC]
		firstByte := sc.boundaryFilePos[ec%subtreeChunkSize] - start
		afterLastByte := sc.boundaryFilePos[ec%subtreeChunkSize+1] - start
		//fmt.Println(" enc range", firstByte, afterLastByte)
		var dec IndexEntries
		if err := rlp.DecodeBytes(enc[firstByte:afterLastByte], &dec); err != nil {
			return nil, err
		}
		entries = append(entries, dec...)
	}
	offset := indexRange.First() % entryChunkSize
	return entries[offset : offset+indexRange.Count()], nil
}

func (tr *TableReader) SeekEntry(target *IndexEntry) (uint64, bool, error) {
	var (
		chunkLevel               uint
		chunkIndex, chunkEntries uint64
	)
	chunkEntries = entryChunkSize
	for range tr.format.subtreeLevels {
		chunkEntries *= subtreeChunkSize
	}
	for chunkLevel < tr.format.subtreeLevels {
		sc, err := tr.getSubtreeChunk(chunkLevel, chunkIndex)
		if err != nil {
			return 0, false, err
		}
		subIndex, _ := sc.boundaryEntries.Find(target)
		//fmt.Println("seek s", chunkLevel, chunkIndex, subIndex, sc.boundaryEntries[max(subIndex, 1)-1], sc.boundaryEntries[min(subIndex, len(sc.boundaryEntries)-1)])
		chunkLevel++
		chunkIndex = chunkIndex*subtreeChunkSize + uint64(subIndex)
		chunkEntries /= subtreeChunkSize
		if chunkIndex*chunkEntries >= tr.EntryCount {
			return tr.EntryCount, false, nil //TODO unit test
		}
	}
	ec, err := tr.GetEntryChunk(chunkIndex)
	if err != nil {
		return 0, false, err
	}
	subIndex, found := ec.entries.Find(target)
	//fmt.Println("seek e", chunkLevel, chunkIndex, subIndex)
	return chunkIndex*entryChunkSize + uint64(subIndex), found, nil
}

type tableWriter struct {
	lock                                       sync.Mutex
	tf                                         *tableFiles
	name                                       string
	entryCount                                 uint64
	format                                     tableFormat
	meta                                       TableMeta
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
	Header            tableHeader
}

func newTableWriter(params *Params, tf *tableFiles, name string, storedState bool, entryCount uint64, forceMemory bool) (*tableWriter, error) {
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
		//fmt.Println("state @ newTableWriter:", state)
		entryCount = state.Header.EntryCount
	}
	format := params.newTableFormat(entryCount)
	if forceMemory {
		format.memoryStorage = format.subtreeLevels + 1
	}
	tw := &tableWriter{
		tf:                tf,
		name:              name,
		hasStoredState:    storedState,
		hasMeta:           state.Header.Meta.LastBlockHash != (common.Hash{}),
		meta:              state.Header.Meta,
		lastSubtreeChunks: make([]*subtreeChunk, format.subtreeLevels),
		writers:           make([]io.WriteCloser, format.subtreeLevels+1),
		writePointers:     make([]int64, format.subtreeLevels+1),
		entryCount:        entryCount,
		format:            format,
	}
	tw.phase = state.Phase
	tw.lastEntryChunk = tw.newEntryChunk()
	tw.nextEntry = state.NextEntry
	tw.tableRoot = state.Header.TableRoot
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
		tw.levelPointers = make([]int64, len(state.Header.LevelPointers))
		for i, p := range state.Header.LevelPointers {
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

	//fmt.Println("+++ open", tw.name)
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

	//fmt.Println("+++ close", tw.name)
	if tw.isDeleted {
		return ErrTableDeleted
	}
	if !tw.isOpen {
		panic("table writer is not open")
	}
	state := writeState{
		Phase:     tw.phase,
		NextEntry: tw.nextEntry,
		Header: tableHeader{
			EntryCount: tw.entryCount,
			TableRoot:  tw.tableRoot,
			Meta:       tw.meta,
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
		state.Header.LevelPointers = make([]uint64, len(tw.levelPointers))
		for i, p := range tw.levelPointers {
			state.Header.LevelPointers[i] = uint64(p)
		}
	case wpFinalized:
		tw.isOpen = false
		return nil
	default:
		panic("invalid table write phase")
	}
	//fmt.Println("entryCount @ tableWriter.close:", tw.entryCount)
	//fmt.Println("state @ tableWriter.close:", state)
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
	tw.isOpen = false
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

func (tw *tableWriter) lastAndNextEntry() (*IndexEntry, uint64) {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if len(tw.lastEntryChunk.entries) != 0 {
		return &tw.lastEntryChunk.entries[len(tw.lastEntryChunk.entries)-1], tw.nextEntry
	}
	for i := int(tw.format.subtreeLevels) - 1; i >= 0; i-- {
		if len(tw.lastSubtreeChunks[i].boundaryEntries) > 0 {
			return &tw.lastSubtreeChunks[i].boundaryEntries[len(tw.lastSubtreeChunks[i].boundaryEntries)-1], tw.nextEntry
		}
	}
	return nil, tw.nextEntry
}

func (tw *tableWriter) addEntry(ie *IndexEntry) error {
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

func (tw *tableWriter) addSubtreeEntry(level uint, boundaryEntry *IndexEntry, beforePos, afterPos int64, hash merkle.Value) error {
	sc := tw.lastSubtreeChunks[level]
	//fmt.Println("addSubtreeEntry", tw.format, sc.height, sc.above, level, beforePos, afterPos)
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
			subtreeRoot := sc.getHash(1)
			var entryCountEnc [32]byte
			binary.LittleEndian.PutUint64(entryCountEnc[0:8], tw.entryCount)
			hasher := sha256.New()
			hasher.Write(subtreeRoot[:])
			hasher.Write(entryCountEnc[:])
			hasher.Sum(tw.tableRoot[:0])
		}
		tw.lastSubtreeChunks[level] = tw.newSubtreeChunk(level)
	}
	return nil
}

func (tw *tableWriter) setMeta(meta TableMeta) {
	tw.lock.Lock()
	defer tw.lock.Unlock()

	if tw.hasMeta {
		panic("TableMeta already exists")
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
			panic("TableMeta missing")
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
				Meta:          tw.meta,
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

func (tw *tableWriter) getPhase() uint { //TODO deleted phase?
	tw.lock.Lock()
	defer tw.lock.Unlock()

	return tw.phase
}

type tableHeader struct {
	LevelPointers []uint64
	EntryCount    uint64
	TableRoot     merkle.Value
	Meta          TableMeta
}

type TableMeta struct {
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
