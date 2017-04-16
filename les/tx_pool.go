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
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

type txPool struct {
	db         ethdb.Database
	reqDist    *requestDistributor
	removePeer peerDropFn
	serverPool odrPeerSelector
	chain      *light.LightChain
	tracker    *light.TxTracker
	config     *params.ChainConfig

	startStopChn chan txPoolStartStopReq
	updateChn    chan light.TxTrackerUpdate
	deliverChn   chan txStatusResp
	quit         chan struct{}
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

func newTxPool(config *params.ChainConfig, odr light.OdrBackend, chain *light.LightChain) *txPool {
	return &txPool{
		db:           odr.Database(),
		chain:        chain,
		tracker:      light.NewTxTracker(odr, chain),
		config:       config,
		startStopChn: make(chan txPoolStartStopReq, 10),
		updateChn:    make(chan light.TxTrackerUpdate, 10),
		deliverChn:   make(chan txStatusResp, 100),
		quit:         make(chan struct{}),
	}
}

func (t *txPool) eventLoop() {
	waiting := make(map[chan *txPoolResults]struct{})
	sentReqs := make(map[uint64]*distReq)
	var (
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
			for _, txh := range u.Added {
				tracked[txh] = struct{}{}
			}
			for _, txh := range u.Removed {
				delete(tracked, txh)
			}
			if u.Head != trackerHead || len(u.Added) > 0 {
				trackerHead = u.Head
				trackerHeadNum = u.HeadNum
				// invalidate all previous requests and results, send new request if necessary
				lastResults = nil
				lastResultsTime = 0
				if lastRequest != nil {
					lastRequest.stop(nil)
					lastRequest = nil
					reqStopChn = nil
					sendNewReq()
				}
			}

		case s := <-t.deliverChn:
			if req, ok := sentReqs[s.reqID]; ok && req.expectResponseFrom(s.peer) {
				t.chain.LockChain()
				var err error
				// if the request is obsolete, the reply is ignored (no error)
				if req == lastRequest && s.head == t.chain.LastBlockHash() {
					if len(s.status) == len(lastRequestHashes) {
						res := &txPoolResults{
							pending: make(map[common.Address]types.Transactions),
							queued:  make(map[common.Address]types.Transactions),
						}
						signer := types.MakeSigner(t.config, big.NewInt(int64(trackerHeadNum)))
						errCh := make(chan bool, len(s.status))
						waitErrCnt := 0
					loop:
						for i, status := range s.status {
							txHash := lastRequestHashes[i]
							switch status.Status {
							case core.TxStatusQueued, core.TxStatusPending:
								tx := core.GetTransactionData(t.db, txHash)
								if tx == nil {
									err = errors.New("Local transaction not found in database")
									break loop
								}
								from, e := types.Sender(signer, tx)
								if e != nil {
									err = e
									break loop
								}
								if status.Status == core.TxStatusPending {
									res.pending[from] = append(res.pending[from], tx)
								} else {
									res.queued[from] = append(res.queued[from], tx)
								}
							case core.TxStatusIncluded:
								var pos core.TxChainPos
								if e := rlp.DecodeBytes(status.Data, &pos); e != nil {
									err = e
									break loop
								}
								ok, waitErrCh := t.tracker.CheckTxChainPos(txHash, pos, errCh)
								if !ok {
									err = errors.New("Invalid tx chain position")
									break loop
								}
								if waitErrCh {
									waitErrCnt++
								}
							case core.TxStatusError:
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
											t.removePeer(s.peer.id)
										}
										return
									}
								}
							}()
						}

						if err == nil {
							for resChn, _ := range waiting {
								resChn <- res
								delete(waiting, resChn)
							}
							lastResults = res
							lastResultsTime = mclock.Now()
						}
					} else {
						err = errors.New("Incorrect number of tx status entries")
					}
				}
				t.chain.UnlockChain()
				req.delivered(s.peer, err == nil)
				s.errChn <- err
			} else {
				s.errChn <- errResp(ErrUnexpectedResponse, "")
			}

		case <-reqStopChn:
			if err := lastRequest.getError(); err != nil && len(waiting) > 0 {
				res := &txPoolResults{err: err}
				for resChn, _ := range waiting {
					resChn <- res
					delete(waiting, resChn)
				}
			}

		case <-t.quit:
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

// Stop stops the light transaction pool
func (t *txPool) Stop() {
	close(t.quit)
	log.Info("Transaction pool stopped")
}

// GetNonce returns the "pending" nonce of a given address. It always queries
// the nonce belonging to the latest header too in order to detect if another
// client using the same key sent a transaction.
func (t *txPool) GetNonce(ctx context.Context, addr common.Address) (uint64, error) {
	return 0, nil
}

// Stats returns the number of currently pending (locally created) transactions
func (t *txPool) Stats() (pending int) {
	return
}

// Add adds a transaction to the pool if valid and passes it to the tx relay
// backend
func (t *txPool) Add(ctx context.Context, tx *types.Transaction) error {
	return nil
}

// GetTransaction returns a transaction if it is contained in the pool
// and nil otherwise.
func (t *txPool) GetTransaction(hash common.Hash) *types.Transaction {
	return nil
}

// GetTransactions returns all currently processable transactions.
// The returned slice may be modified by the caller.
func (t *txPool) GetTransactions() (txs types.Transactions, err error) {
	return nil, nil
}

// Content retrieves the data content of the transaction pool, returning all the
// pending as well as queued transactions, grouped by account and nonce.
func (t *txPool) Content() (map[common.Address]types.Transactions, map[common.Address]types.Transactions) {
	return nil, nil
}

// RemoveTx removes the transaction with the given hash from the pool.
func (t *txPool) RemoveTx(hash common.Hash) {
}
