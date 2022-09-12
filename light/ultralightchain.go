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

// Package light implements on-demand retrieval capable state and chain objects
// for the Ethereum Light Client.
package light

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type UlcPeerInfo struct {
	lock                   sync.RWMutex
	announcedBeaconHeads   [4]common.Hash
	announcedBeaconHeadPtr int
}

func (u *UlcPeerInfo) AddBeaconHead(beaconHead common.Hash) {
	u.lock.Lock()
	defer u.lock.Unlock()

	for _, h := range u.announcedBeaconHeads {
		if h == beaconHead {
			return
		}
	}
	u.announcedBeaconHeads[u.announcedBeaconHeadPtr] = beaconHead
	u.announcedBeaconHeadPtr++
	if u.announcedBeaconHeadPtr == len(u.announcedBeaconHeads) {
		u.announcedBeaconHeadPtr = 0
	}
}

func (u *UlcPeerInfo) HasBeaconHead(beaconHead common.Hash) bool {
	u.lock.RLock()
	defer u.lock.RUnlock()

	for _, bh := range u.announcedBeaconHeads {
		if bh == beaconHead {
			return true
		}
	}
	return false
}
