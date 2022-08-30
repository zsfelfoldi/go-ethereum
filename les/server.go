// Copyright 2016 The go-ethereum Authors
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
	"crypto/ecdsa"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	vfs "github.com/ethereum/go-ethereum/les/vflux/server"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	defaultPosFactors = vfs.PriceFactors{TimeFactor: 0, CapacityFactor: 1, RequestFactor: 1}
	defaultNegFactors = vfs.PriceFactors{TimeFactor: 0, CapacityFactor: 1, RequestFactor: 1}
)

const defaultConnectedBias = time.Minute * 3

type ethBackend interface {
	ArchiveMode() bool
	BlockChain() *core.BlockChain
	BloomIndexer() *core.ChainIndexer
	ChainDb() ethdb.Database
	Synced() bool
	TxPool() *core.TxPool
}

type LesServer struct {
	lesCommons

	archiveMode      bool // Flag whether the ethereum node runs in archive mode.
	handler          *handler
	blockchain       *core.BlockChain
	peers, serverset *peerSet
	vfluxServer      *vfs.Server

	privateKey                   *ecdsa.PrivateKey
	lastAnnounce, signedAnnounce announceData

	// Flow control and capacity management
	fcManager    *flowcontrol.ClientManager
	costTracker  *costTracker
	defParams    flowcontrol.ServerParams
	servingQueue *servingQueue
	clientPool   *vfs.ClientPool

	beaconNodeApi        *beaconNodeApiSource
	beaconChain          *beacon.BeaconChain
	syncCommitteeTracker *beacon.SyncCommitteeTracker

	minCapacity, maxCapacity uint64

	p2pSrv *p2p.Server
}

func NewLesServer(node *node.Node, e ethBackend, config *ethconfig.Config) (*LesServer, error) {
	lesDb, err := node.OpenDatabase("les.server", 0, 0, "eth/db/lesserver/", false)
	if err != nil {
		return nil, err
	}
	// Calculate the number of threads used to service the light client
	// requests based on the user-specified value.

	srv := &LesServer{
		lesCommons: lesCommons{
			genesis:          e.BlockChain().Genesis().Hash(),
			config:           config,
			chainConfig:      e.BlockChain().Config(),
			iConfig:          light.DefaultServerIndexerConfig,
			chainDb:          e.ChainDb(),
			lesDb:            lesDb,
			chainReader:      e.BlockChain(),
			chtIndexer:       light.NewChtIndexer(e.ChainDb(), nil, params.CHTFrequency, params.HelperTrieProcessConfirmations, true),
			bloomTrieIndexer: light.NewBloomTrieIndexer(e.ChainDb(), nil, params.BloomBitsBlocks, params.BloomTrieFrequency, true),
			closeCh:          make(chan struct{}),
		},
		archiveMode:  e.ArchiveMode(),
		peers:        newPeerSet(),
		serverset:    newPeerSet(),
		blockchain:   e.BlockChain(),
		vfluxServer:  vfs.NewServer(time.Millisecond * 10),
		fcManager:    flowcontrol.NewClientManager(nil, &mclock.System{}),
		servingQueue: newServingQueue(int64(time.Millisecond*10), float64(config.LightServ)/100),
		p2pSrv:       node.Server(),
	}

	if config.BeaconConfig != "" {
		if forks, err := beacon.LoadForks(config.BeaconConfig); err == nil {
			fmt.Println("Forks", forks)
			bdata := &beaconNodeApiSource{url: config.BeaconApi}
			if srv.beaconChain = beacon.NewBeaconChain(bdata, nil, e.BlockChain(), e.ChainDb(), forks); srv.beaconChain == nil {
				return nil, fmt.Errorf("Could not initialize beacon chain")
			}
			sct := beacon.NewSyncCommitteeTracker(e.ChainDb(), forks, srv.beaconChain, &mclock.System{})
			srv.beaconChain.SubscribeToProcessedHeads(sct.ProcessedBeaconHead, true)
			bdata.chain = srv.beaconChain
			bdata.sct = sct
			bdata.start()
			srv.beaconNodeApi = bdata
			srv.syncCommitteeTracker = sct
		} else {
			log.Error("Could not load beacon chain config file", "error", err)
			return nil, fmt.Errorf("Could not load beacon chain config file: %v", err)
		}
	}

	srv.costTracker, srv.minCapacity = newCostTracker(e.ChainDb(), config.LightServ, config.LightIngress, config.LightEgress)
	srv.oracle = srv.setupOracle(node, e.BlockChain().Genesis().Hash(), config)

	// Initialize the bloom trie indexer.
	e.BloomIndexer().AddChildIndexer(srv.bloomTrieIndexer)

	issync := e.Synced
	if config.LightNoSyncServe {
		issync = func() bool { return true }
	}

	// Initialize server capacity management fields.
	srv.defParams = flowcontrol.ServerParams{
		BufLimit:    srv.minCapacity * bufLimitRatio,
		MinRecharge: srv.minCapacity,
	}
	// LES flow control tries to more or less guarantee the possibility for the
	// clients to send a certain amount of requests at any time and get a quick
	// response. Most of the clients want this guarantee but don't actually need
	// to send requests most of the time. Our goal is to serve as many clients as
	// possible while the actually used server capacity does not exceed the limits
	totalRecharge := srv.costTracker.totalRecharge()
	srv.maxCapacity = srv.minCapacity * uint64(srv.config.LightPeers)
	if totalRecharge > srv.maxCapacity {
		srv.maxCapacity = totalRecharge
	}
	srv.fcManager.SetCapacityLimits(srv.minCapacity, srv.maxCapacity, srv.minCapacity*2)
	srv.clientPool = vfs.NewClientPool(lesDb, srv.minCapacity, defaultConnectedBias, mclock.System{}, issync)
	srv.clientPool.Start()
	srv.clientPool.SetDefaultFactors(defaultPosFactors, defaultNegFactors)
	srv.vfluxServer.Register(srv.clientPool, "les", "Ethereum light client service")

	srv.handler = newHandler(srv.peers, srv.config.NetworkId)

	fcWrapper := &fcRequestWrapper{
		costTracker:  srv.costTracker,
		servingQueue: srv.servingQueue,
	}

	serverHandler := newServerHandler(srv, e.BlockChain(), e.ChainDb(), e.TxPool(), fcWrapper, issync)
	srv.handler.registerModule(serverHandler)

	beaconServerHandler := &beaconServerHandler{
		syncCommitteeTracker: srv.syncCommitteeTracker,
		beaconChain:          srv.beaconChain,
		blockChain:           srv.blockchain,
		fcWrapper:            fcWrapper,
	}
	srv.handler.registerModule(beaconServerHandler)

	fcServerHandler := &fcServerHandler{
		fcManager:    srv.fcManager,
		costTracker:  srv.costTracker,
		defParams:    srv.defParams,
		servingQueue: srv.servingQueue,
		blockchain:   srv.blockchain,
	}
	srv.handler.registerModule(fcServerHandler)

	vfxServerHandler := &vfxServerHandler{
		fcManager:   srv.fcManager,
		clientPool:  srv.clientPool,
		minCapacity: srv.minCapacity,
		maxPeers:    srv.config.LightPeers,
	}
	srv.handler.registerModule(vfxServerHandler)

	checkpoint := srv.latestLocalCheckpoint()
	if !checkpoint.Empty() {
		log.Info("Loaded latest checkpoint", "section", checkpoint.SectionIndex, "head", checkpoint.SectionHead,
			"chtroot", checkpoint.CHTRoot, "bloomroot", checkpoint.BloomRoot)
	}
	srv.chtIndexer.Start(e.BlockChain())

	node.RegisterProtocols(srv.Protocols())
	node.RegisterAPIs(srv.APIs())
	node.RegisterLifecycle(srv)
	return srv, nil
}

func (s *LesServer) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: "les",
			Service:   NewLightAPI(&s.lesCommons),
		},
		{
			Namespace: "les",
			Service:   NewLightServerAPI(s),
		},
		{
			Namespace: "debug",
			Service:   NewDebugAPI(s),
		},
	}
}

func (s *LesServer) Protocols() []p2p.Protocol {
	ps := s.makeProtocols(ServerProtocolVersions, s.handler.runPeer, func(id enode.ID) interface{} {
		if p := s.peers.peer(id); p != nil {
			return p.Info()
		}
		return nil
	}, nil)
	// Add "les" ENR entries.
	for i := range ps {
		ps[i].Attributes = []enr.Entry{&lesEntry{
			VfxVersion: 1,
		}}
	}
	return ps
}

// Start starts the LES server
func (s *LesServer) Start() error {
	s.privateKey = s.p2pSrv.PrivateKey
	s.handler.start()

	if s.p2pSrv.DiscV5 != nil {
		s.p2pSrv.DiscV5.RegisterTalkHandler("vfx", s.vfluxServer.ServeEncoded)
	}
	if s.beaconChain != nil {
		s.beaconChain.StartSyncing()
	}
	return nil
}

// Stop stops the LES service
func (s *LesServer) Stop() error {
	close(s.closeCh)

	if s.beaconChain != nil {
		s.beaconChain.StopSyncing()
	}
	if s.syncCommitteeTracker != nil {
		s.syncCommitteeTracker.Stop()
	}
	if s.beaconNodeApi != nil {
		s.beaconNodeApi.stop()
	}

	s.clientPool.Stop()
	if s.serverset != nil {
		s.serverset.close()
	}
	s.handler.stop()
	s.fcManager.Stop()
	s.costTracker.stop()
	s.servingQueue.stop()
	if s.vfluxServer != nil {
		s.vfluxServer.Stop()
	}

	// Note, bloom trie indexer is closed by parent bloombits indexer.
	if s.chtIndexer != nil {
		s.chtIndexer.Close()
	}
	if s.lesDb != nil {
		s.lesDb.Close()
	}
	s.wg.Wait()
	log.Info("Les server stopped")

	return nil
}

// broadcastLoop broadcasts new block information to all connected light
// clients. According to the agreement between client and server, server should
// only broadcast new announcement if the total difficulty is higher than the
// last one. Besides server will add the signature if client requires.
func (s *LesServer) broadcastLoop() {
	defer s.wg.Done()

	headCh := make(chan core.ChainHeadEvent, 10)
	headSub := s.blockchain.SubscribeChainHeadEvent(headCh)
	defer headSub.Unsubscribe()

	var (
		lastHead = s.blockchain.CurrentHeader()
		lastTd   = common.Big0
	)
	for {
		select {
		case ev := <-headCh:
			header := ev.Block.Header()
			hash, number := header.Hash(), header.Number.Uint64()
			if s.beaconChain != nil {
				s.beaconChain.ProcessedExecHead(hash)
			}
			if s.beaconNodeApi != nil {
				s.beaconNodeApi.newHead()
			}
			td := s.blockchain.GetTd(hash, number)
			if td == nil || td.Cmp(lastTd) <= 0 {
				continue
			}
			var reorg uint64
			if lastHead != nil {
				// If a setHead has been performed, the common ancestor can be nil.
				if ancestor := rawdb.FindCommonAncestor(s.chainDb, header, lastHead); ancestor != nil {
					reorg = lastHead.Number.Uint64() - ancestor.Number.Uint64()
				}
			}
			lastHead, lastTd = header, td
			log.Debug("Announcing block to peers", "number", number, "hash", hash, "td", td, "reorg", reorg)
			s.broadcast(announceData{Hash: hash, Number: number, Td: td, ReorgDepth: reorg})
		case <-s.closeCh:
			return
		}
	}
}

// broadcast sends the given announcements to all active peers
func (s *LesServer) broadcast(announce announceData) {
	s.lastAnnounce = announce
	for _, peer := range s.peers.allPeers() {
		s.announceOrStore(peer)
	}
}

// announceOrStore sends the requested type of announcement to the given peer or stores
// it for later if the peer is inactive (capacity == 0).
func (s *LesServer) announceOrStore(p *peer) {
	if s.lastAnnounce.Td == nil {
		return
	}
	switch p.announceType {
	case announceTypeSimple:
		p.announceOrStore(s.lastAnnounce)
	case announceTypeSigned:
		if s.signedAnnounce.Hash != s.lastAnnounce.Hash {
			s.signedAnnounce = s.lastAnnounce
			s.signedAnnounce.sign(s.privateKey)
		}
		p.announceOrStore(s.signedAnnounce)
	}
}
