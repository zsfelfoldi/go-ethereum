// Copyright 2015 The go-ethereum Authors
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
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
	"golang.org/x/net/context"
)

type LightTrie struct {
	trie         *trie.SecureTrie
	originalRoot common.Hash
	odr          OdrBackend
	db           ethdb.Database
}

func NewLightTrie(root common.Hash, odr OdrBackend, useFakeMap bool) *LightTrie {
	return &LightTrie{
		// SecureTrie is initialized before first request
		originalRoot: root,
		odr:          odr,
		db:           odr.Database(),
	}
}

// retrieveKey retrieves a single key, returns true and stores nodes in local
// database if successful
func (t *LightTrie) retrieveKey(ctx context.Context, key []byte) bool {
	r := &TrieRequest{ctx: ctx, root: t.originalRoot, key: key}
	return t.odr.Retrieve(r) == nil
}

func (t *LightTrie) do(ctx context.Context, fallbackKey []byte, fn func() error) error {
	err := fn()
	for err != nil {
		mn, ok := err.(*trie.MissingNodeError)
		if !ok {
			return err
		}

		var key []byte
		if mn.PrefixLen+mn.SuffixLen > 0 {
			key = mn.Key
		} else {
			key = fallbackKey
		}
		if !t.retrieveKey(ctx, key) {
			break
		}
		err = fn()
	}
	return err
}

func (t *LightTrie) Get(ctx context.Context, key []byte) (res []byte, err error) {
	err = t.do(ctx, key, func() (err error) {
		if t.trie == nil {
			t.trie, err = trie.NewSecure(t.originalRoot, t.db)
		}
		if err == nil {
			res, err = t.trie.TryGet(key)
		}
		return
	})
	return
}

func (t *LightTrie) Update(ctx context.Context, key, value []byte) (err error) {
	err = t.do(ctx, key, func() (err error) {
		if t.trie == nil {
			t.trie, err = trie.NewSecure(t.originalRoot, t.db)
		}
		if err == nil {
			err = t.trie.TryUpdate(key, value)
		}
		return
	})
	return
}

func (t *LightTrie) Delete(ctx context.Context, key []byte) (err error) {
	err = t.do(ctx, key, func() (err error) {
		if t.trie == nil {
			t.trie, err = trie.NewSecure(t.originalRoot, t.db)
		}
		if err == nil {
			err = t.trie.TryDelete(key)
		}
		return
	})
	return
}
