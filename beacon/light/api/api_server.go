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
	"math"
	"time"

	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/light/request"
	"github.com/ethereum/go-ethereum/beacon/light/sync"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
	"github.com/protolambda/zrnt/eth2/beacon/capella"
)

type ApiServer struct {
	api           *BeaconLightApi
	eventCallback func(evType int, data interface{})
	unsubscribe   func()
}

func NewApiServer(api *BeaconLightApi) *ApiServer {
	return &ApiServer{api: api}
}

func (s *ApiServer) Subscribe(eventCallback func(evType int, data interface{})) {
	s.eventCallback = eventCallback
	s.unsubscribe = s.api.StartHeadListener(func(slot uint64, blockRoot common.Hash) {
		eventCallback(request.EvNewHead, request.HeadInfo{Slot: slot, BlockRoot: blockRoot})
	}, func(head types.SignedHeader) {
		eventCallback(request.EvNewSignedHead, head)
	}, func(err error) {
		log.Warn("Head event stream error", "err", err)
	})
}

func (s *ApiServer) SendRequest(request interface{}) (id interface{}) {
	id := 3333 //TODO
	go func() {
		switch data := request.(type) {
		case sync.ReqUpdates:
			if updates, committees, err := s.api.GetBestUpdatesAndCommittees(data.FirstPeriod, data.count); err == nil {
				s.eventCallback(request.EvValidResponse, request.Response{Id: id, RespData: sync.RespUpdates{Updates: updates, Committees: committees}})
				return
			}
		case sync.ReqOptimisticHead:
			if signedHead, err := s.api.GetOptimisticHeadUpdate(); err == nil {
				s.eventCallback(request.EvValidResponse, request.Response{Id: id, RespData: signedHead})
				return
			}
		case sync.ReqHeader:
			if header, err := s.api.GetHeader(data); err == nil {
				s.eventCallback(request.EvValidResponse, request.Response{Id: id, RespData: header})
				return
			}
		case sync.ReqCheckpointData:
			if bootstrap, err := s.api.GetCheckpointData(data); err == nil {
				s.eventCallback(request.EvValidResponse, request.Response{Id: id, RespData: bootstrap})
				return
			}
		case sync.ReqBeaconBlock:
			if block, err := s.api.GetBeaconBlock(data); err == nil {
				s.eventCallback(request.EvValidResponse, request.Response{Id: id, RespData: block})
				return
			}
		default:
		}
		s.eventCallback(request.EvInvalidResponse, id)
	}()
	return id
}

// Note: UnsubscribeHeads should not be called concurrently with SubscribeHeads
func (s *ApiServer) Unsubscribe() {
	if s.unsubscribe != nil {
		s.unsubscribe()
		s.unsubscribe = nil
	}
}
