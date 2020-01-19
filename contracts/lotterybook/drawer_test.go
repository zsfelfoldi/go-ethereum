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
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestCreateLottery(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	var exit = make(chan struct{})
	defer close(exit)

	// Start the automatic blockchain.
	go func() {
		ticker := time.NewTicker(time.Millisecond * 10)
		for {
			select {
			case <-ticker.C:
				env.backend.Commit()
			case <-exit:
				return
			}
		}
	}()
	drawer, err := NewChequeDrawer(env.drawerAddr, env.contractAddr, bind.NewKeyedTransactor(env.drawerKey), nil, env.backend.Blockchain(), env.backend, env.backend, env.drawerDb)
	if err != nil {
		t.Fatalf("Faield to create drawer, err: %v", err)
	}
	defer drawer.Close()
	drawer.keySigner = func(data []byte) ([]byte, error) {
		sig, _ := crypto.Sign(data, env.drawerKey)
		return sig, nil
	}
	var cases = []struct {
		payers    []common.Address
		amounts   []uint64
		reveal    uint64
		expectErr bool
	}{
		{nil, nil, 10086, true},
		{[]common.Address{env.draweeAddr}, []uint64{128}, 10086, false},
		{[]common.Address{env.drawerAddr}, []uint64{128}, 10086, false},
		{[]common.Address{env.drawerAddr, env.draweeAddr}, []uint64{128, 128}, 10086, false},
		{[]common.Address{env.draweeAddr}, []uint64{128}, 1, true},
	}
	for index, c := range cases {
		_, err := drawer.createLottery(context.Background(), c.payers, c.amounts, c.reveal)
		if c.expectErr {
			if err == nil {
				t.Fatalf("Case %d expect error, got nil", index)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Case %d expect no error: %v", index, err)
		}
	}
}

func TestIssueCheque(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	var exit = make(chan struct{})
	defer close(exit)

	// Start the automatic blockchain.
	go func() {
		ticker := time.NewTicker(time.Millisecond * 10)
		for {
			select {
			case <-ticker.C:
				env.backend.Commit()
			case <-exit:
				return
			}
		}
	}()
	drawer, err := NewChequeDrawer(env.drawerAddr, env.contractAddr, bind.NewKeyedTransactor(env.drawerKey), nil, env.backend.Blockchain(), env.backend, env.backend, env.drawerDb)
	if err != nil {
		t.Fatalf("Faield to create drawer, err: %v", err)
	}
	defer drawer.Close()
	drawer.keySigner = func(data []byte) ([]byte, error) {
		sig, _ := crypto.Sign(data, env.drawerKey)
		return sig, nil
	}
	id, err := drawer.createLottery(context.Background(), []common.Address{env.draweeAddr}, []uint64{128}, 10086)
	if err != nil {
		t.Fatalf("Faield to create lottery, err: %v", err)
	}
	id2, err := drawer.createLottery(context.Background(), []common.Address{env.draweeAddr, env.drawerAddr}, []uint64{40, 60}, 10086)
	if err != nil {
		t.Fatalf("Faield to create lottery, err: %v", err)
	}
	id3, err := drawer.createLottery(context.Background(), []common.Address{env.draweeAddr, env.drawerAddr}, []uint64{40, 45}, 10086)
	if err != nil {
		t.Fatalf("Faield to create lottery, err: %v", err)
	}
	var cases = []struct {
		payer     common.Address
		id        common.Hash
		amount    uint64
		expectErr bool
	}{
		// Lottery 1
		{env.draweeAddr, id, 0, true},
		{env.draweeAddr, id, 32, false},
		{env.draweeAddr, id, 96, false},
		{env.draweeAddr, id, 32, true},
		// Lottery 2, each one has 50 assigned
		{env.draweeAddr, id2, 25, false},
		{env.draweeAddr, id2, 25, false},
		{env.draweeAddr, id2, 1, true},
		// Lottery 3, each one has 42.5 assigned
		{env.draweeAddr, id3, 21, false},
		{env.draweeAddr, id3, 21, false},
		{env.draweeAddr, id3, 1, true},
	}
	for index, c := range cases {
		_, err := drawer.issueCheque(c.payer, c.id, c.amount)
		if c.expectErr && err == nil {
			t.Fatalf("Case %d expect error, got nil", index)
		}
		if !c.expectErr && err != nil {
			t.Fatalf("Case %d expect no error: %v", index, err)
		}
	}
}

func TestAllowance(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	var exit = make(chan struct{})
	defer close(exit)

	// Start the automatic blockchain.
	go func() {
		ticker := time.NewTicker(time.Millisecond * 10)
		for {
			select {
			case <-ticker.C:
				env.backend.Commit()
			case <-exit:
				return
			}
		}
	}()
	drawer, err := NewChequeDrawer(env.drawerAddr, env.contractAddr, bind.NewKeyedTransactor(env.drawerKey), nil, env.backend.Blockchain(), env.backend, env.backend, env.drawerDb)
	if err != nil {
		t.Fatalf("Faield to create drawer, err: %v", err)
	}
	defer drawer.Close()
	drawer.keySigner = func(data []byte) ([]byte, error) {
		sig, _ := crypto.Sign(data, env.drawerKey)
		return sig, nil
	}
	id, err := drawer.createLottery(context.Background(), []common.Address{env.draweeAddr, env.drawerAddr}, []uint64{128, 128}, 10086)
	if err != nil {
		t.Fatalf("Faield to create lottery, err: %v", err)
	}
	var cases = []struct {
		issueFn         func()
		expectAllowance map[common.Address]uint64
	}{
		{nil, map[common.Address]uint64{env.draweeAddr: 128, env.drawerAddr: 128}},
		{func() {
			drawer.issueCheque(env.draweeAddr, id, 32)
		}, map[common.Address]uint64{env.draweeAddr: 96, env.drawerAddr: 128}},
		{func() {
			drawer.issueCheque(env.draweeAddr, id, 96)
		}, map[common.Address]uint64{env.draweeAddr: 0, env.drawerAddr: 128}},
	}
	for _, c := range cases {
		if c.issueFn != nil {
			c.issueFn()
		}
		allowance := drawer.Allowance(id)
		if !reflect.DeepEqual(allowance, c.expectAllowance) {
			t.Fatalf("Allowance mismatch, want: %v, got: %v", c.expectAllowance, allowance)
		}
	}
}

func TestEstimatedExpiry(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	// Start the automatic blockchain.
	var exit = make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Millisecond * 500)
		for {
			select {
			case <-ticker.C:
				env.backend.Commit()
			case <-exit:
				return
			}
		}
	}()
	drawer, err := NewChequeDrawer(env.drawerAddr, env.contractAddr, bind.NewKeyedTransactor(env.drawerKey), nil, env.backend.Blockchain(), env.backend, env.backend, env.drawerDb)
	if err != nil {
		t.Fatalf("Faield to create drawer, err: %v", err)
	}
	defer drawer.Close()
	drawer.keySigner = func(data []byte) ([]byte, error) {
		sig, _ := crypto.Sign(data, env.drawerKey)
		return sig, nil
	}
	id, err := drawer.createLottery(context.Background(), []common.Address{env.draweeAddr, env.drawerAddr}, []uint64{128, 128}, 10086)
	if err != nil {
		t.Fatalf("Faield to create lottery, err: %v", err)
	}
	close(exit)

	var cases = []struct {
		prepare    func()
		id         common.Hash
		expectZero bool
	}{
		{nil, common.HexToHash("deadbeef"), true},
		{nil, id, false},
		{func() { env.commitEmptyBlocks(10) }, id, false},
	}
	for _, c := range cases {
		want := 10086 - env.backend.Blockchain().CurrentBlock().NumberU64()
		if c.expectZero {
			want = 0
		}
		if got := drawer.EstimatedExpiry(c.id); got != want {
			t.Fatalf("Mismatch, want %d, got %d", want, got)
		}
	}
}
