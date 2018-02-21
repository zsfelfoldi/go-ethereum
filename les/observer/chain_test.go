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

package observer_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/observer"
	"github.com/ethereum/go-ethereum/trie"
)

// TestChainCreation tests the correct creation of an
// observer chain with its first block.
func TestChainCreation(t *testing.T) {
	// Infrastructure.
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Errorf("generation of private key failed: %v", err)
	}
	db, err := ethdb.NewMemDatabase()
	if err != nil {
		t.Errorf("creation of memory database failed: %v", err)
	}
	// Chain creation.
	c, err := observer.NewChain(db, privKey)
	if err != nil {
		t.Errorf("creation of new chain failed: %v", err)
	}
	if c.FirstBlock().Number().Uint64() != 0 {
		t.Errorf("first block number is not zero")
	}
	if c.CurrentBlock().Number().Uint64() != 0 {
		t.Errorf("last block number is not zero")
	}
	block, err := c.Block(0)
	if err != nil {
		t.Errorf("cannot retrieve block 0: %v", err)
	}
	if block.Number().Uint64() != 0 {
		t.Errorf("block number 0 returns illegal block bumber")
	}
	c.Close()
}

// TestBlockCreation tests the creation of new blocks.
func TestBlockCreation(t *testing.T) {
	// Infrastructure.
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Errorf("generation of private key failed: %v", err)
	}
	db, err := ethdb.NewMemDatabase()
	if err != nil {
		t.Errorf("creation of memory database failed: %v", err)
	}
	// Chain creation with first new block.
	c, err := observer.NewChain(db, privKey)
	if err != nil {
		t.Errorf("creation of new chain failed: %v", err)
	}
	blockA := c.CreateBlock()
	if blockA.PrevHash() != c.FirstBlock().Hash() {
		t.Errorf("new blocks hash doesn't point to previous one")
	}
	blockNoA := blockA.Number().Uint64()
	if blockNoA != c.FirstBlock().Number().Uint64()+1 {
		t.Errorf("number of created block is wrong")
	}
	if blockNoA != c.CurrentBlock().Number().Uint64() {
		t.Errorf("returned block and current block differ")
	}
	// Second new block.
	blockB := c.CreateBlock()
	if blockB.PrevHash() != blockA.Hash() {
		t.Errorf("new blocks hash doesn't point to previous one")
	}
	blockNoB := blockB.Number().Uint64()
	if blockNoB != blockNoA+1 {
		t.Errorf("number of created block is wrong")
	}
	if blockNoB != c.CurrentBlock().Number().Uint64() {
		t.Errorf("returned block and current block differ")
	}
	c.Close()
}

// TestAutoBlockCreation tests the creation of new blocks
// automatically in the background.
func TestAutoBlockCreation(t *testing.T) {
	// Infrastructure.
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Errorf("generation of private key failed: %v", err)
	}
	db, err := ethdb.NewMemDatabase()
	if err != nil {
		t.Errorf("creation of memory database failed: %v", err)
	}
	// Chain creation.
	c, err := observer.NewChain(db, privKey)
	if err != nil {
		t.Errorf("creation of new chain failed: %v", err)
	}
	blockNo := c.CurrentBlock().Number().Uint64()
	c.AutoCreateBlocks(10 * time.Millisecond)
	// Periodically check current block number.
	for i := 0; i < 10; i++ {
		time.Sleep(15 * time.Millisecond)
		currentNo := c.CurrentBlock().Number().Uint64()
		if currentNo <= blockNo {
			t.Errorf("current block number %d not greater than predecessor %d", currentNo, blockNo)
		}
		blockNo = currentNo
	}
	c.Close()
}

// TestLockAndUnlock tests the locking and unlocking of the rie.
func TestLockAndUnlock(t *testing.T) {
	// Helper.
	update := func(tr *trie.Trie, key, value string) {
		err := tr.TryUpdate([]byte(key), []byte(value))
		if err != nil {
			t.Errorf("updating trie failed")
		}
	}
	assert := func(tr *trie.Trie, key, value string) {
		v, err := tr.TryGet([]byte(key))
		if err != nil {
			t.Errorf("getting from trie failed")
		}
		if string(v) != value {
			t.Errorf("retrieved value from trie is wrong")
		}
	}
	// Infrastructure.
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Errorf("generation of private key failed: %v", err)
	}
	db, err := ethdb.NewMemDatabase()
	if err != nil {
		t.Errorf("creation of memory database failed: %v", err)
	}
	// Chain creation.
	c, err := observer.NewChain(db, privKey)
	if err != nil {
		t.Errorf("creation of new chain failed: %v", err)
	}
	// Lock, unlock, commit trie.
	tr, err := c.LockAndGetTrie()
	if err != nil {
		t.Errorf("cannot lock and get trie: %v", err)
	}
	update(tr, "foo", "123")
	update(tr, "bar", "456")
	update(tr, "baz", "789")
	_, err = c.LockAndGetTrie()
	if err != observer.ErrLockedTrie {
		t.Errorf("expected locked trie error")
	}
	c.UnlockTrie()
	tr, err = c.LockAndGetTrie()
	if err != nil {
		t.Errorf("cannot lock and get trie: %v", err)
	}
	update(tr, "yadda", "999")
	block := c.CreateBlock()
	if block == nil {
		t.Errorf("cannot commit and create block")
	}
	// Check values.
	tr, err = c.LockAndGetTrie()
	if err != nil {
		t.Errorf("cannot lock and get trie: %v", err)
	}
	assert(tr, "foo", "123")
	assert(tr, "bar", "456")
	assert(tr, "baz", "789")
	assert(tr, "yadda", "999")
	c.Close()
}
