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
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
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
	chain   LightChain
	lock    sync.RWMutex
	head    common.Hash
	headNum uint64

	fetchLock sync.RWMutex
	blocks    map[common.Hash]*trackerBlockFetch
}

type TxTrackerUpdate struct {
	Head           common.Hash
	HeadNum        uint64
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

type trackerTxFetch struct {
	hash  common.Hash
	pos   core.TxChainPos
	errCh chan bool
}

type trackerBlockFetch struct {
	timeout mclock.AbsTime
	txFetch []trackerTxFetch
}

const fetchBlockTimeout = time.Second * 10

func (t *TxTracker) foundTxPos(txHash common.Hash, pos core.TxChainPos) {
	batch := t.db.NewBatch()
	core.WriteTransactionChainPosition(batch, txHash, pos)
	batch.Delete(append(trackedHashes, txHash.Bytes()...))
	var encNum [8]byte
	binary.BigEndian.PutUint64(encNum[:], pos.BlockNumber)
	batch.Put(append(txHashesByBlockPrefix, encNum[:]...), nil)
	batch.Write()
}

// called under chain lock
func (t *TxTracker) CheckTxChainPos(txHash common.Hash, pos core.TxChainPos, errCh chan bool) (ok, waitErrCh bool) {
	if core.GetCanonicalHash(t.db, pos.BlockNumber) != pos.BlockHash {
		// chain is locked, local and remote head matches, reported inclusion block is expected to be canonical
		return false, false
	}

	if storedPos := core.GetTransactionChainPosition(t.db, txHash); storedPos.BlockHash != (common.Hash{}) {
		return pos == storedPos, false
	}
	if block := core.GetBlock(t.db, pos.BlockHash, pos.BlockNumber); block == nil {
		txs := block.Transactions()
		if uint64(len(txs)) > pos.TxIndex && txs[pos.TxIndex].Hash() == txHash {
			t.foundTxPos(txHash, pos)
			return true, false
		} else {
			return false, false
		}
	}

	t.fetchLock.Lock()
	defer t.fetchLock.Unlock()

	txFetch := trackerTxFetch{txHash, pos, errCh}

	if f := t.blocks[pos.BlockHash]; f != nil {
		f.timeout = mclock.Now() + mclock.AbsTime(fetchBlockTimeout)
		f.txFetch = append(f.txFetch, txFetch)
	} else {
		f = &trackerBlockFetch{timeout: mclock.Now() + mclock.AbsTime(fetchBlockTimeout), txFetch: []trackerTxFetch{txFetch}}
		t.blocks[pos.BlockHash] = f
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			block, _ := GetBlock(ctx, t.odr, pos.BlockHash, pos.BlockNumber)
			var processed bool

			if block != nil {
				txs := block.Transactions()
				t.chain.LockChain()
				defer t.chain.UnlockChain()
				// if the block is no longer canonical, the tx chain positions have been invalidated
				if core.GetCanonicalHash(t.db, pos.BlockNumber) == pos.BlockHash {
					processed = true
					for _, txFetch := range f.txFetch {
						if uint64(len(txs)) > txFetch.pos.TxIndex && txs[txFetch.pos.TxIndex].Hash() == txFetch.hash {
							t.foundTxPos(txFetch.hash, txFetch.pos)
							txFetch.errCh <- false
						} else {
							txFetch.errCh <- true
						}
					}
				}
			}

			if !processed {
				for _, txFetch := range f.txFetch {
					txFetch.errCh <- false
				}
			}
			t.fetchLock.Lock()
			delete(t.blocks, pos.BlockHash)
			t.fetchLock.Unlock()
		}()
		go func() {
			for {
				t.fetchLock.RLock()
				wait := time.Duration(f.timeout - mclock.Now())
				t.fetchLock.RUnlock()
				if wait <= 0 {
					cancel()
					return
				}
				time.Sleep(wait)
			}
		}()
	}

	return true, true
}
