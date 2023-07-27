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
// GNU Lesser General Public License for more detaiapi.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package api

import (
	"encoding/json"
	"errors"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/beacon/params"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/protolambda/zrnt/eth2/beacon/capella"
)

var (
	urlUpdates    = "/eth/v1/beacon/light_client/updates"
	urlOptimistic = "/eth/v1/beacon/light_client/optimistic_update"
	urlHeaders    = "/eth/v1/beacon/headers"
	urlStateProof = "/eth/v0/beacon/proof/state" //TODO
	urlBootstrap  = "/eth/v1/beacon/light_client/bootstrap"
	urlBlocks     = "/eth/v2/beacon/blocks"
	urlEvents     = "/eth/v1/events"
)

type CommitteeUpdate struct {
	Version           string
	Update            types.LightClientUpdate
	NextSyncCommittee types.SerializedSyncCommittee
}

type jsonBeaconHeader struct {
	Beacon types.Header `json:"beacon"`
}

// See data structure definition here:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/altair/light-client/sync-protocol.md#lightclientupdate
type committeeUpdateJson struct {
	Version string              `json:"version"`
	Data    committeeUpdateData `json:"data"`
}

type committeeUpdateData struct {
	Header                  jsonBeaconHeader              `json:"attested_header"`
	NextSyncCommittee       types.SerializedSyncCommittee `json:"next_sync_committee"`
	NextSyncCommitteeBranch merkle.Values                 `json:"next_sync_committee_branch"`
	FinalizedHeader         *jsonBeaconHeader             `json:"finalized_header,omitempty"`
	FinalityBranch          merkle.Values                 `json:"finality_branch,omitempty"`
	SyncAggregate           types.SyncAggregate           `json:"sync_aggregate"`
	SignatureSlot           common.Decimal                `json:"signature_slot"`
}

func (u *CommitteeUpdate) MarshalJSON() ([]byte, error) {
	enc := committeeUpdateJson{
		Version: u.Version,
		Data: committeeUpdateData{
			Header:                  jsonBeaconHeader{Beacon: u.Update.AttestedHeader.Header},
			NextSyncCommittee:       u.NextSyncCommittee,
			NextSyncCommitteeBranch: u.Update.NextSyncCommitteeBranch,
			//FinalizedHeader         *jsonBeaconHeader             `json:"finalized_header,omitempty"`
			//FinalityBranch          merkle.Values                 `json:"finality_branch,omitempty"`
			SyncAggregate: u.Update.AttestedHeader.Signature,
			SignatureSlot: common.Decimal(u.Update.AttestedHeader.SignatureSlot),
		},
	}
	if u.Update.FinalizedHeader != nil {
		enc.Data.FinalizedHeader = &jsonBeaconHeader{Beacon: *u.Update.FinalizedHeader}
		enc.Data.FinalityBranch = u.Update.FinalityBranch
	}
	return json.Marshal(&enc)
}

// UnmarshalJSON unmarshals from JSON.
func (u *CommitteeUpdate) UnmarshalJSON(input []byte) error {
	var dec committeeUpdateJson
	if err := json.Unmarshal(input, &dec); err != nil {
		return err
	}
	u.Version = dec.Version
	u.NextSyncCommittee = dec.Data.NextSyncCommittee
	u.Update = types.LightClientUpdate{
		AttestedHeader: types.SignedHeader{
			Header:        dec.Data.Header.Beacon,
			Signature:     dec.Data.SyncAggregate,
			SignatureSlot: uint64(dec.Data.SignatureSlot),
		},
		NextSyncCommitteeRoot:   u.NextSyncCommittee.Root(),
		NextSyncCommitteeBranch: dec.Data.NextSyncCommitteeBranch,
		FinalityBranch:          dec.Data.FinalityBranch,
	}
	if dec.Data.FinalizedHeader != nil {
		u.Update.FinalizedHeader = &dec.Data.FinalizedHeader.Beacon
	}
	return nil
}

type jsonOptimisticUpdate struct {
	Data struct {
		Header        jsonBeaconHeader    `json:"attested_header"`
		Aggregate     types.SyncAggregate `json:"sync_aggregate"`
		SignatureSlot common.Decimal      `json:"signature_slot"`
	} `json:"data"`
}

func encodeOptimisticHeadUpdate(header types.SignedHeader) ([]byte, error) {
	var data jsonOptimisticUpdate
	data.Data.Header.Beacon = header.Header
	data.Data.Aggregate = header.Signature
	data.Data.SignatureSlot = common.Decimal(header.SignatureSlot)
	return json.Marshal(&data)
}

func decodeOptimisticHeadUpdate(enc []byte) (types.SignedHeader, error) {
	var data jsonOptimisticUpdate
	if err := json.Unmarshal(enc, &data); err != nil {
		return types.SignedHeader{}, err
	}
	if data.Data.Header.Beacon.StateRoot == (common.Hash{}) {
		// workaround for different event encoding format in Lodestar
		if err := json.Unmarshal(enc, &data.Data); err != nil {
			return types.SignedHeader{}, err
		}
	}

	if len(data.Data.Aggregate.Signers) != params.SyncCommitteeBitmaskSize {
		return types.SignedHeader{}, errors.New("invalid sync_committee_bits length")
	}
	if len(data.Data.Aggregate.Signature) != params.BLSSignatureSize {
		return types.SignedHeader{}, errors.New("invalid sync_committee_signature length")
	}
	return types.SignedHeader{
		Header:        data.Data.Header.Beacon,
		Signature:     data.Data.Aggregate,
		SignatureSlot: uint64(data.Data.SignatureSlot),
	}, nil
}

type jsonHeadEvent struct {
	Slot  common.Decimal `json:"slot"`
	Block common.Hash    `json:"block"`
}

type jsonBeaconBlock struct {
	Data struct {
		Message capella.BeaconBlock `json:"message"`
	} `json:"data"`
}

type jsonBootstrapData struct {
	Data struct {
		Header          jsonBeaconHeader               `json:"header"`
		Committee       *types.SerializedSyncCommittee `json:"current_sync_committee"`
		CommitteeBranch merkle.Values                  `json:"current_sync_committee_branch"`
	} `json:"data"`
}

type jsonHeaderData struct {
	Data struct {
		Root      common.Hash `json:"root"`
		Canonical bool        `json:"canonical"`
		Header    struct {
			Message   types.Header  `json:"message"`
			Signature hexutil.Bytes `json:"signature"`
		} `json:"header"`
	} `json:"data"`
}
