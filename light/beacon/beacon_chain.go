// Copyright 2022 The go-ethereum Authors
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

package beacon

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
)

const (
	// beacon header fields
	BhiSlot          = 8
	BhiProposerIndex = 9
	BhiParentRoot    = 10
	BhiStateRoot     = 11
	BhiBodyRoot      = 12

	// beacon state fields
	BsiGenesisTime       = 32
	BsiGenesisValidators = 33
	BsiForkVersion       = 141
	BsiLatestHeader      = 36
	BsiBlockRoots        = 37
	BsiStateRoots        = 38
	BsiHistoricRoots     = 39
	BsiFinalBlock        = 105
	BsiSyncCommittee     = 54
	BsiNextSyncCommittee = 55
	BsiExecHead          = 908
)

var BsiFinalExecHash = ChildIndex(ChildIndex(BsiFinalBlock, BhiStateRoot), BsiExecHead)

// Header defines a beacon header
type Header struct {
	Slot          uint64
	ProposerIndex uint
	ParentRoot    common.Hash
	StateRoot     common.Hash
	BodyRoot      common.Hash
}

// Hash calculates the block root of the header
func (bh *Header) Hash() common.Hash {
	var values [8]MerkleValue
	binary.LittleEndian.PutUint64(values[0][:8], bh.Slot)
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = MerkleValue(bh.ParentRoot)
	values[3] = MerkleValue(bh.StateRoot)
	values[4] = MerkleValue(bh.BodyRoot)
	return MultiProof{Format: NewRangeFormat(8, 15, nil), Values: values[:]}.rootHash()
}

// Epoch returns the epoch the header belongs to
func (bh *Header) Epoch() uint64 {
	return bh.Slot >> 5
}

// SyncPeriod returns the sync period the header belongs to
func (bh *Header) SyncPeriod() uint64 {
	return bh.Slot >> 13
}

// PeriodStart returns the first slot of the given period
func PeriodStart(period uint64) uint64 {
	return period << 13
}

// PeriodOfSlot returns the sync period that the given slot belongs to
func PeriodOfSlot(slot uint64) uint64 {
	return slot >> 13
}

// HeaderWithoutState stores beacon header fields except the state root which can be
// reconstructed from a partial beacon state proof stored alongside the header
type HeaderWithoutState struct {
	Slot                 uint64
	ProposerIndex        uint
	ParentRoot, BodyRoot common.Hash
}

// Hash calculates the block root of the header
func (bh *HeaderWithoutState) Hash(stateRoot common.Hash) common.Hash {
	return bh.Proof(stateRoot).rootHash()
}

// Proof returns a MultiProof of the header
func (bh *HeaderWithoutState) Proof(stateRoot common.Hash) MultiProof {
	var values [8]MerkleValue
	binary.LittleEndian.PutUint64(values[0][:8], bh.Slot)
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = MerkleValue(bh.ParentRoot)
	values[3] = MerkleValue(stateRoot)
	values[4] = MerkleValue(bh.BodyRoot)
	return MultiProof{Format: NewRangeFormat(8, 15, nil), Values: values[:]}
}

// FullHeader reconstructs a full Header from a HeaderWithoutState and a state root
func (bh *HeaderWithoutState) FullHeader(stateRoot common.Hash) Header {
	return Header{
		Slot:          bh.Slot,
		ProposerIndex: bh.ProposerIndex,
		ParentRoot:    bh.ParentRoot,
		StateRoot:     stateRoot,
		BodyRoot:      bh.BodyRoot,
	}
}
