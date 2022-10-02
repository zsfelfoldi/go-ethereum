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
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
)

func makeTestHeaderWithSingleProof(slot, index uint64, value MerkleValue) (Header, MerkleValues) {
	var branch MerkleValues
	hasher := sha256.New()
	for index > 1 {
		var proofHash MerkleValue
		rand.Read(proofHash[:])
		hasher.Reset()
		if index&1 == 0 {
			hasher.Write(value[:])
			hasher.Write(proofHash[:])
		} else {
			hasher.Write(proofHash[:])
			hasher.Write(value[:])
		}
		hasher.Sum(value[:0])
		index /= 2
		branch = append(branch, proofHash)
	}
	return Header{Slot: slot, StateRoot: value}, branch
}

func makeBitmask(signerCount int) []byte {
	bitmask := make([]byte, 64)
	for i := 0; i < 512; i++ {
		if rand.Intn(512-i) < signerCount {
			bitmas[i/8] += byte(1) << (i & 7)
			signerCount--
		}
	}
	return bitmask
}

type testPeriod struct {
	committee     dummySyncCommittee
	committeeRoot common.Hash
	update        *LightClientUpdate
}

type testChain struct {
	periods []testPeriod
	forks   Forks
}

func (tc testChain) makeTestSignedHead(header Header, signerCount int) SignedHead {
	bitmask := makeBitmask(signerCount)
	return SignedHead{
		Header:    header,
		BitMask:   bitmask,
		Signature: makeDummySignature(tc.periods[(header.Slot+1)>>13], tc.forks.signingRoot(header), bitmask),
	}
}

const finalizedTestUpdate = 8191 // if subPeriodIndex == finalizedTestUpdate then a finalized update is generated

func (tc testChain) makeTestUpdate(period, subPeriodIndex uint64, signerCount int) LightClientUpdate {
	var update LightClientUpdate
	update.NextSyncCommitteeRoot = tc.periods[period+1].committeeRoot
	if subPeriodIndex == finalizedTestUpdate {
		update.FinalizedHeader, update.NextSyncCommitteeBranch = makeTestHeaderWithSingleProof(period<<13+100, BsiNextSyncCommittee, update.NextSyncCommitteeRoot)
		update.Header, update.FinalityBranch = makeTestHeaderWithSingleProof(period<<13+200, BsiFinalBlock, update.FinalizedHeader.Hash())
	} else {
		update.Header, update.NextSyncCommitteeBranch = makeTestHeaderWithSingleProof(period<<13+subPeriodIndex, BsiNextSyncCommittee, update.NextSyncCommitteeRoot)
	}
	signedHead := tc.makeTestSignedHead(update.Header, signerCount)
	update.SyncCommitteeBits, update.SyncCommitteeSignature = signedHead.BitMask, signedHead.Signature
	update.ForkVersion = tc.forks.version(update.Header.Slot >> 5)
	return update
}

func TestSyncCommitteeTracker(t *testing.T) {
	var gvRoot common.Hash
	forks := Forks{
		Fork{Epoch: 0, Version: []byte{0}},
	}
	forks.computeDomains(gvRoot)
	testChain := testChain{
		periods: make([]testPeriod, 10),
		forks:   forks,
	}
	for i := range testChain.periods {

	}
	sct := NewSyncCommitteeTracker(db, forks, constraints, 257, dummyVerifier{}, &mclock.Simulated{})
	sct.SyncWithPeer()
}
