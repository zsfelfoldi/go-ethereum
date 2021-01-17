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
	"net"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/les/lespay"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

type (
	// Server serves lespay requests
	Server struct {
		limiter         *utils.Limiter
		services        map[string]*serviceEntry
		delayPerRequest time.Duration
	}

	// Service is a service registered at the Server and identified by a string id
	Service interface {
		ServiceInfo() (id, desc string)                                      // only called during registration
		Handle(id enode.ID, address string, name string, data []byte) []byte // never called concurrently
	}

	serviceEntry struct {
		id, desc string
		backend  Service
	}
)

// NewServer creates a new Server
func NewServer(delayPerRequest time.Duration) *Server {
	return &Server{
		limiter:         utils.NewLimiter(1000),
		delayPerRequest: delayPerRequest,
		services:        make(map[string]*serviceEntry),
	}
}

// Register registers a Service
func (s *Server) Register(b Service) {
	srv := &serviceEntry{backend: b}
	srv.id, srv.desc = b.ServiceInfo()
	if strings.Contains(srv.id, ":") {
		// srv.id + ":" will be used as a service database prefix
		panic("Service ID contains ':'")
	}
	s.services[srv.id] = srv
}

// Serve serves a lespay request batch
// Note: requests are served by the Handle functions of the registered services. Serve
// may be called concurrently but the Handle functions are called sequentially and
// therefore thread safety is guaranteed.
func (s *Server) Serve(id enode.ID, address string, requests lespay.Requests) lespay.Replies {
	if len(requests) == 0 || len(requests) > lespay.MaxRequestLength {
		return nil
	}
	reqLen := uint(len(requests))
	if reqLen == 0 {
		return nil
	}
	// Note: the value parameter will be supplied by the token sale module (total amount paid)
	ch := <-s.limiter.Add(id, address, 0, reqLen)
	if ch == nil {
		return nil
	}
	// Note: the following section is protected from concurrency by the limiter
	results := make(lespay.Replies, len(requests))
	for i, req := range requests {
		if service := s.services[req.Service]; service != nil {
			results[i] = service.backend.Handle(id, address, req.Name, req.Params)
		}
	}
	time.Sleep(s.delayPerRequest * time.Duration(reqLen))
	// The protected section ends by closing the channel and thereby allowing the limiter to start the next request
	close(ch)
	return results
}

// ServeEncoded serves an encoded lespay request batch and returns the encoded replies
func (s *Server) ServeEncoded(id enode.ID, addr *net.UDPAddr, req []byte) []byte {
	var requests lespay.Requests
	if err := rlp.DecodeBytes(req, &requests); err != nil {
		return nil
	}
	results := s.Serve(id, addr.String(), requests)
	if results == nil {
		return nil
	}
	res, _ := rlp.EncodeToBytes(&results)
	return res
}

// Stop shuts down the server
func (s *Server) Stop() {
	s.limiter.Stop()
}
