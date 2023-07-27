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
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/donovanhide/eventsource"
	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/light/request"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/log"
	"github.com/protolambda/zrnt/eth2/beacon/capella"
)

type Server struct {
	CheckpointStore *light.CheckpointStore
	CommitteeChain  *light.CommitteeChain
	LightChain      *light.LightChain
	RecentBlocks    *lru.Cache[common.Hash, *capella.BeaconBlock]
	HeadTracker     *request.HeadTracker

	eventServer *eventsource.Server
	lastEventId uint64
}

func (s *Server) RegisterAt(mux *http.ServeMux) {
	mux.HandleFunc(urlUpdates, s.handleUpdates)
	mux.HandleFunc(urlOptimistic, s.handleOptimisticHeadUpdate)
	mux.HandleFunc(urlHeaders+"/", s.handleHeaders)
	//mux.HandleFunc(urlStateProof+"/", s.handleStateProof)
	mux.HandleFunc(urlBootstrap+"/", s.handleBootstrap)
	mux.HandleFunc(urlBlocks+"/", s.handleBlocks)
	s.eventServer = eventsource.NewServer()
	s.eventServer.Register("headEvent", eventsource.NewSliceRepository())
	mux.HandleFunc(urlEvents, s.eventServer.Handler("headEvent"))
}

func (s *Server) PublishHeadEvent(slot uint64, blockRoot common.Hash) {
	enc, err := json.Marshal(&jsonHeadEvent{Slot: common.Decimal(slot), Block: blockRoot})
	if err != nil {
		log.Error("Error encoding head event", "error", err)
		return
	}
	s.publishEvent("head", string(enc))
}

func (s *Server) PublishOptimisticHeadUpdate(head types.SignedHeader) {
	enc, err := encodeOptimisticHeadUpdate(head)
	if err != nil {
		log.Error("Error encoding optimistic head update", "error", err)
		return
	}
	s.publishEvent("light_client_optimistic_update", string(enc))
}

type serverEvent struct {
	id, event, data string
}

func (e *serverEvent) Id() string    { return e.id }
func (e *serverEvent) Event() string { return e.event }
func (e *serverEvent) Data() string  { return e.data }

func (s *Server) publishEvent(event, data string) {
	id := atomic.AddUint64(&s.lastEventId, 1)
	s.eventServer.Publish([]string{"headEvent"}, &serverEvent{
		id:    strconv.FormatUint(id, 10),
		event: event,
		data:  data,
	})
}

func (s *Server) handleBootstrap(resp http.ResponseWriter, req *http.Request) {
	fmt.Println("handleBootstrap")
	fmt.Println(" path", req.URL.Path)
	var checkpointHash common.Hash
	if data, err := hexutil.Decode(req.URL.Path[len(urlBootstrap)+1:]); err == nil && len(data) == len(checkpointHash) {
		copy(checkpointHash[:], data)
	} else {
		fmt.Println(2)
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	fmt.Println(" hash", checkpointHash)
	checkpoint := s.CheckpointStore.Get(checkpointHash)
	if checkpoint == nil {
		fmt.Println(3)
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	var bootstrapData jsonBootstrapData
	bootstrapData.Data.Header.Beacon = checkpoint.Header
	bootstrapData.Data.CommitteeBranch = checkpoint.CommitteeBranch
	bootstrapData.Data.Committee = checkpoint.Committee
	respData, err := json.Marshal(&bootstrapData)
	if err != nil {
		fmt.Println(4, err)
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp.Write(respData)
}

func (s *Server) handleUpdates(resp http.ResponseWriter, req *http.Request) {
	fmt.Println("handleUpdates")
	fmt.Println(" path", req.URL.Path)
	startStr, countStr := req.URL.Query().Get("start_period"), req.URL.Query().Get("count")
	start, err := strconv.ParseUint(startStr, 10, 64)
	if err != nil {
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	var count uint64
	if countStr != "" {
		count, err = strconv.ParseUint(countStr, 10, 64)
		if err != nil {
			resp.WriteHeader(http.StatusBadRequest)
			return
		}
	} else {
		count = 1
	}
	fmt.Println("start / count", start, count)

	var updates []CommitteeUpdate
	for period := start; period < start+count; period++ {
		update := s.CommitteeChain.GetUpdate(period)
		if update == nil {
			continue
		}
		committee := s.CommitteeChain.GetCommittee(period + 1)
		if committee == nil {
			continue
		}
		updates = append(updates, CommitteeUpdate{
			Version:           "qwerty", //TODO
			Update:            *update,
			NextSyncCommittee: *committee,
		})
	}
	respData, err := json.Marshal(&updates)
	if err != nil {
		fmt.Println(4, err)
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp.Write(respData)
}

func (s *Server) handleBlocks(resp http.ResponseWriter, req *http.Request) {
	fmt.Println("handleBlocks", req.URL.Path)
	var blockRoot common.Hash //TODO ??number
	if data, err := hexutil.Decode(req.URL.Path[len(urlBlocks)+1:]); err == nil && len(data) == len(blockRoot) {
		copy(blockRoot[:], data)
	} else {
		fmt.Println("handleBlocks: bad request")
		resp.WriteHeader(http.StatusBadRequest)
		return
	}
	fmt.Println("handleBlocks hash:", blockRoot)
	if block, ok := s.RecentBlocks.Get(blockRoot); ok {
		var blockData jsonBeaconBlock
		blockData.Data.Message = *block
		respData, err := json.Marshal(&blockData)
		if err != nil {
			fmt.Println("handleBlocks encode failed:", err)
			resp.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp.Write(respData)
		fmt.Println("handleBlocks: success")
		return
	}
	fmt.Println("handleBlocks: not found")
	resp.WriteHeader(http.StatusNotFound)
}

func (s *Server) handleOptimisticHeadUpdate(resp http.ResponseWriter, req *http.Request) {
	head := s.HeadTracker.ValidatedHead()
	if head.Header == (types.Header{}) {
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	respData, err := encodeOptimisticHeadUpdate(head)
	if err != nil {
		fmt.Println("handleOptimisticHeadUpdate encode failed:", err)
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp.Write(respData)
	fmt.Println("handleOptimisticHeadUpdate: success")
}

func (s *Server) handleHeaders(resp http.ResponseWriter, req *http.Request) {
	fmt.Println("handleHeaders", req.URL.Path)
	blockId := req.URL.Path[len(urlHeaders)+1:]
	var (
		header types.Header
		found  bool
	)
	if blockId == "head" {
		header, _, found = s.LightChain.HeaderRange()
	} else {
		var blockRoot common.Hash //TODO ??number
		if data, err := hexutil.Decode(blockId); err == nil && len(data) == len(blockRoot) {
			copy(blockRoot[:], data)
		} else {
			fmt.Println("handleHeaders: bad request")
			resp.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Println("handleHeaders hash:", blockRoot)
		var err error
		header, err = s.LightChain.GetHeaderByHash(blockRoot)
		found = err == nil
	}
	if !found {
		fmt.Println("handleHeaders: not found")
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	var headerData jsonHeaderData
	headerData.Data.Canonical = true
	headerData.Data.Header.Message = header //TODO signature?
	headerData.Data.Root = header.Hash()
	respData, err := json.Marshal(&headerData)
	if err != nil {
		fmt.Println("handleHeaders encode failed:", err)
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp.Write(respData)
	fmt.Println("handleHeaders: success")
}

func (s *Server) handleStateProof(resp http.ResponseWriter, req *http.Request) {}
