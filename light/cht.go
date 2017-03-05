// Copyright 2016 The go-ethereum Authors
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

package light

import (
	"encoding/binary"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/bloombits"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	ChtFrequency     = bloombits.SectionSize
	ChtConfirmations = 2048 // number of confirmations before a server is expected to have the given CHT available
)

var (
	ErrNoTrustedCht     = errors.New("No trusted canonical hash trie")
	ErrNoHeader         = errors.New("Header not found")
	chtCountKey         = []byte("chtCount2") // uint64 big endian
	chtPrefix           = []byte("chtRoot-")  // chtPrefix + chtNum (uint64 big endian) -> trie root hash
	ChtTablePrefix      = "cht-"
	BloomBitsTriePrefix = []byte("bloom")
)

type ChtNode struct {
	Hash common.Hash
	Td   *big.Int
}

func GetChtRoot(db ethdb.Database, num uint64) common.Hash {
	var encNumber [8]byte
	binary.BigEndian.PutUint64(encNumber[:], num)
	data, _ := db.Get(append(chtPrefix, encNumber[:]...))
	return common.BytesToHash(data)
}

func StoreChtRoot(db ethdb.Database, num uint64, root common.Hash) {
	var encNumber [8]byte
	binary.BigEndian.PutUint64(encNumber[:], num)
	db.Put(append(chtPrefix, encNumber[:]...), root[:])
}

func GetChtCount(db ethdb.Database) uint64 {
	data, _ := db.Get(chtCountKey)
	if len(data) == 8 {
		return binary.BigEndian.Uint64(data[:])
	} else {
		return 0
	}
}

func StoreChtCount(db ethdb.Database, cnt uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], cnt)
	db.Put(chtCountKey, data[:])
}

func WriteTrustedCht(db ethdb.Database, num uint64, root common.Hash) {
	StoreChtRoot(db, num, root)
	StoreChtCount(db, num+1)

	if core.GetBloomBitsAvailable(db) <= num {
		core.StoreBloomBitsAvailable(db, num+1)
	}
}

type ChtProcessorBackend struct {
	db ethdb.Database
}

func NewChtProcessor(db ethdb.Database, stop chan struct{}) *core.ChainSectionProcessor {
	return core.NewChainSectionProcessor(&ChtProcessorBackend{db: db}, ChtFrequency, 0, time.Millisecond*100, stop)
}

func (c *ChtProcessorBackend) Process(idx uint64) bool {
	cdb := ethdb.NewTable(c.db, ChtTablePrefix)

	var t *trie.Trie
	if idx > 0 {
		root := GetChtRoot(c.db, idx-1)
		if root == (common.Hash{}) {
			return false
		}
		var err error
		t, err = trie.New(root, cdb)
		if err != nil {
			return false
		}
	} else {
		t, _ = trie.New(common.Hash{}, cdb)
	}

	for num := idx * ChtFrequency; num < (idx+1)*ChtFrequency; num++ {
		hash := core.GetCanonicalHash(c.db, num)
		if hash == (common.Hash{}) {
			return false
		}
		td := core.GetTd(c.db, hash, num)
		if td == nil {
			return false
		}
		var encNumber [8]byte
		binary.BigEndian.PutUint64(encNumber[:], num)
		data, _ := rlp.EncodeToBytes(ChtNode{hash, td})
		t.Update(encNumber[:], data)
	}

	var compSize, decompSize uint64
	for i := uint64(0); i < bloombits.BloomLength; i++ {
		var encKey [10]byte
		binary.BigEndian.PutUint16(encKey[0:2], uint16(i))
		binary.BigEndian.PutUint64(encKey[2:10], idx)
		key := append(BloomBitsTriePrefix, encKey[:]...)
		data, err := core.GetBloomBits(c.db, i, idx)
		if err != nil {
			return false
		}

		decompSize += bloombits.SectionSize / 8
		compSize += uint64(len(data))
		if len(data) > 0 {
			t.Update(key, data)
		} else {
			t.Delete(key)
		}
	}

	root, err := t.Commit()
	if err != nil {
		return false
	} else {
		log.Info("Storing CHT", "idx", idx, "root", root, "compression ratio", float64(compSize)/float64(decompSize))
		StoreChtRoot(c.db, idx, root)
	}

	return true
}

func (c *ChtProcessorBackend) SetStored(count uint64) {
	StoreChtCount(c.db, count)
}

func (c *ChtProcessorBackend) GetStored() uint64 {
	return GetChtCount(c.db)
}
