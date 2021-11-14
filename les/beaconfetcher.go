// Copyright 2021 The go-ethereum Authors
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
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/fetcher"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

type beaconFetcher struct {
	head      beaconHead
	requestCh chan uint64
	chain     *light.LightChain
}

type beaconHead struct {
	hash    common.Hash
	slot    uint64
	signers int
}

func (bf *beaconFetcher) announced(head beaconHead) {
	bf.lock.Lock()
	defer bf.lock.Unlock()

	if head.slot < bf.head.slot || (head.slot == bf.head.slot && head.signers <= bf.head.signers) {
		return
	}
	bf.head = head
	if bf.requestCh != nil {
		return
	}
	ch := make(chan uint64, 1)
	bf.requestCh = ch
	bf.chain.NewHeadOnDemand(bf.requestCh)
	go func() {
		select {
		case updateFrom := <-ch:
			bf.updateChain(updateFrom)
		case <-bf.quit:
			return
		}
	}()
}

func (bf *beaconFetcher) updateChain(from uint64) {
	bf.lock.Lock()
	head := bf.head
	bf.lock.Unlock()
	oldHead := bf.chain.CurrentHeader().Hash()
	request := &light.HeaderRequest{
		Type:      light.BeaconProof,
		NewHead:   head.hash,
		OldHead:   oldHead,
		MaxAmount: maxHeaderAmount,
	}
	for err:=bf.odr.Retrieve(context.Background, request); err!=nil {
		if err
	}
}
