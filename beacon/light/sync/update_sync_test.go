// Copyright 2023 The go-ethereum Authors
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

package sync

import (
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
)

var config = (&types.ChainConfig{ //TODO
	GenesisValidatorsRoot: common.HexToHash("0x4b363db94e286120d76eb905340fdd4e54bfe9f06bf33ff6cf5ad27f511bfe95"),
	GenesisTime:           1606824023,
}).
	AddFork("GENESIS", 0, []byte{0, 0, 0, 0}).
	AddFork("ALTAIR", 74240, []byte{1, 0, 0, 0})

func TestCheckpointSync(t *testing.T) {
	//TODO allow partial updates, ??enforce time, head announcement order
	clock := &mclock.Simulated{}
	server := newTestNode(config, clock, common.Hash{})
	for period := uint64(0); period <= 10; period++ {
		committee := light.GenerateTestCommittee()
		server.committeeChain.AddFixedCommitteeRoot(period, committee.Root())
		server.committeeChain.AddCommittee(period, committee)
	}
	for period := uint64(0); period < 10; period++ {
		server.committeeChain.InsertUpdate(light.GenerateTestUpdate(config, period, server.committeeChain.GetCommittee(period), server.committeeChain.GetCommittee(period+1), 400, true), nil)
	}
	checkpoint := light.GenerateTestCheckpoint(3, server.committeeChain.GetCommittee(3))
	server.checkpointStore.Store(checkpoint)

	client := newTestNode(config, clock, checkpoint.Header.Hash())

	a, _ := server.committeeChain.NextSyncPeriod()
	b, _ := client.committeeChain.NextSyncPeriod()
	fmt.Println(a, b)

	client.scheduler.RegisterServer(server)
	clock.Run(time.Second)
	head := types.Header{Slot: types.SyncPeriodStart(8)}
	server.setHead(head)
	server.setSignedHead(light.GenerateTestSignedHeader(head, config, server.committeeChain.GetCommittee(8), head.Slot+1, 400))
	clock.Run(time.Second)
	//scheduler.Stop()

	a, _ = server.committeeChain.NextSyncPeriod()
	b, _ = client.committeeChain.NextSyncPeriod()
	fmt.Println(a, b)
	//TODO fix and finish this test
}
