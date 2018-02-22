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
	"bytes"
	"encoding/binary"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	blockPrefix  = []byte("obs-")    // blockPrefix + num -> block
	headBlockKey = []byte("obshead") // keeps track of the last observer block
)

// StmtLookupEntry is a positional metadata to help looking up the statement
// inside its block.
type StmtLookupEntry struct {
	BlockNumber uint64
	Index       uint64
}

// GetBlock retrieves an entire block corresponding to the number.
func GetBlock(db ethdb.Database, number uint64) *Block {
	data, _ := db.Get(mkBlockKey(number))
	if len(data) == 0 {
		return nil
	}
	b := new(Block)
	if err := rlp.Decode(bytes.NewReader(data), b); err != nil {
		log.Error("invalid block RLP", "number", number, "err", err)
		return nil
	}
	return b
}

// GetHeadBlock retrieves the head block.
func GetHeadBlock(db ethdb.Database) *Block {
	key, _ := db.Get(headBlockKey)
	if len(key) == 0 {
		return nil
	}
	data, _ := db.Get(key)
	if len(data) == 0 {
		return nil
	}
	b := new(Block)
	if err := rlp.Decode(bytes.NewReader(data), b); err != nil {
		log.Error("invalid block RLP", "key", key, "err", err)
		return nil
	}
	return b
}

// WriteBlock serializes and writes block into the database. It also
// updates the pointer to the head block.
func WriteBlock(db ethdb.Database, block *Block) error {
	var buf bytes.Buffer
	if err := block.EncodeRLP(&buf); err != nil {
		return err
	}
	key := mkBlockKey(block.header.Number)
	if err := db.Put(key, buf.Bytes()); err != nil {
		log.Crit("failed to store observer chain block data", "err", err)
		return err
	}
	if err := db.Put(headBlockKey, key); err != nil {
		log.Crit("failed to store observer chain head block key", "err", err)
		return err
	}
	return nil
}

// -----
// HELPER
// -----

// mkBlockKey creates the database key for a given block number.
// Ex: obs-0, obs-124
func mkBlockKey(number uint64) []byte {
	enc := make([]byte, 8)
	binary.BigEndian.PutUint64(enc, number)
	return append(blockPrefix, enc...)
}
