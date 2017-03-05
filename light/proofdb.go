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

package light

import (
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
)

// implements trie.Database
type ProofDb struct {
	db                                map[string][]byte
	dataSize                          int
	lock                              sync.RWMutex
	fallback                          trie.Database
	copyFromFallback, writeToFallback bool
}

func NewProofDb() *ProofDb {
	return &ProofDb{
		db: make(map[string][]byte),
	}
}

func (db *ProofDb) SetFallback(fallback trie.Database, copyFromFallback, writeToFallback bool) {
	db.lock.Lock()
	defer db.lock.Unlock()

	db.fallback = fallback
	db.copyFromFallback = copyFromFallback
	db.writeToFallback = writeToFallback
}

func (db *ProofDb) Put(key []byte, value []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if _, ok := db.db[string(key)]; !ok {
		db.db[string(key)] = common.CopyBytes(value)
		db.dataSize += len(value)
		if db.writeToFallback && db.fallback != nil {
			db.fallback.Put(key, value)
		}
	}
	return nil
}

func (db *ProofDb) Get(key []byte) ([]byte, error) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	if entry, ok := db.db[string(key)]; ok {
		return entry, nil
	}
	if db.fallback != nil {
		value, err := db.fallback.Get(key)
		if db.copyFromFallback && err == nil {
			db.db[string(key)] = value
			db.dataSize += len(value)
		}
		return value, err
	}
	return nil, errors.New("not found")
}

func (db *ProofDb) KeyCount() int {
	db.lock.RLock()
	defer db.lock.RUnlock()

	return len(db.db)
}

func (db *ProofDb) DataSize() int {
	db.lock.RLock()
	defer db.lock.RUnlock()

	return db.dataSize
}

func (db *ProofDb) ProofSet() ProofSet {
	db.lock.RLock()
	defer db.lock.RUnlock()

	var values ProofSet
	for _, value := range db.db {
		values = append(values, value)
	}
	return values
}

func (db *ProofDb) StoreAll(target trie.Database) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	for key, value := range db.db {
		target.Put([]byte(key), value)
	}
}

type ProofSet [][]byte

func (ps ProofSet) Store(db trie.Database) {
	for _, node := range ps {
		db.Put(crypto.Keccak256(node), node)
	}
}

func (ps ProofSet) ProofDb() *ProofDb {
	db := NewProofDb()
	ps.Store(db)
	return db
}

func (db *ProofDb) ReadCacheDb() *ProofDb {
	cdb := NewProofDb()
	cdb.SetFallback(db, true, false)
	return cdb
}

func (db *ProofDb) AllKeysRead(cdb *ProofDb) bool {
	return db.KeyCount() == cdb.KeyCount()
}
