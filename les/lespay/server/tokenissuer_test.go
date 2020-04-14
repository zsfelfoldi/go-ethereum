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

package server

import (
	"math/rand"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

var tiTestSetup = TokenIssuerSetup{}

func init() {
	tiTestSetup.Init(ppTestSetup.ActiveFlag, btTestSetup.PriorityFlag, ppTestSetup.CapacityField)
}

func TestTokenPriceCalculation(t *testing.T) {
	clock := &mclock.Simulated{}
	ns := nodestate.NewNodeStateMachine(nil, nil, clock, testSetup)
	var totalAmount uint64
	ti := NewTokenIssuer(ns, tiTestSetup, clock, 1, time.Second, time.Second)
	ti.SetTotalAmountCallback(func() uint64 { return totalAmount })
	ti.SetCapacityFactor(1)
	ti.SetCapacityLimit(1000)
	totalLimit := ti.TotalTokenLimit() // 10^12

	maxDiff := int64(totalLimit / 1000000)
	// inaccuracy increases around both ends of the allowed token range
	min := totalLimit / 100
	max := uint64(float64(totalLimit) * tokenSellMaxRatio)
	for count := 0; count < 100000; count++ {
		start := min + uint64(rand.Int63n(int64(max-min)))
		stop := min + uint64(rand.Int63n(int64(max-min)))
		if start > stop {
			start, stop = stop, start
		}
		// buy (start-stop) tokens in two steps
		mid := start + uint64(rand.Int63n(int64(stop-start+1)))
		totalAmount = start
		cost, ok := ti.TokenBuyPrice(mid - start)
		if !ok {
			t.Fatalf("Failed to buy tokens")
		}
		totalAmount = mid
		cost2, ok := ti.TokenBuyPrice(stop - mid)
		if !ok {
			t.Fatalf("Failed to buy tokens")
		}
		cost += cost2

		// sell the same amount of tokens in two steps
		mid = start + uint64(rand.Int63n(int64(stop-start+1)))
		totalAmount = stop
		refund, ok := ti.TokenSellPrice(stop - mid)
		if !ok {
			t.Fatalf("Failed to sell tokens")
		}
		totalAmount = mid
		refund2, ok := ti.TokenSellPrice(mid - start)
		if !ok {
			t.Fatalf("Failed to sell tokens")
		}
		refund += refund2
		ratio := (refund + 1) / (cost + 1)
		if ratio < 0.999999 || ratio > 1.000001 {
			t.Fatalf("Token selling price does not match buy cost")
		}

		// buy tokens for the previously calculated price in two steps
		pcost := cost * rand.Float64()
		totalAmount = start
		totalAmount += ti.TokenBuyAmount(pcost)
		totalAmount += ti.TokenBuyAmount(cost - pcost)

		diff := int64(totalAmount - stop)
		if diff > maxDiff || diff < -maxDiff {
			t.Fatalf("Bought token amount mismatch")
		}

		// sell tokens for the previously calculated price in two steps
		pcost = cost * rand.Float64()
		totalAmount = stop
		soldAmount, ok := ti.TokenSellAmount(pcost)
		if !ok {
			t.Fatalf("Failed to sell tokens")
		}
		totalAmount -= soldAmount
		soldAmount, ok = ti.TokenSellAmount(cost - pcost)
		if !ok {
			t.Fatalf("Failed to sell tokens")
		}
		totalAmount -= soldAmount

		diff = int64(totalAmount - start)
		if diff > maxDiff || diff < -maxDiff {
			t.Fatalf("Sold token amount mismatch")
		}
	}
}

func TestSingleTokenPrice(t *testing.T) {
	clock := &mclock.Simulated{}
	ns := nodestate.NewNodeStateMachine(nil, nil, clock, testSetup)
	var totalAmount uint64
	ti := NewTokenIssuer(ns, tiTestSetup, clock, 1, time.Second, time.Second)
	ti.SetTotalAmountCallback(func() uint64 { return totalAmount })
	ti.SetCapacityFactor(1)
	ti.SetCapacityLimit(1000)
	totalLimit := ti.TotalTokenLimit() // 10^12

	buyLimit := uint64(float64(totalLimit) * tokenSellMaxRatio)
	for count := 0; count < 10000; count++ {
		totalAmount = uint64(rand.Int63n(int64(buyLimit)))
		relAmount := float64(totalAmount) / float64(totalLimit)
		var expPrice, maxDiff float64
		if relAmount < 0.5 {
			expPrice = relAmount * 2
			maxDiff = 0.001
		} else {
			expPrice = 0.5 / (1 - relAmount)
			maxDiff = 0.001 * expPrice
		}
		price, ok := ti.TokenBuyPrice(1)
		if !ok {
			t.Fatalf("Failed to buy tokens")
		}
		if price < expPrice-maxDiff || price > expPrice+maxDiff {
			t.Fatalf("Token price mismatch")
		}

		price, ok = ti.TokenSellPrice(1)
		if !ok {
			t.Fatalf("Failed to sell tokens")
		}
		if price < expPrice-maxDiff || price > expPrice+maxDiff {
			t.Fatalf("Token price mismatch")
		}

		if relAmount > 0.01 {
			amount := ti.TokenBuyAmount(expPrice * 100)
			if amount < 99 || amount > 101 {
				t.Fatalf("Bought token amount mismatch")
			}

			amount, ok = ti.TokenSellAmount(expPrice * 100)
			if !ok {
				t.Fatalf("Failed to sell tokens")
			}
			if amount < 99 || amount > 101 {
				t.Fatalf("Sold token amount mismatch")
			}
		}
	}
}
