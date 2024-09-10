package filtermaps

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

type FilterRow []uint32

// Strictly monotonically increasing list of lv indices in the range of a filter
// map that are potential matches of certain filter criteria.
// nil means that all lv indices in the filter map range are potential matches.
type potentialMatches []uint64

const (
	logMapHeight    = 12
	mapHeight       = 1 << logMapHeight
	logMapsPerEpoch = 6
	mapsPerEpoch    = 1 << logMapsPerEpoch
	logValuesPerMap = 16
	valuesPerMap    = 1 << logValuesPerMap

	headCacheSize = 8
)

type FilterMaps struct {
	lock    sync.RWMutex
	db      ethdb.KeyValueStore
	closeCh chan chan struct{}

	filterMapsRange
	chain          *core.BlockChain
	filterMapCache map[uint32]*filterMap
	blockPtrCache  *lru.Cache[uint32, uint64]
	lvPointerCache *lru.Cache[uint64, uint64]
	revertPoints   map[uint64]*revertPoint
}

type filterMap [mapHeight]FilterRow

type filterMapsRange struct {
	initialized                      bool
	headLvPointer, tailLvPointer     uint64
	headBlockNumber, tailBlockNumber uint64
	headBlockHash, tailParentHash    common.Hash
}

func NewFilterMaps(db ethdb.KeyValueStore, chain *core.BlockChain) *FilterMaps {
	encRange, err := rawdb.ReadFilterMapsRange(db)
	var rs filterMapsRangeForStorage
	if err == nil {
		if err := rlp.DecodeBytes(encRange, &rs); err != nil {
			log.Crit("Failed to decode filter maps range", "error", err)
		}
	}
	fm := &FilterMaps{
		db:      db,
		chain:   chain,
		closeCh: make(chan chan struct{}),
		filterMapsRange: filterMapsRange{
			initialized:     rs.Initialized,
			headLvPointer:   rs.HeadLvPointer,
			tailLvPointer:   rs.TailLvPointer,
			headBlockNumber: rs.HeadBlockNumber,
			tailBlockNumber: rs.TailBlockNumber,
			headBlockHash:   rs.HeadBlockHash,
			tailParentHash:  rs.TailParentHash,
		},
		filterMapCache: make(map[uint32]*filterMap),
		blockPtrCache:  lru.NewCache[uint32, uint64](1000),
		lvPointerCache: lru.NewCache[uint64, uint64](1000),
		revertPoints:   make(map[uint64]*revertPoint),
	}
	fm.updateMapCache()
	go fm.updateLoop()
	return fm
}

func (f *FilterMaps) Close() {
	ch := make(chan struct{})
	f.closeCh <- ch
	<-ch
}

type filterMapsRangeForStorage struct {
	Initialized                      bool
	HeadLvPointer, TailLvPointer     uint64
	HeadBlockNumber, TailBlockNumber uint64
	HeadBlockHash, TailParentHash    common.Hash
}

// assumes locked mutex
func (f *FilterMaps) setRange(batch ethdb.Batch, newRange filterMapsRange) {
	f.filterMapsRange = newRange
	rs := filterMapsRangeForStorage{
		Initialized:     newRange.initialized,
		HeadLvPointer:   newRange.headLvPointer,
		TailLvPointer:   newRange.tailLvPointer,
		HeadBlockNumber: newRange.headBlockNumber,
		TailBlockNumber: newRange.tailBlockNumber,
		HeadBlockHash:   newRange.headBlockHash,
		TailParentHash:  newRange.tailParentHash,
	}
	encRange, err := rlp.EncodeToBytes(&rs)
	if err != nil {
		log.Crit("Failed to encode filter maps range", "error", err)
	}
	rawdb.WriteFilterMapsRange(batch, encRange)
	f.updateMapCache()
}

// assumes locked mutex
func (f *FilterMaps) updateMapCache() {
	if !f.initialized {
		return
	}
	newFilterMapCache := make(map[uint32]*filterMap)
	firstMap, afterLastMap := uint32(f.tailLvPointer>>logValuesPerMap), uint32((f.headLvPointer+valuesPerMap-1)>>logValuesPerMap)
	headCacheFirst := firstMap + 1
	if afterLastMap > headCacheFirst+headCacheSize {
		headCacheFirst = afterLastMap - headCacheSize
	}
	fm := f.filterMapCache[firstMap]
	if fm == nil {
		fm = new(filterMap)
	}
	newFilterMapCache[firstMap] = fm
	for mapIndex := headCacheFirst; mapIndex < afterLastMap; mapIndex++ {
		fm := f.filterMapCache[mapIndex]
		if fm == nil {
			fm = new(filterMap)
		}
		newFilterMapCache[mapIndex] = fm
	}
	f.filterMapCache = newFilterMapCache
}

func (f *FilterMaps) GetLogByLvIndex(lvIndex uint64) (*types.Log, error) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if lvIndex < f.tailLvPointer || lvIndex > f.headLvPointer {
		return nil, errors.New("log value index outside available range")
	}
	mapIndex := uint32(lvIndex >> logValuesPerMap)
	firstBlockNumber, err := f.getMapBlockPtr(mapIndex)
	if err != nil {
		return nil, err
	}
	var lastBlockNumber uint64
	if mapIndex+1 < uint32((f.headLvPointer+valuesPerMap-1)>>logValuesPerMap) {
		lastBlockNumber, err = f.getMapBlockPtr(mapIndex + 1)
		if err != nil {
			return nil, err
		}
	} else {
		lastBlockNumber = f.headBlockNumber
	}
	for firstBlockNumber < lastBlockNumber {
		midBlockNumber := (firstBlockNumber + lastBlockNumber + 1) / 2
		midLvPointer, err := f.GetBlockLvPointer(midBlockNumber)
		if err != nil {
			return nil, err
		}
		if lvIndex < midLvPointer {
			lastBlockNumber = midBlockNumber - 1
		} else {
			firstBlockNumber = midBlockNumber
		}
	}
	hash := f.chain.GetCanonicalHash(firstBlockNumber)
	receipts := f.chain.GetReceiptsByHash(hash) //TODO small cache
	if receipts == nil {
		return nil, errors.New("receipts not found")
	}
	lvPointer, err := f.GetBlockLvPointer(firstBlockNumber)
	if err != nil {
		return nil, err
	}
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			if lvPointer > lvIndex {
				return nil, errors.New("log value index not found")
			}
			if lvPointer == lvIndex {
				return log, nil
			}
			lvPointer += uint64(len(log.Topics) + 1)
		}
	}
	return nil, errors.New("log value index not found")
}

func (f *FilterMaps) GetBlockLvPointer(blockNumber uint64) (uint64, error) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	if lvPointer, ok := f.lvPointerCache.Get(blockNumber); ok {
		return lvPointer, nil
	}
	lvPointer, err := rawdb.ReadBlockLvPointer(f.db, blockNumber)
	if err != nil {
		return 0, err
	}
	f.lvPointerCache.Add(blockNumber, lvPointer)
	return lvPointer, nil
}

func (f *FilterMaps) storeBlockLvPointer(batch ethdb.Batch, blockNumber, lvPointer uint64) {
	f.lvPointerCache.Add(blockNumber, lvPointer)
	rawdb.WriteBlockLvPointer(batch, blockNumber, lvPointer)
}

func (f *FilterMaps) deleteBlockLvPointer(batch ethdb.Batch, blockNumber uint64) {
	f.lvPointerCache.Remove(blockNumber)
	rawdb.DeleteBlockLvPointer(batch, blockNumber)
}

func (f *FilterMaps) GetFilterMapRow(ctx context.Context, mapIndex, rowIndex uint32) (FilterRow, error) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.getFilterMapRow(mapIndex, rowIndex)
}

func (f *FilterMaps) getFilterMapRowLocked(mapIndex, rowIndex uint32) (FilterRow, error) {
	f.lock.RLock()
	defer f.lock.RUnlock()

	return f.getFilterMapRow(mapIndex, rowIndex)
}

// read only; copy before modification
// for empty row it returns zero length non-nil slice
func (f *FilterMaps) getFilterMapRow(mapIndex, rowIndex uint32) (FilterRow, error) {
	if mapIndex < uint32(f.tailLvPointer>>logValuesPerMap) ||
		mapIndex >= uint32((f.headLvPointer+valuesPerMap-1)>>logValuesPerMap) {
		return make(FilterRow, 0, valuesPerMap/mapHeight), nil
	}
	fm := f.filterMapCache[mapIndex]
	if fm != nil && fm[rowIndex] != nil {
		return fm[rowIndex], nil
	}
	encRow, err := rawdb.ReadFilterMapRow(f.db, toMapRowIndex(mapIndex, rowIndex))
	if err != nil {
		return nil, err
	}
	var filterRow FilterRow
	if len(encRow) > 0 {
		filterRow, err = decodeRow(encRow)
		if err != nil {
			return nil, err
		}
	} else {
		filterRow = make(FilterRow, 0, valuesPerMap/mapHeight)
	}
	if fm != nil {
		fm[rowIndex] = filterRow
	}
	return filterRow, nil
}

func (f *FilterMaps) storeFilterMapRow(batch ethdb.Batch, mapIndex, rowIndex uint32, row FilterRow) {
	if fm := f.filterMapCache[mapIndex]; fm != nil {
		(*fm)[rowIndex] = row
	}
	if len(row) > 0 {
		rawdb.WriteFilterMapRow(batch, toMapRowIndex(mapIndex, rowIndex), encodeRow(row))
	} else {
		rawdb.DeleteFilterMapRow(batch, toMapRowIndex(mapIndex, rowIndex))
	}
}

func (f *FilterMaps) getMapBlockPtr(mapIndex uint32) (uint64, error) {
	if blockPtr, ok := f.blockPtrCache.Get(mapIndex); ok {
		return blockPtr, nil
	}
	blockPtr, err := rawdb.ReadFilterMapBlockPtr(f.db, mapIndex)
	if err != nil {
		return 0, err
	}
	f.blockPtrCache.Add(mapIndex, blockPtr)
	return blockPtr, nil
}

func (f *FilterMaps) storeMapBlockPtr(batch ethdb.Batch, mapIndex uint32, blockPtr uint64) {
	f.blockPtrCache.Add(mapIndex, blockPtr)
	rawdb.WriteFilterMapBlockPtr(batch, mapIndex, blockPtr)
}

func (f *FilterMaps) deleteMapBlockPtr(batch ethdb.Batch, mapIndex uint32) {
	f.blockPtrCache.Remove(mapIndex)
	rawdb.DeleteFilterMapBlockPtr(batch, mapIndex)
}

func toMapRowIndex(mapIndex, rowIndex uint32) uint64 {
	epochIndex, mapSubIndex := mapIndex>>logMapsPerEpoch, mapIndex&(mapsPerEpoch-1)
	return (uint64(epochIndex)<<logMapHeight+uint64(rowIndex))<<logMapsPerEpoch + uint64(mapSubIndex)
}

func fromMapRowIndex(mapRowIndex uint64) (mapIndex, rowIndex uint32) {
	mapSubIndex := uint32(mapRowIndex & (mapsPerEpoch - 1))
	mapRowIndex >>= logMapsPerEpoch
	rowIndex = uint32(mapRowIndex & (mapHeight - 1))
	epochIndex := uint32(mapRowIndex >> logMapHeight)
	return epochIndex<<logMapsPerEpoch + mapSubIndex, rowIndex
}

func encodeRow(row FilterRow) []byte {
	encRow := make([]byte, len(row)*4)
	for i, c := range row {
		binary.LittleEndian.PutUint32(encRow[i*4:(i+1)*4], c)
	}
	return encRow
}

func decodeRow(encRow []byte) (FilterRow, error) {
	if len(encRow)&3 != 0 {
		return nil, errors.New("Invalid encoded filter row length")
	}
	row := make(FilterRow, len(encRow)/4)
	for i := range row {
		row[i] = binary.LittleEndian.Uint32(encRow[i*4 : (i+1)*4])
	}
	return row, nil
}

func columnIndex(lvIndex uint64, logValue common.Hash) uint32 {
	x := uint32(lvIndex % valuesPerMap)
	transformHash := transformHash(uint32(lvIndex/valuesPerMap), logValue)
	x += binary.LittleEndian.Uint32(transformHash[0:4])
	x *= binary.LittleEndian.Uint32(transformHash[4:8])*2 + 1
	x ^= binary.LittleEndian.Uint32(transformHash[8:12])
	x *= binary.LittleEndian.Uint32(transformHash[12:16])*2 + 1
	x += binary.LittleEndian.Uint32(transformHash[16:20])
	x *= binary.LittleEndian.Uint32(transformHash[20:24])*2 + 1
	x ^= binary.LittleEndian.Uint32(transformHash[24:28])
	x *= binary.LittleEndian.Uint32(transformHash[28:32])*2 + 1
	return x
}

func transformHash(mapIndex uint32, logValue common.Hash) (result common.Hash) {
	hasher := sha256.New()
	hasher.Write(logValue[:])
	var indexEnc [4]byte
	binary.LittleEndian.PutUint32(indexEnc[:], mapIndex)
	hasher.Write(indexEnc[:])
	hasher.Sum(result[:0])
	return
}

func rowIndex(epochIndex uint32, logValue common.Hash) uint32 {
	hasher := sha256.New()
	hasher.Write(logValue[:])
	var indexEnc [4]byte
	binary.LittleEndian.PutUint32(indexEnc[:], epochIndex)
	hasher.Write(indexEnc[:])
	var hash common.Hash
	hasher.Sum(hash[:0])
	return binary.LittleEndian.Uint32(hash[:4]) % mapHeight
}

// sorted, no doubles
func (row FilterRow) potentialMatches(mapIndex uint32, logValue common.Hash) (results potentialMatches) {
	transformHash := transformHash(mapIndex, logValue)
	sub1 := binary.LittleEndian.Uint32(transformHash[0:4])
	mul1 := uint32ModInverse(binary.LittleEndian.Uint32(transformHash[4:8])*2 + 1)
	xor1 := binary.LittleEndian.Uint32(transformHash[8:12])
	mul2 := uint32ModInverse(binary.LittleEndian.Uint32(transformHash[12:16])*2 + 1)
	sub2 := binary.LittleEndian.Uint32(transformHash[16:20])
	mul3 := uint32ModInverse(binary.LittleEndian.Uint32(transformHash[20:24])*2 + 1)
	xor2 := binary.LittleEndian.Uint32(transformHash[24:28])
	mul4 := uint32ModInverse(binary.LittleEndian.Uint32(transformHash[28:32])*2 + 1)
	for _, columnIndex := range row {
		if potentialSubIndex := (((((((columnIndex * mul4) ^ xor2) * mul3) - sub2) * mul2) ^ xor1) * mul1) - sub1; potentialSubIndex < valuesPerMap {
			results = append(results, uint64(mapIndex)*valuesPerMap+uint64(potentialSubIndex))
		}
	}
	sort.Sort(results)
	// filter out doubles
	j := 0
	for i, match := range results {
		if i == 0 || match != results[i-1] {
			results[j] = results[i]
			j++
		}
	}
	return results[:j]
}

func (p potentialMatches) Len() int           { return len(p) }
func (p potentialMatches) Less(i, j int) bool { return p[i] < p[j] }
func (p potentialMatches) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func egcd(a, b uint64) (uint64, uint64) {
	if a == 0 {
		return 0, 1
	}
	y, x := egcd(b%a, a)
	return x - (b/a)*y, y
}

func uint32ModInverse(a uint32) uint32 {
	if a&1 == 0 {
		panic("uint32ModInverse called with even argument")
	}
	x, _ := egcd(uint64(a), 0x100000000)
	return uint32(x)
}

func addressValue(address common.Address) common.Hash {
	var result common.Hash
	hasher := sha256.New() //TODO ???
	hasher.Write(address[:])
	hasher.Sum(result[:0])
	return result
}

func topicValue(topic common.Hash) common.Hash {
	var result common.Hash
	hasher := sha256.New() //TODO ???
	hasher.Write(topic[:])
	hasher.Sum(result[:0])
	return result
}
