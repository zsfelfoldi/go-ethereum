// Copyright 2015 The go-ethereum Authors
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

// Package les implements the Light Ethereum Subprotocol.
package les

import (
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/p2p"
	rpc "github.com/ethereum/go-ethereum/rpc"
)

type LesServer struct {
	protocolManager *ProtocolManager
}

func NewLesServer(eth *eth.FullEthereum, config *eth.Config) (*LesServer, error) {
	pm, err := NewProtocolManager(false, config.NetworkId, eth.EventMux(), eth.Pow(), eth.BlockChain(), eth.ChainDb(), nil)
	if err != nil {
		return nil, err
	}
	return &LesServer{protocolManager: pm}, nil
}

// APIs returns the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *LesServer) APIs() []rpc.API {
	return []rpc.API{}
}

// Protocols implements node.Service, returning all the currently configured
// network protocols to start.
func (s *LesServer) Protocols() []p2p.Protocol {
	return s.protocolManager.SubProtocols
}

// Start implements node.Service, starting all internal goroutines needed by the
// Ethereum protocol implementation.
func (s *LesServer) Start(*p2p.Server) error {
	return nil
}

func (s *LesServer) StartPM() {
	s.protocolManager.Start()
}

// Stop implements node.Service, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *LesServer) Stop() error {
	return nil
}

func (s *LesServer) StopPM() {
	s.protocolManager.Stop()
}
