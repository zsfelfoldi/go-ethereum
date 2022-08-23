// Copyright 2019 The go-ethereum Authors
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
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	vfs "github.com/ethereum/go-ethereum/les/vflux/server"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	softResponseLimit = 2 * 1024 * 1024 // Target maximum size of returned blocks, headers or node data.
	estHeaderRlpSize  = 500             // Approximate size of an RLP encoded block header

	MaxHeaderFetch           = 192 // Amount of block headers to be fetched per retrieval request
	MaxBodyFetch             = 32  // Amount of block bodies to be fetched per retrieval request
	MaxReceiptFetch          = 128 // Amount of transaction receipts to allow fetching per request
	MaxCodeFetch             = 64  // Amount of contract codes to allow fetching per request
	MaxProofsFetch           = 64  // Amount of merkle proofs to be fetched per retrieval request
	MaxHelperTrieProofsFetch = 64  // Amount of helper tries to be fetched per retrieval request
	MaxTxSend                = 64  // Amount of transactions to be send per request
	MaxTxStatus              = 256 // Amount of transactions to queried per request
	MaxCommitteeUpdateFetch  = beacon.MaxCommitteeUpdateFetch
	CommitteeCostFactor      = beacon.CommitteeCostFactor
)

var (
	errTooManyInvalidRequest = errors.New("too many invalid requests made")
)

// serverHandler is responsible for serving light client and process
// all incoming light requests.
type serverHandler struct {
	forkFilter forkid.Filter
	blockchain *core.BlockChain
	chainDb    ethdb.Database
	txpool     *core.TxPool
	server     *LesServer
	fcWrapper  *fcRequestWrapper

	synced func() bool // Callback function used to determine whether local node is synced.

	// Testing fields
	addTxsSync bool
}

func newServerHandler(server *LesServer, blockchain *core.BlockChain, chainDb ethdb.Database, txpool *core.TxPool, synced func() bool) *serverHandler {
	handler := &serverHandler{
		forkFilter: forkid.NewFilter(blockchain),
		server:     server,
		blockchain: blockchain,
		chainDb:    chainDb,
		txpool:     txpool,
		synced:     synced,
	}
	return handler
}

func (h *serverHandler) sendHandshake(p *peer, send *keyValueList) {
	// Note: peer.headInfo should contain the last head announced to the client by us.
	// The values announced by the client in the handshake are dummy values for compatibility reasons and should be ignored.
	head := h.blockchain.CurrentHeader()
	hash := head.Hash()
	number := head.Number.Uint64()
	p.headInfo = blockInfo{Hash: hash, Number: number, Td: h.blockchain.GetTd(hash, number)}
	sendHeadInfo(send, p.headInfo)
	sendGeneralInfo(p, send, h.blockchain.Genesis().Hash(), forkid.NewID(h.blockchain.Config(), h.blockchain.Genesis().Hash(), number))

	recentTx := h.server.handler.blockchain.TxLookupLimit()
	if recentTx != txIndexUnlimited {
		if recentTx < blockSafetyMargin {
			recentTx = txIndexDisabled
		} else {
			recentTx -= blockSafetyMargin - txIndexRecentOffset
		}
	}

	// Add some information which services server can offer.
	send.add("serveHeaders", nil)
	send.add("serveChainSince", uint64(0))
	send.add("serveStateSince", uint64(0))

	// If local ethereum node is running in archive mode, advertise ourselves we have
	// all version state data. Otherwise only recent state is available.
	stateRecent := uint64(core.TriesInMemory - blockSafetyMargin)
	if h.server.archiveMode {
		stateRecent = 0
	}
	send.add("serveRecentState", stateRecent)
	send.add("txRelay", nil)
	if p.version >= lpv4 {
		send.add("recentTxLookup", recentTx)
	}
	send.add("flowControl/BL", h.server.defParams.BufLimit)
	send.add("flowControl/MRR", h.server.defParams.MinRecharge)

	var costList RequestCostList
	if h.server.costTracker.testCostList != nil {
		costList = h.server.costTracker.testCostList
	} else {
		costList = h.server.costTracker.makeCostList(h.server.costTracker.globalFactor())
	}
	send.add("flowControl/MRC", costList)
	p.fcCosts = costList.decode(ProtocolLengths[uint(p.version)])
	p.fcParams = h.server.defParams

	// Add advertised checkpoint and register block height which
	// client can verify the checkpoint validity.
	if h.server.oracle != nil && server.oracle.IsRunning() {
		cp, height := h.server.oracle.StableCheckpoint()
		if cp != nil {
			send.add("checkpoint/value", cp)
			send.add("checkpoint/registerHeight", height)
		}
	}
}

func (h *serverHandler) receiveHandshake(p *peer, recv keyValueMap) error {
	if err := receiveGeneralInfo(p, recv, h.blockchain.Genesis().Hash(), h.forkFilter); err != nil {
		return err
	}

	p.server = recv.get("flowControl/MRR", nil) == nil
	if p.server {
		p.announceType = announceTypeNone // connected to another server, send no messages
	} else {
		if recv.get("announceType", &p.announceType) != nil {
			// set default announceType on server side
			p.announceType = announceTypeSimple
		}
	}
	return nil
}

func (h *serverHandler) peerConnected(p *peer) (func(), error) {
	if h.server.handler.blockchain.TxLookupLimit() != txIndexUnlimited && p.version < lpv4 {
		return nil, errors.New("Cannot serve old clients without a complete tx index")
	}

	// Reject light clients if server is not synced. Put this checking here, so
	// that "non-synced" les-server peers are still allowed to keep the connection.
	if !h.synced() { //TODO synced status after merge
		p.Log().Debug("Light server not synced, rejecting peer")
		return nil, p2p.DiscRequested
	}

	// Setup flow control mechanism for the peer
	p.fcClient = flowcontrol.NewClientNode(h.server.fcManager, p.fcParams)

	if p.version <= lpv4 {
		h.server.announceOrStore(p)
	}

	// Mark the peer as being served.
	atomic.StoreUint32(&p.serving, 1) //TODO ???

	return func() {
		atomic.StoreUint32(&p.serving, 0)

		//wg.Wait() // Ensure all background task routines have exited. //TODO ???
		p.fcClient.Disconnect()
		p.fcClient = nil
	}, nil
}

func (h *serverHandler) messageHandlers() messageHandlers {
	return h.fcWrapper.wrapMessageHandlers(RequestServer{BlockChain: h.blockchain, TxPool: h.txpool}.MessageHandlers())
}

// getAccount retrieves an account from the state based on root.
func getAccount(triedb *trie.Database, root, hash common.Hash) (types.StateAccount, error) {
	trie, err := trie.New(common.Hash{}, root, triedb)
	if err != nil {
		return types.StateAccount{}, err
	}
	blob, err := trie.TryGet(hash[:])
	if err != nil {
		return types.StateAccount{}, err
	}
	var acc types.StateAccount
	if err = rlp.DecodeBytes(blob, &acc); err != nil {
		return types.StateAccount{}, err
	}
	return acc, nil
}

// GetHelperTrie returns the post-processed trie root for the given trie ID and section index
func (h *serverHandler) GetHelperTrie(typ uint, index uint64) *trie.Trie {
	var (
		root   common.Hash
		prefix string
	)
	switch typ {
	case htCanonical:
		sectionHead := rawdb.ReadCanonicalHash(h.chainDb, (index+1)*h.server.iConfig.ChtSize-1)
		root, prefix = light.GetChtRoot(h.chainDb, index, sectionHead), light.ChtTablePrefix
	case htBloomBits:
		sectionHead := rawdb.ReadCanonicalHash(h.chainDb, (index+1)*h.server.iConfig.BloomTrieSize-1)
		root, prefix = light.GetBloomTrieRoot(h.chainDb, index, sectionHead), light.BloomTrieTablePrefix
	}
	if root == (common.Hash{}) {
		return nil
	}
	trie, _ := trie.New(common.Hash{}, root, trie.NewDatabase(rawdb.NewTable(h.chainDb, prefix)))
	return trie
}

type beaconServerHandler struct {
	syncCommitteeTracker *beacon.SyncCommitteeTracker
	beaconChain          *beacon.BeaconChain
}

func (h *beaconServerHandler) sendHandshake(p *peer, send *keyValueList) {
	if p.version < lpv5 {
		return
	}
	updateInfo := h.syncCommitteeTracker.GetUpdateInfo()
	fmt.Println("Adding update info", updateInfo)
	send.add("beacon/updateInfo", updateInfo)
}

func (h *beaconServerHandler) receiveHandshake(p *peer, recv keyValueMap) error {
	return nil
}

func (h *beaconServerHandler) peerConnected(p *peer) (func(), error) {
	if h.server.syncCommitteeTracker == nil {
		return nil, nil
	}
	h.server.syncCommitteeTracker.Activate(p)

	return func() {
		h.server.syncCommitteeTracker.Deactivate(p, true)
	}, nil
}

func (h *beaconServerHandler) messageHandlers() messageHandlers {
	return h.fcWrapper.wrapMessageHandlers(BeaconRequestServer{SyncCommitteeTracker: h.syncCommitteeTracker, BeaconChain: h.beaconChain}.MessageHandlers())
}

type vfxServerHandler struct {
	clientPool *vfs.ClientPool
}

func (h *vfxServerHandler) peerConnected(p *peer) (func(), error) {
	if p.balance = h.clientPool.Register(p); p.balance == nil {
		p.Log().Debug("Client pool already closed")
		return nil, p2p.DiscRequested
	}

	return func() {
		h.clientPool.Unregister(p)
		p.balance = nil
	}, nil
}
