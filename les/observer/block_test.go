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
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/observer"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// -----
// TESTS
// -----

// TestStatement tests creating and accessing statements.
func TestStatement(t *testing.T) {
	k := []byte("foo")
	v := []byte("bar")
	st := observer.NewStatement(k, v)
	// Testing simple access.
	if tk := st.Key(); !bytes.Equal(tk, k) {
		t.Errorf("returned key %v is not key %v", tk, k)
	}
	if tv := st.Value(); !bytes.Equal(tv, v) {
		t.Errorf("returned value %v is not value %v", tv, v)
	}
	// Testing encoding and decoding.
	var buf bytes.Buffer
	err := st.EncodeRLP(&buf)
	if err != nil {
		t.Errorf("encoding to RLP returned error: %v", err)
	}
	var tst observer.Statement
	err = tst.DecodeRLP(rlp.NewStream(&buf, 0))
	if err != nil {
		t.Errorf("decoding from RLP returned error: %v", err)
	}
	if tk := tst.Key(); !bytes.Equal(tk, k) {
		t.Errorf("returned key %v is not key %v", tk, k)
	}
	if tv := tst.Value(); !bytes.Equal(tv, v) {
		t.Errorf("returned value %v is not value %v", tv, v)
	}
	sthb := st.Hash().Bytes()
	tsthb := tst.Hash().Bytes()
	if !bytes.Equal(sthb, tsthb) {
		t.Errorf("hashes of original and encoded/decoded one differ")
	}
}

// TestEmptyBlock tests creating and accessing empty blocks.
func TestEmptyBlock(t *testing.T) {
	// Infrastructure.
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Errorf("generation of private key failed")
	}
	// Initial block.
	b := observer.NewBlock(privKey)
	if b.Number().Uint64() != 0 {
		t.Errorf("number of new block is not 0")
	}
	// Testing encoding and decoding.
	var buf bytes.Buffer
	err = b.EncodeRLP(&buf)
	if err != nil {
		t.Errorf("encoding to RLP returned error: %v", err)
	}
	var tb observer.Block
	err = tb.DecodeRLP(rlp.NewStream(&buf, 0))
	if err != nil {
		t.Errorf("decoding from RLP returned error: %v", err)
	}
	if tb.Number().Uint64() != 0 {
		t.Errorf("number of encoded/decoded block is not 0")
	}
}

// TestBlockTrieRoot tests the initial TrieRoot and how it changes if
// some values are set in Trie. Also after setting the new TrieRoot
// (hash of the Trie) the signature of the successor block has to
// differ.
func TestBlockTrieRoot(t *testing.T) {
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
		t.Errorf("generation of private key failed")
	}
	db, err := ethdb.NewMemDatabase()
	if err != nil {
		t.Errorf("creation of memory database failed")
	}
	trieDB := trie.NewDatabase(db)
	// Genesis block.
	genesis := observer.NewBlock(privKey)
	if genesis.Number().Uint64() != 0 {
		t.Errorf("number of genesis block is not 0")
	}
	// Now get trie and modify it to generate a successor block.
	tr, err := trie.New(genesis.TrieRoot(), trieDB)
	if err != nil {
		t.Errorf("instantiating the trie failed")
	}
	update(tr, "foo", "123")
	update(tr, "bar", "456")
	update(tr, "baz", "789")
	trieRoot, err := tr.Commit(nil)
	if err != nil {
		t.Errorf("committing the trie updates failed")
	}
	second := genesis.CreateSuccessor(trieRoot, privKey)
	if second.Number().Uint64() != 1 {
		t.Errorf("number of 2nd block is not 1")
	}
	if bytes.Equal(genesis.Signature(), second.Signature()) {
		t.Errorf("signature of 2nd block has to be different")
	}
	if !bytes.Equal(genesis.Hash().Bytes(), second.PrevHash().Bytes()) {
		t.Errorf("2nd block previous hash has to be genesis block hash")
	}
	// Last but not least a final block.
	tr, err = trie.New(second.TrieRoot(), trieDB)
	if err != nil {
		t.Errorf("instantiating the next trie failed")
	}
	assert(tr, "foo", "123")
	assert(tr, "bar", "456")
	assert(tr, "baz", "789")
	update(tr, "foo", "000")
	update(tr, "yadda", "999")
	trieRoot, err = tr.Commit(nil)
	if err != nil {
		t.Errorf("committing the trie updates failed")
	}
	third := second.CreateSuccessor(trieRoot, privKey)
	if third.Number().Uint64() != 2 {
		t.Errorf("number of 3rd block is not 2")
	}
	if bytes.Equal(second.Signature(), third.Signature()) {
		t.Errorf("signature of 3rd block has to be different")
	}
	if !bytes.Equal(second.Hash().Bytes(), third.PrevHash().Bytes()) {
		t.Errorf("3rd block hash has to be second block hash")
	}
}
