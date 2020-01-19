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
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook/contract"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
)

var errLotteryManagerClosed = errors.New("lottery manager closed")

const (
	activeLotteryQuery = iota
	expiredLotteryQuery
)

type queryReq struct {
	typ int
	ret chan []*Lottery
}

// lotteryManager the manager of local create lotteries for
// all life cycle management.
type lotteryManager struct {
	address     common.Address        // The address of payment sender
	chainReader Blockchain            // The instance use to access local chain
	contract    *contract.LotteryBook // The instance of lottery contract
	cdb         *chequeDB             // The database used to store all payment data
	scope       event.SubscriptionScope
	lotteryFeed event.Feed

	// Lottery sets
	pendingSet  map[uint64][]*Lottery    // A set of pending lotteries, the key is block height of creation
	activeSet   map[uint64][]*Lottery    // A set of active lotteries, the key is block height of reveal
	revealedSet map[common.Hash]*Lottery // A set of revealed lotteries
	expiredSet  map[common.Hash]*Lottery // A set of expired lotteries

	// Channels
	lotteryCh chan *Lottery
	queryCh   chan *queryReq
	deleteCh  chan common.Hash

	closeCh chan struct{}
	wg      sync.WaitGroup
}

// newLotteryManager returns an instance of lottery manager and starts
// the underlying routines for status management.
func newLotteryManager(address common.Address, chainReader Blockchain, contract *contract.LotteryBook, cdb *chequeDB) *lotteryManager {
	m := &lotteryManager{
		address:     address,
		chainReader: chainReader,
		contract:    contract,
		cdb:         cdb,
		pendingSet:  make(map[uint64][]*Lottery),
		activeSet:   make(map[uint64][]*Lottery),
		revealedSet: make(map[common.Hash]*Lottery),
		expiredSet:  make(map[common.Hash]*Lottery),
		lotteryCh:   make(chan *Lottery),
		queryCh:     make(chan *queryReq),
		deleteCh:    make(chan common.Hash),
		closeCh:     make(chan struct{}),
	}
	m.wg.Add(1)
	go m.run()
	return m
}

// run is responsible for managing the entire life cycle of the
// lotteries created locally.
//
// The status of lottery can be classified as four types:
// * pending: lottery is just created, have to wait a few block
//    confirms upon it.
// * active: lottery can be used to make payment, the lottery
//    reveal time has not yet arrived.
// * revealed: lottery has been revealed, can't be used to make
//    payment anymore.
// * expired: no one picks up lottery ticket within required claim
//    time, owner can reown it via resetting or destruct.
//
// External modules can monitor lottery status via subscription or
// direct query.
func (m *lotteryManager) run() {
	defer m.wg.Done()

	// Establish subscriptions
	newHeadCh := make(chan core.ChainHeadEvent, 1024)
	sub := m.chainReader.SubscribeChainHeadEvent(newHeadCh)
	if sub == nil {
		return
	}
	defer sub.Unsubscribe()

	lotteryClaimedCh := make(chan *contract.LotteryBookLotteryClaimed)
	eventSub, err := m.contract.WatchLotteryClaimed(nil, lotteryClaimedCh, nil)
	if err != nil {
		return
	}
	defer eventSub.Unsubscribe()

	var (
		current uint64         // Current height of local blockchain
		events  []LotteryEvent // A batch of cumulative lottery events
	)
	// Initialize local blockchain height
	current = m.chainReader.CurrentHeader().Number.Uint64()

	// Reload all tmp lottery records which we send the transaction out but
	// system crash. We need to ensure whether these lotteries are created
	// or not.
	tmpLotteris := m.cdb.listLotteries(m.address, true)
	for _, lottery := range tmpLotteris {
		ret, err := m.contract.Lotteries(nil, lottery.Id)
		if err != nil {
			continue
		}
		if ret.Amount != 0 {
			m.cdb.writeLottery(m.address, lottery.Id, false, lottery) // Yeah! We recover a unsaved lottery
			log.Debug("Recovered unsaved lottery", "id", lottery.Id, "amount", lottery.Amount)
		}
		m.cdb.deleteLottery(m.address, lottery.Id, true) // Delete the tmp record
	}
	// Reload all stored lotteries in disk during setup(include the recovered)
	lotteries := m.cdb.listLotteries(m.address, false)
	for _, lottery := range lotteries {
		if current < lottery.RevealNumber {
			m.activeSet[current] = append(m.activeSet[current], lottery)
			events = append(events, LotteryEvent{Id: lottery.Id, Status: LotteryActive})
		} else if current < lottery.RevealNumber+lotteryClaimPeriod+lotteryProcessConfirms {
			m.revealedSet[lottery.Id] = lottery
			events = append(events, LotteryEvent{Id: lottery.Id, Status: LotteryRevealed})
		} else {
			m.expiredSet[lottery.Id] = lottery
			events = append(events, LotteryEvent{Id: lottery.Id, Status: LotteryExpired})
		}
	}
	// Setup a ticker for expired lottery GC
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		if len(events) > 0 {
			m.lotteryFeed.Send(events)
			events = events[:0]
		}
		select {
		case ev := <-newHeadCh:
			current = ev.Block.NumberU64()
			// If newly created lotteries have enough confirms, move them to active set.
			for createdAt, lotteries := range m.pendingSet {
				if createdAt+lotteryProcessConfirms < current {
					continue
				}
				for _, lottery := range lotteries {
					m.activeSet[lottery.RevealNumber] = append(m.activeSet[lottery.RevealNumber], lottery)
					events = append(events, LotteryEvent{Id: lottery.Id, Status: LotteryActive})
					log.Debug("Lottery activated", "id", lottery.Id)
				}
				delete(m.pendingSet, createdAt)
			}
			// Clean stale lottery which is already revealed
			for revealAt, lotteries := range m.activeSet {
				if current < revealAt {
					continue
				}
				// Move all revealed lotteries into `revealed` set.
				for _, lottery := range lotteries {
					m.revealedSet[lottery.Id] = lottery
					events = append(events, LotteryEvent{Id: lottery.Id, Status: LotteryRevealed})
					log.Debug("Lottery revealed", "id", lottery.Id)
				}
				delete(m.activeSet, revealAt)
			}
			// Clean stale lottery which is already expired.
			for id, lottery := range m.revealedSet {
				// Move all expired lotteries into `expired` set.
				if lottery.RevealNumber+lotteryClaimPeriod+lotteryProcessConfirms <= current {
					events = append(events, LotteryEvent{Id: lottery.Id, Status: LotteryExpired})
					m.expiredSet[id] = lottery
					delete(m.revealedSet, id)
					log.Debug("Lottery expired", "id", lottery.Id)
				}
			}

		case lottery := <-m.lotteryCh:
			m.pendingSet[current] = append(m.pendingSet[current], lottery)
			events = append(events, LotteryEvent{Id: lottery.Id, Status: LotteryPending})
			log.Debug("Lottery created", "id", lottery.Id, "amout", lottery.Amount, "revealnumber", lottery.RevealNumber)

		case claimedEvent := <-lotteryClaimedCh:
			id := common.Hash(claimedEvent.Id)
			if _, exist := m.revealedSet[id]; exist {
				delete(m.revealedSet, id)
			}
			if _, exist := m.expiredSet[id]; exist {
				delete(m.expiredSet, id)
			}
			log.Debug("Lottery claimed", "id", id)

		case req := <-m.queryCh:
			if req.typ == activeLotteryQuery {
				var ret []*Lottery
				for _, lotteries := range m.activeSet {
					ret = append(ret, lotteries...)
				}
				req.ret <- ret
			} else if req.typ == expiredLotteryQuery {
				var ret []*Lottery
				for _, lottery := range m.expiredSet {
					ret = append(ret, lottery)
				}
				req.ret <- ret
			}

		case id := <-m.deleteCh:
			delete(m.expiredSet, id) // The expired lottery is reset or destroyed

		case <-ticker.C:
			for id := range m.expiredSet {
				// Note it might be expensive for light client to retrieve
				// information from contract.
				ret, err := m.contract.Lotteries(nil, id)
				if err != nil {
					continue
				}
				if ret.Amount == 0 {
					delete(m.expiredSet, id)
					m.cdb.deleteLottery(m.address, id, false)
					log.Debug("Lottery removed", "id", id)
				}
				// Otherwise it can be reowned by sender, keep it.
			}

		case <-m.closeCh:
			return
		}
	}
}

// trackLottery adds a newly created lottery for life cycle management.
func (m *lotteryManager) trackLottery(l *Lottery) error {
	select {
	case m.lotteryCh <- l:
		return nil
	case <-m.closeCh:
		return errLotteryManagerClosed
	}
}

// activeLotteris returns all active lotteris which can be used
// to make payment.
func (m *lotteryManager) activeLotteris() ([]*Lottery, error) {
	reqCh := make(chan []*Lottery, 1)
	select {
	case m.queryCh <- &queryReq{
		typ: activeLotteryQuery,
		ret: reqCh,
	}:
		return <-reqCh, nil
	case <-m.closeCh:
		return nil, errLotteryManagerClosed
	}
}

// expiredLotteris returns all expired lotteris which can be reowned.
func (m *lotteryManager) expiredLotteris() ([]*Lottery, error) {
	reqCh := make(chan []*Lottery, 1)
	select {
	case m.queryCh <- &queryReq{
		typ: expiredLotteryQuery,
		ret: reqCh,
	}:
		return <-reqCh, nil
	case <-m.closeCh:
		return nil, errLotteryManagerClosed
	}
}

func (m *lotteryManager) deleteExpired(id common.Hash) error {
	select {
	case m.deleteCh <- id:
		return nil
	case <-m.closeCh:
		return errLotteryManagerClosed
	}
}

// subscribeLotteryEvent registers a subscription of LotteryEvent.
func (m *lotteryManager) subscribeLotteryEvent(ch chan<- []LotteryEvent) event.Subscription {
	return m.scope.Track(m.lotteryFeed.Subscribe(ch))
}

func (m *lotteryManager) close() {
	m.scope.Close()
	close(m.closeCh)
	m.wg.Wait()
}
