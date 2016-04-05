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

package core

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/rlp"
)

var useNewDb = true
const MissingNumber = uint64(0xffffffffffffffff)

var (
	headHeaderKey = []byte("LastHeader")
	headBlockKey  = []byte("LastBlock")
	headFastKey   = []byte("LastFast")

	blockPrefix    = []byte("block-")
	blockNumPrefix = []byte("block-num-")

	headerSuffix = []byte("-header")
	bodySuffix   = []byte("-body")
	tdSuffix     = []byte("-td")
	// new chaindb using sequential keys
	newBlockPrefix = []byte("blk-")		// newBlockPrefix + uint64 big endian + hash + suffix
	headerPrefix = []byte("header-")		// newHeaderPrefix + uint64 big endian + hash
	headerNumSuffix = []byte("-num")	// newHeaderPrefix + uint64 big endian + headerNumSuffix -> hash

	newBlockHashPrefix = []byte("blk-hash-")	// blockHashPrefix + hash -> num
	blockReceiptsSuffix = []byte("-receipts")
	//
	txMetaSuffix        = []byte{0x01}
	receiptsPrefix      = []byte("receipts-")
	blockReceiptsPrefix = []byte("receipts-block-")

	mipmapPre    = []byte("mipmap-log-bloom-")
	MIPMapLevels = []uint64{1000000, 500000, 100000, 50000, 1000}

	blockHashPrefix = []byte("block-hash-") // [deprecated by the header/block split, remove eventually]

	configPrefix = []byte("ethereum-config-") // config prefix for the db
)

func encodeBlockNumber(number uint64) []byte {
	enc := make([]byte, 8)
	binary.BigEndian.PutUint64(enc, number)
	return enc
}

// GetCanonicalHash retrieves a hash assigned to a canonical block number.
func GetCanonicalHash(db ethdb.Database, number uint64) common.Hash {
	var data []byte
	if useNewDb {
		data, _ = db.Get(append(append(headerPrefix, encodeBlockNumber(number)...), headerNumSuffix...))
	} else {
		data, _ = db.Get(append(blockNumPrefix, big.NewInt(int64(number)).Bytes()...))
	}
	if len(data) == 0 {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

// GetBlockNumber returns the block number assigned to a block hash
// if the corresponding header is present in the database
func GetBlockNumber(db ethdb.Database, hash common.Hash) uint64 {
	data, _ := db.Get(append(newBlockHashPrefix, hash.Bytes()...))
	if len(data) != 8 {
		return MissingNumber
	}
	return binary.BigEndian.Uint64(data)
}

// GetHeadHeaderHash retrieves the hash of the current canonical head block's
// header. The difference between this and GetHeadBlockHash is that whereas the
// last block hash is only updated upon a full block import, the last header
// hash is updated already at header import, allowing head tracking for the
// light synchronization mechanism.
func GetHeadHeaderHash(db ethdb.Database) common.Hash {
	data, _ := db.Get(headHeaderKey)
	if len(data) == 0 {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

// GetHeadBlockHash retrieves the hash of the current canonical head block.
func GetHeadBlockHash(db ethdb.Database) common.Hash {
	data, _ := db.Get(headBlockKey)
	if len(data) == 0 {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

// GetHeadFastBlockHash retrieves the hash of the current canonical head block during
// fast synchronization. The difference between this and GetHeadBlockHash is that
// whereas the last block hash is only updated upon a full block import, the last
// fast hash is updated when importing pre-processed blocks.
func GetHeadFastBlockHash(db ethdb.Database) common.Hash {
	data, _ := db.Get(headFastKey)
	if len(data) == 0 {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

// GetHeaderRLP retrieves a block header in its raw RLP database encoding, or nil
// if the header's not found.
func GetHeaderRLP(db ethdb.Database, hash common.Hash, number uint64) rlp.RawValue {
	if useNewDb {
		data, _ := db.Get(append(append(headerPrefix, encodeBlockNumber(number)...), hash.Bytes()...))
		return data
	} else {
		data, _ := db.Get(append(append(blockPrefix, hash.Bytes()...), headerSuffix...))
		return data
	}
}

// GetHeader retrieves the block header corresponding to the hash, nil if none
// found.
func GetHeader(db ethdb.Database, hash common.Hash, number uint64) *types.Header {
	data := GetHeaderRLP(db, hash, number)
	if len(data) == 0 {
		return nil
	}
	header := new(types.Header)
	if err := rlp.Decode(bytes.NewReader(data), header); err != nil {
		glog.V(logger.Error).Infof("invalid block header RLP for hash %x: %v", hash, err)
		return nil
	}
	return header
}

// GetBodyRLP retrieves the block body (transactions and uncles) in RLP encoding.
func GetBodyRLP(db ethdb.Database, hash common.Hash, number uint64) rlp.RawValue {
	if useNewDb {
		data, _ := db.Get(append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash.Bytes()...), bodySuffix...))
		return data
	} else {
		data, _ := db.Get(append(append(blockPrefix, hash.Bytes()...), bodySuffix...))
		return data
	}
}

// GetBody retrieves the block body (transactons, uncles) corresponding to the
// hash, nil if none found.
func GetBody(db ethdb.Database, hash common.Hash, number uint64) *types.Body {
	data := GetBodyRLP(db, hash, number)
	if len(data) == 0 {
		return nil
	}
	body := new(types.Body)
	if err := rlp.Decode(bytes.NewReader(data), body); err != nil {
		glog.V(logger.Error).Infof("invalid block body RLP for hash %x: %v", hash, err)
		return nil
	}
	return body
}

// GetTd retrieves a block's total difficulty corresponding to the hash, nil if
// none found.
func GetTd(db ethdb.Database, hash common.Hash, number uint64) *big.Int {
	var data []byte
	if useNewDb {
		data, _ = db.Get(append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash[:]...), tdSuffix...))
	} else {
		data, _ = db.Get(append(append(blockPrefix, hash.Bytes()...), tdSuffix...))
	}
	if len(data) == 0 {
		return nil
	}
	td := new(big.Int)
	if err := rlp.Decode(bytes.NewReader(data), td); err != nil {
		glog.V(logger.Error).Infof("invalid block total difficulty RLP for hash %x: %v", hash, err)
		return nil
	}
	return td
}

// GetBlock retrieves an entire block corresponding to the hash, assembling it
// back from the stored header and body.
func GetBlock(db ethdb.Database, hash common.Hash, number uint64) *types.Block {
	// Retrieve the block header and body contents
	header := GetHeader(db, hash, number)
	if header == nil {
		return nil
	}
	body := GetBody(db, hash, header.Number.Uint64())
	if body == nil {
		return nil
	}
	// Reassemble the block and return
	return types.NewBlockWithHeader(header).WithBody(body.Transactions, body.Uncles)
}

// GetBlockReceipts retrieves the receipts generated by the transactions included
// in a block given by its hash.
func GetBlockReceipts(db ethdb.Database, hash common.Hash, number uint64) types.Receipts {
	var data []byte
	if useNewDb {
		data, _ = db.Get(append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash[:]...), blockReceiptsSuffix...))
	} else {
		data, _ = db.Get(append(blockReceiptsPrefix, hash[:]...))
	}
	if len(data) == 0 {
		return nil
	}
	storageReceipts := []*types.ReceiptForStorage{}
	if err := rlp.DecodeBytes(data, &storageReceipts); err != nil {
		glog.V(logger.Error).Infof("invalid receipt array RLP for hash %x: %v", hash, err)
		return nil
	}
	receipts := make(types.Receipts, len(storageReceipts))
	for i, receipt := range storageReceipts {
		receipts[i] = (*types.Receipt)(receipt)
	}
	return receipts
}

// GetTransaction retrieves a specific transaction from the database, along with
// its added positional metadata.
func GetTransaction(db ethdb.Database, hash common.Hash) (*types.Transaction, common.Hash, uint64, uint64) {
	// Retrieve the transaction itself from the database
	data, _ := db.Get(hash.Bytes())
	if len(data) == 0 {
		return nil, common.Hash{}, 0, 0
	}
	var tx types.Transaction
	if err := rlp.DecodeBytes(data, &tx); err != nil {
		return nil, common.Hash{}, 0, 0
	}
	// Retrieve the blockchain positional metadata
	data, _ = db.Get(append(hash.Bytes(), txMetaSuffix...))
	if len(data) == 0 {
		return nil, common.Hash{}, 0, 0
	}
	var meta struct {
		BlockHash  common.Hash
		BlockIndex uint64
		Index      uint64
	}
	if err := rlp.DecodeBytes(data, &meta); err != nil {
		return nil, common.Hash{}, 0, 0
	}
	return &tx, meta.BlockHash, meta.BlockIndex, meta.Index
}

// GetReceipt returns a receipt by hash
func GetReceipt(db ethdb.Database, txHash common.Hash) *types.Receipt {
	data, _ := db.Get(append(receiptsPrefix, txHash[:]...))
	if len(data) == 0 {
		return nil
	}
	var receipt types.ReceiptForStorage
	err := rlp.DecodeBytes(data, &receipt)
	if err != nil {
		glog.V(logger.Core).Infoln("GetReceipt err:", err)
	}
	return (*types.Receipt)(&receipt)
}

// WriteCanonicalHash stores the canonical hash for the given block number.
func WriteCanonicalHash(db ethdb.Database, hash common.Hash, number uint64) error {
	var key []byte
	if useNewDb {
		key = append(append(headerPrefix, encodeBlockNumber(number)...), headerNumSuffix...)
	} else {	
		key = append(blockNumPrefix, big.NewInt(int64(number)).Bytes()...)
	}
	if err := db.Put(key, hash.Bytes()); err != nil {
		glog.Fatalf("failed to store number to hash mapping into database: %v", err)
		return err
	}
	return nil
}

// WriteHeadHeaderHash stores the head header's hash.
func WriteHeadHeaderHash(db ethdb.Database, hash common.Hash) error {
	if err := db.Put(headHeaderKey, hash.Bytes()); err != nil {
		glog.Fatalf("failed to store last header's hash into database: %v", err)
		return err
	}
	return nil
}

// WriteHeadBlockHash stores the head block's hash.
func WriteHeadBlockHash(db ethdb.Database, hash common.Hash) error {
	if err := db.Put(headBlockKey, hash.Bytes()); err != nil {
		glog.Fatalf("failed to store last block's hash into database: %v", err)
		return err
	}
	return nil
}

// WriteHeadFastBlockHash stores the fast head block's hash.
func WriteHeadFastBlockHash(db ethdb.Database, hash common.Hash) error {
	if err := db.Put(headFastKey, hash.Bytes()); err != nil {
		glog.Fatalf("failed to store last fast block's hash into database: %v", err)
		return err
	}
	return nil
}

// WriteHeader serializes a block header into the database.
func WriteHeader(db ethdb.Database, header *types.Header) error {
	data, err := rlp.EncodeToBytes(header)
	if err != nil {
		return err
	}
	hash := header.Hash().Bytes()
	var key []byte
	if useNewDb {
		num := header.Number.Uint64()
		encNum := encodeBlockNumber(num)
		key = append(newBlockHashPrefix, hash...)
		if err := db.Put(key, encNum); err != nil {
			glog.Fatalf("failed to store hash to number mapping into database: %v", err)
			return err
		}
		if num == 0 {
			if err := db.Put([]byte("useSequentialKeys"), []byte{1}); err != nil {
				glog.Fatalf("failed to store sequential key flag: %v", err)
				return err
			}
		}
		key = append(append(headerPrefix, encNum...), hash...)
	} else {
		key = append(append(blockPrefix, hash...), headerSuffix...)
	}
	if err := db.Put(key, data); err != nil {
		glog.Fatalf("failed to store header into database: %v", err)
		return err
	}
	glog.V(logger.Debug).Infof("stored header #%v [%x…]", header.Number, hash[:4])
	return nil
}

// WriteBody serializes the body of a block into the database.
func WriteBody(db ethdb.Database, hash common.Hash, number uint64, body *types.Body) error {
	data, err := rlp.EncodeToBytes(body)
	if err != nil {
		return err
	}
	var key []byte
	if useNewDb {
		key = append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash.Bytes()...), bodySuffix...)
	} else {
		key = append(append(blockPrefix, hash.Bytes()...), bodySuffix...)
	}
	if err := db.Put(key, data); err != nil {
		glog.Fatalf("failed to store block body into database: %v", err)
		return err
	}
	glog.V(logger.Debug).Infof("stored block body [%x…]", hash.Bytes()[:4])
	return nil
}

// WriteTd serializes the total difficulty of a block into the database.
func WriteTd(db ethdb.Database, hash common.Hash, number uint64, td *big.Int) error {
	data, err := rlp.EncodeToBytes(td)
	if err != nil {
		return err
	}
	var key []byte
	if useNewDb {
		key = append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash.Bytes()...), tdSuffix...)
	} else {
		key = append(append(blockPrefix, hash.Bytes()...), tdSuffix...)
	}
	if err := db.Put(key, data); err != nil {
		glog.Fatalf("failed to store block total difficulty into database: %v", err)
		return err
	}
	glog.V(logger.Debug).Infof("stored block total difficulty [%x…]: %v", hash.Bytes()[:4], td)
	return nil
}

// WriteBlock serializes a block into the database, header and body separately.
func WriteBlock(db ethdb.Database, block *types.Block) error {
	// Store the body first to retain database consistency
	if err := WriteBody(db, block.Hash(), block.NumberU64(), &types.Body{block.Transactions(), block.Uncles()}); err != nil {
		return err
	}
	// Store the header too, signaling full block ownership
	if err := WriteHeader(db, block.Header()); err != nil {
		return err
	}
	return nil
}

// WriteBlockReceipts stores all the transaction receipts belonging to a block
// as a single receipt slice. This is used during chain reorganisations for
// rescheduling dropped transactions.
func WriteBlockReceipts(db ethdb.Database, hash common.Hash, number uint64, receipts types.Receipts) error {
	// Convert the receipts into their storage form and serialize them
	storageReceipts := make([]*types.ReceiptForStorage, len(receipts))
	for i, receipt := range receipts {
		storageReceipts[i] = (*types.ReceiptForStorage)(receipt)
	}
	bytes, err := rlp.EncodeToBytes(storageReceipts)
	if err != nil {
		return err
	}
	// Store the flattened receipt slice
	var key []byte
	if useNewDb {
		key = append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash.Bytes()...), blockReceiptsSuffix...)
	} else {
		key = append(blockReceiptsPrefix, hash.Bytes()...)
	}
	if err := db.Put(key, bytes); err != nil {
		glog.Fatalf("failed to store block receipts into database: %v", err)
		return err
	}
	glog.V(logger.Debug).Infof("stored block receipts [%x…]", hash.Bytes()[:4])
	return nil
}

// WriteTransactions stores the transactions associated with a specific block
// into the given database. Beside writing the transaction, the function also
// stores a metadata entry along with the transaction, detailing the position
// of this within the blockchain.
func WriteTransactions(db ethdb.Database, block *types.Block) error {
	batch := db.NewBatch()

	// Iterate over each transaction and encode it with its metadata
	for i, tx := range block.Transactions() {
		// Encode and queue up the transaction for storage
		data, err := rlp.EncodeToBytes(tx)
		if err != nil {
			return err
		}
		if err := batch.Put(tx.Hash().Bytes(), data); err != nil {
			return err
		}
		// Encode and queue up the transaction metadata for storage
		meta := struct {
			BlockHash  common.Hash
			BlockIndex uint64
			Index      uint64
		}{
			BlockHash:  block.Hash(),
			BlockIndex: block.NumberU64(),
			Index:      uint64(i),
		}
		data, err = rlp.EncodeToBytes(meta)
		if err != nil {
			return err
		}
		if err := batch.Put(append(tx.Hash().Bytes(), txMetaSuffix...), data); err != nil {
			return err
		}
	}
	// Write the scheduled data into the database
	if err := batch.Write(); err != nil {
		glog.Fatalf("failed to store transactions into database: %v", err)
		return err
	}
	return nil
}

// WriteReceipts stores a batch of transaction receipts into the database.
func WriteReceipts(db ethdb.Database, receipts types.Receipts) error {
	batch := db.NewBatch()

	// Iterate over all the receipts and queue them for database injection
	for _, receipt := range receipts {
		storageReceipt := (*types.ReceiptForStorage)(receipt)
		data, err := rlp.EncodeToBytes(storageReceipt)
		if err != nil {
			return err
		}
		if err := batch.Put(append(receiptsPrefix, receipt.TxHash.Bytes()...), data); err != nil {
			return err
		}
	}
	// Write the scheduled data into the database
	if err := batch.Write(); err != nil {
		glog.Fatalf("failed to store receipts into database: %v", err)
		return err
	}
	return nil
}

// DeleteCanonicalHash removes the number to hash canonical mapping.
func DeleteCanonicalHash(db ethdb.Database, number uint64) {
	if useNewDb {
		db.Delete(append(append(headerPrefix, encodeBlockNumber(number)...), headerNumSuffix...))
	} else {	
		db.Delete(append(blockNumPrefix, big.NewInt(int64(number)).Bytes()...))
	}
}

// DeleteHeader removes all block header data associated with a hash.
func DeleteHeader(db ethdb.Database, hash common.Hash, number uint64) {
	if useNewDb {
		db.Delete(append(newBlockHashPrefix, hash.Bytes()...))
		db.Delete(append(append(headerPrefix, encodeBlockNumber(number)...), hash.Bytes()...))
	} else {	
		db.Delete(append(append(blockPrefix, hash.Bytes()...), headerSuffix...))
	}
}

// DeleteBody removes all block body data associated with a hash.
func DeleteBody(db ethdb.Database, hash common.Hash, number uint64) {
	if useNewDb {
		db.Delete(append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash.Bytes()...), bodySuffix...))
	} else {	
		db.Delete(append(append(blockPrefix, hash.Bytes()...), bodySuffix...))
	}
}

// DeleteTd removes all block total difficulty data associated with a hash.
func DeleteTd(db ethdb.Database, hash common.Hash, number uint64) {
	if useNewDb {
		db.Delete(append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash.Bytes()...), tdSuffix...))
	} else {	
		db.Delete(append(append(blockPrefix, hash.Bytes()...), tdSuffix...))
	}
}

// DeleteBlock removes all block data associated with a hash.
func DeleteBlock(db ethdb.Database, hash common.Hash, number uint64) {
	DeleteBlockReceipts(db, hash, number)
	DeleteHeader(db, hash, number)
	DeleteBody(db, hash, number)
	DeleteTd(db, hash, number)
}

// DeleteBlockReceipts removes all receipt data associated with a block hash.
func DeleteBlockReceipts(db ethdb.Database, hash common.Hash, number uint64) {
	if useNewDb {
		db.Delete(append(append(append(newBlockPrefix, encodeBlockNumber(number)...), hash.Bytes()...), blockReceiptsSuffix...))
	} else {
		db.Delete(append(blockReceiptsPrefix, hash.Bytes()...))
	}
}

// DeleteTransaction removes all transaction data associated with a hash.
func DeleteTransaction(db ethdb.Database, hash common.Hash) {
	db.Delete(hash.Bytes())
	db.Delete(append(hash.Bytes(), txMetaSuffix...))
}

// DeleteReceipt removes all receipt data associated with a transaction hash.
func DeleteReceipt(db ethdb.Database, hash common.Hash) {
	db.Delete(append(receiptsPrefix, hash.Bytes()...))
}

// [deprecated by the header/block split, remove eventually]
// GetBlockByHashOld returns the old combined block corresponding to the hash
// or nil if not found. This method is only used by the upgrade mechanism to
// access the old combined block representation. It will be dropped after the
// network transitions to eth/63.
func GetBlockByHashOld(db ethdb.Database, hash common.Hash) *types.Block {
	data, _ := db.Get(append(blockHashPrefix, hash[:]...))
	if len(data) == 0 {
		return nil
	}
	var block types.StorageBlock
	if err := rlp.Decode(bytes.NewReader(data), &block); err != nil {
		glog.V(logger.Error).Infof("invalid block RLP for hash %x: %v", hash, err)
		return nil
	}
	return (*types.Block)(&block)
}

// returns a formatted MIP mapped key by adding prefix, canonical number and level
//
// ex. fn(98, 1000) = (prefix || 1000 || 0)
func mipmapKey(num, level uint64) []byte {
	lkey := make([]byte, 8)
	binary.BigEndian.PutUint64(lkey, level)
	key := new(big.Int).SetUint64(num / level * level)

	return append(mipmapPre, append(lkey, key.Bytes()...)...)
}

// WriteMapmapBloom writes each address included in the receipts' logs to the
// MIP bloom bin.
func WriteMipmapBloom(db ethdb.Database, number uint64, receipts types.Receipts) error {
	batch := db.NewBatch()
	for _, level := range MIPMapLevels {
		key := mipmapKey(number, level)
		bloomDat, _ := db.Get(key)
		bloom := types.BytesToBloom(bloomDat)
		for _, receipt := range receipts {
			for _, log := range receipt.Logs {
				bloom.Add(log.Address.Big())
			}
		}
		batch.Put(key, bloom.Bytes())
	}
	if err := batch.Write(); err != nil {
		return fmt.Errorf("mipmap write fail for: %d: %v", number, err)
	}
	return nil
}

// GetMipmapBloom returns a bloom filter using the number and level as input
// parameters. For available levels see MIPMapLevels.
func GetMipmapBloom(db ethdb.Database, number, level uint64) types.Bloom {
	bloomDat, _ := db.Get(mipmapKey(number, level))
	return types.BytesToBloom(bloomDat)
}

// GetBlockChainVersion reads the version number from db.
func GetBlockChainVersion(db ethdb.Database) int {
	var vsn uint
	enc, _ := db.Get([]byte("BlockchainVersion"))
	rlp.DecodeBytes(enc, &vsn)
	return int(vsn)
}

// WriteBlockChainVersion writes vsn as the version number to db.
func WriteBlockChainVersion(db ethdb.Database, vsn int) {
	enc, _ := rlp.EncodeToBytes(uint(vsn))
	db.Put([]byte("BlockchainVersion"), enc)
}

// WriteChainConfig writes the chain config settings to the database.
func WriteChainConfig(db ethdb.Database, hash common.Hash, cfg *ChainConfig) error {
	// short circuit and ignore if nil config. GetChainConfig
	// will return a default.
	if cfg == nil {
		return nil
	}

	jsonChainConfig, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	return db.Put(append(configPrefix, hash[:]...), jsonChainConfig)
}

// GetChainConfig will fetch the network settings based on the given hash.
func GetChainConfig(db ethdb.Database, hash common.Hash) (*ChainConfig, error) {
	jsonChainConfig, _ := db.Get(append(configPrefix, hash[:]...))
	if len(jsonChainConfig) == 0 {
		return nil, ChainConfigNotFoundErr
	}

	var config ChainConfig
	if err := json.Unmarshal(jsonChainConfig, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// CheckDbVersion checks the chain database version and
// upgrades if necessary. Returns true if it was the latest
// version or the conversion was successful.
func CheckDbVersion(db ethdb.Database) bool {
	if !useNewDb {
		return true
	}
	sk, _ := db.Get([]byte("useSequentialKeys"))
	if len(sk) > 0 {
		return true
	}
	// get old head header
	headHash := GetHeadBlockHash(db)
	data, _ := db.Get(append(append(blockPrefix, headHash.Bytes()...), headerSuffix...))
	if len(data) == 0 {
		return false
	}
	header := new(types.Header)
	if err := rlp.Decode(bytes.NewReader(data), header); err != nil {
		return false
	}
	headNum := header.Number.Uint64()
	glog.V(logger.Info).Infof("Converting chain database to latest version (%d blocks)", headNum)

	for i:=uint64(0); i<=headNum; i++ {
		if i % 1000 == 0 {
			glog.V(logger.Info).Infof("converted %d blocks...", i)
		}
		// get old canonical hash
		hash, _ := db.Get(append(blockNumPrefix, big.NewInt(int64(i)).Bytes()...))
		if len(hash) != 0 {
			// get old chain data
			headerRLP, _ := db.Get(append(append(blockPrefix, hash...), headerSuffix...))
			bodyRLP, _ := db.Get(append(append(blockPrefix, hash...), bodySuffix...))
			tdRLP, _ := db.Get(append(append(blockPrefix, hash...), tdSuffix...))
			receiptsRLP, _ := db.Get(append(blockReceiptsPrefix, hash...))
			// store new canonical hash and number
			encNum := encodeBlockNumber(i)
			db.Put(append(append(headerPrefix, encNum...), headerNumSuffix...), hash)
			db.Put(append(newBlockHashPrefix, hash...), encNum)
			// store new chain data
			if len(headerRLP) != 0 {
				db.Put(append(append(headerPrefix, encNum...), hash...), headerRLP)
			}
			if len(bodyRLP) != 0 {
				db.Put(append(append(append(newBlockPrefix, encNum...), hash...), bodySuffix...), bodyRLP)
			}
			if len(tdRLP) != 0 {
				db.Put(append(append(append(newBlockPrefix, encNum...), hash...), tdSuffix...), tdRLP)
			}
			if len(receiptsRLP) != 0 {
				db.Put(append(append(append(newBlockPrefix, encNum...), hash...), blockReceiptsSuffix...), receiptsRLP)
			}
			// delete old chain data
			db.Delete(append(append(blockPrefix, hash...), headerSuffix...))
			db.Delete(append(append(blockPrefix, hash...), bodySuffix...))
			db.Delete(append(append(blockPrefix, hash...), tdSuffix...))
			db.Delete(append(blockReceiptsPrefix, hash...))
			db.Delete(append(blockNumPrefix, big.NewInt(int64(i)).Bytes()...))
		}
	}
	db.Put([]byte("useSequentialKeys"), []byte{1})
	glog.V(logger.Info).Infof("Database conversion successful")
	return true
}