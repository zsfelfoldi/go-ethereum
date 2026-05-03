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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// Config contains the configuration options for NewIndexer.
type Config struct {
	History  uint64 // number of historical blocks to index
	Disabled bool   // disables indexing completely

	// This option enables the checkpoint JSON file generator.
	// If set, the given file will be updated with checkpoint information.
	ExportFileName string

	// expect trie nodes of hash based state scheme in the filtermaps key range;
	// use safe iterator based implementation of DeleteRange that skips them
	HashScheme bool
}

type Params struct {
	tableLevels                []tableLevel
	protocolLevels             []protocolLevel
	fileStorageThresholdHeight uint
}

var DefaultParams = &Params{
	tableLevels: []tableLevel{
		{blockCount: 0x1},
		{blockCount: 0x4},
		{blockCount: 0x10},
		{blockCount: 0x40},
		{blockCount: 0x100},
		{blockCount: 0x400},
		{blockCount: 0x1000},
		{blockCount: 0x4000},
		{blockCount: 0x10000},
		{blockCount: 0x40000},
		{blockCount: 0x100000},
		{blockCount: 0x200000, leanStorage: true},
	},
	protocolLevels: []protocolLevel{
		{tailAge: 5, headAge: 0},
		{tailAge: 20, headAge: 0},
		{tailAge: 80, headAge: 1},
		{tailAge: 320, headAge: 4},
		{tailAge: 8192, headAge: 16},
	},
	fileStorageThresholdHeight: 12,
}

type Indexer struct {
	lock                 sync.Mutex
	params               *Params
	storage              *tableStorage
	requestBlocks        common.Range[uint64]
	currentOp            tableOperation
	headBlock, tailBlock uint64

	shutdown      bool
	updateMergeCh chan struct{}
	mergeWg       sync.WaitGroup
}

func NewIndexer(params *Params, path string) *Indexer {
	fmt.Println("*** PATH", path)
	files, err := newTableFiles(path, 2000000000, 16)
	if err != nil {
		log.Crit("Could not open index table file manager", "error", err) //TODO return?
	}
	storage, err := newTableStorage(params, files)
	if err != nil {
		log.Crit("Could not open index table storage", "error", err)
	}
	ix := &Indexer{
		params:        params,
		storage:       storage,
		updateMergeCh: make(chan struct{}, 1),
	}
	ix.mergeWg.Add(1)
	go ix.mergeLoop()
	return ix
}

func (ix *Indexer) updateActions() {
	completeSet, partialSet := ix.storage.tables()
	currentOp, requestBlocks := ix.params.nextAction(completeSet, partialSet, ix.makeTargetSet(completeSet))
	if currentOp != ix.currentOp {
		ix.currentOp = currentOp
		select {
		case ix.updateMergeCh <- struct{}{}:
		default:
		}
	}
	ix.requestBlocks = requestBlocks
}

func (ix *Indexer) makeTargetSet(complete tableSet) tableSet {
	target := ix.params.rangeTarget(complete, common.SingleRangeSet[uint64](common.NewRange[uint64](ix.tailBlock, ix.headBlock+1-ix.tailBlock)))
	for i, pl := range ix.params.protocolLevels {
		first := max(ix.headBlock, pl.tailAge) - pl.tailAge
		afterLast := max(ix.headBlock+1, pl.headAge) - pl.headAge
		target[i] = target[i].Union(common.SingleRangeSet[uint64](common.NewRange[uint64](first, afterLast-first)))
	}
	return target
}

func (ix *Indexer) GetIndexRoots(blockNumber uint64, parentHash common.Hash, parentRoots []byte, transactions types.Transactions, receipts types.Receipts) ([]byte, error) {
	ix.storage.deleteTablesFromBlock(blockNumber)
	roots := make([]byte, common.HashLength*len(ix.params.protocolLevels))
	entries := txAndLogEntries(blockNumber, transactions, receipts)
	tw, err := ix.storage.addNewTableWriter(tableID{level: 0, index: blockNumber}, uint64(len(entries)))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := tw.addEntry(&entry); err != nil {
			ix.storage.deleteTable(tableID{level: 0, index: blockNumber})
			return nil, err
		}
	}
	tableRoot := tw.getTableRoot()
	updateIndexRoot(roots[:common.HashLength], tableRoot[:])
	for i := 1; i < len(ix.params.protocolLevels); i++ {
		blockCount := ix.params.tableLevels[i].blockCount
		headAge := ix.params.protocolLevels[i].headAge
		if blockNumber >= headAge && (blockNumber-headAge)%blockCount == blockCount-1 { //TODO fork block
			id := tableID{level: i, index: (blockNumber - headAge) / blockCount}
			tr, err := ix.storage.waitTableReader(id)
			if err != nil {
				return nil, err
			}
			updateIndexRoot(roots[common.HashLength*i:common.HashLength*(i+1)], tr.tableRoot[:])
		}
	}
	return roots, nil
}

func updateIndexRoot(rootSection, newTableRoot []byte) {
	hasher := sha256.New()
	hasher.Write(rootSection)
	hasher.Write(newTableRoot)
	var result common.Hash
	hasher.Sum(result[:0])
	copy(rootSection, result[:])
}

func (ix *Indexer) AddBlockData(header *types.Header, body *types.Body, receipts types.Receipts) (ready bool, needBlocks common.Range[uint64]) {
	//TODO headBlock, tailBlock
	return ix.Status()
}

func (ix *Indexer) Revert(blockNumber uint64) {
	ix.storage.deleteTablesFromBlock(blockNumber + 1)
}

func (ix *Indexer) Status() (ready bool, needBlocks common.Range[uint64]) {
	ix.lock.Lock()
	defer ix.lock.Unlock()

	return !ix.requestBlocks.IsEmpty(), ix.requestBlocks
}

func (ix *Indexer) SetHistoryCutoff(blockNumber uint64) {}
func (ix *Indexer) SetFinalized(blockNumber uint64)     {}
func (ix *Indexer) Suspended()                          {}

func (ix *Indexer) Stop() {
	ix.lock.Lock()
	ix.shutdown = true
	ix.lock.Unlock()
	select {
	case ix.updateMergeCh <- struct{}{}:
	default:
	}
	ix.mergeWg.Wait()
	ix.storage.close()
}
