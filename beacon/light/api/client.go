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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/donovanhide/eventsource"
	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/protolambda/zrnt/eth2/beacon/capella"
	"github.com/protolambda/zrnt/eth2/configs"
	"github.com/protolambda/ztyp/tree"
)

var (
	ErrNotFound = errors.New("404 Not Found")
	ErrInternal = errors.New("500 Internal Server Error")
)

// fetcher is an interface useful for debug-harnessing the http api.
type fetcher interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client requests light client information from a beacon node REST API.
// Note: all required API endpoints are currently only implemented by Lodestar.
type Client struct {
	url           string
	client        fetcher
	customHeaders map[string]string
}

func NewClient(url string, customHeaders map[string]string) *Client {
	return &Client{
		url: url,
		client: &http.Client{
			Timeout: time.Second * 10,
		},
		customHeaders: customHeaders,
	}
}

func (api *Client) httpGet(path string) ([]byte, error) {
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

func (api *Client) httpGetf(format string, params ...any) ([]byte, error) {
	return api.httpGet(fmt.Sprintf(format, params...))
}

// GetBestUpdateAndCommittee fetches and validates LightClientUpdate for given
// period and full serialized committee for the next period (committee root hash
// equals update.NextSyncCommitteeRoot).
// Note that the results are validated but the update signature should be verified
// by the caller as its validity depends on the update chain.
// TODO handle valid partial results
func (api *Client) GetBestUpdatesAndCommittees(firstPeriod, count uint64) ([]*types.LightClientUpdate, []*types.SerializedSyncCommittee, error) {
	resp, err := api.httpGetf(urlUpdates+"?start_period=%d&count=%d", firstPeriod, count)
	if err != nil {
		return nil, nil, err
	}

	var data []CommitteeUpdate
	if err := json.Unmarshal(resp, &data); err != nil {
		return nil, nil, err
	}
	if len(data) != int(count) {
		return nil, nil, errors.New("invalid number of committee updates")
	}
	updates := make([]*types.LightClientUpdate, int(count))
	committees := make([]*types.SerializedSyncCommittee, int(count))
	for i, d := range data {
		if d.Update.AttestedHeader.Header.SyncPeriod() != firstPeriod+uint64(i) {
			return nil, nil, errors.New("wrong committee update header period")
		}
		if err := d.Update.Validate(); err != nil {
			return nil, nil, err
		}
		if d.NextSyncCommittee.Root() != d.Update.NextSyncCommitteeRoot {
			return nil, nil, errors.New("wrong sync committee root")
		}
		updates[i], committees[i] = new(types.LightClientUpdate), new(types.SerializedSyncCommittee)
		*updates[i], *committees[i] = d.Update, d.NextSyncCommittee
	}
	return updates, committees, nil
}

// GetOptimisticHeadUpdate fetches a signed header based on the latest available
// optimistic update. Note that the signature should be verified by the caller
// as its validity depends on the update chain.
//
// See data structure definition here:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/altair/light-client/sync-protocol.md#lightclientoptimisticupdate
func (api *Client) GetOptimisticHeadUpdate() (types.SignedHeader, error) {
	resp, err := api.httpGet(urlOptimistic)
	if err != nil {
		return types.SignedHeader{}, err
	}
	return decodeOptimisticHeadUpdate(resp)
}

// GetHeader fetches and validates the beacon header with the given blockRoot.
// If blockRoot is null hash then the latest head header is fetched.
func (api *Client) GetHeader(blockRoot common.Hash) (types.Header, error) {
	var blockId string
	if blockRoot == (common.Hash{}) {
		blockId = "head"
	} else {
		blockId = blockRoot.Hex()
	}
	resp, err := api.httpGetf(urlHeaders+"/%s", blockId)
	if err != nil {
		return types.Header{}, err
	}

	var data jsonHeaderData
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

func (api *Client) GetStateProof(stateRoot common.Hash, format merkle.CompactProofFormat) (merkle.MultiProof, error) {
	proof, err := api.getStateProof(stateRoot.Hex(), format)
	if err != nil {
		return merkle.MultiProof{}, err
	}
	if proof.RootHash() != stateRoot {
		return merkle.MultiProof{}, errors.New("Received proof has incorrect state root")
	}
	return proof, nil
}

func (api *Client) getStateProof(stateId string, format merkle.CompactProofFormat) (merkle.MultiProof, error) {
	resp, err := api.httpGetf(urlStateProof+"/%s?format=0x%x", stateId, format.Encode())
	if err != nil {
		return merkle.MultiProof{}, err
	}
	valueCount := format.ValueCount()
	if len(resp) != valueCount*32 {
		return merkle.MultiProof{}, errors.New("Invalid state proof length")
	}
	values := make(merkle.Values, valueCount)
	for i := range values {
		copy(values[i][:], resp[i*32:(i+1)*32])
	}
	return merkle.MultiProof{Format: format, Values: values}, nil
}

// GetCheckpointData fetches and validates bootstrap data belonging to the given checkpoint.
func (api *Client) GetCheckpointData(checkpointHash common.Hash) (*light.CheckpointData, error) {
	fmt.Println("GetCheckpointData", checkpointHash)
	resp, err := api.httpGetf(urlBootstrap+"/0x%x", checkpointHash[:])
	if err != nil {
		fmt.Println("http", err)
		return nil, err
	}

	// See data structure definition here:
	// https://github.com/ethereum/consensus-specs/blob/dev/specs/altair/light-client/sync-protocol.md#lightclientbootstrap
	type bootstrapData jsonBootstrapData

	var data bootstrapData
	if err := json.Unmarshal(resp, &data); err != nil {
		fmt.Println("json", err)
		return nil, err
	}
	if data.Data.Committee == nil {
		fmt.Println("committee nil")
		return nil, errors.New("sync committee is missing")
	}
	header := data.Data.Header.Beacon
	if header.Hash() != checkpointHash {
		fmt.Println("invalid hash")
		return nil, fmt.Errorf("invalid checkpoint block header, have %v want %v", header.Hash(), checkpointHash)
	}
	checkpoint := &light.CheckpointData{
		Header:          header,
		CommitteeBranch: data.Data.CommitteeBranch,
		CommitteeRoot:   data.Data.Committee.Root(),
		Committee:       data.Data.Committee,
	}
	if err := checkpoint.Validate(); err != nil {
		fmt.Println("invalid proof", err)
		return nil, fmt.Errorf("invalid sync committee Merkle proof: %w", err)
	}
	fmt.Println("success")
	return checkpoint, nil
}

func (api *Client) GetBeaconBlock(blockRoot common.Hash) (*capella.BeaconBlock, error) {
	resp, err := api.httpGetf(urlBlocks+"/0x%x", blockRoot)
	if err != nil {
		return nil, err
	}

	var beaconBlockMessage jsonBeaconBlock
	if err := json.Unmarshal(resp, &beaconBlockMessage); err != nil {
		return nil, fmt.Errorf("invalid block json data: %v", err)
	}
	beaconBlock := new(capella.BeaconBlock)
	*beaconBlock = beaconBlockMessage.Data.Message
	root := common.Hash(beaconBlock.HashTreeRoot(configs.Mainnet, tree.GetHashFn()))
	if root != blockRoot {
		return nil, fmt.Errorf("Beacon block root hash mismatch (expected: %x, got: %x)", blockRoot, root)
	}
	return beaconBlock, nil
}

func decodeHeadEvent(enc []byte) (uint64, common.Hash, error) {
	var data jsonHeadEvent
	if err := json.Unmarshal(enc, &data); err != nil {
		return 0, common.Hash{}, err
	}
	return uint64(data.Slot), data.Block, nil
}

// StartHeadListener creates an event subscription for heads and signed (optimistic)
// head updates and calls the specified callback functions when they are received.
// The callbacks are also called for the current head and optimistic head at startup.
// They are never called concurrently.
func (api *Client) StartHeadListener(headFn func(slot uint64, blockRoot common.Hash), signedFn func(head types.SignedHeader), errFn func(err error)) func() {
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
		req, err := http.NewRequest("GET", api.url+urlEvents+"?topics=head&topics=light_client_optimistic_update", nil)
		if err != nil {
			errFn(fmt.Errorf("Error creating event subscription request: %v", err))
			return
		}
		for k, v := range api.customHeaders {
			req.Header.Set(k, v)
		}
		stream, err := eventsource.SubscribeWithRequest("", req)
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

		fmt.Println("get head info")
		if head, err := api.GetHeader(common.Hash{}); err == nil {
			fmt.Println("head: success")
			headFn(head.Slot, head.Hash())
		} else {
			fmt.Println("head: error", err)
		}
		if signedHead, err := api.GetOptimisticHeadUpdate(); err == nil {
			fmt.Println("signedHead: success")
			signedFn(signedHead)
		} else {
			fmt.Println("signedHead: error", err)
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
					fmt.Println("head event")
					if slot, blockRoot, err := decodeHeadEvent([]byte(event.Data())); err == nil {
						headFn(slot, blockRoot)
						fmt.Println(" slot", slot)
					} else {
						errFn(fmt.Errorf("Error decoding head event: %v", err))
					}
				case "light_client_optimistic_update":
					fmt.Println("signed head event")
					if signedHead, err := decodeOptimisticHeadUpdate([]byte(event.Data())); err == nil {
						fmt.Println(" slot", signedHead.Header.Slot)
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
