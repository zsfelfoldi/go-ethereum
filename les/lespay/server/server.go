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

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	maxCommandLength = 16
	costCutRatio     = 0.1
)

type Server struct {
	limiter        *utils.Limiter
	handlers       map[string]Handler
	sizeCostFactor float64
}

type Handler func(id enode.ID, address string, data []byte) []byte

func NewServer(maxThreadTime, maxBandwidth float64) *Server {
	costFilter := utils.NewCostFilter(costCutRatio, mclock.System{})
	return &Server{
		limiter:        utils.NewLimiter((1/maxThreadTime-1)/(1-costCutRatio), 1000, costFilter, mclock.System{}),
		sizeCostFactor: maxThreadTime * 1000000000 / maxBandwidth,
	}
}

// Note: register every handler before serving requests
func (s *Server) RegisterHandler(name string, handler Handler) {
	s.handlers[name] = handler
}

func (s *Server) Serve(id enode.ID, addr *net.UDPAddr, req []byte) []byte {
	type command struct {
		Name   string
		Params []byte
	}
	var commands []command
	if err := rlp.DecodeBytes(req, &commands); err != nil || len(commands) == 0 || len(commands) > maxCommandLength {
		return nil
	}
	address := addr.String()
	ch := <-s.limiter.Add(id, address, 0, uint64(len(commands)))
	if ch == nil {
		return nil
	}
	start := mclock.Now()
	results := make([][]byte, len(commands))
	for i, cmd := range commands {
		if handler, ok := s.handlers[cmd.Name]; ok {
			results[i] = handler(id, address, cmd.Params)
		}
	}
	res, err := rlp.EncodeToBytes(&results)
	sizeCost := float64(len(res)+100) * s.sizeCostFactor
	cost := float64(mclock.Now() - start)
	if sizeCost > cost {
		cost = sizeCost
	}
	ch <- cost
	if err != nil {
		return nil
	}
	return res
}

func (s *Server) Stop() {
	s.limiter.Stop()
}
