// Copyright 2020 The go-ethereum Authors
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

package lotterybook

import (
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook/contract"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

func TestStateTransition(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	_, _, c, err := contract.DeployLotteryBook(bind.NewKeyedTransactor(env.drawerKey), env.backend)
	if err != nil {
		t.Fatalf("Failed to deploy contract: %v", err)
	}
	env.backend.Commit()
	cdb := newChequeDB(rawdb.NewMemoryDatabase())
	mgr := newLotteryManager(env.drawerAddr, env.backend.Blockchain(), c, cdb)
	defer mgr.close()

	events := make(chan []LotteryEvent, 1024)
	eventSub := mgr.subscribeLotteryEvent(events)
	defer eventSub.Unsubscribe()

	_, _ = mgr.activeLotteris() // Ensure internal initialization is done
	current := env.backend.Blockchain().CurrentHeader().Number.Uint64()
	l, _, _, _ := env.newRawLottery([]common.Address{env.draweeAddr}, []uint64{128}, 30)
	var cases = []struct {
		testFn func()
		expect []LotteryEvent
	}{
		{func() { mgr.trackLottery(l) }, []LotteryEvent{{Id: l.Id, Status: LotteryPending}}},
		{func() { env.commitEmptyBlocks(lotteryProcessConfirms) }, []LotteryEvent{{Id: l.Id, Status: LotteryActive}}},
		{func() { env.commitEmptyUntil(current + 30) }, []LotteryEvent{{Id: l.Id, Status: LotteryRevealed}}},
		{func() { env.commitEmptyUntil(current + 30 + lotteryClaimPeriod + lotteryProcessConfirms) }, []LotteryEvent{{Id: l.Id, Status: LotteryExpired}}},
	}
	for index, c := range cases {
		c.testFn()
		if !env.checkEvent(events, c.expect) {
			t.Fatalf("Case %d failed", index)
		}
	}
	lotteries, err := mgr.expiredLotteris()
	if err != nil {
		t.Fatalf("Failed to retrieve lotteries :%v", err)
	}
	if len(lotteries) != 1 && lotteries[0].Id != l.Id {
		t.Fatal("Expect to retrieve expired lottery")
	}
}

func TestStateRecovery(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	_, _, c, err := contract.DeployLotteryBook(bind.NewKeyedTransactor(env.drawerKey), env.backend)
	if err != nil {
		t.Fatalf("Failed to deploy contract: %v", err)
	}
	env.backend.Commit()
	cdb := newChequeDB(rawdb.NewMemoryDatabase())
	mgr := newLotteryManager(env.drawerAddr, env.backend.Blockchain(), c, cdb)

	current := env.backend.Blockchain().CurrentHeader().Number.Uint64()
	l1, _, _, _ := env.newRawLottery([]common.Address{env.draweeAddr}, []uint64{128}, 30)
	l2, _, _, _ := env.newRawLottery([]common.Address{env.draweeAddr}, []uint64{128}, 40)
	cdb.writeLottery(env.drawerAddr, l1.Id, false, l1)
	cdb.writeLottery(env.drawerAddr, l2.Id, false, l2)

	// Close and restart
	mgr.close()
	mgr = newLotteryManager(env.drawerAddr, env.backend.Blockchain(), c, cdb)
	active, err := mgr.activeLotteris()
	if err != nil {
		t.Fatalf("Failed to retrieve active lotteries: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("Expect has 2 active lotteris")
	}
	env.commitEmptyUntil(current + 40 + lotteryClaimPeriod + lotteryProcessConfirms)
	expired, err := mgr.expiredLotteris()
	if err != nil {
		t.Fatalf("Failed to retrieve active lotteries: %v", err)
	}
	if len(expired) != 2 {
		t.Fatalf("Expect has 2 expired lotteris")
	}
}
