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
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/rlp"
)

type txPool struct {
	db         ethdb.Database
	reqDist    *requestDistributor
	removePeer peerDropFn
	serverPool odrPeerSelector
	chain      light.LightChain
	config     *params.ChainConfig

	tracker      *light.TxTracker
	startStopChn chan txPoolStartStopReq
	updateChn    chan light.TxTrackerUpdate
	deliverChn   chan txStatusData
}

type txPoolStartStopReq struct {
	resChn chan *txPoolResults
	start  bool
}

type txStatusResp struct {
	peer   *peer
	reqID  uint64
	head   common.Hash
	status []core.TxStatusData
	errChn chan error
}

type txPoolResults struct {
	pending, queued map[common.Address]types.Transactions
	err             error
}

func (t *txPool) eventLoop() {
	waiting := make(map[chan *txPoolResults]struct{})
	sentReqs := make(map[uint64]*distReq)
	var (
		updateCnt         uint64
		lastResults       *txPoolResults
		lastResultsTime   mclock.AbsTime
		lastRequest       *distReq
		lastRequestHashes []common.Hash
		reqStopChn        chan struct{}
		tracked           map[common.Hash]struct{}
		trackerHead       common.Hash
		trackerHeadNum    uint64
	)

	sendNewReq := func() {
		head := trackerHead
		amount := len(tracked)
		txHashes := make([]common.Hash, 0, amount)
		for txh, _ := range tracked {
			txHashes = append(txHashes, txh)
		}

		reqID := getNextReqID()
		lastRequestHashes = txHashes
		lastRequest = &distReq{
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

		sentReqs[reqID] = lastRequest
		reqStopChn = t.reqDist.retrieve(lastRequest, func(p distPeer, respTime time.Duration, srto, hrto bool) {
			pp := p.(*peer)
			if t.serverPool != nil {
				t.serverPool.adjustResponseTime(pp.poolEntry, respTime, srto)
			}
			if hrto {
				pp.Log().Debug("Request timed out hard")
				t.removePeer(pp.id)
			}
		})
	}

	for {
		select {
		case s := <-t.startStopChn:
			if s.start {
				if lastResults != nil && time.Duration(mclock.Now()-lastResultsTime) < time.Second {
					s.resChn <- lastResults
				} else {
					waiting[s.resChn] = struct{}{}
					if lastRequest == nil {
						sendNewReq()
					}
				}
			} else {
				if _, ok := waiting[s.resChn]; ok {
					close(s.resChn)
					delete(waiting, s.resChn)
				}
				if len(waiting) == 0 && lastRequest != nil {
					lastRequest.stop(nil)
					lastRequest = nil
					reqStopChn = nil
				}
			}

		case u := <-t.updateChn:
			if u.Head != trackerHead || len(u.Added) > 0 {
				trackerHead == u.Head
				//headnum
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

		case s := <-t.deliverChn:
			if req, ok := sentReqs[s.reqID]; ok {
				t.chain.LockChain()
				valid := false
				var err error
				// if the request is obsolete, the reply is ignored (not valid but also no error)
				if req == lastRequest && s.head == chain.LastBlockHash() {
					if len(s.status) == len(lastRequestHashes) {
						valid = true
						res := &txPoolResults{
							pending: make(map[common.Address]types.Transactions),
							queued:  make(map[common.Address]types.Transactions),
						}
						signer := types.MakeSigner(t.config, big.NewInt(trackerHeadNum))
						errCh := make(chan bool, len(s.status))
						waitErrCnt := 0
					loop:
						for i, status := range s.status {
							txHash := lastRequestHashes[i]
							switch s.Status {
							case core.TxStatusQueued, core.TxStatusPending:
								tx := core.GetTransactionData(t.db, txHash)
								if tx == nil {
									valid = false
									err = errors.New("Local transaction not found in database")
									break loop
								}
								from, e := types.Sender(signer, tx)
								if e != nil {
									valid = false
									err = e
									break loop
								}
								if s.Status == core.TxStatusPending {
									res.pending[from] = append(res.pending[from], tx)
								} else {
									res.queued[from] = append(res.queued[from], tx)
								}
							case core.TxStatusIncluded:
								var pos core.TxChainPos
								if e := rlp.DecodeBytes(s.Data, &pos); e != nil {
									valid = false
									err = e
									break loop
								}
								ok, waitErrCh := t.tracker.CheckTxChainPos(txHash, pos, errCh)
								if !ok {
									valid = false
									err = errors.New("Invalid tx chain position")
									break loop
								}
								if waitErrCh {
									waitErrCnt++
								}
							case core.TxStatusError:
								valid = false
								err = errors.New("Unexpected TxStatusError")
								break loop
							}
						}

						if waitErrCnt > 0 {
							go func() {
								for waitErrCnt > 0 {
									if <-errCh {
										s.peer.responseErrors++
										if s.peer.responseErrors > maxResponseErrors {
											t.removePeer(s.peer)
										}
										return
									}
								}
							}()
						}

					} else {
						err = errors.New("Incorrect number of tx status entries")
					}

					if valid {
						for resChn, _ := range waiting {
							resChn <- res
							delete(waiting, resChn)
						}
						lastResults = res
						lastResultsTime = mclock.Now()
					}
				}
				t.chain.UnlockChain()
				req.delivered(s.peer, valid)
				s.errChn <- err
			} else {
				s.errChn <- ErrUnexpectedResponse
			}

		case <-reqStopChn:
			if err := lastRequest.getError(); err != nil && len(waiting) > 0 {
				res := &txPoolResults{err: err}
				for resChn, _ := range waiting {
					resChn <- res
					delete(waiting, resChn)
				}
			}

		case <-t.stop:
			if lastRequest != nil {
				lastRequest.stop(nil)
				lastRequest = nil
				reqStopChn = nil
			}
			return
		}
	}
}

func (t *txPool) getContents(ctx context.Context) (map[common.Address]types.Transactions, map[common.Address]types.Transactions, error) {
	chn := make(chan *txPoolResults)
	t.startStopChn <- txPoolStartStopReq{chn, true}
	select {
	case res := <-chn:
		return res.pending, res.queued, res.err
	case <-ctx.Done():
		t.startStopChn <- txPoolStartStopReq{chn, false}
		return nil, nil, ctx.Err()
	}
}

func (t *txPool) deliver(peer *peer, reqID uint64, head common.Hash, status []core.TxStatusData) error {
	errChn := make(chan error)
	t.deliverChn <- txStatusResp{peer, reqID, head, status, errChn}
	return <-errChn
}
