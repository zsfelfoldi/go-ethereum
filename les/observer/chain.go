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

package observer

import (
	"crypto/ecdsa"
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	ErrNoFirstBlock      = errors.New("first block not found in observer chain")
	ErrNoBlock           = errors.New("block not found in observer chain")
	ErrNoLockedTrie      = errors.New("no locked trie")
	ErrCannotCreateBlock = errors.New("cannot create new block")
)

// -----
// CHAIN
// -----

// Chain represents the canonical observer chain given a database with a
// genesis block.
type Chain struct {
	mu           sync.Mutex
	db           ethdb.Database
	privateKey   *ecdsa.PrivateKey
	firstBlock   *Block
	currentBlock *Block

	trieMu sync.RWMutex
	trie   *trie.Trie
}

// NewChain returns a fully initialised Observer chain
// using information available in the database
func NewChain(db ethdb.Database, privKey *ecdsa.PrivateKey) (*Chain, error) {
	c := &Chain{
		db:         db,
		privateKey: privKey,
	}
	// Generate genesis block.
	firstBlock := GetBlock(db, 0)
	if firstBlock == nil {
		firstBlock = NewBlock(privKey)
	}
	c.firstBlock = firstBlock
	c.currentBlock = firstBlock
	if err := WriteBlock(db, firstBlock); err != nil {
		return nil, err
	}
	if err := WriteLastObserverBlockHash(db, firstBlock.Hash()); err != nil {
		return nil, err
	}
	return c, nil
}

// Block returns a single block by its number.
func (c *Chain) Block(number uint64) (*Block, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := GetBlock(c.db, number)
	if b == nil {
		return nil, ErrNoBlock
	}
	return b, nil
}

// FirstBlock returns the first block of the observer chain.
func (c *Chain) FirstBlock() *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstBlock
}

// CurrentBlock returns the current active block.
func (c *Chain) CurrentBlock() *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentBlock
}

// LockAndGetTrie lock trie mutex and get r/w access to the current observer trie.
func (c *Chain) LockAndGetTrie() (*trie.Trie, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.trie != nil {
		return c.trie, nil
	}
	c.trieMu.Lock()
	tr, err := trie.New(c.currentBlock.TrieRoot(), trie.NewDatabase(c.db))
	if err != nil {
		return nil, err
	}
	c.trie = tr
	return tr, nil
}

// UnlockTrie unlocks the trie mutex.
func (c *Chain) UnlockTrie() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.trie == nil {
		// Avoid unlocking unlocked trie.
		return
	}
	c.trieMu.Unlock()
}

// CreateBlock commits current trie and seals a new block; continues using the same trie
// values are persistent, we will care about garbage collection later.
func (c *Chain) CreateBlock() (*Block, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var hash common.Hash
	var err error
	if c.trie != nil {
		// Commit the trie.
		hash, err = c.trie.Commit(nil)
		if err != nil {
			c.trie = nil
			c.trieMu.Unlock()
			return nil, err
		}
	} else {
		// No trie, use current one.
		hash = c.currentBlock.TrieRoot()
	}
	// Create successor block.
	successor := c.currentBlock.CreateSuccessor(hash, c.privateKey)
	if successor != nil {
		return nil, ErrCannotCreateBlock
	}
	c.currentBlock = successor
	return c.currentBlock, nil
}

// AutoCreateBlocks starts a goroutine automatically creating blocks periodically until
// the chain is closed. It's non-blocking.
func (c *Chain) AutoCreateBlocks(period time.Duration) {

}

// Close closes the chain.
func (c *Chain) Close() {

}
