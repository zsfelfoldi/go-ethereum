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

package les

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	errInvalidMessageType  = errors.New("invalid message type")
	errInvalidEntryCount   = errors.New("invalid number of response entries")
	errHeaderUnavailable   = errors.New("header unavailable")
	errTxHashMismatch      = errors.New("transaction hash mismatch")
	errUncleHashMismatch   = errors.New("uncle hash mismatch")
	errReceiptHashMismatch = errors.New("receipt hash mismatch")
	errDataHashMismatch    = errors.New("data hash mismatch")
	errCHTHashMismatch     = errors.New("cht hash mismatch")
	errCHTNumberMismatch   = errors.New("cht number mismatch")
	errUselessNodes        = errors.New("useless nodes in merkle proof nodeset")
)

type LesOdrRequest interface {
	GetCost(*peer) uint64
	CanSend(beacon.Header, *peer) bool
	Request(uint64, beacon.Header, *peer) error
	Validate(ethdb.Database, beacon.Header, *Msg) error
}

func LesRequest(req light.OdrRequest) LesOdrRequest {
	switch r := req.(type) {
	case *light.BlockRequest:
		return (*BlockRequest)(r)
	case *light.ReceiptsRequest:
		return (*ReceiptsRequest)(r)
	case *light.TrieRequest:
		return (*TrieRequest)(r)
	case *light.CodeRequest:
		return (*CodeRequest)(r)
	case *light.ChtRequest:
		return (*ChtRequest)(r)
	case *light.BloomRequest:
		return (*BloomRequest)(r)
	case *light.TxStatusRequest:
		return (*TxStatusRequest)(r)
	case *light.BeaconInitRequest:
		return (*BeaconInitRequest)(r)
	case *light.BeaconDataRequest:
		return (*BeaconDataRequest)(r)
	case *light.ExecHeadersRequest:
		return (*ExecHeadersRequest)(r)
	case *light.HeadersByHashRequest:
		return (*HeadersByHashRequest)(r)
	default:
		return nil
	}
}

// BlockRequest is the ODR request type for block bodies
type BlockRequest light.BlockRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *BlockRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetBlockBodiesMsg, 1)
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *BlockRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	if peer.version >= lpv5 {
		return peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
	}
	return peer.HasBlock(r.Hash, r.Number, false)
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *BlockRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting block body", "hash", r.Hash)
	return peer.requestBodies(reqID, []common.Hash{r.Hash})
}

// Valid processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (r *BlockRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating block body", "hash", r.Hash)

	// Ensure we have a correct message with a single block body
	if msg.MsgType != MsgBlockBodies {
		return errInvalidMessageType
	}
	bodies := msg.Obj.([]*types.Body)
	if len(bodies) != 1 {
		return errInvalidEntryCount
	}
	body := bodies[0]

	// Retrieve our stored header and validate block content against it
	if r.Header == nil {
		r.Header = rawdb.ReadHeader(db, r.Hash, r.Number)
	}
	if r.Header == nil {
		return errHeaderUnavailable
	}
	if r.Header.TxHash != types.DeriveSha(types.Transactions(body.Transactions), trie.NewStackTrie(nil)) {
		return errTxHashMismatch
	}
	if r.Header.UncleHash != types.CalcUncleHash(body.Uncles) {
		return errUncleHashMismatch
	}
	// Validations passed, encode and store RLP
	data, err := rlp.EncodeToBytes(body)
	if err != nil {
		return err
	}
	r.Rlp = data
	return nil
}

// ReceiptsRequest is the ODR request type for block receipts by block hash
type ReceiptsRequest light.ReceiptsRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *ReceiptsRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetReceiptsMsg, 1)
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *ReceiptsRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	if peer.version >= lpv5 {
		return peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
	}
	return peer.HasBlock(r.Hash, r.Number, false)
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *ReceiptsRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting block receipts", "hash", r.Hash)
	return peer.requestReceipts(reqID, []common.Hash{r.Hash})
}

// Valid processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (r *ReceiptsRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating block receipts", "hash", r.Hash)

	// Ensure we have a correct message with a single block receipt
	if msg.MsgType != MsgReceipts {
		return errInvalidMessageType
	}
	receipts := msg.Obj.([]types.Receipts)
	if len(receipts) != 1 {
		return errInvalidEntryCount
	}
	receipt := receipts[0]

	// Retrieve our stored header and validate receipt content against it
	if r.Header == nil {
		r.Header = rawdb.ReadHeader(db, r.Hash, r.Number)
	}
	if r.Header == nil {
		return errHeaderUnavailable
	}
	if r.Header.ReceiptHash != types.DeriveSha(receipt, trie.NewStackTrie(nil)) {
		return errReceiptHashMismatch
	}
	// Validations passed, store and return
	r.Receipts = receipt
	return nil
}

type ProofReq struct {
	BHash       common.Hash
	AccKey, Key []byte
	FromLevel   uint
}

// ODR request type for state/storage trie entries, see LesOdrRequest interface
type TrieRequest light.TrieRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *TrieRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetProofsV2Msg, 1)
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *TrieRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	if peer.version >= lpv5 {
		return peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
	}
	return peer.HasBlock(r.Id.BlockHash, r.Id.BlockNumber, true)
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *TrieRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting trie proof", "root", r.Id.Root, "key", r.Key)
	req := ProofReq{
		BHash:  r.Id.BlockHash,
		AccKey: r.Id.AccKey,
		Key:    r.Key,
	}
	return peer.requestProofs(reqID, []ProofReq{req})
}

// Valid processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (r *TrieRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating trie proof", "root", r.Id.Root, "key", r.Key)

	if msg.MsgType != MsgProofsV2 {
		return errInvalidMessageType
	}
	proofs := msg.Obj.(light.NodeList)
	// Verify the proof and store if checks out
	nodeSet := proofs.NodeSet()
	reads := &readTraceDB{db: nodeSet}
	if _, err := trie.VerifyProof(r.Id.Root, r.Key, reads); err != nil {
		return fmt.Errorf("merkle proof verification failed: %v", err)
	}
	// check if all nodes have been read by VerifyProof
	if len(reads.reads) != nodeSet.KeyCount() {
		return errUselessNodes
	}
	r.Proof = nodeSet
	return nil
}

type CodeReq struct {
	BHash  common.Hash
	AccKey []byte
}

// ODR request type for node data (used for retrieving contract code), see LesOdrRequest interface
type CodeRequest light.CodeRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *CodeRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetCodeMsg, 1)
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *CodeRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	if peer.version >= lpv5 {
		return peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
	}
	return peer.HasBlock(r.Id.BlockHash, r.Id.BlockNumber, true)
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *CodeRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting code data", "hash", r.Hash)
	req := CodeReq{
		BHash:  r.Id.BlockHash,
		AccKey: r.Id.AccKey,
	}
	return peer.requestCode(reqID, []CodeReq{req})
}

// Valid processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (r *CodeRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating code data", "hash", r.Hash)

	// Ensure we have a correct message with a single code element
	if msg.MsgType != MsgCode {
		return errInvalidMessageType
	}
	reply := msg.Obj.([][]byte)
	if len(reply) != 1 {
		return errInvalidEntryCount
	}
	data := reply[0]

	// Verify the data and store if checks out
	if hash := crypto.Keccak256Hash(data); r.Hash != hash {
		return errDataHashMismatch
	}
	r.Data = data
	return nil
}

const (
	// helper trie type constants
	htCanonical = iota // Canonical hash trie
	htBloomBits        // BloomBits trie

	// helper trie auxiliary types
	// htAuxNone = 1 ; deprecated number, used in les2/3 previously.
	htAuxHeader = 2 // applicable for htCanonical, requests for relevant headers
)

type HelperTrieReq struct {
	Type              uint
	TrieIdx           uint64
	Key               []byte
	FromLevel, AuxReq uint
}

type HelperTrieResps struct { // describes all responses, not just a single one
	Proofs  light.NodeList
	AuxData [][]byte
}

// ODR request type for requesting headers by Canonical Hash Trie, see LesOdrRequest interface
type ChtRequest light.ChtRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *ChtRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetHelperTrieProofsMsg, 1)
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *ChtRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	peer.lock.RLock()
	defer peer.lock.RUnlock()

	return peer.headInfo.Number >= r.Config.ChtConfirms && r.ChtNum <= (peer.headInfo.Number-r.Config.ChtConfirms)/r.Config.ChtSize
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *ChtRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting CHT", "cht", r.ChtNum, "block", r.BlockNum)
	var encNum [8]byte
	binary.BigEndian.PutUint64(encNum[:], r.BlockNum)
	req := HelperTrieReq{
		Type:    htCanonical,
		TrieIdx: r.ChtNum,
		Key:     encNum[:],
		AuxReq:  htAuxHeader,
	}
	return peer.requestHelperTrieProofs(reqID, []HelperTrieReq{req})
}

// Valid processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (r *ChtRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating CHT", "cht", r.ChtNum, "block", r.BlockNum)

	if msg.MsgType != MsgHelperTrieProofs {
		return errInvalidMessageType
	}
	resp := msg.Obj.(HelperTrieResps)
	if len(resp.AuxData) != 1 {
		return errInvalidEntryCount
	}
	nodeSet := resp.Proofs.NodeSet()
	headerEnc := resp.AuxData[0]
	if len(headerEnc) == 0 {
		return errHeaderUnavailable
	}
	header := new(types.Header)
	if err := rlp.DecodeBytes(headerEnc, header); err != nil {
		return errHeaderUnavailable
	}
	// Verify the CHT
	var (
		node      light.ChtNode
		encNumber [8]byte
	)
	binary.BigEndian.PutUint64(encNumber[:], r.BlockNum)

	reads := &readTraceDB{db: nodeSet}
	value, err := trie.VerifyProof(r.ChtRoot, encNumber[:], reads)
	if err != nil {
		return fmt.Errorf("merkle proof verification failed: %v", err)
	}
	if len(reads.reads) != nodeSet.KeyCount() {
		return errUselessNodes
	}
	if err := rlp.DecodeBytes(value, &node); err != nil {
		return err
	}
	if node.Hash != header.Hash() {
		return errCHTHashMismatch
	}
	if r.BlockNum != header.Number.Uint64() {
		return errCHTNumberMismatch
	}
	// Verifications passed, store and return
	r.Header = header
	r.Proof = nodeSet
	r.Td = node.Td
	return nil
}

type BloomReq struct {
	BloomTrieNum, BitIdx, SectionIndex, FromLevel uint64
}

// ODR request type for requesting headers by Canonical Hash Trie, see LesOdrRequest interface
type BloomRequest light.BloomRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *BloomRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetHelperTrieProofsMsg, len(r.SectionIndexList))
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *BloomRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	peer.lock.RLock()
	defer peer.lock.RUnlock()

	if peer.version < lpv2 {
		return false
	}
	return peer.headInfo.Number >= r.Config.BloomTrieConfirms && r.BloomTrieNum <= (peer.headInfo.Number-r.Config.BloomTrieConfirms)/r.Config.BloomTrieSize
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *BloomRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting BloomBits", "bloomTrie", r.BloomTrieNum, "bitIdx", r.BitIdx, "sections", r.SectionIndexList)
	reqs := make([]HelperTrieReq, len(r.SectionIndexList))

	var encNumber [10]byte
	binary.BigEndian.PutUint16(encNumber[:2], uint16(r.BitIdx))

	for i, sectionIdx := range r.SectionIndexList {
		binary.BigEndian.PutUint64(encNumber[2:], sectionIdx)
		reqs[i] = HelperTrieReq{
			Type:    htBloomBits,
			TrieIdx: r.BloomTrieNum,
			Key:     common.CopyBytes(encNumber[:]),
		}
	}
	return peer.requestHelperTrieProofs(reqID, reqs)
}

// Valid processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (r *BloomRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating BloomBits", "bloomTrie", r.BloomTrieNum, "bitIdx", r.BitIdx, "sections", r.SectionIndexList)

	// Ensure we have a correct message with a single proof element
	if msg.MsgType != MsgHelperTrieProofs {
		return errInvalidMessageType
	}
	resps := msg.Obj.(HelperTrieResps)
	proofs := resps.Proofs
	nodeSet := proofs.NodeSet()
	reads := &readTraceDB{db: nodeSet}

	r.BloomBits = make([][]byte, len(r.SectionIndexList))

	// Verify the proofs
	var encNumber [10]byte
	binary.BigEndian.PutUint16(encNumber[:2], uint16(r.BitIdx))

	for i, idx := range r.SectionIndexList {
		binary.BigEndian.PutUint64(encNumber[2:], idx)
		value, err := trie.VerifyProof(r.BloomTrieRoot, encNumber[:], reads)
		if err != nil {
			return err
		}
		r.BloomBits[i] = value
	}

	if len(reads.reads) != nodeSet.KeyCount() {
		return errUselessNodes
	}
	r.Proofs = nodeSet
	return nil
}

// TxStatusRequest is the ODR request type for transaction status
type TxStatusRequest light.TxStatusRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *TxStatusRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetTxStatusMsg, len(r.Hashes))
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *TxStatusRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	return peer.txHistory != txIndexDisabled && peer.version < lpv5 || peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *TxStatusRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting transaction status", "count", len(r.Hashes))
	return peer.requestTxStatus(reqID, r.Hashes)
}

// Validate processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (r *TxStatusRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating transaction status", "count", len(r.Hashes))

	if msg.MsgType != MsgTxStatus {
		return errInvalidMessageType
	}
	status := msg.Obj.([]light.TxStatus)
	if len(status) != len(r.Hashes) {
		return errInvalidEntryCount
	}
	r.Status = status
	return nil
}

// readTraceDB stores the keys of database reads. We use this to check that received node
// sets contain only the trie nodes necessary to make proofs pass.
type readTraceDB struct {
	db    ethdb.KeyValueReader
	reads map[string]struct{}
}

// Get returns a stored node
func (db *readTraceDB) Get(k []byte) ([]byte, error) {
	if db.reads == nil {
		db.reads = make(map[string]struct{})
	}
	db.reads[string(k)] = struct{}{}
	return db.db.Get(k)
}

// Has returns true if the node set contains the given key
func (db *readTraceDB) Has(key []byte) (bool, error) {
	_, err := db.Get(key)
	return err == nil, nil
}

type BeaconInitRequest light.BeaconInitRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *BeaconInitRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetBeaconInitMsg, 1)
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *BeaconInitRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	return peer.version >= lpv5
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *BeaconInitRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting beacon init data", "checkpoint root", r.Checkpoint)
	return peer.requestBeaconInit(reqID, r.Checkpoint)
}

// Validate processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (request *BeaconInitRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating beacon init data", "checkpoint root", request.Checkpoint)
	// Ensure we have a correct message with a single proof element
	if msg.MsgType != MsgBeaconInit {
		return errInvalidMessageType
	}
	reply := msg.Obj.(BeaconInitResponse)

	reader := beacon.MultiProof{Format: beacon.StateProofFormats[beacon.HspInitData], Values: reply.ProofValues}.Reader(nil)
	proofRoot, ok := beacon.TraverseProof(reader, nil)
	if !ok || !reader.Finished() {
		return errors.New("Multiproof format error")
	}
	if reply.Header.Hash(proofRoot) != request.Checkpoint {
		return errors.New("Checkpoint block root does not match")
	}

	request.Block = &beacon.BlockData{
		Header:      reply.Header,
		StateRoot:   proofRoot,
		BlockRoot:   request.Checkpoint,
		ProofFormat: beacon.HspInitData,
		StateProof:  reply.ProofValues,
	}
	return nil
}

type BeaconDataRequest light.BeaconDataRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *BeaconDataRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetBeaconDataMsg, int(r.Length))
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *BeaconDataRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	return peer.version >= lpv5 && peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *BeaconDataRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting beacon block data", "reference block root", beaconHeader.Hash(), "last slot", r.LastSlot, "length", r.Length)
	return peer.requestBeaconData(GetBeaconDataPacket{
		ReqID:     reqID,
		BlockRoot: beaconHeader.Hash(),
		LastSlot:  r.LastSlot,
		Length:    r.Length,
	})
}

// Validate processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (request *BeaconDataRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating beacon block data", "reference block root", beaconHeader.Hash(), "last slot", request.LastSlot, "length", request.Length)
	// Ensure we have a correct message with a single proof element
	if msg.MsgType != MsgBeaconData {
		return errInvalidMessageType
	}
	reply := msg.Obj.(BeaconDataResponse)

	/*fmt.Println("Validating BeaconDataRequest")
	fmt.Println(" refHead:", beaconHeader)
	fmt.Println(" request:", request)
	fmt.Println(" reply:", reply)*/
	var (
		firstSlot       uint64      // first slot of returned range
		firstParentRoot common.Hash // parent root of first returned block
	)
	if reply.ParentHeader.StateRoot != (common.Hash{}) { // if missing then first returned is the genesis
		firstSlot = uint64(reply.ParentHeader.Slot) + 1
		firstParentRoot = reply.ParentHeader.Hash()
	}
	lastSlot := firstSlot + uint64(len(reply.StateProofFormats)) - 1 // last slot of returned range

	var reqFirstSlot, reqLastSlot uint64 // first and last slot of requested range

	if request.LastSlot < uint64(beaconHeader.Slot) {
		reqLastSlot = request.LastSlot
	} else {
		reqLastSlot = uint64(beaconHeader.Slot)
	}

	if reqLastSlot >= request.Length {
		reqFirstSlot = reqLastSlot + 1 - request.Length
	}

	// check that the returned range and individual state proof formats are expected
	if firstSlot > reqFirstSlot || lastSlot < reqLastSlot {
		return errors.New("Returned slots do not cover requested range")
	}
	for i := firstSlot; i < reqFirstSlot; i++ {
		if reply.StateProofFormats[int(i-firstSlot)] != 0 {
			// range extension is only expected until first non-empty slot
			return errors.New("Unexpected range extension at the beginning of returned range")
		}
	}
	for i := reqLastSlot; i < lastSlot; i++ {
		if reply.StateProofFormats[int(i-firstSlot)] != 0 {
			// range extension is only expected until first non-empty slot
			return errors.New("Unexpected range extension at the end of returned range")
		}
	}
	if reply.StateProofFormats[int(lastSlot-firstSlot)] == 0 {
		return errors.New("Range ends with empty slot")
	}
	var (
		nonZeroCount int
		expInit      bool
	)
	for i, format := range reply.StateProofFormats {
		slot := firstSlot + uint64(i)
		if slot&31 == 0 {
			expInit = true
		}
		if format != 0 {
			nonZeroCount++
			exp := byte(beacon.HspLongTerm)
			if slot >= request.TailShortTerm {
				exp += beacon.HspShortTerm
			}
			if expInit {
				exp += beacon.HspInitData
				expInit = false
			}
			if format&exp != exp {
				return errors.New("Insufficient state proof")
			}
		}
	}
	if nonZeroCount != len(reply.Headers) {
		return errors.New("Number of returned headers do not match non-empty state proofs")
	}

	fmt.Println("*** len(reply.StateProofFormats)", len(reply.StateProofFormats), "len(reply.ProofValues)", len(reply.ProofValues))
	format := beacon.SlotRangeFormat(uint64(beaconHeader.Slot), firstSlot, reply.StateProofFormats)
	reader := beacon.MultiProof{Format: format, Values: reply.ProofValues}.Reader(nil)
	target := make([]*beacon.MerkleValues, len(reply.StateProofFormats))
	targetMap := make(map[uint64]beacon.ProofWriter)
	stateProofWriter := beacon.NewMultiProofWriter(format, nil, func(index uint64) beacon.ProofWriter {
		return targetMap[index]
	})
	writer := beacon.MergedWriter{stateProofWriter}
	if firstSlot+0x2000 < uint64(beaconHeader.Slot) {
		tailProofIndex := beacon.ChildIndex((0x2000000+firstSlot>>13)*2+1, 0x2000+firstSlot%0x1fff)
		request.TailProof = beacon.MultiProof{Format: beacon.NewRangeFormat(tailProofIndex, tailProofIndex, nil)}
		i := beacon.ChildIndex(beacon.BsiHistoricRoots, tailProofIndex)
		tailProofWriter := beacon.NewMultiProofWriter(beacon.NewRangeFormat(i, i, nil), nil, func(index uint64) beacon.ProofWriter {
			if index == beacon.BsiHistoricRoots {
				return beacon.NewMultiProofWriter(request.TailProof.Format, &request.TailProof.Values, nil)
			}
			return nil
		})
		writer = append(writer, tailProofWriter)
	}

	for i := range target {
		target[i] = new(beacon.MerkleValues)
		index := beacon.SlotProofIndex(uint64(beaconHeader.Slot), firstSlot+uint64(i))
		subWriter := beacon.NewMultiProofWriter(beacon.StateProofFormats[reply.StateProofFormats[i]], target[i], nil)
		if index == 1 {
			// add to the MergedWriter as a separate MultiProofWriter
			// adding it to stateProofWriter as a subtree at index 1 would prevent traversing the other beacon states that are children of the head block state
			writer = append(writer, subWriter)
		} else {
			targetMap[index] = subWriter
		}
	}

	proofRoot, ok := beacon.TraverseProof(reader, writer)
	if ok && reader.Finished() {
		if proofRoot != beaconHeader.StateRoot {
			return errors.New("Multiproof root hash does not match")
		}
	} else {
		return errors.New("Multiproof format error")
	}
	blocks := make([]*beacon.BlockData, len(reply.Headers))
	lastRoot := firstParentRoot
	var (
		blockPtr       int
		stateRootDiffs beacon.MerkleValues
	)
	slot := firstSlot
	//fmt.Println("*** slot range")
	for i, format := range reply.StateProofFormats {
		//fmt.Println(" proofFormat", format, "proofLen", len(*target[i]), "len(stateRootDiffs)", len(stateRootDiffs))
		if format == 0 {
			stateRootDiffs = append(stateRootDiffs, (*target[i])[0])
		} else {
			header := reply.Headers[blockPtr]
			block := &beacon.BlockData{
				Header: beacon.HeaderWithoutState{
					Slot:          slot,
					ProposerIndex: header.ProposerIndex,
					BodyRoot:      header.BodyRoot,
					ParentRoot:    lastRoot,
				},
				ProofFormat:    format,
				StateProof:     *target[i],
				ParentSlotDiff: uint64(len(stateRootDiffs) + 1),
				StateRootDiffs: stateRootDiffs,
			}
			block.CalculateRoots()
			lastRoot = block.BlockRoot
			blocks[blockPtr] = block
			blockPtr++
			stateRootDiffs = nil
		}
		slot++
	}

	/*fmt.Println("*** blocks")
	for _, block := range blocks {
		fmt.Println(" slot", block.Header.Slot, "proofFormat", block.ProofFormat, "len(stateRootDiffs)", len(block.StateRootDiffs), "parentSlotDiff", block.ParentSlotDiff)
	}*/

	// compare latest_header root in last block's state with the reconstructed header
	lastBlock := blocks[len(blocks)-1]
	if hash, ok := lastBlock.GetStateValue(beacon.BsiLatestHeader); ok {
		if lastBlock.Header.Hash(common.Hash{}) != common.Hash(hash) {
			return errors.New("Last reconstructed header does not match latest_header field of beacon state")
		}
	} else {
		log.Error("Beacon state field latest_header missing in last retrieved block") // should not happen
		return errors.New("Beacon state field latest_header missing in last retrieved block")
	}

	request.ParentHeader, request.Blocks = reply.ParentHeader, blocks
	//fmt.Println(" request after validation:", request)
	return nil
}

type ExecHeadersRequest light.ExecHeadersRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *ExecHeadersRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetExecHeadersMsg, int(r.Amount))
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *ExecHeadersRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	return peer.version >= lpv5 && peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *ExecHeadersRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting exec headers", "mode", r.ReqMode, "reference block root", beaconHeader.Hash(), "historic number", r.HistoricNumber, "amount", r.Amount)
	return peer.requestExecHeaders(GetExecHeadersPacket{
		ReqID:          reqID,
		ReqMode:        r.ReqMode,
		BlockRoot:      beaconHeader.Hash(),
		HistoricNumber: r.HistoricNumber,
		Amount:         r.Amount,
		FullBlocks:     r.FullBlocks,
	})
}

// Validate processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (request *ExecHeadersRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating exec headers", "mode", request.ReqMode, "reference block root", beaconHeader.Hash(), "historic number", request.HistoricNumber, "amount", request.Amount)
	// Ensure we have a correct message with a single proof element
	if msg.MsgType != MsgExecHeaders {
		return errInvalidMessageType
	}
	reply := msg.Obj.(ExecHeadersResponse)

	//fmt.Println(" ref slot", beaconHeader.Slot, " historic slot", reply.HistoricSlot)
	if reply.HistoricSlot > uint64(beaconHeader.Slot) {
		//fmt.Println(" validate err 1")
		return errors.New("Invalid historic slot")
	}

	hc := uint64(len(reply.ExecHeaders))
	if hc > request.Amount || hc == 0 || (hc < request.Amount && hc != reply.ExecHeaders[int(hc)-1].Number.Uint64()) {
		//fmt.Println(" validate err 2")
		return errors.New("Invalid number of exec headers returned")
	}

	var leafIndex uint64 // beacon state index where the last execution header hash is expected
	switch request.ReqMode {
	case light.HeadMode:
		leafIndex = beacon.BsiExecHead
	case light.HistoricMode:
		leafIndex = beacon.ChildIndex(beacon.SlotProofIndex(uint64(beaconHeader.Slot), reply.HistoricSlot), beacon.BsiExecHead)
	case light.FinalizedMode:
		leafIndex = beacon.BsiFinalExecHash
	default:
		panic(nil)
	}

	format := beacon.NewIndexMapFormat().AddLeaf(leafIndex, nil)
	reader := beacon.MultiProof{Format: format, Values: reply.ProofValues}.Reader(nil)
	target := make(beacon.MerkleValues, 1)
	writer := beacon.NewValueWriter(format, target, func(index uint64) int {
		if index == leafIndex {
			return 0
		}
		return -1
	})
	proofRoot, ok := beacon.TraverseProof(reader, writer)
	if ok && reader.Finished() {
		if proofRoot != beaconHeader.StateRoot {
			//fmt.Println(" validate err 3")
			return errors.New("Multiproof root hash does not match")
		}
	} else {
		//fmt.Println(" validate err 4")
		return errors.New("Multiproof format error")
	}
	expRoot := common.Hash(target[0])
	for i := len(reply.ExecHeaders) - 1; i >= 0; i-- {
		if reply.ExecHeaders[i].Hash() != expRoot {
			//fmt.Println(" validate err 5")
			return errors.New("Exec header root hash does not match")
		}
		expRoot = reply.ExecHeaders[i].ParentHash
	}
	request.ExecHeaders = reply.ExecHeaders
	if request.FullBlocks {
		if len(reply.ExecBodies) != len(reply.ExecHeaders) {
			return errInvalidEntryCount
		}
		request.ExecBlocks = make([]*types.Block, len(reply.ExecBodies))
		for i, bodyRlp := range reply.ExecBodies {
			body := new(types.Body)
			if err := rlp.DecodeBytes(bodyRlp, body); err != nil {
				return err
			}
			header := reply.ExecHeaders[i]
			if header.TxHash != types.DeriveSha(types.Transactions(body.Transactions), trie.NewStackTrie(nil)) {
				return errTxHashMismatch
			}
			if header.UncleHash != types.CalcUncleHash(body.Uncles) {
				return errUncleHashMismatch
			}
			request.ExecBlocks[i] = types.NewBlockWithHeader(header).WithBody(body.Transactions, body.Uncles)
		}
	}

	/*for _, h := range reply.ExecHeaders {
		//fmt.Println(" header", h)
	}*/
	//fmt.Println(" validate success")
	return nil
}

type HeadersByHashRequest light.HeadersByHashRequest

// GetCost returns the cost of the given ODR request according to the serving
// peer's cost table (implementation of LesOdrRequest)
func (r *HeadersByHashRequest) GetCost(peer *peer) uint64 {
	return peer.getRequestCost(GetBlockHeadersMsg, int(r.Amount))
}

// CanSend tells if a certain peer is suitable for serving the given request
func (r *HeadersByHashRequest) CanSend(beaconHeader beacon.Header, peer *peer) bool {
	return peer.version < lpv5 || peer.ulcInfo.HasBeaconHead(beaconHeader.Hash())
}

// Request sends an ODR request to the LES network (implementation of LesOdrRequest)
func (r *HeadersByHashRequest) Request(reqID uint64, beaconHeader beacon.Header, peer *peer) error {
	peer.Log().Debug("Requesting headers by hash", "last block hash", r.BlockHash, "amount", r.Amount)
	return peer.requestHeadersByHash(reqID, r.BlockHash, r.Amount, 0, true)
}

// Validate processes an ODR request reply message from the LES network
// returns true and stores results in memory if the message was a valid reply
// to the request (implementation of LesOdrRequest)
func (request *HeadersByHashRequest) Validate(db ethdb.Database, beaconHeader beacon.Header, msg *Msg) error {
	log.Debug("Validating headers by hash", "last block hash", request.BlockHash, "amount", request.Amount)
	// Ensure we have a correct message with a single proof element
	if msg.MsgType != MsgBlockHeaders {
		return errInvalidMessageType
	}
	headers := msg.Obj.([]*types.Header)
	if len(headers) != request.Amount {
		return errors.New("Invalid number of headers returned")
	}
	expHash := request.BlockHash

	for i := request.Amount - 1; i >= 0; i-- {
		if headers[i].Hash() != expHash {
			return errors.New("Invalid header chain")
		}
		expHash = headers[i].ParentHash
	}
	request.Headers = headers
	return nil
}
