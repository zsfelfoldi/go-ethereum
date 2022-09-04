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
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light/beacon"
)

// NoOdr is the default context passed to an ODR capable function when the ODR
// service is not required.
var NoOdr = context.Background()

// ErrNoPeers is returned if no peers capable of serving a queued request are available
var ErrNoPeers = errors.New("no suitable peers available")

// OdrBackend is an interface to a backend service that handles ODR retrievals type
type OdrBackend interface {
	Database() ethdb.Database
	ChtIndexer() *core.ChainIndexer
	BloomTrieIndexer() *core.ChainIndexer
	BloomIndexer() *core.ChainIndexer
	Retrieve(ctx context.Context, req OdrRequest) error
	RetrieveTxStatus(ctx context.Context, req *TxStatusRequest) error
	RetrieveWithBeaconHeader(ctx context.Context, beaconHead beacon.Header, req OdrRequest) error
	IndexerConfig() *IndexerConfig
	//GetBeaconHead() beacon.Header
}

// OdrRequest is an interface for retrieval requests
type OdrRequest interface {
	StoreResult(db ethdb.Database)
}

// TrieID identifies a state or account storage trie
type TrieID struct {
	BlockHash, Root common.Hash
	BlockNumber     uint64
	AccKey          []byte
}

// StateTrieID returns a TrieID for a state trie belonging to a certain block
// header.
func StateTrieID(header *types.Header) *TrieID {
	return &TrieID{
		BlockHash:   header.Hash(),
		BlockNumber: header.Number.Uint64(),
		AccKey:      nil,
		Root:        header.Root,
	}
}

// StorageTrieID returns a TrieID for a contract storage trie at a given account
// of a given state trie. It also requires the root hash of the trie for
// checking Merkle proofs.
func StorageTrieID(state *TrieID, addrHash, root common.Hash) *TrieID {
	return &TrieID{
		BlockHash:   state.BlockHash,
		BlockNumber: state.BlockNumber,
		AccKey:      addrHash[:],
		Root:        root,
	}
}

// TrieRequest is the ODR request type for state/storage trie entries
type TrieRequest struct {
	Id    *TrieID
	Key   []byte
	Proof *NodeSet
}

// StoreResult stores the retrieved data in local database
func (req *TrieRequest) StoreResult(db ethdb.Database) {
	req.Proof.Store(db)
}

// CodeRequest is the ODR request type for retrieving contract code
type CodeRequest struct {
	Id   *TrieID // references storage trie of the account
	Hash common.Hash
	Data []byte
}

// StoreResult stores the retrieved data in local database
func (req *CodeRequest) StoreResult(db ethdb.Database) {
	rawdb.WriteCode(db, req.Hash, req.Data)
}

// BlockRequest is the ODR request type for retrieving block bodies
type BlockRequest struct {
	Hash   common.Hash
	Number uint64
	Header *types.Header
	Rlp    []byte
}

// StoreResult stores the retrieved data in local database
func (req *BlockRequest) StoreResult(db ethdb.Database) {
	rawdb.WriteBodyRLP(db, req.Hash, req.Number, req.Rlp)
}

// ReceiptsRequest is the ODR request type for retrieving receipts.
type ReceiptsRequest struct {
	Untrusted bool // Indicator whether the result retrieved is trusted or not
	Hash      common.Hash
	Number    uint64
	Header    *types.Header
	Receipts  types.Receipts
}

// StoreResult stores the retrieved data in local database
func (req *ReceiptsRequest) StoreResult(db ethdb.Database) {
	if !req.Untrusted {
		rawdb.WriteReceipts(db, req.Hash, req.Number, req.Receipts)
	}
}

// ChtRequest is the ODR request type for retrieving header by Canonical Hash Trie
type ChtRequest struct {
	Config           *IndexerConfig
	ChtNum, BlockNum uint64
	ChtRoot          common.Hash
	Header           *types.Header
	Td               *big.Int
	Proof            *NodeSet
}

// StoreResult stores the retrieved data in local database
func (req *ChtRequest) StoreResult(db ethdb.Database) {
	hash, num := req.Header.Hash(), req.Header.Number.Uint64()
	rawdb.WriteHeader(db, req.Header)
	rawdb.WriteTd(db, hash, num, req.Td)
	rawdb.WriteCanonicalHash(db, hash, num)
}

// BloomRequest is the ODR request type for retrieving bloom filters from a CHT structure
type BloomRequest struct {
	OdrRequest
	Config           *IndexerConfig
	BloomTrieNum     uint64
	BitIdx           uint
	SectionIndexList []uint64
	BloomTrieRoot    common.Hash
	BloomBits        [][]byte
	Proofs           *NodeSet
}

// StoreResult stores the retrieved data in local database
func (req *BloomRequest) StoreResult(db ethdb.Database) {
	for i, sectionIdx := range req.SectionIndexList {
		sectionHead := rawdb.ReadCanonicalHash(db, (sectionIdx+1)*req.Config.BloomTrieSize-1)
		// if we don't have the canonical hash stored for this section head number, we'll still store it under
		// a key with a zero sectionHead. GetBloomBits will look there too if we still don't have the canonical
		// hash. In the unlikely case we've retrieved the section head hash since then, we'll just retrieve the
		// bit vector again from the network.
		rawdb.WriteBloomBits(db, req.BitIdx, sectionIdx, sectionHead, req.BloomBits[i])
	}
}

// TxStatus describes the status of a transaction
type TxStatus struct {
	Status core.TxStatus
	Lookup *rawdb.LegacyTxLookupEntry `rlp:"nil"`
	Error  string
}

// TxStatusRequest is the ODR request type for retrieving transaction status
type TxStatusRequest struct {
	Hashes []common.Hash
	Status []TxStatus
}

// StoreResult stores the retrieved data in local database
func (req *TxStatusRequest) StoreResult(db ethdb.Database) {}

type BeaconInitRequest struct {
	OdrRequest
	Checkpoint common.Hash

	Block *beacon.BlockData // filled by Validate
}

func (req *BeaconInitRequest) StoreResult(db ethdb.Database) {}

type BeaconDataRequest struct {
	OdrRequest
	LastSlot uint64 // last slot of requested range (reference block is used if LastSlot is higher than its slot number)
	Length   uint64 // number of requested slots

	RefBeaconHead beacon.Header
	TailShortTerm uint64 // HspShortTerm state data is expected to be available and returned in Blocks starting from this slot
	// Note that for the first block ParentSlotDiff and StateRootDiffs are not proven; retrieving the
	// range ending with the previous slot will update it if necessary, therefore they are guaranteed valid
	// for each but the first block of any continuous range.
	//TODO ^^^ ???
	ParentHeader beacon.Header
	Blocks       []*beacon.BlockData // filled by Validate
	TailProof    beacon.MultiProof   // proof root is the historic tree root in the reference block's beacon state; proves first relevant entry in the historic tree
}

func (req *BeaconDataRequest) StoreResult(db ethdb.Database) {}

const (
	HeadMode = iota
	HistoricMode
	FinalizedMode
)

type ExecHeadersRequest struct {
	OdrRequest
	ReqMode        uint   // 0: head  1: historic  2: finalized
	HistoricNumber uint64 // highest requested exec header number (for historic mode only)
	Amount         uint64 // number of requested exec headers
	FullBlocks     bool

	RefBeaconHead beacon.Header
	ExecHeaders   []*types.Header
	ExecBlocks    []*types.Block
}

func (req *ExecHeadersRequest) StoreResult(db ethdb.Database) {}

type HeadersByHashRequest struct {
	OdrRequest
	BlockHash common.Hash // block hash of last requested header
	Amount    int         // number of requested headers

	Headers []*types.Header
}

func (req *HeadersByHashRequest) StoreResult(db ethdb.Database) {}
