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
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
)

type lotteryStatus int

const (
	lotteryNotExistent lotteryStatus = iota
	lotteryValid
	lotteryExpired
)

// lotteryCache is a wrapper of a lru cache which can cache
// lotteries status to aviod too many expensive queries.
type lotteryCache struct {
	lru *lru.Cache
}

func newLotteryCache() *lotteryCache {
	lru, _ := lru.New(256)
	return &lotteryCache{lru: lru}
}

func (c *lotteryCache) status(id common.Hash, now uint64) (lotteryStatus, *Lottery) {
	elem, exist := c.lru.Get(id)
	if !exist {
		return lotteryNotExistent, nil
	}
	lottery := elem.(*Lottery)
	if now < lottery.RevealNumber {
		return lotteryValid, lottery
	}
	return lotteryExpired, nil
}

func (c *lotteryCache) add(id common.Hash, lottery *Lottery) {
	c.lru.Add(id, lottery)
}

// ChequeDrawee represents the payment drawee in a off-chain payment channel.
// In chequeDrawee the most basic functions related to payment are defined
// here like: AddCheque.
//
// ChequeDrawee is self-contained and stateful. It will track all received
// cheques and claim the lottery if it's the lucky winner. Only AddCheque
// is exposed and needed by external caller.
//
// In addition, the structure depends on the blockchain state of the local node.
// In order to avoid inconsistency, you need to ensure that the local node has
// completed synchronization before using drawee.
type ChequeDrawee struct {
	address  common.Address       // Address used by chequeDrawee to accept payment
	cdb      *chequeDB            // Database which saves all received payments
	book     *LotteryBook         // Shared lottery contract used to verify deposit and claim payment
	opts     *bind.TransactOpts   // Signing handler for transaction signing
	cmgr     *chequeManager       // The manager for all received cheques management
	lcache   *lotteryCache        // Lottery status cache for avoid too many on-chain queries.
	cBackend bind.ContractBackend // Blockchain backend for contract interaction
	dBackend bind.DeployBackend   // Blockchain backend for contract interaction
	chain    Blockchain           // Backend for local blockchain accessing

	// Testing hooks
	onClaimedHook func(common.Hash) // onClaimedHook is called if a lottery is successfully claimed
}

// NewChequeDrawee creates a payment drawee instance which handles all payments.
func NewChequeDrawee(opts *bind.TransactOpts, address, contractAddr common.Address, chain Blockchain, cBackend bind.ContractBackend, dBackend bind.DeployBackend, db ethdb.Database) (*ChequeDrawee, error) {
	if contractAddr == (common.Address{}) {
		return nil, errors.New("empty contract address")
	}
	book, err := newLotteryBook(contractAddr, cBackend)
	if err != nil {
		return nil, err
	}
	cdb := newChequeDB(db)
	drawee := &ChequeDrawee{
		address:  address,
		cdb:      cdb,
		book:     book,
		opts:     opts,
		lcache:   newLotteryCache(),
		cBackend: cBackend,
		dBackend: dBackend,
		chain:    chain,
	}
	drawee.cmgr = newChequeManager(address, chain, book.contract, cdb, drawee.claim)
	return drawee, nil
}

func (drawee *ChequeDrawee) Close() {
	drawee.cmgr.close()
}

// AddCheque receives a cheque from the specified drawer, checks the validity
// and stores it locally. Besides, this function will return the net amount of
// this cheque.
func (drawee *ChequeDrawee) AddCheque(drawer common.Address, c *Cheque) (uint64, error) {
	if err := validateCheque(c, drawer, drawee.address, drawee.book.address); err != nil {
		return 0, err
	}
	current := drawee.chain.CurrentHeader().Number.Uint64()
	status, lottery := drawee.lcache.status(c.LotteryId, current)
	switch status {
	case lotteryNotExistent:
		l, err := drawee.book.contract.Lotteries(nil, c.LotteryId)
		if err != nil {
			return 0, err
		}
		// TODO what if the sender is deliberately attacking us
		// via sending cheques without deposit? Read status from
		// contract is not trivial.
		// Short circuit if the lottery is already revealed.
		if l.Amount == 0 {
			return 0, errors.New("invalid lottery")
		}
		if current >= l.RevealNumber {
			invalidChequeMeter.Mark(1)
			return 0, errors.New("expired lottery")
		}
		// Cache the valid lottery, skip query next time
		lottery = &Lottery{
			Amount:       l.Amount,
			RevealNumber: l.RevealNumber,
		}
		drawee.lcache.add(c.LotteryId, lottery)
	case lotteryValid:
	case lotteryExpired:
		invalidChequeMeter.Mark(1)
		return 0, errors.New("expired lottery")
	}
	var diff uint64
	stored := drawee.cdb.readCheque(drawee.address, c.Signer(), c.LotteryId, false)
	if stored != nil {
		if stored.SignedRange >= c.SignedRange {
			// There are many cases can lead to this situation:
			// * Drawer sends a stale cheque deliberately
			// * Drawer's chequedb is broken, it loses all payment history
			// No matter which reason, reject the cheque here.
			staleChequeMeter.Mark(1)
			return 0, errors.New("stale cheque")
		}
		// Figure out the net newly signed reveal range
		diff = c.SignedRange - stored.SignedRange
	} else {
		// Reject cheque if the paid amount is zero.
		if c.SignedRange == c.LowerLimit || c.SignedRange == 0 {
			invalidChequeMeter.Mark(1)
			return 0, errors.New("invalid payment amount")
		}
		diff = c.SignedRange - c.LowerLimit + 1
	}
	// It may lose precision but it's ok.
	assigned := lottery.Amount >> (len(c.Witness) - 1)

	// Note the following calculation may lose precision, but it's okish.
	//
	// In theory diff/interval WON't be very small. So it's the best choice
	// to calculate percentage first. Otherwise the calculation may overflow.
	diffAmount := uint64((float64(diff) / float64(c.UpperLimit-c.LowerLimit+1)) * float64(assigned))
	if diffAmount == 0 {
		invalidChequeMeter.Mark(1)
		return 0, errors.New("invalid payment amount")
	}
	drawee.cdb.writeCheque(drawee.address, drawer, c, false)
	drawee.cmgr.trackCheque(c, lottery.RevealNumber)
	return diffAmount, nil
}

// claim sends a on-chain transaction to claim the specified lottery.
func (drawee *ChequeDrawee) claim(context context.Context, cheque *Cheque) error {
	var proofslice [][32]byte
	for i := 1; i < len(cheque.Witness); i++ {
		var p [32]byte
		copy(p[:], cheque.Witness[i].Bytes())
		proofslice = append(proofslice, p)
	}
	start := time.Now()
	tx, err := drawee.book.contract.Claim(drawee.opts, cheque.LotteryId, cheque.RevealRange, cheque.Sig[64], common.BytesToHash(cheque.Sig[:common.HashLength]), common.BytesToHash(cheque.Sig[common.HashLength:2*common.HashLength]), cheque.ReceiverSalt, proofslice)
	if err != nil {
		return err
	}
	receipt, err := bind.WaitMined(context, drawee.dBackend, tx)
	if err != nil {
		return err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return ErrTransactionFailed
	}
	if drawee.onClaimedHook != nil {
		drawee.onClaimedHook(cheque.LotteryId)
	}
	claimDurationTimer.UpdateSince(start)
	log.Debug("Claimed lottery", "id", cheque.LotteryId)
	return nil
}

// Cheques returns all active cheques locally received.
func (drawee *ChequeDrawee) Cheques() []*WrappedCheque {
	return drawee.cmgr.activeCheques()
}
