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
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package les

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

const blipSoftTimeout = time.Second * 2

type Blip struct {
	handler      *handler
	peers        *peerSet
	fcManager    *flowcontrol.ClientManager
	costTracker  *costTracker
	defParams    flowcontrol.ServerParams
	servingQueue *servingQueue
	reqDist      *requestDistributor
	retriever    *retrieveManager
	odr          *LesOdr
	chainDb      ethdb.Database

	beaconNodeApi           *beaconNodeApiSource
	blockchain              *core.BlockChain
	beaconChain             *beacon.BeaconChain
	syncCommitteeCheckpoint *beacon.WeakSubjectivityCheckpoint
	syncCommitteeTracker    *beacon.SyncCommitteeTracker
}

type blipBackend interface {
	BlockChain() *core.BlockChain
	ChainDb() ethdb.Database
}

func NewBlip(node *node.Node, b blipBackend, config *ethconfig.Config) (*Blip, error) {
	forks, err := beacon.LoadForks(config.BeaconConfig)
	if err != nil {
		log.Error("Could not load beacon chain config file", "error", err)
		return nil, fmt.Errorf("Could not load beacon chain config file: %v", err)
	}
	blip := &Blip{
		chainDb:      b.ChainDb(),
		peers:        newPeerSet(),
		blockchain:   b.BlockChain(),
		fcManager:    flowcontrol.NewClientManager(nil, &mclock.System{}),
		servingQueue: newServingQueue(int64(time.Millisecond*10), float64(config.LightServ)/100),
	}

	blip.reqDist = newRequestDistributor(blip.peers, &mclock.System{})
	blip.retriever = newRetrieveManager(blip.peers, blip.reqDist, func() time.Duration { return blipSoftTimeout })
	blip.odr = NewLesOdr(blip.chainDb, light.DefaultClientIndexerConfig, blip.peers, blip.retriever)

	if config.BeaconApi != "" {
		blip.beaconNodeApi = &beaconNodeApiSource{url: config.BeaconApi}
		log.Info("Beacon sync: using beacon node API")
	} else {
		var beaconCheckpoint common.Hash
		if config.BeaconCheckpoint != "" {
			if c, err := hexutil.Decode(config.BeaconCheckpoint); err == nil && len(c) == 32 {
				copy(beaconCheckpoint[:len(c)], c)
				log.Info("Beacon sync: using specified weak subjectivity checkpoint")
			} else {
				log.Error("Beacon sync: invalid weak subjectivity checkpoint specified")
			}
		}
		blip.syncCommitteeCheckpoint = beacon.NewWeakSubjectivityCheckpoint(blip.chainDb, (*odrDataSource)(blip.odr), beaconCheckpoint, nil)
		if blip.syncCommitteeCheckpoint == nil {
			log.Error("Beacon sync: cannot sync without either a beacon node API or a weak subjectivity checkpoint")
			return nil, fmt.Errorf("No weak subjectivity checkpoint")
		}
		if beaconCheckpoint == (common.Hash{}) {
			log.Info("Beacon sync: using previously stored weak subjectivity checkpoint")
		}
	}

	if blip.beaconChain = beacon.NewBeaconChain(blip.beaconNodeApi, (*odrDataSource)(blip.odr), blip.blockchain, blip.chainDb, forks); blip.beaconChain == nil {
		return nil, fmt.Errorf("Could not initialize beacon chain")
	}
	blip.syncCommitteeTracker = beacon.NewSyncCommitteeTracker(blip.chainDb, forks, blip.beaconChain, &mclock.System{})
	blip.syncCommitteeTracker.SubscribeToNewHeads(blip.odr.SetBeaconHead)
	blip.syncCommitteeTracker.SubscribeToNewHeads(func(head beacon.Header) {
		blip.beaconChain.SyncToHead(head, nil)
	})
	blip.beaconChain.SubscribeToProcessedHeads(blip.syncCommitteeTracker.ProcessedBeaconHead, true)
	blip.beaconNodeApi.chain = blip.beaconChain
	blip.beaconNodeApi.sct = blip.syncCommitteeTracker
	blip.beaconNodeApi.start()

	blip.handler = newHandler(blip.peers, config.NetworkId)

	fcWrapper := &fcRequestWrapper{
		costTracker:  srv.costTracker,
		servingQueue: srv.servingQueue,
	}

	beaconServerHandler := &beaconServerHandler{
		syncCommitteeTracker: blip.syncCommitteeTracker,
		beaconChain:          blip.beaconChain,
		blockChain:           blip.blockchain,
		fcWrapper:            fcWrapper,
	}
	blip.handler.registerHandshakeModule(beaconServerHandler)
	blip.handler.registerConnectionModule(beaconServerHandler)
	blip.handler.registerMessageHandlers(beaconServerHandler.messageHandlers())

	beaconClientHandler := &beaconClientHandler{
		syncCommitteeTracker:    blip.syncCommitteeTracker,
		syncCommitteeCheckpoint: blip.syncCommitteeCheckpoint,
		retriever:               blip.retriever,
	}
	blip.handler.registerHandshakeModule(beaconClientHandler)
	blip.handler.registerConnectionModule(beaconClientHandler)
	blip.handler.registerMessageHandlers(beaconClientHandler.messageHandlers())

	node.RegisterProtocols(blip.Protocols())
	//node.RegisterAPIs(blip.APIs())
	node.RegisterLifecycle(blip)
	return blip, nil
}

func (s *Blip) Protocols() []p2p.Protocol {
	return []p2p.Protocol{{
		Name:     "blip",
		Version:  1,
		Length:   ProtocolLengths[lpv5],
		NodeInfo: nil,
		Run: func(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
			return s.handler.runPeer(1, peer, rw)
		},
		PeerInfo: func(id enode.ID) interface{} {
			if p := s.peers.peer(id); p != nil {
				return p.Info()
			}
			return nil
		},
		DialCandidates: nil,
	}}
}

// Start implements node.Lifecycle, starting all internal goroutines needed by the
// light ethereum protocol implementation.
func (s *Blip) Start() error {
	log.Warn("Beacon Light Protocol is an experimental feature")
	s.beaconChain.StartSyncing()
	return nil
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *Blip) Stop() error {
	s.syncCommitteeTracker.Stop()
	s.beaconChain.StopSyncing()
	s.handler.stop()
	s.reqDist.close()
	s.odr.Stop()
	log.Info("Beacon Light Protocol stopped")
	return nil
}
