// Copyright 2016 The go-ethereum Authors
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

// Package les implements the Light Ethereum Subprotocol.
package les

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/light"
	"golang.org/x/net/context"
)

type txPool struct {
	reqDist *requestDistributor

	tracker   *light.TxTracker
	updateChn chan light.TxTrackerUpdate
}

type txPoolStartStopReq struct {
	id     interface{}
	resChn chan *txPoolResults
}

func (t *txPool) eventLoop() {
	waiting := make(map[interface{}]chan *txPoolResults)
	reqs := make(map[uint64]uint64) // reqID -> updateCnt; deleted when answered or timed out
	var (
		updateCnt                  uint64
		lastResults                *txPoolResults
		lastResultsTime            mclock.AbsTime
		requestQueued, requestSent *distReq
		tracked                    map[common.Hash]struct{}
		trackerHead                common.Hash
	)

	for {
		select {
		case s := <-t.startStopReq:
			if s.resChn == nil {
				close(waiting[id])
				delete(waiting, s.id)
			} else {
				waiting[id] = s.resChn
			}

		case u := <-t.updateChn:
			if u.Head != trackerHead || len(u.Added) > 0 {
				trackerHead == u.Head
				// invalidate all previous requests and results
				updateCnt++
				lastResults = nil
				lastResultsTime = 0
				if requestQueued != nil {
					t.reqDist.cancel(requestQueued)
				}
				requestSent = nil

			}
			for _, txh := range u.Added {
				tracked[txh] = struct{}{}
			}
			for _, txh := range u.Removed {
				delete(tracked, txh)
			}

		case <-t.deliverChn:
		case <-t.stop:
		case <-sendNewReq:
			if requestQueued == nil && requestSent == nil {
				head := trackerHead
				amount := len(tracked)
				txHashes := make([]common.Hash, 0, amount)
				for txh, _ := range tracked {
					txHashes = append(txHashes, txh)
				}

				reqID := getNextReqID()
				rq := &distReq{
					getCost: func(dp distPeer) uint64 {
						peer := dp.(*peer)
						return peer.GetRequestCost(GetTxStatusMsg, amount)
					},
					canSend: func(dp distPeer) bool {
						return dp.(*peer).Head() == head
					},
					request: func(dp distPeer) func() {
						peer := dp.(*peer)
						cost := peer.GetRequestCost(GetTxStatusMsg, amount)
						peer.fcServer.QueueRequest(reqID, cost)
						return func() { peer.RequestTxStatus(reqID, cost, txHashes) }
					},
				}

				reqs[reqID] = updateCnt
				requestQueued = rq
				sentChn := t.reqDist.queue(rq)

				go func() {
					_, ok := <-sentChn
					if ok {
						sentReq <- reqID
					} else {
						cancelledReq <- reqID
					}
				}()
			}

		case reqID := <-sentReq:
			if reqs[reqID] == updateCnt {
				requestSent = requestQueued
			}
			requestQueued = nil
			go func() {
				time.Sleep(softRequestTimeout)
				checkSoftTimeout <- reqID
			}()

		case reqID := <-cancelledReq:
			requestQueued = nil
			delete(reqs, reqID)
			go func() {
				time.Sleep(time.Millisecond * 10)
				sendNewReq <- struct{}{}
			}()

		case reqID := <-checkSoftTimeout:
			if cnt, ok := reqs[reqID]; ok {
				//TODO update stats
				if cnt == updateCnt && requestSent != nil {
					// still valid, try resending
					requestQueued = requestSent
					requestSent = nil
					sentChn := t.reqDist.queue(rq)

					go func() {
						_, ok := <-sentChn
						if ok {
							sentReq <- reqID
						} else {
							cancelledReq <- reqID
						}
					}()
				}
			}
		}

	}
}

func (t *txPool) getContents(ctx context.Context) (map[common.Address]types.Transactions, map[common.Address]types.Transactions, error) {
	for {

		sent := false
		select {
		case _, ok := <-sentChn:
			if ok {
				// request successfully sent
				sent = true
			} else {
				// no suitable peers at the moment, wait a little bit and retry
				time.Sleep(time.Millisecond * 10)
			}
		case <-updateChn:
			// pool updated, cancel request and retry
			sent = !t.reqDist.cancel(rq)
		case <-ctx.Done():
			// cancelled, return with error
			return nil, nil, ctx.Err()
		}
	}
}

func (t *txPool) deliver(reqID uint64, head common.Hash, status []core.TxChainPos) {

}
