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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

type peerConnection interface {
	peerConnected(*peer) (func(), error)
}

type peerHandshake interface {
	sendHandshake(*peer, *keyValueList)
	receiveHandshake(*peer, keyValueMap) error
}

/*type messageHandler interface {
	peerConnected(*peer)
	peerDisconnected(*peer)
}*/

type handler struct {
}

func newHandler() *handler {

}

// runPeer is the p2p protocol run function for the given version.
func (h *handler) runPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter) error {
	peer := newPeer(int(version), h.server.config.NetworkId, p, newMeteredMsgWriter(rw, int(version)))
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
		wg.Wait() // Ensure all background task routines have exited.
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
	p.Log().Trace("Light Ethereum message arrived", "code", msg.Code, "bytes", msg.Size)
}
