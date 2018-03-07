// Copyright 2018 The go-ethereum Authors
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

package trie

import (
	"bytes"
	//"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/hashtree"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// secureKeyLength is the length of the above prefix + 32byte hash.
const secureKeyLength = 100

// DatabaseReader wraps the Get and Has method of a backing store for the trie.
type DatabaseReader interface {
	// Get retrieves the value associated with key form the database.
	Get(key []byte) (value []byte, err error)

	// Has retrieves whether a key is present in the database.
	Has(key []byte) (bool, error)
}

// Database is an intermediate write layer between the trie data structures and
// the disk database. The aim is to accumulate trie writes in-memory and only
// periodically flush a couple tries to disk, garbage collecting the remainder.
type Database struct {
	*databaseShared
	prefix []byte
	reader *hashtree.Reader
}

type databaseShared struct {
	diskdb    ethdb.Database              // Persistent storage for matured trie nodes
	nodes     map[common.Hash]*cachedNode // Data and references relationships of a node
	preimages map[common.Hash][]byte      // Preimages of nodes from the secure trie
	seckeybuf [secureKeyLength]byte       // Ephemeral buffer for calculating preimage keys

	gctime  time.Duration      // Time spent on garbage collection since last commit
	gcnodes uint64             // Nodes garbage collected since last commit
	gcsize  common.StorageSize // Data storage garbage collected since last commit

	nodesSize     common.StorageSize // Storage size of the nodes cache
	preimagesSize common.StorageSize // Storage size of the preimages cache

	lock sync.RWMutex
}

// cachedNode is all the information we know about a single cached node in the
// memory database write layer.
type cachedNode struct {
	blob     []byte                   // Cached data block of the trie node
	parents  map[common.Hash]int      // Number of live nodes referencing this one
	children map[common.Hash][][]byte // Children referenced by this nodes
}

func (c *cachedNode) addParent(hash common.Hash) {
	c.parents[hash]++
}

func (c *cachedNode) removeParent(hash common.Hash) {
	c.parents[hash]--
	if c.parents[hash] == 0 {
		delete(c.parents, hash)
	}
}

func (c *cachedNode) addChild(child common.Hash, path []byte) bool {
	paths := c.children[child]
	for _, p := range paths {
		if bytes.Equal(path, p) {
			return false
		}
	}
	c.children[child] = append(paths, path)
	return true
}

func (c *cachedNode) removeChild(child common.Hash, path []byte) bool {
	paths := c.children[child]
	pos := -1
	for i, p := range paths {
		if bytes.Equal(path, p) {
			pos = i
			break
		}
	}
	if pos != -1 {
		l := len(paths)
		if l == 1 {
			delete(c.children, child)
		} else {
			l--
			paths[pos] = paths[l]
			c.children[child] = paths[:l]
		}
		return true
	}
	//log.Error("Failed to dereference trie node (unknown path)")
	return false
}

// NewDatabase creates a new trie database to store ephemeral trie content before
// its written out to disk or garbage collected.
func NewDatabase(diskdb ethdb.Database, prefix []byte) *Database {
	return &Database{
		prefix: prefix,
		reader: hashtree.NewReader(diskdb, string(prefix)),
		databaseShared: &databaseShared{
			diskdb: diskdb,
			nodes: map[common.Hash]*cachedNode{
				{}: {children: make(map[common.Hash][][]byte)},
			},
			preimages: make(map[common.Hash][]byte),
		},
	}
}

func (db *Database) ChildDatabase(path []byte) *Database {
	prefix := append(db.prefix, path...)
	return &Database{
		prefix:         prefix,
		reader:         hashtree.NewReader(db.diskdb, string(prefix)),
		databaseShared: db.databaseShared,
	}
}

// DiskDB retrieves the persistent storage backing the trie database.
func (db *Database) DiskDB() DatabaseReader {
	return db.diskdb
}

// Insert writes a new trie node to the memory database if it's yet unknown. The
// method will make a copy of the slice.
func (db *Database) Insert(hash common.Hash, blob []byte) {
	db.lock.Lock()
	defer db.lock.Unlock()

	db.insert(hash, blob)
}

// insert is the private locked version of Insert.
func (db *Database) insert(hash common.Hash, blob []byte) {
	if _, ok := db.nodes[hash]; ok {
		return
	}
	db.nodes[hash] = &cachedNode{
		blob:     common.CopyBytes(blob),
		parents:  make(map[common.Hash]int),
		children: make(map[common.Hash][][]byte),
	}
	db.nodesSize += common.StorageSize(common.HashLength + len(blob))
}

// insertPreimage writes a new trie node pre-image to the memory database if it's
// yet unknown. The method will make a copy of the slice.
//
// Note, this method assumes that the database's lock is held!
func (db *Database) insertPreimage(hash common.Hash, preimage []byte) {
	if _, ok := db.preimages[hash]; ok {
		return
	}
	db.preimages[hash] = common.CopyBytes(preimage)
	db.preimagesSize += common.StorageSize(common.HashLength + len(preimage))
}

var nullHash = crypto.Keccak256Hash(nil)

// Node retrieves a cached trie node from memory. If it cannot be found cached,
// the method queries the persistent database for the content.
func (db *Database) Node(prefix []byte, hash common.Hash) ([]byte, error) {
	// Retrieve the node from cache if available
	db.lock.RLock()
	node := db.nodes[hash]
	db.lock.RUnlock()

	if node != nil {
		return node.blob, nil
	}

	if hash == nullHash {
		return nil, nil
	}
	// Content unavailable in memory, attempt to retrieve from disk

	data, err := db.reader.Get(prefix, hash[:])
	/*if err != nil {
		fmt.Printf("missing %x at prefix %x + %x\n", hash[:], db.prefix, prefix)
	}*/
	return data, err
}

// preimage retrieves a cached trie node pre-image from memory. If it cannot be
// found cached, the method queries the persistent database for the content.
func (db *Database) preimage(hash common.Hash) ([]byte, error) {
	// Retrieve the node from cache if available
	db.lock.RLock()
	preimage := db.preimages[hash]
	db.lock.RUnlock()

	if preimage != nil {
		return preimage, nil
	}
	// Content unavailable in memory, attempt to retrieve from disk
	return db.reader.Get(SecHashTreePos(hash[:]), hash[:])
}

// Nodes retrieves the hashes of all the nodes cached within the memory database.
// This method is extremely expensive and should only be used to validate internal
// states in test code.
func (db *Database) Nodes() []common.Hash {
	db.lock.RLock()
	defer db.lock.RUnlock()

	var hashes = make([]common.Hash, 0, len(db.nodes))
	for hash := range db.nodes {
		if hash != (common.Hash{}) { // Special case for "root" references/nodes
			hashes = append(hashes, hash)
		}
	}
	return hashes
}

// Reference adds a new reference from a parent node to a child node.
func (db *Database) Reference(child common.Hash, parent common.Hash, path []byte) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	db.reference(child, parent, path)
}

// reference is the private locked version of Reference.
func (db *Database) reference(child common.Hash, parent common.Hash, path []byte) {
	// If the node does not exist, it's a node pulled from disk, skip
	//fmt.Printf("+ref %x %x %x ", parent[:], child[:], path)
	node, ok := db.nodes[child]
	if !ok {
		//fmt.Println("(child missing)")
		return
	}
	if db.nodes[parent].addChild(child, path) {
		node.addParent(parent)
		//fmt.Println("(added)")
	} /* else {
		fmt.Println("(already exists)")
	}*/
}

// Dereference removes an existing reference from a parent node to a child node.
func (db *Database) Dereference(child common.Hash, parent common.Hash, path []byte) {
	db.lock.Lock()
	defer db.lock.Unlock()

	nodes, storage, start := len(db.nodes), db.nodesSize, time.Now()
	db.dereference(child, parent, path)

	db.gcnodes += uint64(nodes - len(db.nodes))
	db.gcsize += storage - db.nodesSize
	db.gctime += time.Since(start)

	log.Debug("Dereferenced trie from memory database", "nodes", nodes-len(db.nodes), "size", storage-db.nodesSize, "time", time.Since(start),
		"gcnodes", db.gcnodes, "gcsize", db.gcsize, "gctime", db.gctime, "livenodes", len(db.nodes), "livesize", db.nodesSize)
}

// dereference is the private locked version of Dereference.
func (db *Database) dereference(child common.Hash, parent common.Hash, path []byte) {
	//fmt.Printf("-ref %x %x %x\n", parent[:], child[:], path)
	// Dereference the parent-child
	db.nodes[parent].removeChild(child, path)

	// If the node does not exist, it's a previously committed node.
	node, ok := db.nodes[child]
	if !ok {
		return
	}
	node.removeParent(parent)
	// If there are no more references to the child, delete it and cascade
	if len(node.parents) == 0 {
		for hash, paths := range node.children {
			for _, path := range paths {
				db.dereference(hash, child, path)
			}
		}
		delete(db.nodes, child)
		db.nodesSize -= common.StorageSize(common.HashLength + len(node.blob))
	}
}

// Commit iterates over all the children of a particular node, writes them out
// to disk, forcefully tearing down all references in both directions.
//
// As a side effect, all pre-images accumulated up to this point are also written.
func (db *Database) Commit(node common.Hash, report bool, version uint64, gc *hashtree.GarbageCollector) error {
	if gc != nil {
		gc.LockWrite()
		defer gc.UnlockWrite()
	}

	// Create a database batch to flush persistent data out. It is important that
	// outside code doesn't see an inconsistent state (referenced data removed from
	// memory cache during commit but not yet in persistent storage). This is ensured
	// by only uncaching existing data when the database write finalizes.
	//fmt.Println("Commit")

	db.lock.RLock()

	start := time.Now()
	batch := db.diskdb.NewBatch()
	writer := hashtree.NewWriter(batch, string(db.prefix), version, gc)

	// Move all of the accumulated preimages into a write batch
	for hash, preimage := range db.preimages {
		if err := writer.Put(SecHashTreePos(hash[:]), hash[:], preimage); err != nil {
			log.Error("Failed to commit preimage from trie database", "err", err)
			db.lock.RUnlock()
			return err
		}
		if batch.ValueSize() > ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return err
			}
			batch.Reset()
		}
	}
	// Move the trie itself into the batch, flushing if enough data is accumulated
	nodes, storage := len(db.nodes), db.nodesSize+db.preimagesSize
	if err := db.commit(node, batch, writer, nil); err != nil {
		log.Error("Failed to commit trie from trie database", "err", err)
		db.lock.RUnlock()
		return err
	}
	// Write batch ready, unlock for readers during persistence
	if err := batch.Write(); err != nil {
		log.Error("Failed to write trie to disk", "err", err)
		db.lock.RUnlock()
		return err
	}
	db.lock.RUnlock()

	// Write successful, clear out the flushed data
	db.lock.Lock()
	defer db.lock.Unlock()

	db.preimages = make(map[common.Hash][]byte)
	db.preimagesSize = 0

	db.uncache(node, common.Hash{}, nil, nil)

	logger := log.Info
	if !report {
		logger = log.Debug
	}
	logger("Persisted trie from memory database", "nodes", nodes-len(db.nodes), "size", storage-db.nodesSize, "time", time.Since(start),
		"gcnodes", db.gcnodes, "gcsize", db.gcsize, "gctime", db.gctime, "livenodes", len(db.nodes), "livesize", db.nodesSize)

	// Reset the garbage collection statistics
	db.gcnodes, db.gcsize, db.gctime = 0, 0, 0

	return nil
}

// commit is the private locked version of Commit.
func (db *Database) commit(hash common.Hash, batch ethdb.Batch, writer *hashtree.Writer, path []byte) error {
	// If the node does not exist, it's a previously committed node
	node, ok := db.nodes[hash]
	if !ok {
		return nil
	}
	for child, paths := range node.children {
		for _, p := range paths {
			if err := db.commit(child, batch, writer, concat(path, p...)); err != nil {
				return err
			}
		}
	}
	//fmt.Printf("write %x %x %x\n", hexToHashTreePos(path), hash[:], node.blob)

	if err := writer.Put(hexToHashTreePos(path), hash[:], node.blob); err != nil {
		return err
	}
	// If we've reached an optimal match size, commit and start over
	if batch.ValueSize() >= ethdb.IdealBatchSize {
		if err := batch.Write(); err != nil {
			return err
		}
		batch.Reset()
	}
	return nil
}

func (db *Database) uniquePath(hash common.Hash, path []byte, cached map[common.Hash][]byte) bool {
	//fmt.Printf("*")
	if p, ok := cached[hash]; ok {
		unique := bytes.Equal(p, path)
		//fmt.Printf("unique %x %x %v (cached)\n", hash[:], path, unique)
		return unique
	}
	// If the node does not exist, there is no other reference path
	node, ok := db.nodes[hash]
	if !ok {
		cached[hash] = common.CopyBytes(path)
		//fmt.Printf("unique %x %x true (missing node)\n", hash[:], path)
		return true
	}
	for parentHash, _ := range node.parents {
		//fmt.Printf("p")
		if parent, ok := db.nodes[parentHash]; ok {
			parentPaths := parent.children[hash]

			if len(parentPaths) == 0 {
				//fmt.Printf("0 ")
			} else {
				//fmt.Printf("%d %x %x | ", len(parentPaths), parentPaths[0], path[len(path)-len(parentPaths[0]):])
			}

			if len(parentPaths) != 1 ||
				len(parentPaths[0]) > len(path) ||
				!bytes.Equal(parentPaths[0], path[len(path)-len(parentPaths[0]):]) ||
				!db.uniquePath(parentHash, path[:len(path)-len(parentPaths[0])], cached) {
				//cached[hash] = false
				//fmt.Printf("unique %x %x false\n", hash[:], path)
				return false
			}

		}
	}
	cached[hash] = common.CopyBytes(path)
	//fmt.Printf("unique %x %x true\n", hash[:], path)
	return true
}

// uncache is the post-processing step of a commit operation where the already
// persisted trie is removed from the cache. The reason behind the two-phase
// commit is to ensure consistent data availability while moving from memory
// to disk.
func (db *Database) uncache(hash, parent common.Hash, path []byte, cachedUniquePath map[common.Hash][]byte) {
	if cachedUniquePath == nil {
		cachedUniquePath = make(map[common.Hash][]byte)
	}

	// If the node does not exist, we're done on this path
	node, ok := db.nodes[hash]
	if !ok {
		return
	}

	if !db.uniquePath(hash, path, cachedUniquePath) {
		node.removeParent(parent)
		return
	}

	// Otherwise uncache the node's subtries and remove the node itself too
	for child, paths := range node.children {

		if c, ok := db.nodes[child]; ok {
			if c.parents[hash] != len(paths) {
				//fmt.Printf("ref mismatch %x %x %d %d\n", hash, child, len(paths), c.parents[hash])
			}
		}

		for _, p := range paths {
			db.uncache(child, hash, concat(path, p...), cachedUniquePath)
		}
	}

	// Remove child refs from parents
	for parent, _ := range node.parents {
		delete(db.nodes[parent].children, hash)
	}

	delete(db.nodes, hash)
	db.nodesSize -= common.StorageSize(common.HashLength + len(node.blob))
}

// Size returns the current storage size of the memory cache in front of the
// persistent database layer.
func (db *Database) Size() common.StorageSize {
	db.lock.RLock()
	defer db.lock.RUnlock()

	return db.nodesSize + db.preimagesSize
}
