// Copyright 2014 The go-ethereum Authors
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

// Package eth implements the Ethereum protocol.
package eth

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/rlp"
)

// upgradeSequentialKeys checks the chain database version and
// starts a background process to make upgrades if necessary.
// Returns a stop function that blocks until the process has
// been safely stopped.
func upgradeSequentialKeys(db ethdb.Database) (stopFn func()) {
	var keyPtr []byte
	var phase byte
	storePtr := func() {
		// store current ptr
		db.Put([]byte("useSequentialKeys"), append([]byte{phase}, keyPtr...))
	}
	// read current ptr
	data, _ := db.Get([]byte("useSequentialKeys"))
	if len(data) > 0 {
		phase = data[0]
		keyPtr = data[1:]
	}
	if phase == 4 {
		return nil // nothing to do
	}

	glog.V(logger.Info).Infof("Upgrading chain database to use sequential keys")

	stopChn := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		it := db.(*ethdb.LDBDatabase).NewIterator()
		it.Seek(keyPtr)
		cnt, cc, cb, cr := 0, 0, 0, 0
loop:	for {
			keyPtr := it.Key()
			switch phase {
			case 0:
				phase++
				it.Seek([]byte("block-num-"))
			case 1:
				if string(keyPtr[0:10]) == "block-num-" {
					// if key is too long, it is probably a "block-" entry
					// where the first 4 bytes of the hash happens to be "num-"
					if len(keyPtr) < 20 {
						cc++
						if cc % 100000 == 0 {
							glog.V(logger.Info).Infof("converting %d canonical numbers...", cc)
						}
						number := big.NewInt(0).SetBytes(keyPtr[10:]).Uint64()
						newKey := []byte("header-12345678-num")
						binary.BigEndian.PutUint64(newKey[7:15], number)
						db.Put(newKey, it.Value())
						db.Delete(keyPtr)
					}
					it.Next()
				} else {
					glog.V(logger.Info).Infof("converted %d canonical numbers...", cc)
					phase++
					it.Release()
					it = db.(*ethdb.LDBDatabase).NewIterator()
					it.Seek([]byte("block-"))
				}
			case 2:
				if string(keyPtr[0:6]) == "block-" {
					if len(keyPtr) < 38 {
						// invalid entry
						db.Delete(keyPtr)
						it.Next()
					} else {
						cb++
						if cb % 10000 == 0 {
							glog.V(logger.Info).Infof("converting %d blocks...", cb)
						}
						// convert header, body, td and block receipts
						var keyPrefix [38]byte
						copy(keyPrefix[:], keyPtr[0:38])
						hash := keyPrefix[6:38]
						upgradeSequentialKeysForHash(db, hash)
						// delete old db entries belonging to this hash
						for len(it.Key()) >= 38 && bytes.Equal(it.Key()[0:38], keyPrefix[:]) {
							db.Delete(it.Key())
							it.Next()
						}
						db.Delete(append([]byte("receipts-block-"), hash...))
					}
				} else {
					glog.V(logger.Info).Infof("converted %d blocks...", cb)
					phase++
					it.Release()
					it = db.(*ethdb.LDBDatabase).NewIterator()
					it.Seek([]byte("receipts-block-"))
				}
			case 3:
				if string(keyPtr[0:15]) == "receipts-block-" {
					// phase 2 already converted receipts belonging to existing
					// blocks, just remove if there's anything left
					cr++
					db.Delete(keyPtr)
					it.Next()
				} else {
					if cr > 0 {
						glog.V(logger.Info).Infof("removed %d orphaned block receipts...", cr)
					}
					phase++
				}
			case 4:
				glog.V(logger.Info).Infof("Database conversion successful")
				break loop
			}
			
			select {
			case <-time.After(time.Microsecond * 100): // make sure other processes don't get starved
			case <-stopChn:
				keyPtr = it.Key()
				glog.V(logger.Info).Infof("Database conversion aborted")
				break loop
			}
			cnt++
			if cnt % 1000 == 0 {
				storePtr()
			}
		}
		it.Release()
		storePtr()
		close(stopped)
	}()

	return func() {
		close(stopChn)
		<-stopped
	}
}

// upgradeSequentialKeysForHash upgrades the header, body, td and block receipts
// database entries belonging to a single hash (doesn't delete old data).
func upgradeSequentialKeysForHash(db ethdb.Database, hash []byte) {
	// get old chain data and block number
	headerRLP, _ := db.Get(append(append([]byte("block-"), hash...), []byte("-header")...))
	if len(headerRLP) == 0 {
		return
	}
	header := new(types.Header)
	if err := rlp.Decode(bytes.NewReader(headerRLP), header); err != nil {
		return
	}
	number := header.Number.Uint64()
	bodyRLP, _ := db.Get(append(append([]byte("block-"), hash...), []byte("-body")...))
	tdRLP, _ := db.Get(append(append([]byte("block-"), hash...), []byte("-td")...))
	receiptsRLP, _ := db.Get(append([]byte("receipts-block-"), hash...))
	// store new hash -> number association
	encNum := make([]byte, 8)
	binary.BigEndian.PutUint64(encNum, number)
	db.Put(append([]byte("blk-hash-"), hash...), encNum)
	// store new chain data
	db.Put(append(append([]byte("header-"), encNum...), hash...), headerRLP)
	if len(bodyRLP) != 0 {
		db.Put(append(append(append([]byte("blk-"), encNum...), hash...), []byte("-body")...), bodyRLP)
	}
	if len(tdRLP) != 0 {
		db.Put(append(append(append([]byte("blk-"), encNum...), hash...), []byte("-td")...), tdRLP)
	}
	if len(receiptsRLP) != 0 {
		db.Put(append(append(append([]byte("blk-"), encNum...), hash...), []byte("-receipts")...), receiptsRLP)
	}
}

// upgradeChainDatabase ensures that the chain database stores block split into
// separate header and body entries.
func upgradeChainDatabase(db ethdb.Database) error {
	// Short circuit if the head block is stored already as separate header and body
	data, err := db.Get([]byte("LastBlock"))
	if err != nil {
		return nil
	}
	head := common.BytesToHash(data)

	if block := core.GetBlockByHashOld(db, head); block == nil {
		return nil
	}
	// At least some of the database is still the old format, upgrade (skip the head block!)
	glog.V(logger.Info).Info("Old database detected, upgrading...")

	if db, ok := db.(*ethdb.LDBDatabase); ok {
		blockPrefix := []byte("block-hash-")
		for it := db.NewIterator(); it.Next(); {
			// Skip anything other than a combined block
			if !bytes.HasPrefix(it.Key(), blockPrefix) {
				continue
			}
			// Skip the head block (merge last to signal upgrade completion)
			if bytes.HasSuffix(it.Key(), head.Bytes()) {
				continue
			}
			// Load the block, split and serialize (order!)
			block := core.GetBlockByHashOld(db, common.BytesToHash(bytes.TrimPrefix(it.Key(), blockPrefix)))

			if err := core.WriteTd(db, block.Hash(), block.NumberU64(), block.DeprecatedTd()); err != nil {
				return err
			}
			if err := core.WriteBody(db, block.Hash(), block.NumberU64(), block.Body()); err != nil {
				return err
			}
			if err := core.WriteHeader(db, block.Header()); err != nil {
				return err
			}
			if err := db.Delete(it.Key()); err != nil {
				return err
			}
		}
		// Lastly, upgrade the head block, disabling the upgrade mechanism
		current := core.GetBlockByHashOld(db, head)

		if err := core.WriteTd(db, current.Hash(), current.NumberU64(), current.DeprecatedTd()); err != nil {
			return err
		}
		if err := core.WriteBody(db, current.Hash(), current.NumberU64(), current.Body()); err != nil {
			return err
		}
		if err := core.WriteHeader(db, current.Header()); err != nil {
			return err
		}
	}
	return nil
}

func addMipmapBloomBins(db ethdb.Database) (err error) {
	const mipmapVersion uint = 2

	// check if the version is set. We ignore data for now since there's
	// only one version so we can easily ignore it for now
	var data []byte
	data, _ = db.Get([]byte("setting-mipmap-version"))
	if len(data) > 0 {
		var version uint
		if err := rlp.DecodeBytes(data, &version); err == nil && version == mipmapVersion {
			return nil
		}
	}

	defer func() {
		if err == nil {
			var val []byte
			val, err = rlp.EncodeToBytes(mipmapVersion)
			if err == nil {
				err = db.Put([]byte("setting-mipmap-version"), val)
			}
			return
		}
	}()
	latestHash := core.GetHeadBlockHash(db)
	latestBlock := core.GetBlock(db, latestHash, core.GetBlockNumber(db, latestHash))
	if latestBlock == nil { // clean database
		return
	}

	tstart := time.Now()
	glog.V(logger.Info).Infoln("upgrading db log bloom bins")
	for i := uint64(0); i <= latestBlock.NumberU64(); i++ {
		hash := core.GetCanonicalHash(db, i)
		if (hash == common.Hash{}) {
			return fmt.Errorf("chain db corrupted. Could not find block %d.", i)
		}
		core.WriteMipmapBloom(db, i, core.GetBlockReceipts(db, hash, i))
	}
	glog.V(logger.Info).Infoln("upgrade completed in", time.Since(tstart))
	return nil
}
