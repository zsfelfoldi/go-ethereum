// Copyright 2018 The go-ethereum Authors
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
	"sync"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

var (
	ErrMinBW   = errors.New("bandwidth too small")
	ErrTotalBW = errors.New("total bandwidth exceeded")
)

// PublicLesServerAPI  provides an API to access the les server.
// It offers only methods that operate on public data that is freely available to anyone.
type PrivateLesServerAPI struct {
	server *LesServer
	pm     *ProtocolManager
	vip    *vipClientPool
}

// NewPublicLesServerAPI creates a new les server API.
func NewPrivateLesServerAPI(server *LesServer) *PrivateLesServerAPI {
	vip := &vipClientPool{
		clients: make(map[enode.ID]vipClientInfo),
		totalBw: server.totalBandwidth,
		pm:      server.protocolManager,
	}
	server.protocolManager.vipClientPool = vip
	return &PrivateLesServerAPI{
		server: server,
		pm:     server.protocolManager,
		vip:    vip,
	}
}

func (api *PrivateLesServerAPI) BandwidthLimits() (total, min uint64) {
	return api.server.totalBandwidth, api.server.minBandwidth
}

type vipClientInfo struct {
	bw        uint64
	connected bool
	updateBw  func(uint64)
}

type vipClientPool struct {
	lock                                  sync.Mutex
	pm                                    *ProtocolManager
	clients                               map[enode.ID]vipClientInfo
	totalBw, totalVipBw, totalConnectedBw uint64
	connectedCount                        int
}

func (api *PrivateLesServerAPI) AssignBandwidth(id enode.ID, bw uint64) error {
	if bw < api.server.minBandwidth {
		return ErrMinBW
	}

	api.vip.lock.Lock()
	defer api.vip.lock.Unlock()

	c := api.vip.clients[id]
	if api.vip.totalVipBw+bw > api.vip.totalBw+c.bw {
		return ErrTotalBW
	}
	api.vip.totalVipBw += bw - c.bw
	if c.connected {
		api.vip.totalConnectedBw += bw - c.bw
		api.pm.clientPool.setConnLimit(api.pm.maxFreePeers(api.vip.connectedCount, api.vip.totalConnectedBw))
		c.updateBw(bw)
	}
	c.bw = bw
	api.vip.clients[id] = c
	return nil
}

func (v *vipClientPool) connect(id enode.ID, updateBw func(uint64)) bool {
	v.lock.Lock()
	defer v.lock.Unlock()

	c := v.clients[id]
	if c.bw == 0 || c.connected {
		return false
	}
	c.bw = 50000
	c.connected = true
	c.updateBw = updateBw
	v.clients[id] = c
	v.connectedCount++
	v.totalConnectedBw += c.bw
	v.pm.clientPool.setConnLimit(v.pm.maxFreePeers(v.connectedCount, v.totalConnectedBw))
	/*go func() {
		for {
			updateBw(100000)
			time.Sleep(time.Second * 20)
			updateBw(1000)
			time.Sleep(time.Second * 20)
		}
	}()*/
	return true
}

func (v *vipClientPool) disconnect(id enode.ID) {
	v.lock.Lock()
	defer v.lock.Unlock()

	c := v.clients[id]
	if c.bw == 0 || !c.connected {
		return
	}
	c.connected = false
	v.clients[id] = c
	v.connectedCount--
	v.totalConnectedBw -= c.bw
	v.pm.clientPool.setConnLimit(v.pm.maxFreePeers(v.connectedCount, v.totalConnectedBw))
}
