// Copyright 2023 The go-ethereum Authors
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
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

func determineRange(db ethdb.Iteratee, keyPrefix []byte) Range {
	var (
		iter      = db.NewIterator(keyPrefix, nil)
		keyLength = len(keyPrefix)
		p         Range
	)
	defer iter.Release()
	for iter.Next() {
		if len(iter.Key()) != keyLength+8 {
			log.Warn("Invalid key length in the canonical chain database", "key", fmt.Sprintf("%#x", iter.Key()))
			continue
		}
		period := binary.BigEndian.Uint64(iter.Key()[keyLength : keyLength+8])
		if p.Start == 0 {
			p.Start = period
		} else if p.End != period {
			log.Warn("Gap in the canonical chain database")
			break // continuity guaranteed
		}
		p.End = period + 1
	}
	return p
}

// fixedCommitteeRootsStore stores fixedCommitteeRoots
type fixedCommitteeRootsStore struct {
	periods Range
	cache   *lru.Cache[uint64, common.Hash]
}

// newFixedCommitteeRootsStore creates a new fixedCommitteeRootsStore and
// verifies the continuity of the range in database.
func newFixedCommitteeRootsStore(db ethdb.Iteratee) *fixedCommitteeRootsStore {
	return &fixedCommitteeRootsStore{
		cache:   lru.NewCache[uint64, common.Hash](100),
		periods: determineRange(db, rawdb.FixedCommitteeRootKey),
	}
}

// databaseKey returns the database key belonging to the given period.
func (cs *fixedCommitteeRootsStore) databaseKey(period uint64) []byte {
	return binary.BigEndian.AppendUint64(rawdb.FixedCommitteeRootKey, period)
}

// add adds the given item to the database. It also ensures that the range remains
// continuous. Can be used either with a batch or database backend.
func (cs *fixedCommitteeRootsStore) add(backend ethdb.KeyValueWriter, period uint64, value common.Hash) error {
	if !cs.periods.CanExpand(period) {
		return fmt.Errorf("period expansion is not allowed, first: %d, next: %d, period: %d", cs.periods.Start, cs.periods.End, period)
	}
	if err := backend.Put(cs.databaseKey(period), value[:]); err != nil {
		return err
	}
	cs.cache.Add(period, value)
	cs.periods.Expand(period)
	return nil
}

// deleteFrom removes items starting from the given period.
func (cs *fixedCommitteeRootsStore) deleteFrom(db ethdb.KeyValueWriter, fromPeriod uint64) (deleted Range) {
	keepRange, deleteRange := cs.periods.Split(fromPeriod)
	deleteRange.Each(func(period uint64) {
		db.Delete(cs.databaseKey(period))
		cs.cache.Remove(period)
	})
	cs.periods = keepRange
	return deleteRange
}

// get returns the item at the given period or the null value of the given type
// if no item is present.
// Note: get is thread safe in itself and therefore can be called either with
// locked or unlocked chain mutex.
func (cs *fixedCommitteeRootsStore) get(backend ethdb.KeyValueReader, period uint64) (value common.Hash, ok bool) {
	if value, ok = cs.cache.Get(period); ok {
		return
	}
	enc, err := backend.Get(cs.databaseKey(period))
	if err != nil {
		return common.Hash{}, false
	}
	if len(enc) != common.HashLength {
		log.Error("Error decoding canonical store value", "error", "incorrect length for committee root entry in the database")
	}
	return common.BytesToHash(enc), true
}

// serializedSyncCommitteeStore stores SerializedSyncCommittee
type serializedSyncCommitteeStore struct {
	periods Range
	cache   *lru.Cache[uint64, *types.SerializedSyncCommittee]
}

// newSerializedSyncCommitteSTore creates a new serializedSyncCommitteeStore and
// verifies the continuity of the range in database.
func newSerializedSyncCommitteSTore(db ethdb.Iteratee) *serializedSyncCommitteeStore {
	return &serializedSyncCommitteeStore{
		cache:   lru.NewCache[uint64, *types.SerializedSyncCommittee](100),
		periods: determineRange(db, rawdb.SyncCommitteeKey),
	}
}

// databaseKey returns the database key belonging to the given period.
func (cs *serializedSyncCommitteeStore) databaseKey(period uint64) []byte {
	return binary.BigEndian.AppendUint64(rawdb.SyncCommitteeKey, period)
}

// add adds the given item to the database. It also ensures that the range remains
// continuous. Can be used either with a batch or database backend.
func (cs *serializedSyncCommitteeStore) add(db ethdb.KeyValueWriter, period uint64, value *types.SerializedSyncCommittee) error {
	if !cs.periods.CanExpand(period) {
		return fmt.Errorf("period expansion is not allowed, first: %d, next: %d, period: %d", cs.periods.Start, cs.periods.End, period)
	}
	if err := db.Put(cs.databaseKey(period), value[:]); err != nil {
		return err
	}
	cs.cache.Add(period, value)
	cs.periods.Expand(period)
	return nil
}

// deleteFrom removes items starting from the given period.
func (cs *serializedSyncCommitteeStore) deleteFrom(db ethdb.KeyValueWriter, fromPeriod uint64) (deleted Range) {
	keepRange, deleteRange := cs.periods.Split(fromPeriod)
	deleteRange.Each(func(period uint64) {
		db.Delete(cs.databaseKey(period))
		cs.cache.Remove(period)
	})
	cs.periods = keepRange
	return deleteRange
}

// get returns the item at the given period or the null value of the given type
// if no item is present.
// Note: get is thread safe in itself and therefore can be called either with
// locked or unlocked chain mutex.
func (cs *serializedSyncCommitteeStore) get(db ethdb.KeyValueReader, period uint64) (value *types.SerializedSyncCommittee, ok bool) {
	if value, ok = cs.cache.Get(period); ok {
		return
	}
	enc, err := db.Get(cs.databaseKey(period))
	if err != nil {
		return nil, false
	}
	if len(enc) != types.SerializedSyncCommitteeSize {
		log.Error("Error decoding canonical store value", "error", "incorrect length for serialized committee entry in the database")
		return nil, false
	}
	committee := new(types.SerializedSyncCommittee)
	copy(committee[:], enc)
	return committee, true
}

// updatesStore stores lightclient updates
type updatesStore struct {
	periods Range
	cache   *lru.Cache[uint64, *types.LightClientUpdate]
}

// newUpdatesStore creates a new updatesStore and
// verifies the continuity of the range in database.
func newUpdatesStore(db ethdb.Iteratee) *updatesStore {
	return &updatesStore{
		cache:   lru.NewCache[uint64, *types.LightClientUpdate](100),
		periods: determineRange(db, rawdb.BestUpdateKey),
	}
}

// databaseKey returns the database key belonging to the given period.
func (cs *updatesStore) databaseKey(period uint64) []byte {
	return binary.BigEndian.AppendUint64(rawdb.BestUpdateKey, period)
}

// add adds the given item to the database. It also ensures that the range remains
// continuous. Can be used either with a batch or database backend.
func (cs *updatesStore) add(db ethdb.KeyValueWriter, period uint64, update *types.LightClientUpdate) error {
	if !cs.periods.CanExpand(period) {
		return fmt.Errorf("period expansion is not allowed, first: %d, next: %d, period: %d", cs.periods.Start, cs.periods.End, period)
	}
	enc, err := rlp.EncodeToBytes(update)
	if err != nil {
		return err
	}
	if err := db.Put(cs.databaseKey(period), enc); err != nil {
		return err
	}
	cs.cache.Add(period, update)
	cs.periods.Expand(period)
	return nil
}

// deleteFrom removes items starting from the given period.
func (cs *updatesStore) deleteFrom(db ethdb.KeyValueWriter, fromPeriod uint64) (deleted Range) {
	keepRange, deleteRange := cs.periods.Split(fromPeriod)
	deleteRange.Each(func(period uint64) {
		db.Delete(cs.databaseKey(period))
		cs.cache.Remove(period)
	})
	cs.periods = keepRange
	return deleteRange
}

// get returns the item at the given period or the null value of the given type
// if no item is present.
// Note: get is thread safe in itself and therefore can be called either with
// locked or unlocked chain mutex.
func (cs *updatesStore) get(db ethdb.KeyValueReader, period uint64) (value *types.LightClientUpdate, ok bool) {
	if value, ok = cs.cache.Get(period); ok {
		return
	}
	enc, err := db.Get(cs.databaseKey(period))
	if err != nil {
		return nil, false
	}
	update := new(types.LightClientUpdate)
	if err := rlp.DecodeBytes(enc, update); err != nil {
		log.Error("Error decoding canonical store value", "error", err)
		return nil, false
	}
	return update, true
}
