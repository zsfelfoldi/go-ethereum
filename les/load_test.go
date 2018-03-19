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
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
)

type testLoadPeer struct {
	fcServer *flowcontrol.ServerNode
}

func (p *testLoadPeer) waitBefore(maxCost uint64) (time.Duration, float64) {
	return p.fcServer.CanSend(maxCost)
}

func (p *testLoadPeer) canQueue() bool {
	return true
}

func (p *testLoadPeer) queueSend(f func()) {
	f()
}

const testRequestCost = 1000000

func TestLoadBalance(t *testing.T) {
	clock := mclock.NewSimulatedClock()
	flowcontrol.Clock = clock
	distClock = clock

	quit := make(chan struct{})
	//defer close(quit)

	fcManager := flowcontrol.NewClientManager(25, 10, 1000000000)
	params := &flowcontrol.ServerParams{
		BufLimit:    30000000,
		MinRecharge: 50000,
	}

	fcClient := flowcontrol.NewClientNode(fcManager, params)
	fcServer := flowcontrol.NewServerNode(params)
	dist := newRequestDistributor(nil, quit)
	peer := &testLoadPeer{fcServer: fcServer}
	dist.registerTestPeer(peer)

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
				clock.Sleep(time.Millisecond)
				bvAfter, _ := fcClient.RequestProcessed(testRequestCost) // realCost
				//fmt.Println(bvAfter / 1000000)
				fcServer.GotReply(reqID, bvAfter)
			case <-quit:
				return
			}
		}
	}()

	expCh := make(chan struct{}, 100)
	go func() {
		//start := time.Now()
		for cnt := 0; cnt < 10000; cnt++ {
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

				//fmt.Println(cnt, " ", time.Since(start))
				sentCh := dist.queue(rq)
				go func() {
					if <-sentCh != nil {
					} else {
						//time.Sleep(time.Microsecond * 100)
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
	fmt.Println(clock.Now())
}
