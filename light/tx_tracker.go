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
	"bytes"
	"context"
	"encoding/binary"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/ethdb"
)

var (
	txHashesByBlockPrefix = []byte("txByBlock-")
	trackedHashes         = []byte("tracked-")
)

type TxTracker struct {
	db      ethdb.Database
	odr     OdrBackend
	lock    sync.RWMutex
	head    common.Hash
	headNum uint64
}

type TxTrackerUpdate struct {
	Head           common.Hash
	Added, Removed []common.Hash
}

func forEachKey(db ethdb.Database, startPrefix, endPrefix []byte, fn func(key []byte)) {
	it := db.(*ethdb.LDBDatabase).NewIterator()
	it.Seek(startPrefix)
	for it.Valid() {
		key := it.Key()
		if bytes.Compare(key, endPrefix) == 1 {
			break
		}
		fn(common.CopyBytes(key))
		it.Next()
	}
	it.Release()
}

// implements core.ChainProcessor
func (t *TxTracker) NewHead(headNum uint64, rollBack bool) {
	t.lock.Lock()
	defer t.lock.Unlock()

	batch := t.db.NewBatch()
	if rollBack && headNum < t.headNum {
		var start, stop [8]byte
		binary.BigEndian.PutUint64(start[:], headNum+1)
		binary.BigEndian.PutUint64(stop[:], t.headNum)
		forEachKey(t.db, append(txHashesByBlockPrefix, start[:]...), append(txHashesByBlockPrefix, stop[:]...), func(key []byte) {
			hash := common.BytesToHash(key[len(txHashesByBlockPrefix)+8:])
			batch.Delete(key)
			core.DeleteTransactionChainPosition(batch, hash)
			batch.Put(append(trackedHashes, hash.Bytes()...), nil)
		})
	}

	t.head = core.GetCanonicalHash(t.db, headNum)
	t.headNum = headNum
	batch.Write()
}

// blocking
func (t *TxTracker) CheckTxChainPos(ctx context.Context, hash common.Hash, pos core.TxChainPos) bool {
	p := core.GetTransactionChainPosition(t.db, hash)
	if p.BlockHash != (common.Hash{}) {
		return pos == p
	}

	GetBlock(ctx, t.odr, pos.BlockHash, pos.BlockIndex)
}
