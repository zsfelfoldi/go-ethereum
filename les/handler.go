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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/p2p"
)

type handshakeModule interface {
	sendHandshake(*peer, *keyValueList)
	receiveHandshake(*peer, keyValueMap) error
}

type connectionModule interface {
	peerConnected(*peer) (func(), error)
}

type messageHandlerModule interface {
	messageHandlers() messageHandlers
}

type messageHandler func(*peer, p2p.Msg) error

type messageHandlerWithCodeAndVersion struct {
	code                      uint64
	firstVersion, lastVersion int
	handler                   messageHandler
}

type messageHandlers []messageHandlerWithCodeAndVersion

type codeAndVersion struct {
	code    uint64
	version int
}

type handler struct {
	handshakeModules  []handshakeModule
	connectionModules []connectionModule
	messageHandlers   map[codeAndVersion]messageHandler
	networkId         uint64

	closeCh chan struct{}
	wg      sync.WaitGroup
}

func newHandler(networkId uint64) *handler {
	return &handler{
		networkId:       networkId,
		closeCh:         make(chan struct{}),
		messageHandlers: make(map[codeAndVersion]messageHandler),
	}
}

// stop stops the protocol handler.
func (h *handler) stop() {
	close(h.closeCh)
	h.wg.Wait()
}

func (h *handler) registerHandshakeModule(m handshakeModule) {
	h.handshakeModules = append(h.handshakeModules, m)
}

func (h *handler) registerConnectionModule(m connectionModule) {
	h.connectionModules = append(h.connectionModules, m)
}

func (h *handler) registerMessageHandlers(handlers messageHandlers) {
	for _, hh := range handlers {
		for version := hh.firstVersion; version <= hh.lastVersion; version++ {
			h.messageHandlers[codeAndVersion{code: hh.code, version: version}] = hh.handler
		}
	}
}

// runPeer is the p2p protocol run function for the given version.
func (h *handler) runPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter) error {
	peer := newPeer(int(version), h.networkId, p, newMeteredMsgWriter(rw, int(version)))
	defer peer.close()
	h.wg.Add(1)
	defer h.wg.Done()
	return h.handle(peer)
}

func (h *handler) handle(p *peer) error {
	p.Log().Debug("Light Ethereum peer connected", "name", p.Name())

	p.connectedAt = mclock.Now()
	if err := p.handshake(h.handshakeModules); err != nil {
		p.Log().Debug("Light Ethereum handshake failed", "err", err)
		return err
	}

	var discFns []func()
	defer func() {
		for i := len(discFns) - 1; i >= 0; i-- {
			discFns[i]()
		}
	}()

	for _, m := range h.connectionModules {
		discFn, err := m.peerConnected(p)
		if discFn != nil {
			discFns = append(discFns, discFn)
		}
		if err != nil {
			return err
		}
	}

	defer func() {
		//wg.Wait() // Ensure all background task routines have exited. //TODO ???
		connectionTimer.Update(time.Duration(mclock.Now() - p.connectedAt))
	}()

	for {
		select {
		case err := <-p.errCh:
			p.Log().Debug("Protocol handler error", "err", err)
			return err
		default:
		}
		if err := h.handleMsg(p); err != nil {
			p.Log().Debug("Light Ethereum message handling failed", "err", err)
			return err
		}
	}
}

// handleMsg is invoked whenever an inbound message is received from a remote
// peer. The remote connection is torn down upon returning any error.
func (h *handler) handleMsg(p *peer) error {
	// Read the next message from the remote peer, and ensure it's fully consumed
	msg, err := p.rw.ReadMsg()
	if err != nil {
		return err
	}
	defer msg.Discard()
	p.Log().Trace("Light Ethereum message arrived", "code", msg.Code, "bytes", msg.Size)
	if msg.Size > ProtocolMaxMsgSize {
		return errResp(ErrMsgTooLarge, "%v > %v", msg.Size, ProtocolMaxMsgSize)
	}

	if handler, ok := h.messageHandlers[codeAndVersion{code: msg.Code, version: p.version}]; ok {
		return handler(p, msg)
	} else {
		p.Log().Trace("Received invalid message", "code", msg.Code, "protocolVersion", p.version)
		return errResp(ErrInvalidMsgCode, "code: %v  protocolVersion: &v", msg.Code, p.version)
	}
}
