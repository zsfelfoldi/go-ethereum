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
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	// "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/les/downloader"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
)

type beaconClientHandler struct {
	syncCommitteeTracker *beacon.SyncCommitteeTracker
	retriever            *retrieveManager
	odr                  *LesOdr
}

func (bc *beaconClientHandler) registerMessageHandlers(h *handler) {
	h.registerMessageHandler(BeaconInitMsg, lpv5, lpvLatest)
	h.registerMessageHandler(BeaconDataMsg, lpv5, lpvLatest)
	h.registerMessageHandler(ExecHeadersMsg, lpv5, lpvLatest)
	h.registerMessageHandler(CommitteeProofsMsg, lpv5, lpvLatest)
	h.registerMessageHandler(AdvertiseCommitteeProofsMsg, lpv5, lpvLatest)
	h.registerMessageHandler(SignedBeaconHeadsMsg, lpv5, lpvLatest)
}

func (bc *beaconClientHandler) sendHandshake(*peer, *keyValueList) {}

func (bc *beaconClientHandler) receiveHandshake(p *peer, recv keyValueMap) error {
	fmt.Println("Handshake with peer", p.id, "version", p.version)
	updateInfo := new(beacon.UpdateInfo)
	if err := recv.get("beacon/updateInfo", updateInfo); err == nil {
		fmt.Println("Received update info", *updateInfo)
		p.updateInfo = updateInfo
	}
	return nil
}

func (bc *beaconClientHandler) handleMessage(p *peer, msg p2p.Msg) error {
	var deliverMsg *Msg

	switch msg.Code {
	case BeaconInitMsg:
		p.Log().Trace("Received beacon init response")
		var resp struct {
			ReqID, BV          uint64
			BeaconInitResponse //TODO check RLP encoding
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgBeaconInit,
			ReqID:   resp.ReqID,
			Obj:     resp.BeaconInitResponse,
		}
	case BeaconDataMsg:
		p.Log().Trace("Received beacon data response")
		var resp struct {
			ReqID, BV          uint64
			BeaconDataResponse //TODO check RLP encoding
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgBeaconData,
			ReqID:   resp.ReqID,
			Obj:     resp.BeaconDataResponse,
		}
	case ExecHeadersMsg:
		p.Log().Trace("Received exec headers response")
		var resp struct {
			ReqID, BV           uint64
			ExecHeadersResponse //TODO check RLP encoding
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgExecHeaders,
			ReqID:   resp.ReqID,
			Obj:     resp.ExecHeadersResponse,
		}
	case CommitteeProofsMsg:
		p.Log().Trace("Received committee proofs response")
		var resp struct {
			ReqID, BV             uint64
			beacon.CommitteeReply //TODO check RLP encoding
		}
		fmt.Println("Received CommitteeProofsMsg")
		if err := msg.Decode(&resp); err != nil {
			fmt.Println(" decode err", err)
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		fmt.Println(" decode ok")
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgCommitteeProofs,
			ReqID:   resp.ReqID,
			Obj:     resp.CommitteeReply,
		}
	case AdvertiseCommitteeProofsMsg:
		p.Log().Trace("Received committee proofs advertisement")
		var resp beacon.UpdateInfo
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		h.backend.syncCommitteeTracker.SyncWithPeer(sctServerPeer{peer: p, retriever: bc.retriever}, &resp)
	case SignedBeaconHeadsMsg:
		p.Log().Trace("Received beacon chain head update")
		fmt.Println("*** Received beacon chain head update")
		var heads []beacon.SignedHead
		if err := msg.Decode(&heads); err != nil {
			fmt.Println(" decode error", err)
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		for _, head := range heads {
			hash := head.Header.Hash()
			p.AnnouncedBeaconHead(hash, bc.odr.getExecHeader(hash))
		}
		bc.syncCommitteeTracker.AddSignedHeads(sctServerPeer{peer: p, retriever: bc.retriever}, heads)
	default:
		panic(nil)
	}

	// Deliver the received response to retriever.
	if deliverMsg != nil {
		if err := bc.retriever.deliver(p, deliverMsg); err != nil {
			if val := p.bumpInvalid(); val > maxResponseErrors {
				return err
			}
		}
	}
	return nil
}

// clientHandler is responsible for receiving and processing all incoming server
// responses.
type clientHandler struct {
	forkFilter forkid.Filter
	checkpoint *params.TrustedCheckpoint
	//fetcher    *lightFetcher
	downloader *downloader.Downloader
	backend    *LightEthereum

	// Hooks used in the testing
	syncStart func(header *types.Header) // Hook called when the syncing is started
	syncEnd   func(header *types.Header) // Hook called when the syncing is done
}

func newClientHandler(ulcServers []string, ulcFraction int, checkpoint *params.TrustedCheckpoint, backend *LightEthereum) *clientHandler {
	handler := &clientHandler{
		forkFilter: forkid.NewFilter((*dummyChain)(backend.blockchain)),
		checkpoint: checkpoint,
		backend:    backend,
		closeCh:    make(chan struct{}),
	}
	if ulcServers != nil {
		ulc, err := newULC(ulcServers, ulcFraction)
		if err != nil {
			log.Error("Failed to initialize ultra light client")
		}
		handler.ulc = ulc
		log.Info("Enable ultra light client mode")
	}
	/*var height uint64
	if checkpoint != nil {
		height = (checkpoint.SectionIndex+1)*params.CHTFrequency - 1
	}
	handler.fetcher = newLightFetcher(backend.blockchain, backend.engine, backend.peers, handler.ulc, backend.chainDb, backend.reqDist, handler.synchronise)
	handler.downloader = downloader.New(height, backend.chainDb, backend.eventMux, nil, backend.blockchain, handler.removePeer)
	handler.backend.peers.subscribe((*downloaderPeerNotify)(handler))*/
	return handler
}

/*
func (h *clientHandler) start() {
	//h.fetcher.start()
}

func (h *clientHandler) stop() {
	close(h.closeCh)
	//h.downloader.Terminate()
	//h.fetcher.stop()
	h.wg.Wait()
}
*/

type dummyChain light.LightChain

func (d *dummyChain) Config() *params.ChainConfig { return (*light.LightChain)(d).Config() }

func (d *dummyChain) Genesis() *types.Block { return (*light.LightChain)(d).Genesis() }

func (d *dummyChain) CurrentHeader() *types.Header { return d.Genesis().Header() }

func (h *clientHandler) sendHandshake(*peer, *keyValueList) {
	p.announceType = announceTypeSimple
	*lists = (*lists).add("announceType", p.announceType)
}

func (h *clientHandler) receiveHandshake(p *peer, recv keyValueMap) error {
	var (
		rHash common.Hash
		rNum  uint64
		rTd   *big.Int
	)
	if err := recv.get("headTd", &rTd); err != nil {
		return err
	}
	if err := recv.get("headHash", &rHash); err != nil {
		return err
	}
	if err := recv.get("headNum", &rNum); err != nil {
		return err
	}
	p.headInfo = blockInfo{Hash: rHash, Number: rNum, Td: rTd}
	if recv.get("serveChainSince", &p.chainSince) != nil {
		p.onlyAnnounce = true
	}
	if recv.get("serveRecentChain", &p.chainRecent) != nil {
		p.chainRecent = 0
	}
	if recv.get("serveStateSince", &p.stateSince) != nil {
		p.onlyAnnounce = true
	}
	if recv.get("serveRecentState", &p.stateRecent) != nil {
		p.stateRecent = 0
	}
	if recv.get("txRelay", nil) != nil {
		p.onlyAnnounce = true
	}
	if p.version >= lpv4 {
		var recentTx uint
		if err := recv.get("recentTxLookup", &recentTx); err != nil {
			return err
		}
		p.txHistory = uint64(recentTx)
	} else {
		// The weak assumption is held here that legacy les server(les2,3)
		// has unlimited transaction history. The les serving in these legacy
		// versions is disabled if the transaction is unindexed.
		p.txHistory = txIndexUnlimited
	}
	if p.onlyAnnounce && !p.trusted {
		return errResp(ErrUselessPeer, "peer cannot serve requests")
	}
	// Parse flow control handshake packet.
	var sParams flowcontrol.ServerParams
	if err := recv.get("flowControl/BL", &sParams.BufLimit); err != nil {
		return err
	}
	if err := recv.get("flowControl/MRR", &sParams.MinRecharge); err != nil {
		return err
	}
	var MRC RequestCostList
	if err := recv.get("flowControl/MRC", &MRC); err != nil {
		return err
	}
	p.fcParams = sParams
	p.fcServer = flowcontrol.NewServerNode(sParams, &mclock.System{})
	p.fcCosts = MRC.decode(ProtocolLengths[uint(p.version)])

	recv.get("checkpoint/value", &p.checkpoint)
	recv.get("checkpoint/registerHeight", &p.checkpointNumber)

	if !p.onlyAnnounce {
		for msgCode := range reqAvgTimeCost {
			if p.fcCosts[msgCode] == nil {
				return errResp(ErrUselessPeer, "peer does not support message %d", msgCode)
			}
		}
	}
	return nil
}

func (h *clientHandler) peerConnected(*peer) (func(), error) {}

func (h *clientHandler) handle(p *peer, noInitAnnounce bool) error {
	fmt.Println("handle", p.id)
	if h.backend.peers.len() >= h.backend.config.LightPeers && !p.Peer.Info().Network.Trusted {
		return p2p.DiscTooManyPeers
	}
	p.Log().Debug("Light Ethereum peer connected", "name", p.Name())

	// Execute the LES handshake
	forkid := forkid.NewID(h.backend.blockchain.Config(), h.backend.genesis, 0 /*h.backend.blockchain.CurrentHeader().Number.Uint64()*/)
	if err := p.HandshakeWithServer(h.backend.blockchain.Genesis().Hash(), forkid, h.forkFilter); err != nil {
		fmt.Println(" handshake err", err)
		p.Log().Debug("Light Ethereum handshake failed", "err", err)
		return err
	}
	fmt.Println(" handshake ok")
	// Register peer with the server pool
	if h.backend.serverPool != nil {
		if nvt, err := h.backend.serverPool.RegisterNode(p.Node()); err == nil {
			p.setValueTracker(nvt)
			p.updateVtParams()
			defer func() {
				p.setValueTracker(nil)
				h.backend.serverPool.UnregisterNode(p.Node())
			}()
		} else {
			return err
		}
	}
	// Register the peer locally
	if err := h.backend.peers.register(p); err != nil {
		p.Log().Error("Light Ethereum peer registration failed", "err", err)
		return err
	}

	serverConnectionGauge.Update(int64(h.backend.peers.len()))

	connectedAt := mclock.Now()
	defer func() {
		h.backend.peers.unregister(p.id)
		connectionTimer.Update(time.Duration(mclock.Now() - connectedAt))
		serverConnectionGauge.Update(int64(h.backend.peers.len()))
	}()

	// Discard all the announces after the transition
	// Also discarding initial signal to prevent syncing during testing.
	/*if !(noInitAnnounce || h.backend.merger.TDDReached()) {
		h.fetcher.announce(p, &announceData{Hash: p.headInfo.Hash, Number: p.headInfo.Number, Td: p.headInfo.Td})
	}*/
	if p.updateInfo != nil {
		h.backend.syncCommitteeCheckpoint.TriggerFetch()
		sctPeer := sctServerPeer{peer: p, retriever: h.backend.retriever}
		h.backend.syncCommitteeTracker.SyncWithPeer(sctPeer, p.updateInfo)
		defer h.backend.syncCommitteeTracker.Disconnect(sctPeer)
	}

	// Mark the peer starts to be served.
	atomic.StoreUint32(&p.serving, 1)
	defer atomic.StoreUint32(&p.serving, 0)

	// Spawn a main loop to handle all incoming messages.
	for {
		if err := h.handleMsg(p); err != nil {
			p.Log().Debug("Light Ethereum message handling failed", "err", err)
			p.fcServer.DumpLogs()
			return err
		}
	}
}

func (bc *beaconClientHandler) registerMessageHandlers(h *handler) {
	h.registerMessageHandler(AnnounceMsg, lpv1, lpv4)
	h.registerMessageHandler(BlockHeadersMsg, lpv1, lpvLatest)
	h.registerMessageHandler(BlockBodiesMsg, lpv1, lpvLatest)
	h.registerMessageHandler(CodeMsg, lpv1, lpvLatest)
	h.registerMessageHandler(ReceiptsMsg, lpv1, lpvLatest)
	h.registerMessageHandler(ProofsV2Msg, lpv2, lpvLatest)
	h.registerMessageHandler(HelperTrieProofsMsg, lpv2, lpv4)
	h.registerMessageHandler(TxStatusMsg, lpv2, lpvLatest)
	h.registerMessageHandler(StopMsg, lpv3, lpvLatest)
	h.registerMessageHandler(Resume, lpv3, lpvLatest)
}

func (h *clientHandler) handleMessage(p *peer, msg p2p.Msg) error {
	var deliverMsg *Msg

	switch msg.Code {
	case AnnounceMsg:
		p.Log().Trace("Received announce message")
		var req announceData
		if err := msg.Decode(&req); err != nil {
			return errResp(ErrDecode, "%v: %v", msg, err)
		}
		if err := req.sanityCheck(); err != nil {
			return err
		}
		update, size := req.Update.decode()
		if p.rejectUpdate(size) {
			return errResp(ErrRequestRejected, "")
		}
		p.updateFlowControl(update)
		p.updateVtParams()

		if req.Hash != (common.Hash{}) {
			if p.announceType == announceTypeNone {
				return errResp(ErrUnexpectedResponse, "")
			}
			if p.announceType == announceTypeSigned {
				if err := req.checkSignature(p.ID(), update); err != nil {
					p.Log().Trace("Invalid announcement signature", "err", err)
					return err
				}
				p.Log().Trace("Valid announcement signature")
			}
			p.Log().Trace("Announce message content", "number", req.Number, "hash", req.Hash, "td", req.Td, "reorg", req.ReorgDepth)

			// Update peer head information first and then notify the announcement
			p.updateHead(req.Hash, req.Number, req.Td)

			// Discard all the announces after the transition
			/*if !h.backend.merger.TDDReached() {
				h.fetcher.announce(p, &req)
			}*/
		}
	case BlockHeadersMsg:
		p.Log().Trace("Received block header response message")
		var resp struct {
			ReqID, BV uint64
			Headers   []*types.Header
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		//headers := resp.Headers
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)

		// Filter out the explicitly requested header by the retriever
		//if h.backend.retriever.requested(resp.ReqID) {
		deliverMsg = &Msg{
			MsgType: MsgBlockHeaders,
			ReqID:   resp.ReqID,
			Obj:     resp.Headers,
		}
		//} else {
		// Filter out any explicitly requested headers, deliver the rest to the downloader
		/*filter := len(headers) == 1
			if filter {
				headers = h.fetcher.deliverHeaders(p, resp.ReqID, resp.Headers)
			}
			if len(headers) != 0 || !filter {
				if err := h.downloader.DeliverHeaders(p.id, headers); err != nil {
					log.Debug("Failed to deliver headers", "err", err)
				}
			}
		}*/
	case BlockBodiesMsg:
		p.Log().Trace("Received block bodies response")
		var resp struct {
			ReqID, BV uint64
			Data      []*types.Body
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgBlockBodies,
			ReqID:   resp.ReqID,
			Obj:     resp.Data,
		}
	case CodeMsg:
		p.Log().Trace("Received code response")
		var resp struct {
			ReqID, BV uint64
			Data      [][]byte
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgCode,
			ReqID:   resp.ReqID,
			Obj:     resp.Data,
		}
	case ReceiptsMsg:
		p.Log().Trace("Received receipts response")
		var resp struct {
			ReqID, BV uint64
			Receipts  []types.Receipts
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgReceipts,
			ReqID:   resp.ReqID,
			Obj:     resp.Receipts,
		}
	case ProofsV2Msg:
		p.Log().Trace("Received les/2 proofs response")
		var resp struct {
			ReqID, BV uint64
			Data      light.NodeList
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgProofsV2,
			ReqID:   resp.ReqID,
			Obj:     resp.Data,
		}
	case HelperTrieProofsMsg:
		p.Log().Trace("Received helper trie proof response")
		var resp struct {
			ReqID, BV uint64
			Data      HelperTrieResps
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgHelperTrieProofs,
			ReqID:   resp.ReqID,
			Obj:     resp.Data,
		}
	case TxStatusMsg:
		p.Log().Trace("Received tx status response")
		var resp struct {
			ReqID, BV uint64
			Status    []light.TxStatus
		}
		if err := msg.Decode(&resp); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ReceivedReply(resp.ReqID, resp.BV)
		p.answeredRequest(resp.ReqID)
		deliverMsg = &Msg{
			MsgType: MsgTxStatus,
			ReqID:   resp.ReqID,
			Obj:     resp.Status,
		}
	case StopMsg:
		p.freezeServer()
		h.backend.retriever.frozen(p)
		p.Log().Debug("Service stopped")
	case ResumeMsg:
		var bv uint64
		if err := msg.Decode(&bv); err != nil {
			return errResp(ErrDecode, "msg %v: %v", msg, err)
		}
		p.fcServer.ResumeFreeze(bv)
		p.unfreezeServer()
		p.Log().Debug("Service resumed")

	default:
		panic(nil)
	}
	// Deliver the received response to retriever.
	if deliverMsg != nil {
		if err := h.backend.retriever.deliver(p, deliverMsg); err != nil {
			if val := p.bumpInvalid(); val > maxResponseErrors {
				return err
			}
		}
	}
	return nil
}

func (h *clientHandler) removePeer(id string) {
	h.backend.peers.unregister(id)
}

type peerConnection struct {
	handler *clientHandler
	peer    *peer
}

func (pc *peerConnection) Head() (common.Hash, *big.Int) {
	return pc.peer.HeadAndTd()
}

func (pc *peerConnection) RequestHeadersByHash(origin common.Hash, amount int, skip int, reverse bool) error {
	rq := &distReq{
		getCost: func(dp distPeer) uint64 {
			peer := dp.(*peer)
			return peer.getRequestCost(GetBlockHeadersMsg, amount)
		},
		canSend: func(dp distPeer) bool {
			return dp.(*peer) == pc.peer
		},
		request: func(dp distPeer) func() {
			reqID := rand.Uint64()
			peer := dp.(*peer)
			cost := peer.getRequestCost(GetBlockHeadersMsg, amount)
			peer.fcServer.QueuedRequest(reqID, cost)
			return func() { peer.requestHeadersByHash(reqID, origin, amount, skip, reverse) }
		},
	}
	_, ok := <-pc.handler.backend.reqDist.queue(rq)
	if !ok {
		return light.ErrNoPeers
	}
	return nil
}

func (pc *peerConnection) RequestHeadersByNumber(origin uint64, amount int, skip int, reverse bool) error {
	rq := &distReq{
		getCost: func(dp distPeer) uint64 {
			peer := dp.(*peer)
			return peer.getRequestCost(GetBlockHeadersMsg, amount)
		},
		canSend: func(dp distPeer) bool {
			return dp.(*peer) == pc.peer
		},
		request: func(dp distPeer) func() {
			reqID := rand.Uint64()
			peer := dp.(*peer)
			cost := peer.getRequestCost(GetBlockHeadersMsg, amount)
			peer.fcServer.QueuedRequest(reqID, cost)
			return func() { peer.requestHeadersByNumber(reqID, origin, amount, skip, reverse) }
		},
	}
	_, ok := <-pc.handler.backend.reqDist.queue(rq)
	if !ok {
		return light.ErrNoPeers
	}
	return nil
}

// RetrieveSingleHeaderByNumber requests a single header by the specified block
// number. This function will wait the response until it's timeout or delivered.
func (pc *peerConnection) RetrieveSingleHeaderByNumber(context context.Context, number uint64) (*types.Header, error) {
	reqID := rand.Uint64()
	rq := &distReq{
		getCost: func(dp distPeer) uint64 {
			peer := dp.(*peer)
			return peer.getRequestCost(GetBlockHeadersMsg, 1)
		},
		canSend: func(dp distPeer) bool {
			return dp.(*peer) == pc.peer
		},
		request: func(dp distPeer) func() {
			peer := dp.(*peer)
			cost := peer.getRequestCost(GetBlockHeadersMsg, 1)
			peer.fcServer.QueuedRequest(reqID, cost)
			return func() { peer.requestHeadersByNumber(reqID, number, 1, 0, false) }
		},
	}
	var header *types.Header
	if err := pc.handler.backend.retriever.retrieve(context, reqID, rq, func(peer distPeer, msg *Msg) error {
		if msg.MsgType != MsgBlockHeaders {
			return errInvalidMessageType
		}
		headers := msg.Obj.([]*types.Header)
		if len(headers) != 1 {
			return errInvalidEntryCount
		}
		header = headers[0]
		return nil
	}, nil); err != nil {
		return nil, err
	}
	return header, nil
}

// downloaderPeerNotify implements peerSetNotify
/*type downloaderPeerNotify clientHandler

func (d *downloaderPeerNotify) registerPeer(p *peer) {
	h := (*clientHandler)(d)
	pc := &peerConnection{
		handler: h,
		peer:    p,
	}
	h.downloader.RegisterLightPeer(p.id, eth.ETH66, pc)
}

func (d *downloaderPeerNotify) unregisterPeer(p *peer) {
	h := (*clientHandler)(d)
	h.downloader.UnregisterPeer(p.id)
}
*/
