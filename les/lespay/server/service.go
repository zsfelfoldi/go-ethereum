// Copyright 2020 The go-ethereum Authors
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

package server

import (
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/lespay"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	maxRequestLength = 16
	costCutRatio     = 0.1
)

type (
	Server struct {
		ns                          *nodestate.NodeStateMachine
		db                          ethdb.Database
		costFilter                  *utils.CostFilter
		limiter                     *utils.Limiter
		services, alias             map[string]*serviceEntry
		priority                    []*serviceEntry
		sleepFactor, sizeCostFactor float64

		opService *serviceEntry
		opBatch   ethdb.Batch
	}

	Service interface {
		ServiceInfo() (string, string, lespay.ServiceRange) // only called during registration
		Distance() uint64
		Handle(id enode.ID, address string, name string, data []byte) []byte
	}

	ServiceWithBalance interface {
		Service
		GetBalance(id enode.ID) int64
		AddBalance(id enode.ID, amount int64) (int64, int64, error)
	}

	PaymentService interface {
		Service
		CurrencyId() string
		GetBalance(id enode.ID) *big.Int
		AddBalance(id enode.ID, amount *big.Int) (*big.Int, *big.Int, error)
	}

	serviceEntry struct {
		id, desc     string
		serviceRange lespay.ServiceRange
		backend      Service
	}

	DbAccess struct {
		server  *Server
		service *serviceEntry
		prefix  []byte
	}
)

func NewServer(ns *nodestate.NodeStateMachine, db ethdb.Database, maxThreadTime, maxBandwidth float64) *Server {
	s := &Server{
		ns:             ns,
		db:             db,
		costFilter:     utils.NewCostFilter(costCutRatio, 0.01),
		limiter:        utils.NewLimiter(1000),
		sleepFactor:    (1/maxThreadTime - 1) / (1 - costCutRatio),
		sizeCostFactor: maxThreadTime * 1000000000 / maxBandwidth,
		services:       make(map[string]*serviceEntry),
	}
	s.Register(s)
	return s
}

func (s *Server) Register(b Service) *DbAccess {
	srv := &serviceEntry{backend: b}
	srv.id, srv.desc, srv.serviceRange = b.ServiceInfo()
	if strings.Contains(srv.id, ":") {
		panic("Service ID contains ':'")
	}
	s.services[srv.id] = srv
	s.priority = append(s.priority, srv)
	return &DbAccess{
		server:  s,
		service: srv,
		prefix:  append([]byte(srv.id), byte(':')),
	}
}

func (s *Server) Resolve(serviceID string) Service {
	var srv *serviceEntry
	if len(serviceID) > 0 && serviceID[0] == ':' {
		// service alias
		srv = s.alias[serviceID[1:]]
	} else {
		srv = s.services[serviceID]
	}
	if srv != nil {
		return srv.backend
	}
	return nil
}

func (s *Server) Serve(id enode.ID, addr *net.UDPAddr, req []byte) []byte {
	var requests lespay.Requests
	if err := rlp.DecodeBytes(req, &requests); err != nil || len(requests) == 0 || len(requests) > maxRequestLength {
		return nil
	}
	priorWeight := uint64(len(requests))
	if priorWeight == 0 {
		return nil
	}
	address := addr.String()
	ch := <-s.limiter.Add(id, address, 0, priorWeight)
	if ch == nil {
		return nil
	}
	start := mclock.Now()
	results := make([][]byte, len(requests))
	s.alias = make(map[string]*serviceEntry)
	for i, req := range requests {
		if service := s.Resolve(req.Service); service != nil {
			results[i] = service.Handle(id, address, req.Name, req.Params)
		}
	}
	s.alias = nil
	res, err := rlp.EncodeToBytes(&results)
	cost := float64(mclock.Now() - start)
	sizeCost := float64(len(res)+100) * s.sizeCostFactor
	if sizeCost > cost {
		cost = sizeCost
	}
	fWeight := float64(priorWeight) / maxRequestLength
	filteredCost, limit := s.costFilter.Filter(cost, fWeight)
	time.Sleep(time.Duration(filteredCost * s.sleepFactor))
	if limit*fWeight <= filteredCost {
		ch <- fWeight
	} else {
		ch <- filteredCost / limit
	}
	if err != nil {
		return nil
	}
	return res
}

func (s *Server) Stop() {
	s.limiter.Stop()
}

func (s *Server) ServiceInfo() (string, string, lespay.ServiceRange) {
	return "sm", "Service map", nil
}

func (s *Server) Distance() uint64 {
	return 0
}

func (s *Server) Handle(id enode.ID, address string, name string, data []byte) []byte {
	switch name {
	case lespay.ServiceMapFilterName:
		return s.serveFilter(data)
	case lespay.ServiceMapQueryName:
		return s.serveQuery(data)
	}
	return nil
}

func (s *Server) serveFilter(data []byte) []byte {
	var req lespay.ServiceMapFilterReq
	if rlp.DecodeBytes(data, &req) != nil {
		return nil
	}
	var resp lespay.ServiceMapFilterResp
	for _, srv := range s.priority {
		if srv.serviceRange.Includes(req.FilterRange) {
			if resp == nil && req.SetAlias != "" {
				s.alias[req.SetAlias] = srv
			}
			resp = append(resp, srv.id)
		}
	}
	res, _ := rlp.EncodeToBytes(&resp)
	return res
}

func (s *Server) serveQuery(data []byte) []byte {
	var req lespay.ServiceMapQueryReq
	if rlp.DecodeBytes(data, &req) != nil {
		return nil
	}
	var service *serviceEntry
	if len(req) > 0 && req[0] == '$' {
		// service alias
		service = s.alias[string(req[1:])]
	} else {
		service = s.services[string(req)]
	}
	if service == nil {
		return nil
	}
	resp := lespay.ServiceMapQueryResp{
		Id:       service.id,
		Desc:     service.desc,
		Distance: service.backend.Distance(),
		Range:    service.serviceRange,
	}
	res, _ := rlp.EncodeToBytes(&resp)
	return res
}

func (d *DbAccess) Operation(fn func(), write bool) {
	d.server.opService = d.service
	if write {
		d.server.opBatch = d.server.db.NewBatch()
	}
	d.server.ns.Operation(fn)
	d.server.opService = nil
	if write {
		d.server.opBatch.Write()
		d.server.opBatch = nil
	}
}

func (d *DbAccess) SubOperation(srv *serviceEntry, fn func()) {
	if d.server.opService != d.service {
		panic("Database access not allowed")
	}
	d.server.opService = srv
	fn()
	d.server.opService = d.service
}

func (d *DbAccess) Has(key []byte) (bool, error) {
	if d.server.opService != d.service {
		panic("Database access not allowed")
	}
	return d.server.db.Has(append(d.prefix, key...))
}

func (d *DbAccess) Get(key []byte) ([]byte, error) {
	if d.server.opService != d.service {
		panic("Database access not allowed")
	}
	return d.server.db.Get(append(d.prefix, key...))
}

func (d *DbAccess) Put(key []byte, value []byte) error {
	if d.server.opService != d.service {
		panic("Database access not allowed")
	}
	return d.server.opBatch.Put(append(d.prefix, key...), value)
}

func (d *DbAccess) Delete(key []byte) error {
	if d.server.opService != d.service {
		panic("Database access not allowed")
	}
	return d.server.opBatch.Delete(append(d.prefix, key...))
}

func (d *DbAccess) NewIterator(prefix []byte, start []byte) ethdb.Iterator {
	if d.server.opService != d.service {
		panic("Database access not allowed")
	}
	return d.server.db.NewIterator(append(d.prefix, prefix...), start)
}
