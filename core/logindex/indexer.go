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
	path       string
	initTables bool
	tables     []tableInfo
}

func NewIndexer(path string) *Indexer {
	fmt.Println("*** PATH", path)
	i := &Indexer{
		path: path,
	}
	i.readTableList()
	return i
}

type tableInfo struct {
	fileName string
	meta     tableMeta
}

func (ix *Indexer) readTableList() {
	entries, err := os.ReadDir(ix.path)
	if err != nil {
		log.Error("Failed to scan log index directory", "error", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := filepath.Join(ix.path, entry.Name())
		tr, err := newTableReader(fileName)
		if err != nil {
			log.Error("Failed to open index table (removed)", "file", fileName, "error", err)
			os.Remove(fileName)
			continue
		}
		ix.tables = append(ix.tables, tableInfo{
			fileName: fileName,
			meta:     tr.meta,
		})
		tr.close()
	}
	if len(ix.tables) == 0 {
		return
	}
	sort.Slice(ix.tables, func(i, j int) bool {
		return ix.tables[i].meta.LastBlockNumber < ix.tables[j].meta.LastBlockNumber
	})
	ix.initTables = true
}

func (ix *Indexer) lastTableConfirmed() {

}

func (ix *Indexer) GetIndexRoots(parentHash common.Hash, transactions types.Transactions, receipts types.Receipts) []byte {
	return nil
}

func (ix *Indexer) AddBlockData(header *types.Header, body *types.Body, receipts types.Receipts) (ready bool, needBlocks common.Range[uint64]) {
	if ix.initTables {
		lastTable := ix.tables[len(ix.tables)-1]
		if header.Number.Uint64() == lastTable.meta.LastBlockNumber {
			if header.Hash() == lastTable.meta.LastBlockHash {
				ix.lastTableConfirmed()
			} else {
				log.Warn("Last index table not canonical (removed)", "lastBlockNumber", lastTable.meta.LastBlockNumber, "lastBlockHash", lastTable.meta.LastBlockHash)
				os.Remove(lastTable.fileName)
				ix.tables = ix.tables[:len(ix.tables)-1]
				ix.initTables = len(ix.tables) != 0
			}
		}
		return ix.Status()
	}
	return ix.Status()
}

func (ix *Indexer) Revert(blockNumber uint64) {}

func (ix *Indexer) Status() (ready bool, needBlocks common.Range[uint64]) {
	if ix.initTables {
		return true, common.NewRange[uint64](ix.tables[len(ix.tables)-1].meta.LastBlockNumber, 1)
	}
	return false, common.Range[uint64]{}
}

func (ix *Indexer) SetHistoryCutoff(blockNumber uint64) {}
func (ix *Indexer) SetFinalized(blockNumber uint64)     {}
func (ix *Indexer) Suspended()                          {}
func (ix *Indexer) Stop()                               {}
