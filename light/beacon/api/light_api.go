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
// GNU Lesser General Public License for more detaiapi.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	cbeacon "github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/light/beacon"
)

// BeaconLightApi requests light client information from a beacon node REST API.
// Note: all required API endpoints are currently only implemented by Lodestar.
type BeaconLightApi struct {
	url           string
	client        *http.Client
	customHeaders map[string]string
}

func NewBeaconLightApi(url string, customHeaders map[string]string) *BeaconLightApi {
	return &BeaconLightApi{
		url: url,
		client: &http.Client{
			Timeout: time.Second * 10,
		},
		customHeaders: customHeaders,
	}
}

func (api *BeaconLightApi) httpGet(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", api.url+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range api.customHeaders {
		req.Header.Set(k, v)
	}
	resp, err := api.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// GetBestUpdateAndCommittee fetches and validates LightClientUpdate for given period and full serialized
// committee for the next period (committee root hash equals update.NextSyncCommitteeRoot).
// Note that the results are validated but the update signature should be verified by the caller as its
// validity depends on the update chain.
func (api *BeaconLightApi) GetBestUpdateAndCommittee(period uint64) (beacon.LightClientUpdate, []byte, error) {
	resp, err := api.httpGet("/eth/v1/beacon/light_client/updates?start_period=" + strconv.Itoa(int(period)) + "&count=1")
	if err != nil {
		return beacon.LightClientUpdate{}, nil, err
	}

	type committeeUpdate struct {
		Header                  beacon.Header       `json:"attested_header"`
		NextSyncCommittee       syncCommitteeJson   `json:"next_sync_committee"`
		NextSyncCommitteeBranch beacon.MerkleValues `json:"next_sync_committee_branch"`
		FinalizedHeader         beacon.Header       `json:"finalized_header"`
		FinalityBranch          beacon.MerkleValues `json:"finality_branch"`
		Aggregate               syncAggregate       `json:"sync_aggregate"`
	}

	var data struct {
		Data []committeeUpdate `json:"data"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return beacon.LightClientUpdate{}, nil, err
	}
	if len(data.Data) != 1 {
		return beacon.LightClientUpdate{}, nil, errors.New("invalid number of committee updates")
	}
	c := data.Data[0]
	if len(c.NextSyncCommittee.Pubkeys) != 512 {
		return beacon.LightClientUpdate{}, nil, errors.New("invalid number of pubkeys in next_sync_committee")
	}

	committee, ok := c.NextSyncCommittee.serialize()
	if !ok {
		return beacon.LightClientUpdate{}, nil, errors.New("invalid sync committee")
	}
	update := beacon.LightClientUpdate{
		Header:                  c.Header,
		NextSyncCommitteeRoot:   beacon.SerializedCommitteeRoot(committee),
		NextSyncCommitteeBranch: c.NextSyncCommitteeBranch,
		FinalizedHeader:         c.FinalizedHeader,
		FinalityBranch:          c.FinalityBranch,
		SyncCommitteeBits:       c.Aggregate.BitMask,
		SyncCommitteeSignature:  c.Aggregate.Signature,
	}
	if err := update.Validate(); err != nil {
		return beacon.LightClientUpdate{}, nil, err
	}
	return update, committee, nil
}

// syncAggregate represents an aggregated BLS signature with BitMask referring to a subset
// of the corresponding sync committee
type syncAggregate struct {
	BitMask   hexutil.Bytes `json:"sync_committee_bits"`
	Signature hexutil.Bytes `json:"sync_committee_signature"`
}

// GetHeadUpdate fetches the latest available signed header.
// Note that the signature should be verified by the caller as its validity depends on the update chain.
func (api *BeaconLightApi) GetHeadUpdate() (beacon.SignedHead, error) {
	resp, err := api.httpGet("/eth/v1/beacon/light_client/optimistic_update")
	if err != nil {
		return beacon.SignedHead{}, err
	}

	var data struct {
		Data struct {
			Aggregate syncAggregate `json:"sync_aggregate"`
			Header    beacon.Header `json:"attested_header"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return beacon.SignedHead{}, err
	}
	if len(data.Data.Aggregate.BitMask) != 64 {
		return beacon.SignedHead{}, errors.New("invalid sync_committee_bits length")
	}
	if len(data.Data.Aggregate.Signature) != 96 {
		return beacon.SignedHead{}, errors.New("invalid sync_committee_signature length")
	}
	return beacon.SignedHead{
		Header:    data.Data.Header,
		BitMask:   data.Data.Aggregate.BitMask,
		Signature: data.Data.Aggregate.Signature,
	}, nil
}

// syncCommitteeJson is the JSON representation of a sync committee
type syncCommitteeJson struct {
	Pubkeys   []hexutil.Bytes `json:"pubkeys"`
	Aggregate hexutil.Bytes   `json:"aggregate_pubkey"`
}

// serialize returns the serialized version of the committee
func (s *syncCommitteeJson) serialize() ([]byte, bool) {
	if len(s.Pubkeys) != 512 {
		return nil, false
	}
	sk := make([]byte, 513*48)
	for i, key := range s.Pubkeys {
		if len(key) != 48 {
			return nil, false
		}
		copy(sk[i*48:(i+1)*48], key[:])
	}
	if len(s.Aggregate) != 48 {
		return nil, false
	}
	copy(sk[512*48:], s.Aggregate[:])
	return sk, true
}

// GetHead fetches and validates the beacon header with the given blockRoot.
// If blockRoot is null hash then the latest head header is fetched.
func (api *BeaconLightApi) GetHeader(blockRoot common.Hash) (beacon.Header, error) {
	path := "/eth/v1/beacon/headers/"
	if blockRoot == (common.Hash{}) {
		path += "head"
	} else {
		path += blockRoot.Hex()
	}
	resp, err := api.httpGet(path)
	if err != nil {
		return beacon.Header{}, err
	}

	var data struct {
		Data struct {
			Root      common.Hash `json:"root"`
			Canonical bool        `json:"canonical"`
			Header    struct {
				Message   beacon.Header `json:"message"`
				Signature hexutil.Bytes `json:"signature"`
			} `json:"header"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return beacon.Header{}, err
	}
	header := data.Data.Header.Message
	if blockRoot == (common.Hash{}) {
		blockRoot = data.Data.Root
	}
	if header.Hash() != blockRoot {
		return beacon.Header{}, errors.New("retrieved beacon header root does not match")
	}
	return header, nil
}

// GetStateProof fetches and validates a Merkle proof for the specified parts of the recent
// beacon state referenced by stateRoot. If successful the returned multiproof has the format
// specified by expFormat. The state subset specified by the list of string keys (paths) should
// cover the subset specified by expFormat.
func (api *BeaconLightApi) GetStateProof(stateRoot common.Hash, paths []string, expFormat beacon.ProofFormat) (beacon.MultiProof, error) {
	path := "/eth/v1/beacon/light_client/proof/" + stateRoot.Hex() + "?paths=" + paths[0]
	for i := 1; i < len(paths); i++ {
		path += "&paths=" + paths[i]
	}
	resp, err := api.httpGet(path)
	if err != nil {
		return beacon.MultiProof{}, err
	}
	proof, err := beacon.ParseSSZMultiProof(resp)
	if err != nil {
		return beacon.MultiProof{}, err
	}
	var values beacon.MerkleValues
	reader := proof.Reader(nil)
	root, ok := beacon.TraverseProof(reader, beacon.NewMultiProofWriter(expFormat, &values, nil))
	if !ok || !reader.Finished() || root != stateRoot {
		return beacon.MultiProof{}, errors.New("invalid state proof")
	}
	return beacon.MultiProof{Format: expFormat, Values: values}, nil
}

// GetCheckpointData fetches and validates bootstrap data belonging to the given checkpoint.
func (api *BeaconLightApi) GetCheckpointData(ctx context.Context, checkpoint common.Hash) (beacon.Header, beacon.CheckpointData, []byte, error) {
	resp, err := api.httpGet("/eth/v1/beacon/light_client/bootstrap/" + checkpoint.String())
	if err != nil {
		return beacon.Header{}, beacon.CheckpointData{}, nil, err
	}

	type bootstrapData struct {
		Data struct {
			Header          beacon.Header       `json:"header"`
			Committee       syncCommitteeJson   `json:"current_sync_committee"`
			CommitteeBranch beacon.MerkleValues `json:"current_sync_committee_branch"`
		} `json:"data"`
	}

	var data bootstrapData
	if err := json.Unmarshal(resp, &data); err != nil {
		return beacon.Header{}, beacon.CheckpointData{}, nil, err
	}
	committee, ok := data.Data.Committee.serialize()
	if !ok {
		return beacon.Header{}, beacon.CheckpointData{}, nil, errors.New("invalid sync committee JSON")
	}
	committeeRoot := beacon.SerializedCommitteeRoot(committee)
	expStateRoot, ok := beacon.VerifySingleProof(data.Data.CommitteeBranch, beacon.BsiSyncCommittee, beacon.MerkleValue(committeeRoot), 0)
	if !ok || expStateRoot != data.Data.Header.StateRoot {
		return beacon.Header{}, beacon.CheckpointData{}, nil, errors.New("invalid sync committee Merkle proof")
	}
	checkpointData := beacon.CheckpointData{
		Checkpoint:     checkpoint,
		Period:         uint64(data.Data.Header.Slot) >> 13,
		CommitteeRoots: []common.Hash{committeeRoot},
	}
	return data.Data.Header, checkpointData, committee, nil
}

// GetExecutionPayload fetches the execution block belonging to the beacon block specified
// by beaconRoot and validates its block hash against the expected execRoot.
func (api *BeaconLightApi) GetExecutionPayload(beaconRoot, execRoot common.Hash) (*types.Block, error) {
	resp, err := api.httpGet("/eth/v2/beacon/blocks/" + beaconRoot.Hex())
	if err != nil {
		return nil, err
	}

	var beaconBlock struct {
		Data struct {
			Message struct {
				Body struct {
					Payload struct {
						ParentHash    common.Hash     `json:"parent_hash"`
						FeeRecipient  common.Address  `json:"fee_recipient"`
						StateRoot     common.Hash     `json:"state_root"`
						ReceiptsRoot  common.Hash     `json:"receipts_root"`
						LogsBloom     hexutil.Bytes   `json:"logs_bloom"`
						PrevRandao    common.Hash     `json:"prev_randao"`
						BlockNumber   common.Decimal  `json:"block_number"`
						GasLimit      common.Decimal  `json:"gas_limit"`
						GasUsed       common.Decimal  `json:"gas_used"`
						Timestamp     common.Decimal  `json:"timestamp"`
						ExtraData     hexutil.Bytes   `json:"extra_data"`
						BaseFeePerGas common.Decimal  `json:"base_fee_per_gas"`
						BlockHash     common.Hash     `json:"block_hash"`
						Transactions  []hexutil.Bytes `json:"transactions"`
					} `json:"execution_payload"`
				} `json:"body"`
			} `json:"message"`
		} `json:"data"`
	}

	if err := json.Unmarshal(resp, &beaconBlock); err != nil {
		return nil, err
	}

	payload := beaconBlock.Data.Message.Body.Payload
	transactions := make([][]byte, len(payload.Transactions))
	for i, tx := range payload.Transactions {
		transactions[i] = tx
	}
	execData := cbeacon.ExecutableDataV1{
		ParentHash:    payload.ParentHash,
		FeeRecipient:  payload.FeeRecipient,
		StateRoot:     payload.StateRoot,
		ReceiptsRoot:  payload.ReceiptsRoot,
		LogsBloom:     payload.LogsBloom,
		Random:        payload.PrevRandao,
		Number:        uint64(payload.BlockNumber),
		GasLimit:      uint64(payload.GasLimit),
		GasUsed:       uint64(payload.GasUsed),
		Timestamp:     uint64(payload.Timestamp),
		ExtraData:     payload.ExtraData,
		BaseFeePerGas: big.NewInt(int64(payload.BaseFeePerGas)),
		BlockHash:     payload.BlockHash,
		Transactions:  transactions,
	}
	block, err := cbeacon.ExecutableDataToBlock(execData)
	if err != nil {
		return nil, err
	}
	if block.Hash() != execRoot {
		return nil, errors.New("exec block root does not match")
	}
	return block, nil
}
