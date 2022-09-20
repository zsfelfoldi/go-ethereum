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

type Header struct {
	Slot          common.Decimal `json:"slot"`
	ProposerIndex common.Decimal `json:"proposer_index"`
	ParentRoot    common.Hash    `json:"parent_root"`
	StateRoot     common.Hash    `json:"state_root"`
	BodyRoot      common.Hash    `json:"body_root"`
}

func (bh *Header) Hash() common.Hash {
	var values [8]MerkleValue
	binary.LittleEndian.PutUint64(values[0][:8], uint64(bh.Slot))
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = MerkleValue(bh.ParentRoot)
	values[3] = MerkleValue(bh.StateRoot)
	values[4] = MerkleValue(bh.BodyRoot)
	return MultiProof{Format: NewRangeFormat(8, 15, nil), Values: values[:]}.rootHash()
}

type HeaderWithoutState struct {
	Slot                 uint64
	ProposerIndex        uint
	ParentRoot, BodyRoot common.Hash
}

func (bh *HeaderWithoutState) Hash(stateRoot common.Hash) common.Hash {
	return bh.Proof(stateRoot).rootHash()
}

func (bh *HeaderWithoutState) Proof(stateRoot common.Hash) MultiProof {
	var values [8]MerkleValue
	binary.LittleEndian.PutUint64(values[0][:8], bh.Slot)
	binary.LittleEndian.PutUint64(values[1][:8], uint64(bh.ProposerIndex))
	values[2] = MerkleValue(bh.ParentRoot)
	values[3] = MerkleValue(stateRoot)
	values[4] = MerkleValue(bh.BodyRoot)
	return MultiProof{Format: NewRangeFormat(8, 15, nil), Values: values[:]}
}

func (bh *HeaderWithoutState) FullHeader(stateRoot common.Hash) Header {
	return Header{
		Slot:          common.Decimal(bh.Slot),
		ProposerIndex: common.Decimal(bh.ProposerIndex),
		ParentRoot:    bh.ParentRoot,
		StateRoot:     stateRoot,
		BodyRoot:      bh.BodyRoot,
	}
}
