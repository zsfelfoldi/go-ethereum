// Copyright 2025 The go-ethereum Authors
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

package filtermaps

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/types"
)

type TestDataset struct {
	tree        *memTreeView
	blockPtrs   []uint64
	logsFn      func(block uint64) []*types.Log
	params      *Params
	Description string      `json:"description"`
	HeadBlock   uint64      `json:"head_block"`
	NextIndex   uint64      `json:"next_index"`
	RootHash    common.Hash `json:"root_hash"`
}

type TestQuery struct {
	Description string           `json:"description"`
	FromBlock   int64            `json:"fromBlock"`
	ToBlock     int64            `json:"toBlock"`
	Address     []common.Address `json:"address"`
	Topics      [][]common.Hash  `json:"topics"`
	Results     []*types.Log     `json:"results"`
}

func MakeTestDataset(desc string, blockCount uint64, logsFn func(block uint64) []*types.Log) *TestDataset {
	fmt.Println("Generating test dataset:", desc)
	mt := &memTree{roots: make(map[uint64]memTreeRoot)}
	tree := mt.newWriter(0, common.Hash{})
	dataset := &TestDataset{
		params:      &DefaultParams,
		tree:        tree,
		blockPtrs:   make([]uint64, blockCount),
		logsFn:      logsFn,
		Description: desc,
		HeadBlock:   blockCount - 1,
	}
	h := &Hasher{
		tree:            tree,
		params:          dataset.params,
		rowMappingCache: lru.NewCache[common.Hash, lvPosition](cachedRowMappings),
	}
	h.params.deriveFields()
	h.InitGenesis()
	for block := range blockCount {
		logs := logsFn(block)
		for _, log := range logs {
			_, dataset.NextIndex = h.AddLogEvent(log)
		}
		if block < blockCount-1 {
			dataset.blockPtrs[block], dataset.NextIndex = h.AddBlockDelimiter(&types.Header{Number: big.NewInt(int64(block))})
		} else {
			dataset.blockPtrs[block] = dataset.NextIndex
		}
	}
	fmt.Println("  number of blocks:", blockCount)
	fmt.Println("  number of log value entries:", dataset.NextIndex)
	fmt.Println("  number of tree nodes:", tree.tree.nodeCount)
	fmt.Println("  number of leaf nodes:", tree.tree.knownNodes())
	dataset.RootHash = tree.rootHash()
	if tree.tree.knownNodes() != tree.tree.nodeCount {
		panic("some tree nodes not hashed")
	}
	fmt.Println("  log index root hash:", dataset.RootHash)
	fmt.Println()
	return dataset
}

func (d *TestDataset) Run(query *TestQuery, filename string) {
	fmt.Println("Generating test query proof:", query.Description)
	fq := &filterQuery{
		firstBlock: uint64(query.FromBlock),
		lastBlock:  uint64(query.ToBlock),
		addresses:  query.Address,
		topics:     query.Topics,
	}
	for block := range d.HeadBlock + 1 {
		for _, log := range d.logsFn(block) {
			if fq.match(log) {
				query.Results = append(query.Results, log)
			}
		}
	}
	fmt.Println("  expected number of results:", len(query.Results))
	var firstIndex uint64
	if fq.firstBlock > 0 {
		firstIndex = d.blockPtrs[fq.firstBlock-1]
	}
	mp := d.params.proveQuery(d.tree, fq, firstIndex, d.blockPtrs[fq.lastBlock])
	fmt.Println("  leaf nodes:", len(mp.leaves))
	fmt.Println("  proof nodes:", len(mp.proof))
	proofData := make([]byte, 16+48*len(mp.leaves)+32*len(mp.proof))
	binary.LittleEndian.PutUint64(proofData[0:8], uint64(len(mp.leaves)))
	binary.LittleEndian.PutUint64(proofData[8:16], uint64(len(mp.proof)))
	ptr := 16
	for _, index := range mp.leafIndices {
		binary.LittleEndian.PutUint64(proofData[ptr:ptr+8], index.lo)
		binary.LittleEndian.PutUint64(proofData[ptr+8:ptr+16], index.hi)
		ptr += 16
	}
	for _, node := range mp.leaves {
		copy(proofData[ptr:ptr+32], node[:])
		ptr += 32
	}
	for _, node := range mp.proof {
		copy(proofData[ptr:ptr+32], node[:])
		ptr += 32
	}
	fmt.Println("  proof size:", len(proofData), "bytes")
	fmt.Println("Verifying proof...")
	logs, err := d.params.verifyProof(mp, fq, d.RootHash, d.HeadBlock)
	if err != nil {
		panic(err)
	}
	fmt.Println("  number of results:", len(logs))
	if len(logs) != len(query.Results) {
		panic("results mismatch")
	}
	/*for i, log := range logs {
		if !reflect.DeepEqual(log, query.Results[i]) {
			fmt.Println("q", *query.Results[i])
			fmt.Println("p", *log)
			panic("results mismatch")
		}
	}*/ //TODO
	fmt.Println("Writing test case files:", filename)

	type testDataAndQuery struct {
		Data  *TestDataset `json:"data"`
		Query *TestQuery   `json:"query"`
	}
	meta := testDataAndQuery{
		Data:  d,
		Query: query,
	}
	metaJson, _ := json.Marshal(&meta)
	f, _ := os.Create(filename + ".json")
	f.Write(metaJson)
	f.Close()
	f, _ = os.Create(filename + ".proof")
	f.Write(proofData)
	f.Close()
	fmt.Println()
}
