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
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/trie"
)

// Errors of observer chain.
var (
	ErrNoFirstBlock      = errors.New("first block not found in observer chain")
	ErrNoBlock           = errors.New("block not found in observer chain")
	ErrLockedTrie        = errors.New("locked trie")
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

	closeC chan struct{}
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
		return nil, ErrLockedTrie
	}
	c.trieMu.Lock()
	tr, err := trie.New(c.currentBlock.TrieRoot(), trie.NewDatabase(c.db))
	if err != nil {
		c.trieMu.Unlock()
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
	c.commitTrie()
}

// CreateBlock commits current trie and seals a new block.
func (c *Chain) CreateBlock() *Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.trie != nil {
		// Commit the trie.
		c.commitTrie()
	} else {
		// No trie, use current one.
		c.currentBlock = c.currentBlock.CreateSuccessor(c.currentBlock.TrieRoot(), c.privateKey)
	}
	return c.currentBlock
}

// AutoCreateBlocks starts a goroutine automatically creating blocks periodically until
// the chain is closed. It's non-blocking.
func (c *Chain) AutoCreateBlocks(period time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeC != nil {
		return
	}
	c.closeC = make(chan struct{})
	go c.loop(period)
}

// Close closes the chain.
func (c *Chain) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeC == nil {
		return
	}
	close(c.closeC)
	c.closeC = nil
}

// commitTrie internally is used by UnlockTrie and CreateBlock. It commits
// the trie and creates the new block based on the hash.
func (c *Chain) commitTrie() {
	trieRoot, err := c.trie.Commit(nil)
	if err != nil {
		log.Debug(fmt.Sprint(err))
		return
	}
	block := c.currentBlock.CreateSuccessor(trieRoot, c.privateKey)
	if err := WriteBlock(c.db, block); err != nil {
		log.Debug(fmt.Sprint(err))
		return
	}
	if err := WriteLastObserverBlockHash(c.db, block.Hash()); err != nil {
		log.Debug(fmt.Sprint(err))
		return
	}
	c.currentBlock = block
	c.trie = nil
	c.trieMu.Unlock()
}

// loop realizes the chains backend goroutine.
func (c *Chain) loop(period time.Duration) {
	ticker := time.NewTicker(period)
	for {
		select {
		case <-ticker.C:
			// Time to create a new block.
			c.CreateBlock()
		case <-c.closeC:
			// Close has been called.
			// TODO: Check for cleanup tasks like locked tries.
			return
		}
	}
}
