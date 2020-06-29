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
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook/contract"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/log"
)

var errChequeManagerClosed = errors.New("cheque manager closed")

// WrappedCheque wraps the cheque and an additional field.
type WrappedCheque struct {
	Cheque       *Cheque
	RevealNumber uint64
}

// chequeManager the manager of received cheques for all life cycle management.
type chequeManager struct {
	address  common.Address
	chain    Blockchain
	contract *contract.LotteryBook
	cdb      *chequeDB
	queryCh  chan chan []*WrappedCheque
	chequeCh chan *WrappedCheque
	closeCh  chan struct{}
	wg       sync.WaitGroup
	claim    func(context.Context, *Cheque) error
}

// newChequeManager returns an instance of cheque manager and starts
// the underlying routines for status management.
func newChequeManager(address common.Address, chain Blockchain, contract *contract.LotteryBook, cdb *chequeDB, claim func(context.Context, *Cheque) error) *chequeManager {
	mgr := &chequeManager{
		address:  address,
		chain:    chain,
		contract: contract,
		cdb:      cdb,
		queryCh:  make(chan chan []*WrappedCheque),
		chequeCh: make(chan *WrappedCheque),
		closeCh:  make(chan struct{}),
		claim:    claim,
	}
	mgr.wg.Add(1)
	go mgr.run()
	return mgr
}

// run starts a background routine for cheques management.
func (m *chequeManager) run() {
	defer m.wg.Done()

	// Establish subscriptions
	newHeadCh := make(chan core.ChainHeadEvent, 1024)
	sub := m.chain.SubscribeChainHeadEvent(newHeadCh)
	if sub == nil {
		return
	}
	defer sub.Unsubscribe()

	var (
		current = m.chain.CurrentHeader().Number.Uint64()
		active  = make(map[uint64][]*Cheque)
	)
	// checkAndClaim checks whether the cheque is the winner or not.
	// If so, claim the corresponding lottery via sending on-chain
	// transaction.
	checkAndClaim := func(cheque *Cheque, hash common.Hash) (err error) {
		defer func() {
			// No matter we aren't the lucky winner or we already claim
			// the lottery, delete the record anyway. Keep it if any error
			// occurs.
			if err == nil {
				m.cdb.deleteCheque(m.address, cheque.Signer(), cheque.LotteryId, false)
			}
		}()
		if !cheque.reveal(hash) {
			loseLotteryGauge.Inc(1)
			return nil
		}
		// todo(rjl493456442) if any error occurs(but we are the lucky winner), re-try
		// is necesssary.
		winLotteryGauge.Inc(1)
		ctx, cancelFn := context.WithTimeout(context.Background(), txTimeout)
		defer cancelFn()
		return m.claim(ctx, cheque)
	}
	// Read all stored cheques received locally
	cheques, drawers := m.cdb.listCheques(m.address, nil)
	for index, cheque := range cheques {
		ret, err := m.contract.Lotteries(nil, cheque.LotteryId)
		if err != nil {
			log.Error("Failed to retrieve corresponding lottery", "error", err)
			continue
		}
		// If the amount of corresponding lottery is 0, it means the lottery
		// is claimed or reset by someone, just delete it.
		if ret.Amount == 0 {
			log.Debug("Lottery is claimed")
			m.cdb.deleteCheque(m.address, drawers[index], cheque.LotteryId, false)
			continue
		}
		// The valid claim block range is [revealNumber+1, revealNumber+256].
		// However the head block can be reorged with very high chance. So
		// a small processing confirms is applied to ensure the reveal hash
		// is stable enough.
		//
		// For receiver, the reasonable claim range (revealNumber+6, revealNumber+256].
		if current < ret.RevealNumber+lotteryProcessConfirms {
			active[ret.RevealNumber] = append(active[ret.RevealNumber], cheque)
		} else if current < ret.RevealNumber+lotteryClaimPeriod {
			// Lottery can still be claimed, try it!
			revealHash := m.chain.GetHeaderByNumber(ret.RevealNumber)

			// Create an independent routine to claim the lottery.
			// This function may takes very long time, don't block
			// the entire thread here. It's ok to spin up routines
			// blindly here, there won't have too many cheques to claim.
			go checkAndClaim(cheque, revealHash.Hash())
		} else {
			// Lottery is already out of claim window, delete it.
			log.Debug("Cheque expired", "lotteryid", cheque.LotteryId)
			m.cdb.deleteCheque(m.address, drawers[index], cheque.LotteryId, false)
		}
	}
	for {
		select {
		case ev := <-newHeadCh:
			current = ev.Block.NumberU64()
			for revealAt, cheques := range active {
				// Short circuit if they are still active lotteries.
				if current < revealAt+lotteryProcessConfirms {
					continue
				}
				// Wipe all cheques if they are already stale.
				if current >= revealAt+lotteryClaimPeriod {
					for _, cheque := range cheques {
						m.cdb.deleteCheque(m.address, cheque.Signer(), cheque.LotteryId, false)
					}
					delete(active, revealAt)
					continue
				}
				revealHash := m.chain.GetHeaderByNumber(revealAt).Hash()
				for _, cheque := range cheques {
					// Create an independent routine to claim the lottery.
					// This function may takes very long time, don't block
					// the entire thread here. It's ok to spin up routines
					// blindly here, there won't have too many cheques to claim.
					go checkAndClaim(cheque, revealHash)
				}
				delete(active, revealAt)
			}

		case req := <-m.chequeCh:
			var replaced bool
			cheques := active[req.RevealNumber]
			for index, cheque := range cheques {
				if cheque.LotteryId == req.Cheque.LotteryId {
					cheques[index] = req.Cheque // Replace the original one
					replaced = true
					break
				}
			}
			if !replaced {
				active[req.RevealNumber] = append(active[req.RevealNumber], req.Cheque)
			}

		case retCh := <-m.queryCh:
			var ret []*WrappedCheque
			for revealAt, cheques := range active {
				for _, cheque := range cheques {
					ret = append(ret, &WrappedCheque{
						Cheque:       cheque,
						RevealNumber: revealAt,
					})
				}
			}
			retCh <- ret

		case <-m.closeCh:
			return
		}
	}
}

// trackCheque adds a newly received cheque for life cycle management.
func (m *chequeManager) trackCheque(cheque *Cheque, revealAt uint64) error {
	select {
	case m.chequeCh <- &WrappedCheque{Cheque: cheque, RevealNumber: revealAt}:
		return nil
	case <-m.closeCh:
		return errChequeManagerClosed
	}
}

// activeCheques returns all active cheques received which is
// waiting for reveal.
func (m *chequeManager) activeCheques() []*WrappedCheque {
	reqCh := make(chan []*WrappedCheque, 1)
	select {
	case m.queryCh <- reqCh:
		return <-reqCh
	case <-m.closeCh:
		return nil
	}
}

func (m *chequeManager) close() {
	close(m.closeCh)
	m.wg.Wait()
}
