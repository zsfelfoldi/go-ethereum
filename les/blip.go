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
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core"
	cb "github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/catalyst"
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

const (
	blipSoftTimeout = time.Second * 2
	blipServRate    = 50 //20
)

type Blip struct {
	handler      *handler
	peers        *peerSet
	fcManager    *flowcontrol.ClientManager
	costTracker  *costTracker
	minCapacity  uint64
	defParams    flowcontrol.ServerParams
	servingQueue *servingQueue
	reqDist      *requestDistributor
	retriever    *retrieveManager
	odr          *LesOdr
	chainDb      ethdb.Database
	backend      *eth.Ethereum //TODO ugly hack

	fetchLock               sync.Mutex
	fetchHeader             beacon.Header
	fetchTrigger, fetchStop chan struct{}
	fetchCancel             func()

	beaconNodeApi           *beaconNodeApiSource
	consensusApi            *catalyst.ConsensusAPI
	blockchain              *core.BlockChain
	beaconChain             *beacon.BeaconChain
	syncCommitteeCheckpoint *beacon.WeakSubjectivityCheckpoint
	syncCommitteeTracker    *beacon.SyncCommitteeTracker
}

/*type blipBackend interface {
	BlockChain() *core.BlockChain
	ChainDb() ethdb.Database
}*/

func NewBlip(node *node.Node, b /*blipBackend*/ *eth.Ethereum, config *ethconfig.Config) (*Blip, error) {
	forks, err := beacon.LoadForks(config.BeaconConfig)
	if err != nil {
		log.Error("Could not load beacon chain config file", "error", err)
		return nil, fmt.Errorf("Could not load beacon chain config file: %v", err)
	}
	blip := &Blip{
		chainDb:      b.ChainDb(),
		peers:        newPeerSet(),
		backend:      b,
		blockchain:   b.BlockChain(),
		fcManager:    flowcontrol.NewClientManager(nil, &mclock.System{}),
		servingQueue: newServingQueue(int64(time.Millisecond*10), float64(blipServRate)/100),
	}

	blip.costTracker, blip.minCapacity = newCostTracker(blip.chainDb, blipServRate, 10000, 10000) //100
	//fmt.Println("*** minCapacity:", blip.minCapacity)

	// Initialize server capacity management fields.
	blip.defParams = flowcontrol.ServerParams{
		BufLimit:    blip.minCapacity * bufLimitRatio,
		MinRecharge: blip.minCapacity,
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

	var (
		sctConstraints beacon.SctConstraints
		dataSource     beacon.BeaconDataSource
	)
	if blip.beaconNodeApi != nil {
		dataSource = blip.beaconNodeApi
	}
	if blip.beaconChain = beacon.NewBeaconChain(dataSource, (*odrDataSource)(blip.odr), blip.blockchain, blip.chainDb, forks); blip.beaconChain == nil {
		return nil, fmt.Errorf("Could not initialize beacon chain")
	}
	if blip.beaconNodeApi != nil {
		sctConstraints = blip.beaconChain
	} else {
		sctConstraints = blip.syncCommitteeCheckpoint
	}
	blip.syncCommitteeTracker = beacon.NewSyncCommitteeTracker(blip.chainDb, forks, sctConstraints, &mclock.System{})
	blip.syncCommitteeTracker.SubscribeToNewHeads(blip.odr.SetBeaconHead)
	blip.beaconChain.SubscribeToProcessedHeads(blip.syncCommitteeTracker.ProcessedBeaconHead, true)
	if blip.beaconNodeApi != nil {
		blip.beaconNodeApi.chain = blip.beaconChain
		blip.beaconNodeApi.sct = blip.syncCommitteeTracker
		blip.beaconNodeApi.start()
	} else {
		/*blip.syncCommitteeTracker.SubscribeToNewHeads(func(head beacon.Header) {
			blip.beaconChain.SyncToHead(head, nil)
		})*/
	}

	blip.handler = newHandler(blip.peers, config.NetworkId)

	fcWrapper := &fcRequestWrapper{
		costTracker:  blip.costTracker,
		servingQueue: blip.servingQueue,
	}

	beaconServerHandler := &beaconServerHandler{
		syncCommitteeTracker: blip.syncCommitteeTracker,
		beaconChain:          blip.beaconChain,
		blockChain:           blip.blockchain,
		fcWrapper:            fcWrapper,
		beaconNodeApi:        blip.beaconNodeApi,
	}
	blip.handler.registerModule(beaconServerHandler)

	fcServerHandler := &fcServerHandler{
		fcManager:    blip.fcManager,
		costTracker:  blip.costTracker,
		defParams:    blip.defParams,
		servingQueue: blip.servingQueue,
		blockchain:   blip.blockchain,
	}
	blip.handler.registerModule(fcServerHandler)

	beaconClientHandler := &beaconClientHandler{
		syncCommitteeTracker:    blip.syncCommitteeTracker,
		syncCommitteeCheckpoint: blip.syncCommitteeCheckpoint,
		retriever:               blip.retriever,
	}
	blip.handler.registerModule(beaconClientHandler)

	fcClientHandler := &fcClientHandler{
		retriever: blip.retriever,
	}
	blip.handler.registerModule(fcClientHandler)

	node.RegisterProtocols(blip.Protocols())
	//node.RegisterAPIs(blip.APIs())
	node.RegisterLifecycle(blip)
	return blip, nil
}

func (s *Blip) Protocols() []p2p.Protocol {
	return []p2p.Protocol{{
		Name:     "blip",
		Version:  5, //TODO
		Length:   ProtocolLengths[lpv5],
		NodeInfo: nil,
		Run: func(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
			return s.handler.runPeer(5, peer, rw) //TODO
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
	s.handler.start()
	s.beaconChain.StartSyncing()
	if s.beaconNodeApi == nil {
		s.consensusApi = s.backend.ConsensusAPI.(*catalyst.ConsensusAPI)
		s.startBlockFetcher()
		s.syncCommitteeTracker.SubscribeToNewHeads(func(head beacon.Header) {
			s.fetchLock.Lock()
			s.fetchHeader = head
			s.fetchLock.Unlock()
			select {
			case s.fetchTrigger <- struct{}{}:
			default:
			}
		})
	}
	return nil
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *Blip) Stop() error {
	s.stopBlockFetcher()
	s.syncCommitteeTracker.Stop()
	s.beaconChain.StopSyncing()
	s.handler.stop()
	s.reqDist.close()
	s.odr.Stop()
	log.Info("Beacon Light Protocol stopped")
	return nil
}

func (s *Blip) startBlockFetcher() {
	s.fetchTrigger = make(chan struct{}, 1)

	go func() {
		for {
			<-s.fetchTrigger
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			s.fetchLock.Lock()
			head := s.fetchHeader
			stopCh := s.fetchStop
			s.fetchCancel = cancel
			s.fetchLock.Unlock()
			if stopCh != nil {
				close(stopCh)
				return
			}
			r := &light.ExecHeadersRequest{
				ReqMode:    light.HeadMode,
				Amount:     1,
				FullBlocks: true,
			}
			//fmt.Println("*** fetching block")
			var headHash common.Hash
			if err := s.odr.RetrieveWithBeaconHeader(ctx, head, r); err == nil && len(r.ExecBlocks) == 1 {
				headHash = r.ExecBlocks[0].Hash()
				log.Info("Received new block from BLiP", "hash", headHash, "number", r.ExecBlocks[0].NumberU64())
				//fmt.Println("*** fetched block", headHash)
				e := cb.BlockToExecutableData(r.ExecBlocks[0])
				/*status, err :=*/ s.consensusApi.NewPayloadV1(*e)
				//fmt.Println("*** NewPayloadV1:", status, err)
			}
			r = &light.ExecHeadersRequest{
				ReqMode: light.FinalizedMode,
				Amount:  1,
			}
			if err := s.odr.RetrieveWithBeaconHeader(ctx, head, r); err == nil && len(r.ExecHeaders) == 1 {
				finalizedHash := r.ExecHeaders[0].Hash()
				//fmt.Println("*** fetched finalized header", finalizedHash)
				st := cb.ForkchoiceStateV1{
					HeadBlockHash:      headHash,
					SafeBlockHash:      finalizedHash,
					FinalizedBlockHash: finalizedHash,
				}
				/*resp, err2 :=*/ s.consensusApi.ForkchoiceUpdatedV1(st, nil)
				//fmt.Println("*** ForkchoiceUpdatedV1:", resp, err2)
			}
		}
	}()
}

func (s *Blip) stopBlockFetcher() {
	if s.fetchTrigger == nil {
		return
	}
	stopCh := make(chan struct{})
	s.fetchLock.Lock()
	cancel := s.fetchCancel
	s.fetchStop = stopCh
	s.fetchLock.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case s.fetchTrigger <- struct{}{}:
	default:
	}
	<-stopCh
}
