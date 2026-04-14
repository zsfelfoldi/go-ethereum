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
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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

type Indexer struct {
	storage                   *tableStorage
	lock                      sync.RWMutex
	unknown, valid, mergeable tableSet
	requestBlocks             common.Range[uint64]
	mergeTarget               tableID
	updateMergeCh             chan struct{}
	closeMergeCh              chan chan struct{}
	mergeWg                   sync.WaitGroup
}

func NewIndexer(path string) *Indexer {
	fmt.Println("*** PATH", path)
	storage, allTables, err := newTableStorage(path)
	if err != nil {
		log.Crit("Could not open index table storage", "error", err)
	}
	ix := &Indexer{
		storage:       storage,
		unknown:       allTables,
		updateMergeCh: make(chan struct{}, 1),
		closeMergeCh:  make(chan struct{}),
	}
	ix.mergeWg.Add(1)
	go ix.mergeLoop()
	return ix
}

func (ix *Indexer) GetIndexRoots(blockNumber uint64, parentHash common.Hash, parentRoots []byte, transactions types.Transactions, receipts types.Receipts) []byte {
	ix.deleteTablesFromBlock(blockNumber)
	roots := make([]byte, common.HashLength*len(ix.params.consensusLevels))
	for i := 1; i < len(ix.params.consensusLevels); i++ {
		level := ix.params.consensusLevels[i]
		if blockNumber >= ix.params.consensusBlockAges[i] { //TODO fork block
			id := tableID{level: level, index: (blockNumber - ix.params.consensusBlockAges[i]) >> level}
			for {
				tr, err := tr.ix.storage.getTableReader(id)
				if err != nil {
					log.Error("")
					return nil
				}
				if tr == nil {
					//				if ch :=
					return nil //
				}
			}
		}
	}

	entries := txAndLogEntries(blockNumber, transactions, receipts)

	return nil
}

func (ix *Indexer) AddBlockData(header *types.Header, body *types.Body, receipts types.Receipts) (ready bool, needBlocks common.Range[uint64]) {
	return ix.Status()
}

func (ix *Indexer) Revert(blockNumber uint64) {}

func (ix *Indexer) Status() (ready bool, needBlocks common.Range[uint64]) {
	i.lock.RLock()
	defer i.lock.RUnlock()

	return !i.requestBlocks.IsEmpty(), i.requestBlocks
}

func (ix *Indexer) SetHistoryCutoff(blockNumber uint64) {}
func (ix *Indexer) SetFinalized(blockNumber uint64)     {}
func (ix *Indexer) Suspended()                          {}

func (ix *Indexer) Stop() {
	close(ix.closeMergeCh)
	ix.mergeWg.Wait()
	ix.storage.close()
}

func (ix *Indexer) updateMerge() {
	select {
	case ix.updateMergeCh <- struct{}{}:
	default:
	}
}
