// Copyright 2017 The go-ethereum Authors
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

// Package light implements on-demand retrieval capable state and chain objects
// for the Ethereum Light Client.
package les

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/les/flowcontrol"
)

const testRequestCost = 1000

func TestLoadBalance(t *testing.T) {
	quit := make(chan struct{})
	//defer close(quit)

	fcManager := flowcontrol.NewClientManager(50, 10, 1000000000)
	params := &flowcontrol.ServerParams{
		BufLimit:    300000000,
		MinRecharge: 50000,
	}

	fcClient := flowcontrol.NewClientNode(fcManager, params)
	fcServer := flowcontrol.NewServerNode(params)
	peers := newPeerSet()
	dist := newRequestDistributor(peers, quit)

	serveCh := make(chan uint64, 1000)
	go func() {
		for {
			select {
			case reqID := <-serveCh:
				bufValue, _ := fcClient.AcceptRequest()
				if testRequestCost > bufValue {
					recharge := time.Duration((testRequestCost - bufValue) * 1000000 / params.MinRecharge)
					t.Errorf("Request came too early (%v)", recharge)
				}
				time.Sleep(time.Millisecond)
				bvAfter, _ := fcClient.RequestProcessed(testRequestCost) // realCost
				fcServer.GotReply(reqID, bvAfter)
			case <-quit:
				return
			}
		}
	}()

	expCh := make(chan struct{}, 100)
	go func() {
		for cnt := 0; cnt < 5000; {
			select {
			case expCh <- struct{}{}:
				reqID := genReqID()
				rq := &distReq{
					getCost: func(dp distPeer) uint64 {
						return testRequestCost
					},
					canSend: func(dp distPeer) bool {
						return true
					},
					request: func(dp distPeer) func() {
						fcServer.QueueRequest(reqID, testRequestCost)
						return func() {
							serveCh <- reqID
						}
					},
				}

				sentCh := dist.queue(rq)
				go func() {
					if <-sentCh != nil {
						cnt++
					} else {
						time.Sleep(time.Millisecond)
					}
					<-expCh
				}()
			case <-quit:
				return
			}
		}
		close(quit)
	}()

	<-quit
}
