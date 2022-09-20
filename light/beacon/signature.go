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
	"bytes"
	"math/rand"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	bls "github.com/protolambda/bls12-381-util"
)

type syncCommittee interface{}

type committeeSigVerifier interface {
	deserializeSyncCommittee(enc []byte) syncCommittee
	verifySignature(committee syncCommittee, signedRoot common.Hash, bitmask, signature []byte) bool // expects
}

// blsSyncCommittee is a set of sync committee signer pubkeys
type blsSyncCommittee struct {
	keys      [512]*bls.Pubkey
	aggregate *bls.Pubkey
}

type BLSVerifier struct{}

func (BLSVerifier) deserializeSyncCommittee(enc []byte) syncCommittee {
	if len(enc) != 513*48 {
		log.Error("Wrong input size for deserializeSyncCommittee", "expected", 513*48, "got", len(enc))
		return nil
	}
	sc := new(blsSyncCommittee)
	for i := 0; i <= 512; i++ {
		pk := new(bls.Pubkey)
		var sk [48]byte
		copy(sk[:], enc[i*48:(i+1)*48])
		if err := pk.Deserialize(&sk); err != nil {
			log.Error("bls.Pubkey.Deserialize failed", "error", err, "data", sk)
			return nil
		}
		if i < 512 {
			sc.keys[i] = pk
		} else {
			sc.aggregate = pk
		}
	}
	return sc
}

func (BLSVerifier) verifySignature(committee syncCommittee, signingRoot common.Hash, bitmask, signature []byte) bool {
	if len(signature) != 96 || len(bitmask) != 64 {
		////fmt.Println("wrong sig size")
		return false
	}
	var sig bls.Signature
	var sigBytes [96]byte
	copy(sigBytes[:], signature)
	if err := sig.Deserialize(&sigBytes); err != nil {
		////fmt.Println("sig deserialize error", err)
		return false
	}
	var signerKeys [512]*bls.Pubkey
	blsCommittee := committee.(*blsSyncCommittee)
	signerCount := 0
	for i, key := range blsCommittee.keys {
		if bitmask[i/8]&(byte(1)<<(i%8)) != 0 {
			signerKeys[signerCount] = key
			signerCount++
		}
	}
	return bls.FastAggregateVerify(signerKeys[:signerCount], signingRoot[:], &sig)
}

type dummySyncCommittee [32]byte

type dummyVerifier struct{}

func (dummyVerifier) deserializeSyncCommittee(enc []byte) syncCommittee {
	if len(enc) != 513*48 {
		log.Error("Wrong input size for deserializeSyncCommittee", "expected", 513*48, "got", len(enc))
		return nil
	}
	var sc dummySyncCommittee
	copy(sc[:], enc[:32])
	return sc
}

func (dummyVerifier) verifySignature(committee syncCommittee, signingRoot common.Hash, bitmask, signature []byte) bool {
	return bytes.Equal(signature, makeDummySignature(committee.(dummySyncCommittee), signingRoot, bitmask))
}

func randomDummySyncCommittee() dummySyncCommittee {
	var sc dummySyncCommittee
	rand.Read(sc[:])
	return sc
}

func serializeDummySyncCommittee(sc dummySyncCommittee) []byte {
	enc := make([]byte, 513*48)
	copy(enc[:32], sc[:])
	return enc
}

func makeDummySignature(committee dummySyncCommittee, signingRoot common.Hash, bitmask []byte) []byte {
	sig := make([]byte, 96)
	for i, b := range committee[:] {
		sig[i] = b ^ signingRoot[i]
	}
	copy(sig[32:], bitmask)
	return sig
}
