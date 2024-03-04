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
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package api

import (
	"github.com/ethereum/go-ethereum/beacon/light/request"
	"github.com/ethereum/go-ethereum/beacon/light/sync"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

// ApiServer is a wrapper around BeaconLightApi that implements request.requestServer.
type ApiServer struct {
	api           *BeaconLightApi
	eventCallback func(event request.Event)
	unsubscribe   func()
	lastId        uint64
}

// NewApiServer creates a new ApiServer.
func NewApiServer(api *BeaconLightApi) *ApiServer {
	return &ApiServer{api: api}
}

// Subscribe implements request.requestServer.
func (s *ApiServer) Subscribe(eventCallback func(event request.Event)) {
	s.eventCallback = eventCallback
	listener := HeadEventListener{
		OnNewHead: func(slot uint64, blockRoot common.Hash) {
			log.Debug("New head received", "slot", slot, "blockRoot", blockRoot)
			eventCallback(request.Event{Type: sync.EvNewHead, Data: types.HeadInfo{Slot: slot, BlockRoot: blockRoot}})
		},
		OnSignedHead: func(head types.SignedHeader) {
			log.Debug("New signed head received", "slot", head.Header.Slot, "blockRoot", head.Header.Hash(), "signerCount", head.Signature.SignerCount())
			eventCallback(request.Event{Type: sync.EvNewSignedHead, Data: head})
		},
		OnFinality: func(head types.FinalityUpdate) {
			log.Debug("New finality update received", "slot", head.Attested.Slot, "blockRoot", head.Attested.Hash(), "signerCount", head.Signature.SignerCount())
			eventCallback(request.Event{Type: sync.EvNewFinalityUpdate, Data: head})
		},
		OnError: func(err error) {
			log.Warn("Head event stream error", "err", err)
		},
	}
	s.unsubscribe = s.api.StartHeadListener(listener)
}

// SendRequest implements request.requestServer.
func (s *ApiServer) SendRequest(id request.ID, req request.Request) {
	go func() {
		var resp request.Response
		switch data := req.(type) {
		case sync.ReqUpdates:
			log.Debug("Requesting light client update", "period", data.FirstPeriod, "count", data.Count)
			if updates, committees, err := s.api.GetBestUpdatesAndCommittees(data.FirstPeriod, data.Count); err == nil {
				resp = sync.RespUpdates{Updates: updates, Committees: committees}
			}
		case sync.ReqHeader:
			log.Debug("Requesting beacon header", "hash", common.Hash(data))
			if header, err := s.api.GetHeader(common.Hash(data)); err == nil {
				resp = header
			}
		case sync.ReqCheckpointData:
			log.Debug("Requesting beacon checkpoint data", "hash", common.Hash(data))
			if bootstrap, err := s.api.GetCheckpointData(common.Hash(data)); err == nil {
				resp = bootstrap
			}
		case sync.ReqBeaconBlock:
			log.Debug("Requesting beacon block", "hash", common.Hash(data))
			if block, err := s.api.GetBeaconBlock(common.Hash(data)); err == nil {
				resp = block
			}
		default:
		}
		if resp != nil {
			s.eventCallback(request.Event{Type: request.EvResponse, Data: request.RequestResponse{ID: id, Request: req, Response: resp}})
		} else {
			s.eventCallback(request.Event{Type: request.EvFail, Data: request.RequestResponse{ID: id, Request: req}})
		}
	}()
}

// Unsubscribe implements request.requestServer.
// Note: Unsubscribe should not be called concurrently with Subscribe.
func (s *ApiServer) Unsubscribe() {
	if s.unsubscribe != nil {
		s.unsubscribe()
		s.unsubscribe = nil
	}
}
