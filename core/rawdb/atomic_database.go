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

package rawdb

import (
	"sync"

	"github.com/ethereum/go-ethereum/ethdb"
)

// AtomicDatabase is a wrapper of underlying ethdb.Dababase but offers more
// additional atomicity guarantee.
//
// The typical usage of atomic database is: the db handler is used by different
// modules but atomicity guarantee is required when update some data across the
// modules. In this case, call `OpenTransaction` to switch db to atomic mode, do
// whatever operation needed and call `CloseTransaction` to commit the change.
type AtomicDatabase struct {
	db    ethdb.Database
	lock  sync.Mutex
	ref   int
	batch ethdb.Batch // It's not thread safe!!
}

func NewAtomicDatabase(db ethdb.Database) ethdb.Database {
	return &AtomicDatabase{db: db}
}

// OpenTransaction creates an underlying batch to contain all
// following changes. Commit the previous batch if it's not nil.
func (db *AtomicDatabase) OpenTransaction() {
	db.lock.Lock()
	defer db.lock.Unlock()

	// If the atomic mode is already activated, increase
	// the batch reference.
	if db.batch != nil {
		db.ref += 1
		return
	}
	db.ref, db.batch = 1, db.db.NewBatch()
}

// CloseTransaction commits all changes in the current batch and
// finishes the atomic db updating.
func (db *AtomicDatabase) CloseTransaction() {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.batch == nil {
		return
	}
	db.ref -= 1
	if db.ref != 0 {
		return
	}
	db.batch.Write()
	db.batch = nil
}

// Close implements the Database interface and closes the underlying database.
func (db *AtomicDatabase) Close() error {
	return db.db.Close()
}

// Has is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) Has(key []byte) (bool, error) {
	return db.db.Has(key)
}

// Get is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) Get(key []byte) ([]byte, error) {
	return db.db.Get(key)
}

// HasAncient is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) HasAncient(kind string, number uint64) (bool, error) {
	return db.db.HasAncient(kind, number)
}

// Ancient is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) Ancient(kind string, number uint64) ([]byte, error) {
	return db.db.Ancient(kind, number)
}

// Ancients is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) Ancients() (uint64, error) {
	return db.db.Ancients()
}

// AncientSize is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) AncientSize(kind string) (uint64, error) {
	return db.db.AncientSize(kind)
}

// AppendAncient is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) AppendAncient(number uint64, hash, header, body, receipts, td []byte) error {
	return db.db.AppendAncient(number, hash, header, body, receipts, td)
}

// TruncateAncients is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) TruncateAncients(items uint64) error {
	return db.db.TruncateAncients(items)
}

// Sync is a noop passthrough that just forwards the request to the underlying
// database.
func (db *AtomicDatabase) Sync() error {
	return db.db.Sync()
}

// Put inserts the given value into the database. If the transaction
// is opened, put the value in batch, otherwise just forwards the
// request to the underlying database.
func (db *AtomicDatabase) Put(key []byte, value []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.batch != nil {
		return db.batch.Put(key, value)
	}
	return db.db.Put(key, value)
}

// Delete removes the specified entry from the database. If the transaction
// is opened, put the deletion in batch, otherwise just forwards the request
// to the underlying database.
func (db *AtomicDatabase) Delete(key []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	if db.batch != nil {
		return db.batch.Delete(key)
	}
	return db.db.Delete(key)
}

// NewIterator creates a binary-alphabetical iterator over the entire keyspace
// contained within the database.
func (db *AtomicDatabase) NewIterator() ethdb.Iterator {
	return db.NewIteratorWithPrefix(nil)
}

// NewIteratorWithStart creates a binary-alphabetical iterator over a subset of
// database content starting at a particular initial key (or after, if it does
// not exist).
func (db *AtomicDatabase) NewIteratorWithStart(start []byte) ethdb.Iterator {
	return db.db.NewIteratorWithStart(start)
}

// NewIteratorWithPrefix creates a binary-alphabetical iterator over a subset
// of database content with a particular key prefix.
func (db *AtomicDatabase) NewIteratorWithPrefix(prefix []byte) ethdb.Iterator {
	return db.db.NewIteratorWithPrefix(prefix)
}

// Stat returns a particular internal stat of the database.
func (db *AtomicDatabase) Stat(property string) (string, error) {
	return db.db.Stat(property)
}

// Compact flattens the underlying data store for the given key range. In essence,
// deleted and overwritten versions are discarded, and the data is rearranged to
// reduce the cost of operations needed to access them.
//
// A nil start is treated as a key before all keys in the data store; a nil limit
// is treated as a key after all keys in the data store. If both is nil then it
// will compact entire data store.
func (db *AtomicDatabase) Compact(start []byte, limit []byte) error {
	return db.db.Compact(start, limit)
}

// NewBatch creates a write-only database that buffers changes to its host db
// until a final write is called, each operation prefixing all keys with the
// pre-configured string.
func (db *AtomicDatabase) NewBatch() ethdb.Batch {
	return db.db.NewBatch()
}
