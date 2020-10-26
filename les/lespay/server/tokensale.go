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
	"math"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/lespay"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

type TokenSale struct {
	server                        *Server
	bt                            *BalanceTracker
	clock                         mclock.Clock
	lock                          sync.Mutex
	lastUpdate                    mclock.AbsTime
	bc                            utils.BondingCurve
	maxLimit                      int64
	limitAdjustRate, bpAdjustRate float64 // per nanosecond
	minLogBasePrice               utils.Fixed64

	totalTokenAmount func() int64
	currencyId       string
}

func NewTokenSale(server *Server, service ServiceWithBalance, clock mclock.Clock, minBasePrice float64) *TokenSale {
	minLogBasePrice := utils.Float64ToFixed64(math.Log2(minBasePrice))
	return &TokenSale{
		server:          server,
		service:         service,
		clock:           clock,
		lastUpdate:      clock.Now(),
		bc:              utils.NewBondingCurve(utils.LinHyperCurve, 1, minLogBasePrice),
		minLogBasePrice: minLogBasePrice,
	}
}

// assumes that no new tokes have been added; should be called before token purchase
// relAmount is never increased
func (t *TokenSale) adjustCurve() {
	now := t.clock.Now()
	dt := now - t.lastUpdate
	t.lastUpdate = now

	targetLimit := t.bc.TokenLimit() + int64(t.limitAdjustRate*float64(dt))
	if targetLimit > t.maxLimit {
		targetLimit = t.maxLimit
	}
	bpAdjust := t.bpAdjustRate * (float64(t.bc.TokenAmount())/float64(t.bc.TokenLimit())*2 - 1)
	targetLogBasePrice := t.bc.LogBasePrice() + utils.Fixed64(bpAdjust*float64(dt))
	if targetLogBasePrice < t.minLogBasePrice {
		targetLogBasePrice = t.minLogBasePrice
	}
	t.bc.Adjust(t.totalTokenAmount(), targetLimit, targetLogBasePrice)
}

func (t *TokenSale) SetLimitAdjustRate(limitAdjustRate float64) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.adjustCurve()
	t.limitAdjustRate = limitAdjustRate
}

func (t *TokenSale) SetBasePriceAdjustRate(bpAdjustRate float64) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.adjustCurve()
	t.bpAdjustRate = bpAdjustRate * float64(utils.Float64ToFixed64(1))
}

func (t *TokenSale) SetMaxLimit(maxLimit int64) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.adjustCurve()
	t.maxLimit = maxLimit
}

func (t *TokenSale) Handle(id enode.ID, address string, name string, data []byte) []byte {
	switch name {
	case lespay.ExchangeName:
		return t.serveExchange(id, data)
	case lespay.PriceQueryName:
		return t.servePriceQuery(id, data)
	}
	return nil
}

func (t *TokenSale) serveExchange(id enode.ID, data []byte) []byte {
	var (
		req    lespay.ExchangeReq
		result lespay.ExchangeResp
	)
	if rlp.DecodeBytes(data, &req) != nil {
		return nil
	}

	pmService, _ := t.server.Resolve(req.PaymentId).(PaymentService)
	if pmService == nil || pmService.CurrencyId() != t.currencyId {
		return nil
	}
	currencyBalance := pmService.GetBalance(id) // big.Int
	tokenBalance := t.service.GetBalance(id)    // int64

	t.lock.Lock()
	defer t.lock.Unlock()
	t.adjustCurve()

	minTokens, maxTokens := req.MinTokens.Int64(), req.MaxTokens.Int64()
	if minTokens < -tokenBalance {
		minTokens = -tokenBalance
	}
	mci := req.MaxCurrency.Inf()
	if minTokens <= maxTokens && mci != -1 {
		var maxCurrency *big.Int
		if mci == 0 {
			maxCurrency = req.MaxCurrency.BigInt()
		}
		tokensEx, currencyEx := t.bc.Exchange(minTokens, maxTokens, maxCurrency)
		if currencyEx != nil {
			tokenBalance += tokensEx
			currencyBalance.Add(currencyBalance, currencyEx)
			if tokenBalance < 0 || currencyBalance.Sign() == -1 {
				panic(nil)
			}
			t.service.AddBalance(id, tokensEx)
			pmService.AddBalance(id, currencyEx)
			result.TokensEx.SetInt64(tokensEx)
			result.CurrencyEx.SetBigInt(currencyEx)
		}
	}
	result.TokenBalance.SetInt64(tokenBalance)
	result.CurrencyBalance.SetBigInt(currencyBalance)
	res, _ := rlp.EncodeToBytes(&result)
	return res
}

func (t *TokenSale) servePriceQuery(id enode.ID, data []byte) []byte {
	var req lespay.PriceQueryReq
	if rlp.DecodeBytes(data, &req) != nil {
		return nil
	}

	pmService, _ := t.server.Resolve(req.PaymentId).(PaymentService)
	if pmService == nil || pmService.CurrencyId() != t.currencyId {
		return nil
	}

	t.lock.Lock()
	defer t.lock.Unlock()
	t.adjustCurve()
	result := make(lespay.PriceQueryResp, len(req.TokenAmounts)) //TODO limit length
	for i, amount := range req.TokenAmounts {
		if price := t.bc.Price(amount.Int64()); price != nil {
			result[i].SetBigInt(price)
		} else {
			result[i].SetInf(1)
		}
	}
	res, _ := rlp.EncodeToBytes(&result)
	return res
}
