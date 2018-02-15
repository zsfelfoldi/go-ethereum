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
	"encoding/binary"
	"io"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// -----
// HEADER
// -----

// Header contains the header fields of a block opposite to statements
// internal data. Signature is based on the hash of the RLP encoding
// of the struct while the Signature field is set to nil.
type Header struct {
	PrevHash      common.Hash `json:"prevHash"       gencodec:"required"`
	Number        uint64      `json:"number"         gencodec:"required"`
	Time          uint64      `json:"time"           gencodec:"required"`
	TrieRoot      common.Hash `json:"trieRoot"      gencodec:"required"`
	SignatureType string      `json:"signatureType"  gencodec:"required"`
	Signature     []byte      `json:"signature"      gencodec:"required"`
}

// hash returns the block hash of the header, which is simply the keccak256
// hash of its RLP encoding.
func (h *Header) hash() common.Hash {
	return rlpHash(h)
}

// sign adds a signature to the block heater by the given private key.
func (h *Header) sign(privKey *ecdsa.PrivateKey) {
	unsignedData := &Header{
		PrevHash:      h.PrevHash,
		Number:        h.Number,
		Time:          h.Time,
		TrieRoot:      h.TrieRoot,
		SignatureType: h.SignatureType,
	}
	rlp, _ := rlp.EncodeToBytes(unsignedData)
	h.Signature, _ = crypto.Sign(crypto.Keccak256(rlp), privKey)
}

// -----
// BLOCK
// -----

// Block represents one block on the observer chain.
type Block struct {
	header *Header

	// Caches.
	hash atomic.Value
	size atomic.Value
}

// NewBlock creates a new first block (genesis block).
func NewBlock(privKey *ecdsa.PrivateKey) *Block {
	b := &Block{
		header: &Header{
			PrevHash:      common.Hash{},
			Number:        0,
			Time:          uint64(time.Now().Unix()),
			TrieRoot:      types.EmptyRootHash,
			SignatureType: "ECDSA",
		},
	}
	b.header.sign(privKey)
	return b
}

// CreateSuccessor creates the block following to this block. The
// new trie root is set and the block will be signed.
func (b *Block) CreateSuccessor(trieRoot common.Hash, privKey *ecdsa.PrivateKey) *Block {
	sb := &Block{
		header: &Header{
			PrevHash:      b.Hash(),
			Number:        b.header.Number + 1,
			Time:          uint64(time.Now().Unix()),
			TrieRoot:      trieRoot,
			SignatureType: "ECDSA",
		},
	}
	sb.header.sign(privKey)
	return sb
}

// TrieRoot returns the trie root of the block.
func (b *Block) TrieRoot() common.Hash {
	return b.header.TrieRoot
}

// Number returns the block number as big.Int.
func (b *Block) Number() *big.Int {
	return new(big.Int).SetUint64(b.header.Number)
}

// EncodedNumber returns the block number in a big endian
// encoded way.
func (b *Block) EncodedNumber() []byte {
	enc := make([]byte, 8)
	binary.BigEndian.PutUint64(enc, b.header.Number)
	return enc
}

// Time returns the block time as big.Int.
func (b *Block) Time() *big.Int {
	return new(big.Int).SetUint64(b.header.Time)
}

// Signature returns the signature of the block.
func (b *Block) Signature() []byte {
	sig := make([]byte, len(b.header.Signature))
	copy(sig, b.header.Signature)
	return sig
}

// Hash returns the keccak256 hash of the block's header.
// The hash is computed on the first call and cached thereafter.
func (b *Block) Hash() common.Hash {
	if hash := b.hash.Load(); hash != nil {
		return hash.(common.Hash)
	}
	v := b.header.hash()
	b.hash.Store(v)
	return v
}

// PrevHash returns the hash of the previous block.
func (b *Block) PrevHash() common.Hash {
	return b.header.PrevHash
}

// Size returns the storage size of the block.
func (b *Block) Size() common.StorageSize {
	if size := b.size.Load(); size != nil {
		return size.(common.StorageSize)
	}
	c := writeCounter(0)
	rlp.Encode(&c, b)
	b.size.Store(common.StorageSize(c))
	return common.StorageSize(c)
}

// EncodeRLP implements rlp.Encoder.
func (b *Block) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, b.header)
}

// DecodeRLP implements rlp.Decoder.
func (b *Block) DecodeRLP(s *rlp.Stream) error {
	var h Header
	_, size, _ := s.Kind()
	if err := s.Decode(&h); err != nil {
		return err
	}
	b.header = &h
	b.size.Store(common.StorageSize(rlp.ListSize(size)))
	return nil
}
