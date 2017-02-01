// Copyright 2014 The go-ethereum Authors
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

package filters

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/net/context"
)

type Backend interface {
	ChainDb() ethdb.Database
	EventMux() *event.TypeMux
	HeaderByNumber(ctx context.Context, blockNr rpc.BlockNumber) (*types.Header, error)
	GetReceipts(ctx context.Context, blockHash common.Hash) (types.Receipts, error)
	GetBloomBits(ctx context.Context, bitIdx uint64, sectionIdxList []uint64) ([][]byte, error)
}

// Filter can be used to retrieve and filter logs.
type Filter struct {
	backend                 Backend
	useMipMap, useBloomBits bool

	created time.Time

	db             ethdb.Database
	begin, end     int64
	addresses      []common.Address
	topics         [][]common.Hash
	addressIndexes []types.BloomIndexList
	topicIndexes   [][]types.BloomIndexList
}

// New creates a new filter which uses a bloom filter on blocks to figure out whether
// a particular block is interesting or not.
// MipMaps allow past blocks to be searched much more efficiently, but are not available
// to light clients.
func New(backend Backend, useMipMap bool) *Filter {
	return &Filter{
		backend:      backend,
		useMipMap:    useMipMap,
		useBloomBits: !useMipMap,
		db:           backend.ChainDb(),
	}
}

// SetBeginBlock sets the earliest block for filtering.
// -1 = latest block (i.e., the current block)
// hash = particular hash from-to
func (f *Filter) SetBeginBlock(begin int64) {
	f.begin = begin
}

// SetEndBlock sets the latest block for filtering.
func (f *Filter) SetEndBlock(end int64) {
	f.end = end
}

// SetAddresses matches only logs that are generated from addresses that are included
// in the given addresses.
func (f *Filter) SetAddresses(addr []common.Address) {
	f.addresses = addr
	if f.useBloomBits {
		f.addressIndexes = make([]types.BloomIndexList, len(addr))
		for i, b := range addr {
			f.addressIndexes[i] = types.BloomIndexes(b.Bytes())
		}
	}
}

// SetTopics matches only logs that have topics matching the given topics.
func (f *Filter) SetTopics(topics [][]common.Hash) {
	f.topics = topics
	if f.useBloomBits {
		f.topicIndexes = make([][]types.BloomIndexList, len(topics))
		for j, topicList := range topics {
			f.topicIndexes[j] = make([]types.BloomIndexList, len(topicList))
			for i, b := range topicList {
				f.topicIndexes[j][i] = types.BloomIndexes(b.Bytes())
			}
		}
	}
}

// FindOnce searches the blockchain for matching log entries, returning
// all matching entries from the first block that contains matches,
// updating the start point of the filter accordingly. If no results are
// found, a nil slice is returned.
func (f *Filter) FindOnce(ctx context.Context) ([]*types.Log, error) {
	head, _ := f.backend.HeaderByNumber(ctx, rpc.LatestBlockNumber)
	if head == nil {
		return nil, nil
	}
	headBlockNumber := head.Number.Uint64()

	var beginBlockNo uint64 = uint64(f.begin)
	if f.begin == -1 {
		beginBlockNo = headBlockNumber
	}
	var endBlockNo uint64 = uint64(f.end)
	if f.end == -1 {
		endBlockNo = headBlockNumber
	}

	// if no addresses are present we can't make use of fast search which
	// uses the mipmap bloom filters to check for fast inclusion and uses
	// higher range probability in order to ensure at least a false positive
	if !f.useMipMap || len(f.addresses) == 0 {
		logs, blockNumber, err := f.getLogs(ctx, beginBlockNo, endBlockNo)
		f.begin = int64(blockNumber + 1)
		return logs, err
	}

	logs, blockNumber := f.mipFind(beginBlockNo, endBlockNo, 0)
	f.begin = int64(blockNumber + 1)
	return logs, nil
}

// Run filters logs with the current parameters set
func (f *Filter) Find(ctx context.Context) (logs []*types.Log, err error) {
	for {
		newLogs, err := f.FindOnce(ctx)
		if len(newLogs) == 0 || err != nil {
			return logs, err
		}
		logs = append(logs, newLogs...)
	}
}

func (f *Filter) mipFind(start, end uint64, depth int) (logs []*types.Log, blockNumber uint64) {
	level := core.MIPMapLevels[depth]
	// normalise numerator so we can work in level specific batches and
	// work with the proper range checks
	for num := start / level * level; num <= end; num += level {
		// find addresses in bloom filters
		bloom := core.GetMipmapBloom(f.db, num, level)
		// Don't bother checking the first time through the loop - we're probably picking
		// up where a previous run left off.
		first := true
		for _, addr := range f.addresses {
			if first || bloom.TestBytes(addr[:]) {
				first = false
				// range check normalised values and make sure that
				// we're resolving the correct range instead of the
				// normalised values.
				start := uint64(math.Max(float64(num), float64(start)))
				end := uint64(math.Min(float64(num+level-1), float64(end)))
				if depth+1 == len(core.MIPMapLevels) {
					l, blockNumber, _ := f.getLogs(context.Background(), start, end)
					if len(l) > 0 {
						return l, blockNumber
					}
				} else {
					l, blockNumber := f.mipFind(start, end, depth+1)
					if len(l) > 0 {
						return l, blockNumber
					}
				}
			}
		}
	}

	return nil, end
}

func binaryAnd(a, b []byte) bool {
	nonZero := false
	for i, bb := range b {
		aa := a[i] & bb
		if aa != 0 {
			nonZero = true
		}
		a[i] = aa
	}
	return nonZero
}

func binaryOr(a, b []byte) {
	for i, bb := range b {
		a[i] |= bb
	}
}

const (
	bloomSectionSize = 4096
	bloomClusterSize = 16 * bloomSectionSize
)

func decompressBloomBits(bits []byte) []byte {
	if len(bits) == bloomSectionSize/8 {
		// make a copy so that output is always detached from input
		c := make([]byte, bloomSectionSize/8)
		copy(c, bits)
		return c
	}
	dc, ofs := decompressBits(bits, bloomSectionSize/8)
	if ofs != len(bits) {
		panic(nil)
	}
	return dc
}

func decompressBits(bits []byte, targetLen int) ([]byte, int) {
	lb := len(bits)
	dc := make([]byte, targetLen)
	if lb == 0 {
		return dc, 0
	}

	l := targetLen / 8
	var (
		b   []byte
		ofs int
	)
	if l == 1 {
		b = bits[0:1]
		ofs = 1
	} else {
		b, ofs = decompressBits(bits, l)
	}
	for i, _ := range dc {
		if b[i/8]&(1<<byte(7-i%8)) != 0 {
			if ofs == lb {
				panic(nil)
			}
			dc[i] = bits[ofs]
			ofs++
		}
	}
	return dc, ofs
}

func (f *Filter) bitFilterGroup(ctx context.Context, sections []uint64, indexes []types.BloomIndexList) ([][]byte, bool) {
	bits := make(map[uint][][]byte)
	var bitCnt int
	type returnRec struct {
		idx  uint
		data [][]byte
	}
	returnChn := make(chan returnRec)

	for _, idxs := range indexes {
		for _, idx := range idxs {
			if _, ok := bits[idx]; !ok {
				bits[idx] = nil
				bitCnt++

				go func(idx uint) {
					fmt.Println("get", len(sections))
					data, err := f.backend.GetBloomBits(ctx, uint64(idx), sections)
					fmt.Println("received", len(sections), len(data), err)
					var bits [][]byte
					if err == nil {
						bits = make([][]byte, len(data))
						for i, compBits := range data {
							bits[i] = decompressBloomBits(compBits)
						}
					}
					returnChn <- returnRec{idx, bits}
				}(idx)
			}
		}
	}

	fail := false
	for i := 0; i < bitCnt; i++ {
		r := <-returnChn
		bits[r.idx] = r.data
		if len(r.data) != len(sections) {
			fail = true
		}
	}
	if fail {
		return nil, false
	}

	var orVector [][]byte
	for _, idxs := range indexes {
		andVector := bits[idxs[0]]
		nonZero := false
		for i, _ := range andVector {
			if binaryAnd(andVector[i], bits[idxs[1]][i]) && binaryAnd(andVector[i], bits[idxs[2]][i]) {
				nonZero = true
				if orVector != nil {
					binaryOr(orVector[i], andVector[i])
				}
			}
		}
		if orVector == nil && nonZero {
			orVector = andVector
		}

	}
	return orVector, true
}

func (f *Filter) bitFilter(ctx context.Context, sections []uint64) ([][]byte, bool) {
	var (
		ok        bool
		andVector [][]byte
	)

	if len(f.addresses) > 0 {
		andVector, ok = f.bitFilterGroup(ctx, sections, f.addressIndexes)
		if !ok {
			return nil, false
		}
		if andVector == nil {
			return nil, true
		}
	}

loop:
	for j, topics := range f.topics {
		if len(topics) == 0 {
			return nil, true
		}
		for _, topic := range topics {
			if (topic == common.Hash{}) {
				continue loop
			}
		}

		var (
			sectionList []uint64
			idxList     []int
		)
		for i, v := range andVector {
			if v != nil {
				sectionList = append(sectionList, sections[i])
				idxList = append(idxList, i)
			}
		}

		v, ok := f.bitFilterGroup(ctx, sectionList, f.topicIndexes[j])
		if !ok {
			return nil, false
		}
		if v == nil {
			return nil, true
		}
		zero := true
		for i, idx := range idxList {
			if binaryAnd(andVector[idx], v[i]) {
				zero = false
			}
		}
		if zero {
			return nil, true
		}
	}

	if andVector == nil {
		// we did nothing, it was a "match all" filter
		all := bytes.Repeat([]byte{255}, bloomSectionSize/8)
		andVector := make([][]byte, len(sections))
		for i, _ := range andVector {
			andVector[i] = all
		}
	}

	return andVector, true
}

func (f *Filter) getLogsCluster(ctx context.Context, clusterIdx, start, end uint64) (logs []*types.Log, blockNumber uint64, err error) {
	startSection := start / bloomSectionSize
	endSection := end / bloomSectionSize
	fmt.Println(start, end, startSection, endSection)
	sections := make([]uint64, endSection+1-startSection)
	for i, _ := range sections {
		sections[i] = startSection + uint64(i)
	}
	match, ok := f.bitFilter(ctx, sections)
	if !ok {
		return f.getLogsClassic(ctx, start, end)
	}
	if match == nil {
		return nil, end, nil
	}

	i := start
loop:
	for i <= end {
		bitIdx := uint64(i - startSection*bloomSectionSize)
		byteIdx := bitIdx / 8
		arr := match[byteIdx/(bloomSectionSize/8)]
		arrIdx := byteIdx % (bloomSectionSize / 8)
		if arr == nil {
			i += bloomSectionSize - arrIdx
			continue loop
		}
		if arr[arrIdx]&(1<<(7-bitIdx%8)) != 0 {
			// Get the logs of the block
			blockNumber := rpc.BlockNumber(i)
			header, err := f.backend.HeaderByNumber(ctx, blockNumber)
			if header == nil || err != nil {
				return nil, end, err
			}
			receipts, err := f.backend.GetReceipts(ctx, header.Hash())
			if err != nil {
				return nil, end, err
			}
			var unfiltered []*types.Log
			for _, receipt := range receipts {
				unfiltered = append(unfiltered, ([]*types.Log)(receipt.Logs)...)
			}
			logs = filterLogs(unfiltered, nil, nil, f.addresses, f.topics)
			fmt.Println("bloom match at", i, "   len(unfiltered) =", len(unfiltered), "   len(logs) =", len(logs), "   bloomMatch:", f.bloomFilter(header.Bloom))
			if len(logs) > 0 {
				return logs, uint64(blockNumber), nil
			}
		}
		i++
	}

	return logs, end, nil
}

func (f *Filter) getLogs(ctx context.Context, start, end uint64) (logs []*types.Log, blockNumber uint64, err error) {
	if !f.useBloomBits {
		return f.getLogsClassic(ctx, start, end)
	}

	type returnRec struct {
		logs                    []*types.Log
		blockNumber, clusterIdx uint64
		err                     error
	}

	startCluster := start / bloomClusterSize
	endCluster := end / bloomClusterSize

	if startCluster == endCluster {
		return f.getLogsCluster(ctx, startCluster, start, end)
	}

	workers := endCluster + 1 - startCluster
	if workers > 10 {
		workers = 10
	}
	startChn := make(chan uint64)
	stopChn := make(chan struct{})
	returnChn := make(chan returnRec, workers)

	defer close(stopChn)

	for i := 0; i < int(workers); i++ {
		go func() {
			for i := range startChn {
				s := i * bloomClusterSize
				if start > s {
					s = start
				}
				e := (i+1)*bloomClusterSize - 1
				if end < e {
					e = end
				}
				logs, blockNumber, err := f.getLogsCluster(ctx, i, s, e)
				returnChn <- returnRec{logs, blockNumber, i, err}
				if len(logs) > 0 || err != nil {
					return
				}
			}
		}()
	}

	go func() {
		defer close(startChn)
		for i := startCluster; i <= endCluster; i++ {
			select {
			case startChn <- i:
			case <-ctx.Done():
				return
			case <-stopChn:
				return
			}
		}
	}()

	results := make(map[uint64]returnRec)

	for i := startCluster; i <= endCluster; i++ {
		ret, ok := results[i]
		if ok {
			delete(results, i)
		} else {
		loop:
			for {
				select {
				case ret = <-returnChn:
					if ret.clusterIdx == i {
						break loop
					} else {
						results[ret.clusterIdx] = ret
					}
				case <-ctx.Done():
					return
				}
			}
		}

		if ret.err != nil {
			return ret.logs, end, ret.err

		}
		if len(ret.logs) > 0 {
			return ret.logs, ret.blockNumber, nil
		}
	}

	return nil, end, nil
}

func (f *Filter) getLogsClassic(ctx context.Context, start, end uint64) (logs []*types.Log, blockNumber uint64, err error) {

	for i := start; i <= end; i++ {
		blockNumber := rpc.BlockNumber(i)
		header, err := f.backend.HeaderByNumber(ctx, blockNumber)
		if header == nil || err != nil {
			return logs, end, err
		}

		// Use bloom filtering to see if this block is interesting given the
		// current parameters
		if f.bloomFilter(header.Bloom) {
			// Get the logs of the block
			receipts, err := f.backend.GetReceipts(ctx, header.Hash())
			if err != nil {
				return nil, end, err
			}
			var unfiltered []*types.Log
			for _, receipt := range receipts {
				unfiltered = append(unfiltered, ([]*types.Log)(receipt.Logs)...)
			}
			logs = filterLogs(unfiltered, nil, nil, f.addresses, f.topics)
			if len(logs) > 0 {
				return logs, uint64(blockNumber), nil
			}
		}
	}

	return logs, end, nil
}

func includes(addresses []common.Address, a common.Address) bool {
	for _, addr := range addresses {
		if addr == a {
			return true
		}
	}

	return false
}

// filterLogs creates a slice of logs matching the given criteria.
func filterLogs(logs []*types.Log, fromBlock, toBlock *big.Int, addresses []common.Address, topics [][]common.Hash) []*types.Log {
	var ret []*types.Log
Logs:
	for _, log := range logs {
		if fromBlock != nil && fromBlock.Int64() >= 0 && fromBlock.Uint64() > log.BlockNumber {
			continue
		}
		if toBlock != nil && toBlock.Int64() >= 0 && toBlock.Uint64() < log.BlockNumber {
			continue
		}

		if len(addresses) > 0 && !includes(addresses, log.Address) {
			continue
		}

		logTopics := make([]common.Hash, len(topics))
		copy(logTopics, log.Topics)

		// If the to filtered topics is greater than the amount of topics in logs, skip.
		if len(topics) > len(log.Topics) {
			continue Logs
		}

		for i, topics := range topics {
			var match bool
			for _, topic := range topics {
				// common.Hash{} is a match all (wildcard)
				if (topic == common.Hash{}) || log.Topics[i] == topic {
					match = true
					break
				}
			}

			if !match {
				continue Logs
			}
		}
		ret = append(ret, log)
	}

	return ret
}

func (f *Filter) bloomFilter(bloom types.Bloom) bool {
	return bloomFilter(bloom, f.addresses, f.topics)
}

func bloomFilter(bloom types.Bloom, addresses []common.Address, topics [][]common.Hash) bool {
	if len(addresses) > 0 {
		var included bool
		for _, addr := range addresses {
			if types.BloomLookup(bloom, addr) {
				included = true
				break
			}
		}

		if !included {
			return false
		}
	}

	for _, sub := range topics {
		var included bool
		for _, topic := range sub {
			if (topic == common.Hash{}) || types.BloomLookup(bloom, topic) {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}

	return true
}
