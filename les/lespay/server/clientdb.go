// Copyright 2020 The go-ethereum Authors
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

package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	//	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
	lru "github.com/hashicorp/golang-lru"
)

const balanceCacheLimit = 8192 // the maximum number of cached items in service token balance queue

// currencyBalance represents the client's currency balance.
type currencyBalance struct {
	amount uint64
	typ    string
}

// EncodeRLP implements rlp.Encoder
func (b *currencyBalance) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{b.amount, b.typ})
}

// DecodeRLP implements rlp.Decoder
func (b *currencyBalance) DecodeRLP(s *rlp.Stream) error {
	var entry struct {
		Amount uint64
		Type   string
	}
	if err := s.Decode(&entry); err != nil {
		return err
	}
	b.amount, b.typ = entry.Amount, entry.Type
	return nil
}

const (
	// nodeDBVersion is the version identifier of the node data in db
	//
	// Changelog:
	// * Replace `lastTotal` with `meta` in positive balance: version 0=>1
	// * Rework balance, add currency balance: version 1=>2
	nodeDBVersion = 2

	// dbCleanupCycle is the cycle of db for useless data cleanup
	dbCleanupCycle = time.Hour
)

var (
	posBalancePrefix      = []byte("pb:")         // dbVersion(uint16 big endian) + posBalancePrefix + id -> positive balance
	negBalancePrefix      = []byte("nb:")         // dbVersion(uint16 big endian) + negBalancePrefix + ip -> negative balance
	curBalancePrefix      = []byte("cb:")         // dbVersion(uint16 big endian) + curBalancePrefix + id -> currency balance
	paymentReceiverPrefix = []byte("pr:")         // dbVersion(uint16 big endian) + paymentReceiverPrefix + id + "receiverName:" -> receiver namespace
	expirationKey         = []byte("expiration:") // dbVersion(uint16 big endian) + expirationKey -> posExp, negExp
)

type atomicWriteLock struct {
	released chan struct{}
	batch    ethdb.Batch
}

type nodeDB struct {
	db            ethdb.KeyValueStore
	cache         *lru.Cache
	clock         mclock.Clock
	closeCh       chan struct{}
	evictCallBack func(mclock.AbsTime, bool, utils.ExpiredValue) bool // Callback to determine whether the balance can be evicted.
	idLockMutex   sync.Mutex
	idLocks       map[string]atomicWriteLock
	cleanupHook   func() // Test hook used for testing
}

func newNodeDB(db ethdb.KeyValueStore, clock mclock.Clock) *nodeDB {
	var buff [2]byte
	binary.BigEndian.PutUint16(buff[:], uint16(nodeDBVersion))

	cache, _ := lru.New(balanceCacheLimit)
	ndb := &nodeDB{
		db:      db, //TODO rawdb.NewTable(db, string(buff[:])),
		cache:   cache,
		clock:   clock,
		closeCh: make(chan struct{}),
		idLocks: make(map[string]atomicWriteLock),
	}
	go ndb.expirer()
	return ndb
}

func (db *nodeDB) close() {
	close(db.closeCh)
}

func (db *nodeDB) atomicWriteLock(id []byte) ethdb.KeyValueWriter {
	db.idLockMutex.Lock()
	for {
		ch := db.idLocks[string(id)].released
		if ch == nil {
			break
		}
		db.idLockMutex.Unlock()
		<-ch
		db.idLockMutex.Lock()
	}
	batch := db.db.NewBatch()
	db.idLocks[string(id)] = atomicWriteLock{
		released: make(chan struct{}),
		batch:    batch,
	}
	db.idLockMutex.Unlock()
	return batch
}

func (db *nodeDB) atomicWriteUnlock(id []byte) {
	db.idLockMutex.Lock()
	awl := db.idLocks[string(id)]
	awl.batch.Write()
	close(awl.released)
	delete(db.idLocks, string(id))
	db.idLockMutex.Unlock()
}

func (db *nodeDB) writer(id []byte) ethdb.KeyValueWriter {
	db.idLockMutex.Lock()
	batch := db.idLocks[string(id)].batch
	db.idLockMutex.Unlock()
	if batch == nil {
		return db.db
	}
	return batch
}

func idKey(id []byte, neg bool) []byte {
	prefix := posBalancePrefix
	if neg {
		prefix = negBalancePrefix
	}
	return append(prefix, id...)
}

func receiverPrefix(id enode.ID, receiver string) []byte {
	return append(append(paymentReceiverPrefix, id.Bytes()...), []byte(receiver+":")...)
}

func (db *nodeDB) getExpiration() (utils.Fixed64, utils.Fixed64) {
	blob, err := db.db.Get(expirationKey)
	if err != nil || len(blob) != 16 {
		return 0, 0
	}
	return utils.Fixed64(binary.BigEndian.Uint64(blob[:8])), utils.Fixed64(binary.BigEndian.Uint64(blob[8:16]))
}

func (db *nodeDB) setExpiration(pos, neg utils.Fixed64) {
	var buff [16]byte
	binary.BigEndian.PutUint64(buff[:8], uint64(pos))
	binary.BigEndian.PutUint64(buff[8:16], uint64(neg))
	db.db.Put(expirationKey, buff[:16])
}

func (db *nodeDB) getCurrencyBalance(id enode.ID) currencyBalance {
	var b currencyBalance
	enc, err := db.db.Get(append(curBalancePrefix, id.Bytes()...))
	if err != nil || len(enc) == 0 {
		return b
	}
	if err := rlp.DecodeBytes(enc, &b); err != nil {
		log.Crit("Failed to decode positive balance", "err", err)
	}
	return b
}

func (db *nodeDB) setCurrencyBalance(id enode.ID, b currencyBalance) {
	enc, err := rlp.EncodeToBytes(&(b))
	if err != nil {
		log.Crit("Failed to encode currency balance", "err", err)
	}
	db.writer(id.Bytes()).Put(append(curBalancePrefix, id.Bytes()...), enc)
}

func (db *nodeDB) getOrNewBalance(id []byte, neg bool) utils.ExpiredValue {
	key := idKey(id, neg)
	item, exist := db.cache.Get(string(key))
	if exist {
		return item.(utils.ExpiredValue)
	}
	var b utils.ExpiredValue
	enc, err := db.db.Get(key)
	if err != nil || len(enc) == 0 {
		return b
	}
	if err := rlp.DecodeBytes(enc, &b); err != nil {
		log.Crit("Failed to decode positive balance", "err", err)
	}
	db.cache.Add(string(key), b)
	return b
}

func (db *nodeDB) setBalance(id []byte, neg bool, b utils.ExpiredValue) {
	key := idKey(id, neg)
	enc, err := rlp.EncodeToBytes(&(b))
	if err != nil {
		log.Crit("Failed to encode positive balance", "err", err)
	}
	if neg {
		db.db.Put(key, enc)
	} else {
		db.writer(id).Put(key, enc)
	}
	db.cache.Add(string(key), b)
}

func (db *nodeDB) delBalance(id []byte, neg bool) {
	key := idKey(id, neg)
	if neg {
		db.db.Delete(key)
	} else {
		db.writer(id).Delete(key)
	}
	db.cache.Remove(string(key))
}

// getPosBalanceIDs returns a lexicographically ordered list of IDs of accounts
// with a positive balance
func (db *nodeDB) getPosBalanceIDs(start, stop enode.ID, maxCount int) (result []enode.ID) {
	if maxCount <= 0 {
		return
	}
	it := db.db.NewIteratorWithStart(idKey(start.Bytes(), false))
	defer it.Release()
	for i := len(stop[:]) - 1; i >= 0; i-- {
		stop[i]--
		if stop[i] != 255 {
			break
		}
	}
	stopKey := idKey(stop.Bytes(), false)
	keyLen := len(stopKey)

	for it.Next() {
		var id enode.ID
		if len(it.Key()) != keyLen || bytes.Compare(it.Key(), stopKey) == 1 {
			return
		}
		copy(id[:], it.Key()[keyLen-len(id):])
		result = append(result, id)
		if len(result) == maxCount {
			return
		}
	}
	return
}

func (db *nodeDB) expirer() {
	for {
		select {
		case <-db.clock.After(dbCleanupCycle):
			db.expireNodes()
		case <-db.closeCh:
			return
		}
	}
}

// expireNodes iterates the whole node db and checks whether the
// token balances can deleted.
func (db *nodeDB) expireNodes() {
	var (
		visited int
		deleted int
		start   = time.Now()
	)
	for index, prefix := range [][]byte{posBalancePrefix, negBalancePrefix} {
		iter := db.db.NewIteratorWithPrefix(prefix)
		for iter.Next() {
			visited += 1
			var balance utils.ExpiredValue
			if err := rlp.DecodeBytes(iter.Value(), &balance); err != nil {
				log.Crit("Failed to decode negative balance", "err", err)
			}
			if db.evictCallBack != nil && db.evictCallBack(db.clock.Now(), index != 0, balance) {
				deleted += 1
				db.db.Delete(iter.Key())
			}
		}
	}
	// Invoke testing hook if it's not nil.
	if db.cleanupHook != nil {
		db.cleanupHook()
	}
	log.Debug("Expire nodes", "visited", visited, "deleted", deleted, "elapsed", common.PrettyDuration(time.Since(start)))
}
