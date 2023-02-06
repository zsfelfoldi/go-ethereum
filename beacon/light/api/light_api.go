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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/donovanhide/eventsource"
	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/light/types"
	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/beacon/params"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ctypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/protolambda/zrnt/eth2/beacon/capella"
	"github.com/protolambda/zrnt/eth2/configs"
	"github.com/protolambda/ztyp/tree"
)

var (
	ErrNotFound = errors.New("404 Not Found")
	ErrInternal = errors.New("500 Internal Server Error")
)

// BeaconLightApi requests light client information from a beacon node REST API.
// Note: all required API endpoints are currently only implemented by Lodestar.
type BeaconLightApi struct {
	url               string
	client            *http.Client
	customHeaders     map[string]string
	stateProofVersion int
}

func NewBeaconLightApi(url string, customHeaders map[string]string, stateProofVersion int) *BeaconLightApi {
	return &BeaconLightApi{
		url: url,
		client: &http.Client{
			Timeout: time.Second * 10,
		},
		customHeaders:     customHeaders,
		stateProofVersion: stateProofVersion,
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
	switch resp.StatusCode {
	case 200:
		return io.ReadAll(resp.Body)
	case 404:
		return nil, ErrNotFound
	case 500:
		return nil, ErrInternal
	default:
		return nil, fmt.Errorf("Unexpected error from API endpoint \"%s\": status code %d", path, resp.StatusCode)
	}
}

func (api *BeaconLightApi) httpGetf(format string, params ...any) ([]byte, error) {
	return api.httpGet(fmt.Sprintf(format, params...))
}

// GetBestUpdateAndCommittee fetches and validates LightClientUpdate for given
// period and full serialized committee for the next period (committee root hash
// equals update.NextSyncCommitteeRoot).
// Note that the results are validated but the update signature should be verified
// by the caller as its validity depends on the update chain.
//TODO handle valid partial results
func (api *BeaconLightApi) GetBestUpdatesAndCommittees(firstPeriod, count uint64) ([]*types.LightClientUpdate, []*types.SerializedCommittee, error) {
	resp, err := api.httpGetf("/eth/v1/beacon/light_client/updates?start_period=%d&count=%d", firstPeriod, count)
	if err != nil {
		return nil, nil, err
	}

	var data []types.CommitteeUpdate
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, nil, err
	}
	if len(data) != int(count) {
		return nil, nil, errors.New("invalid number of committee updates")
	}
	updates := make([]*types.LightClientUpdate, int(count))
	committees := make([]*types.SerializedCommittee, int(count))
	for i, d := range data {
		if d.Update.Header.SyncPeriod() != firstPeriod+uint64(i) {
			return nil, nil, errors.New("wrong committee update header period")
		}
		if err := d.Update.Validate(); err != nil {
			return nil, nil, err
		}
		if d.NextSyncCommittee.Root() != d.Update.NextSyncCommitteeRoot {
			return nil, nil, errors.New("wrong sync committee root")
		}
		updates[i], committees[i] = d.Update, d.NextSyncCommittee
	}
	return updates, committees, nil
}

// GetOptimisticHeadUpdate fetches a signed header based on the latest available
// optimistic update. Note that the signature should be verified by the caller
// as its validity depends on the update chain.
//
// See data structure definition here:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/altair/light-client/sync-protocol.md#lightclientoptimisticupdate
func (api *BeaconLightApi) GetOptimisticHeadUpdate() (types.SignedHead, error) {
	resp, err := api.httpGet("/eth/v1/beacon/light_client/optimistic_update")
	if err != nil {
		return types.SignedHead{}, err
	}
	return decodeOptimisticHeadUpdate(resp)
}

func decodeOptimisticHeadUpdate(enc []byte) (types.SignedHead, error) {
	var data struct {
		Data struct {
			Header        types.JsonBeaconHeader `json:"attested_header"`
			Aggregate     types.SyncAggregate    `json:"sync_aggregate"`
			SignatureSlot common.Decimal         `json:"signature_slot"`
		} `json:"data"`
	}
	if err := json.Unmarshal(enc, &data); err != nil {
		return types.SignedHead{}, err
	}
	if data.Data.Header.Beacon.StateRoot == (common.Hash{}) {
		// workaround for different event encoding format in Lodestar
		if err := json.Unmarshal(enc, &data.Data); err != nil {
			return types.SignedHead{}, err
		}
	}

	if len(data.Data.Aggregate.BitMask) != params.SyncCommitteeBitmaskSize {
		return types.SignedHead{}, errors.New("invalid sync_committee_bits length")
	}
	if len(data.Data.Aggregate.Signature) != params.BlsSignatureSize {
		return types.SignedHead{}, errors.New("invalid sync_committee_signature length")
	}
	return types.SignedHead{
		Header:        data.Data.Header.Beacon,
		SyncAggregate: data.Data.Aggregate,
		SignatureSlot: uint64(data.Data.SignatureSlot),
	}, nil
}

// GetHead fetches and validates the beacon header with the given blockRoot.
// If blockRoot is null hash then the latest head header is fetched.
func (api *BeaconLightApi) GetHeader(blockRoot common.Hash) (types.Header, error) {
	var blockId string
	if blockRoot == (common.Hash{}) {
		blockId = "head"
	} else {
		blockId = blockRoot.Hex()
	}
	resp, err := api.httpGetf("/eth/v1/beacon/headers/%s", blockId)
	if err != nil {
		return types.Header{}, err
	}

	var data struct {
		Data struct {
			Root      common.Hash `json:"root"`
			Canonical bool        `json:"canonical"`
			Header    struct {
				Message   types.Header  `json:"message"`
				Signature hexutil.Bytes `json:"signature"`
			} `json:"header"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &data); err != nil {
		return types.Header{}, err
	}
	header := data.Data.Header.Message
	if blockRoot == (common.Hash{}) {
		blockRoot = data.Data.Root
	}
	if header.Hash() != blockRoot {
		return types.Header{}, errors.New("retrieved beacon header root does not match")
	}
	return header, nil
}

// does not verify state root
func (api *BeaconLightApi) GetHeadStateProof(format merkle.ProofFormat, paths []string) (merkle.MultiProof, error) {
	if api.stateProofVersion >= 2 {
		encFormat, bitLength := EncodeCompactProofFormat(format)
		return api.getStateProof("head", format, encFormat, bitLength)
	} else {
		proof, _, err := api.getOldStateProof("head", format, paths)
		return proof, err
	}
}

type StateProofSub struct {
	api       *BeaconLightApi
	format    merkle.ProofFormat
	paths     []string
	encFormat []byte
	bitLength int
}

func (api *BeaconLightApi) SubscribeStateProof(format merkle.ProofFormat, paths []string, first, period int) (*StateProofSub, error) {
	if api.stateProofVersion == 0 {
		return nil, errors.New("State proof API disabled")
	}
	encFormat, bitLength := EncodeCompactProofFormat(format)
	if api.stateProofVersion >= 2 {
		_, err := api.httpGetf("/eth/v0/beacon/proof/subscribe/states?format=0x%x&first=%d&period=%d", encFormat, first, period)
		if err != nil {
			return nil, err
		}
	}
	return &StateProofSub{
		api:       api,
		format:    format,
		encFormat: encFormat,
		bitLength: bitLength,
		paths:     paths,
	}, nil
}

// verifies state root
func (sub *StateProofSub) Get(stateRoot common.Hash) (merkle.MultiProof, error) {
	var (
		proof    merkle.MultiProof
		rootHash common.Hash
		err      error
	)
	if sub.api.stateProofVersion >= 2 {
		proof, err = sub.api.getStateProof(stateRoot.Hex(), sub.format, sub.encFormat, sub.bitLength)
		if err == nil {
			rootHash = proof.RootHash()
		}
	} else {
		proof, rootHash, err = sub.api.getOldStateProof(stateRoot.Hex(), sub.format, sub.paths)
	}
	if err != nil {
		return merkle.MultiProof{}, err
	}
	if rootHash != stateRoot {
		return merkle.MultiProof{}, errors.New("Received proof has incorrect state root")
	}
	return proof, nil
}

func (api *BeaconLightApi) getStateProof(stateId string, format merkle.ProofFormat, encFormat []byte, bitLength int) (merkle.MultiProof, error) {
	resp, err := api.httpGetf("/eth/v0/beacon/proof/state/%s?format=0x%x", stateId, encFormat)
	if err != nil {
		return merkle.MultiProof{}, err
	}
	valueCount := (bitLength + 1) / 2
	if len(resp) != valueCount*32 {
		return merkle.MultiProof{}, errors.New("Invalid state proof length")
	}
	values := make(merkle.Values, valueCount)
	for i := range values {
		copy(values[i][:], resp[i*32:(i+1)*32])
	}
	return merkle.MultiProof{Format: format, Values: values}, nil
}

// getOldStateProof fetches and validates a Merkle proof for the specified parts of
// the recent beacon state referenced by stateRoot. If successful the returned
// multiproof has the format specified by expFormat. The state subset specified by
// the list of string keys (paths) should cover the subset specified by expFormat.
func (api *BeaconLightApi) getOldStateProof(stateId string, expFormat merkle.ProofFormat, paths []string) (merkle.MultiProof, common.Hash, error) {
	path := "/eth/v0/beacon/proof/state/" + stateId + "?paths=" + paths[0]
	for i := 1; i < len(paths); i++ {
		path += "&paths=" + paths[i]
	}
	resp, err := api.httpGet(path)
	if err != nil {
		return merkle.MultiProof{}, common.Hash{}, err
	}
	proof, err := parseSSZMultiProof(resp)
	if err != nil {
		return merkle.MultiProof{}, common.Hash{}, err
	}
	var values merkle.Values
	reader := proof.Reader(nil)
	root, ok := merkle.TraverseProof(reader, merkle.NewMultiProofWriter(expFormat, &values, nil))
	if !ok || !reader.Finished() {
		return merkle.MultiProof{}, common.Hash{}, errors.New("invalid state proof")
	}
	return merkle.MultiProof{Format: expFormat, Values: values}, root, nil
}

// parseSSZMultiProof creates a MultiProof from a serialized format:
//
// 1 byte: always 1
// 2 bytes: leafCount
// (leafCount-1) * 2 bytes: as the tree is traversed in depth-first, left-to-right
//     order, the number of leaves on the left branch of each traversed non-leaf
//     subtree are listed here
// leafCount * 32 bytes: leaf values and internal sibling hashes in the same traversal order
//
// Note: this is the format generated by the /eth/v1/beacon/light_client/proof/
// beacon light client API endpoint which is currently only supported by Lodestar.
// A different format is proposed to be standardized so this function will
// probably be replaced later, see here:
//
// https://github.com/ethereum/consensus-specs/pull/3148
// https://github.com/ethereum/beacon-APIs/pull/267
func parseSSZMultiProof(proof []byte) (merkle.MultiProof, error) {
	if len(proof) < 3 || proof[0] != 1 {
		return merkle.MultiProof{}, errors.New("invalid proof length")
	}
	var (
		leafCount   = int(binary.LittleEndian.Uint16(proof[1:3]))
		format      = merkle.NewIndexMapFormat()
		values      = make(merkle.Values, leafCount)
		valuesStart = leafCount*2 + 1
	)
	if len(proof) != leafCount*34+1 {
		return merkle.MultiProof{}, errors.New("invalid proof length")
	}
	if err := parseMultiProofFormat(format, 1, proof[3:valuesStart]); err != nil {
		return merkle.MultiProof{}, err
	}
	for i := range values {
		copy(values[i][:], proof[valuesStart+i*32:valuesStart+(i+1)*32])
	}
	return merkle.MultiProof{Format: format, Values: values}, nil
}

// parseMultiProofFormat recursively parses the SSZ serialized proof format
func parseMultiProofFormat(indexMap merkle.IndexMapFormat, index uint64, format []byte) error {
	indexMap.AddLeaf(index, nil)
	if len(format) == 0 {
		return nil
	}
	boundary := int(binary.LittleEndian.Uint16(format[:2])) * 2
	if boundary > len(format) {
		return errors.New("invalid proof format")
	}
	if err := parseMultiProofFormat(indexMap, index*2, format[2:boundary]); err != nil {
		return err
	}
	if err := parseMultiProofFormat(indexMap, index*2+1, format[boundary:]); err != nil {
		return err
	}
	return nil
}

// EncodeCompactProofFormat encodes a merkle.ProofFormat into a binary compact
// proof format. See description here:
// https://github.com/ChainSafe/consensus-specs/blob/feat/multiproof/ssz/merkle-proofs.md#compact-multiproofs
func EncodeCompactProofFormat(format merkle.ProofFormat) ([]byte, int) {
	target := make([]byte, 0, 64)
	var bitLength int
	encodeProofFormatSubtree(format, &target, &bitLength)
	return target, bitLength
}

// encodeProofFormatSubtree recursively encodes a subtree of a proof format into
// binary compact format.
func encodeProofFormatSubtree(format merkle.ProofFormat, target *[]byte, bitLength *int) {
	bytePtr, bitMask := *bitLength>>3, byte(128)>>(*bitLength&7)
	*bitLength++
	if bytePtr == len(*target) {
		*target = append(*target, byte(0))
	}
	if left, right := format.Children(); left != nil {
		(*target)[bytePtr] += bitMask
		encodeProofFormatSubtree(left, target, bitLength)
		encodeProofFormatSubtree(right, target, bitLength)
	}
}

// GetCheckpointData fetches and validates bootstrap data belonging to the given checkpoint.
func (api *BeaconLightApi) GetCheckpointData(checkpointHash common.Hash) (*light.CheckpointData, error) {
	resp, err := api.httpGetf("/eth/v1/beacon/light_client/bootstrap/0x%x", checkpointHash[:])
	if err != nil {
		return nil, err
	}

	// See data structure definition here:
	// https://github.com/ethereum/consensus-specs/blob/dev/specs/altair/light-client/sync-protocol.md#lightclientbootstrap
	type bootstrapData struct {
		Data struct {
			Header          types.JsonBeaconHeader     `json:"header"`
			Committee       *types.SerializedCommittee `json:"current_sync_committee"`
			CommitteeBranch merkle.Values              `json:"current_sync_committee_branch"`
		} `json:"data"`
	}

	var data bootstrapData
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, err
	}
	header := data.Data.Header.Beacon
	if header.Hash() != checkpointHash {
		return nil, errors.New("invalid checkpoint block header")
	}
	checkpoint := &light.CheckpointData{
		Header:          header,
		CommitteeBranch: data.Data.CommitteeBranch,
		CommitteeRoot:   data.Data.Committee.Root(),
		Committee:       data.Data.Committee,
	}
	if !checkpoint.Validate() {
		return nil, errors.New("invalid sync committee Merkle proof")
	}
	return checkpoint, nil
}

// GetExecutionPayload fetches the execution block belonging to the beacon block
// specified by beaconRoot and validates its block hash against the expected execRoot.
func (api *BeaconLightApi) GetExecutionPayload(header types.Header) (*ctypes.Block, error) {
	resp, err := api.httpGetf("/eth/v2/beacon/blocks/0x%x", header.Hash())
	if err != nil {
		return nil, err
	}

	spec := configs.Mainnet
	// note: eth2 api endpoints serve bellatrix.SignedBeaconBlock instead
	// also try  github.com/protolambda/eth2api for api bindings
	//var beaconBlock bellatrix.BeaconBlock
	var beaconBlock capella.BeaconBlock
	myJSONBlockData := resp
	var beaconBlockMessage struct {
		Data struct {
			Message capella.BeaconBlock `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(myJSONBlockData, &beaconBlockMessage); err != nil {
		return nil, fmt.Errorf("invalid block json data: %v", err)
	}
	beaconBlock = beaconBlockMessage.Data.Message
	beaconBodyRoot := common.Hash(beaconBlock.Body.HashTreeRoot(spec, tree.GetHashFn()))
	if beaconBodyRoot != header.BodyRoot {
		return nil, fmt.Errorf("Beacon body root hash mismatch (expected: %x, got: %x)", header.BodyRoot.Bytes(), beaconBodyRoot.Bytes())
	}

	payload := &beaconBlock.Body.ExecutionPayload
	txs := make([]*ctypes.Transaction, len(payload.Transactions))
	for i, opaqueTx := range payload.Transactions {
		var tx ctypes.Transaction
		if err := tx.UnmarshalBinary(opaqueTx); err != nil {
			return nil, fmt.Errorf("failed to parse tx %d: %v", i, err)
		}
		txs[i] = &tx
	}
	withdrawals := make([]*ctypes.Withdrawal, len(payload.Withdrawals))
	for i, w := range payload.Withdrawals {
		withdrawals[i] = &ctypes.Withdrawal{
			Index:     uint64(w.Index),
			Validator: uint64(w.ValidatorIndex),
			Address:   common.Address(w.Address),
			Amount:    uint64(w.Amount),
		}
	}
	wroot := ctypes.DeriveSha(ctypes.Withdrawals(withdrawals), trie.NewStackTrie(nil))
	execHeader := &ctypes.Header{
		ParentHash:      common.Hash(payload.ParentHash),
		UncleHash:       ctypes.EmptyUncleHash,
		Coinbase:        common.Address(payload.FeeRecipient),
		Root:            common.Hash(payload.StateRoot),
		TxHash:          ctypes.DeriveSha(ctypes.Transactions(txs), trie.NewStackTrie(nil)),
		ReceiptHash:     common.Hash(payload.ReceiptsRoot),
		Bloom:           ctypes.Bloom(payload.LogsBloom),
		Difficulty:      big.NewInt(0), // constant
		Number:          new(big.Int).SetUint64(uint64(payload.BlockNumber)),
		GasLimit:        uint64(payload.GasLimit),
		GasUsed:         uint64(payload.GasUsed),
		Time:            uint64(payload.Timestamp),
		Extra:           []byte(payload.ExtraData),
		MixDigest:       common.Hash(payload.PrevRandao), // reused in merge
		Nonce:           ctypes.BlockNonce{},             // zero
		BaseFee:         (*uint256.Int)(&payload.BaseFeePerGas).ToBig(),
		WithdrawalsHash: &wroot,
	}
	execBlock := ctypes.NewBlockWithHeader(execHeader).WithBody(txs, nil).WithWithdrawals(withdrawals)
	if execBlock.Hash() != common.Hash(payload.BlockHash) {
		return nil, fmt.Errorf("Sanity check failed, payload hash does not match.")
	}
	return execBlock, nil
}

func decodeHeadEvent(enc []byte) (uint64, common.Hash, error) {
	var data struct {
		Slot  common.Decimal `json:"slot"`
		Block common.Hash    `json:"block"`
	}
	if err := json.Unmarshal(enc, &data); err != nil {
		return 0, common.Hash{}, err
	}
	return uint64(data.Slot), data.Block, nil
}

// StartHeadListener creates an event subscription for heads and signed (optimistic)
// head updates and calls the specified callback functions when they are received.
// The callbacks are also called for the current head and optimistic head at startup.
// They are never called concurrently.
func (api *BeaconLightApi) StartHeadListener(headFn func(slot uint64, blockRoot common.Hash), signedFn func(head types.SignedHead), errFn func(err error)) func() {
	closeCh := make(chan struct{})   // initiate closing the stream
	closedCh := make(chan struct{})  // stream closed (or failed to create)
	stoppedCh := make(chan struct{}) // sync loop stopped
	streamCh := make(chan *eventsource.Stream, 1)
	go func() {
		defer close(closedCh)
		// when connected to a Lodestar node the subscription blocks until the
		// first actual event arrives; therefore we create the subscription in
		// a separate goroutine while letting the main goroutine sync up to the
		// current head
		stream, err := eventsource.Subscribe(api.url+"/eth/v1/events?topics=head&topics=light_client_optimistic_update", "")
		if err != nil {
			errFn(fmt.Errorf("Error creating event subscription: %v", err))
			close(streamCh)
			return
		}
		streamCh <- stream
		<-closeCh
		stream.Close()
	}()
	go func() {
		defer close(stoppedCh)

		if head, err := api.GetHeader(common.Hash{}); err == nil {
			headFn(head.Slot, head.Hash())
		}
		if signedHead, err := api.GetOptimisticHeadUpdate(); err == nil {
			signedFn(signedHead)
		}
		stream := <-streamCh
		if stream == nil {
			return
		}
		for {
			select {
			case event, ok := <-stream.Events:
				if !ok {
					break
				}
				switch event.Event() {
				case "head":
					if slot, blockRoot, err := decodeHeadEvent([]byte(event.Data())); err == nil {
						headFn(slot, blockRoot)
					} else {
						errFn(fmt.Errorf("Error decoding head event: %v", err))
					}
				case "light_client_optimistic_update":
					if signedHead, err := decodeOptimisticHeadUpdate([]byte(event.Data())); err == nil {
						signedFn(signedHead)
					} else {
						errFn(fmt.Errorf("Error decoding optimistic update event: %v", err))
					}
				default:
					errFn(fmt.Errorf("Unexpected event: %s", event.Event()))
				}
			case err, ok := <-stream.Errors:
				if !ok {
					break
				}
				errFn(err)
			}
		}
	}()
	return func() {
		close(closeCh)
		<-closedCh
		<-stoppedCh
	}
}
