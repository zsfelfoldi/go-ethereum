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
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
)

var testClock mclock.Clock

type testLoadPeer struct {
	fcClient *flowcontrol.ClientNode
	fcServer *flowcontrol.ServerNode
	serveCh  chan uint64
}

func newTestLoadPeer(t *testing.T, client *testLoadClient, server *testLoadServer, quit chan struct{}) *testLoadPeer {
	peer := &testLoadPeer{
		fcClient: flowcontrol.NewClientNode(server.fcManager, server.params),
		fcServer: flowcontrol.NewServerNode(server.params),
		serveCh:  make(chan uint64, 1000),
	}
	client.dist.registerTestPeer(peer)

	go func() {
		for {
			select {
			case reqID := <-peer.serveCh:
				bufValue, _ := peer.fcClient.AcceptRequest()
				if testRequestCost > bufValue {
					recharge := time.Duration((testRequestCost - bufValue) * 1000000 / server.params.MinRecharge)
					t.Errorf("Request came too early (%v)", recharge)
				}
				server.processTask(time.Microsecond * 500)
				bvAfter, _ := peer.fcClient.RequestProcessed(testRequestCost)
				peer.fcServer.GotReply(reqID, bvAfter)
			case <-quit:
				return
			}
		}
	}()

	return peer
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

type testLoadTask struct {
	procTime time.Duration
	finished chan struct{}
}

type testLoadServer struct {
	fcManager *flowcontrol.ClientManager
	params    *flowcontrol.ServerParams
	queue     chan testLoadTask
}

func newTestLoadServer(params *flowcontrol.ServerParams, threads int, quit chan struct{}) *testLoadServer {
	s := &testLoadServer{
		fcManager: flowcontrol.NewClientManager(50, 10, 1000000000),
		params:    params,
		queue:     make(chan testLoadTask, 10000),
	}
	for i := 0; i < threads; i++ {
		go func() {
			for {
				select {
				case task := <-s.queue:
					testClock.Sleep(task.procTime)
					close(task.finished)
				case <-quit:
					return
				}
			}
		}()
	}
	return s
}

func (s *testLoadServer) processTask(procTime time.Duration) {
	finished := make(chan struct{})
	s.queue <- testLoadTask{procTime: procTime, finished: finished}
	<-finished
}

type testLoadClient struct {
	dist  *requestDistributor
	quit  chan struct{}
	count uint64
}

func newTestLoadClient(quit chan struct{}) *testLoadClient {
	return &testLoadClient{
		dist: newRequestDistributor(nil, quit),
		quit: quit,
	}
}

func (c *testLoadClient) sendRequests() {
	expCh := make(chan struct{}, 100)
	for {
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
					peer := dp.(*testLoadPeer)
					peer.fcServer.QueueRequest(reqID, testRequestCost)
					return func() {
						peer.serveCh <- reqID
					}
				},
			}

			sentCh := c.dist.queue(rq)
			go func() {
				<-sentCh
				<-expCh
			}()
		case <-c.quit:
			return
		}
		atomic.AddUint64(&c.count, 1)
	}
}

func (c *testLoadClient) requestsSent() uint64 {
	return atomic.LoadUint64(&c.count)
}

const testRequestCost = 1000000

const (
	testClientCount   = 10
	testServerCount   = 2
	testServerThreads = 4
)

func TestLoadBalance(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	testClock = mclock.NewSimulatedClock()
	flowcontrol.Clock = testClock
	distClock = testClock
	params := &flowcontrol.ServerParams{
		BufLimit:    30000000,
		MinRecharge: 50000,
	}

	clients := make([]*testLoadClient, testClientCount)
	for i, _ := range clients {
		clients[i] = newTestLoadClient(quit)
	}
	servers := make([]*testLoadServer, testServerCount)
	for i, _ := range servers {
		servers[i] = newTestLoadServer(params, testServerThreads, quit)
	}

	for _, client := range clients {
		for _, server := range servers {
			newTestLoadPeer(t, client, server, quit)
		}
	}

	for _, client := range clients {
		go client.sendRequests()
	}

	s := make([]uint64, testClientCount)
	testClock.Sleep(time.Second * 5)
	for i, client := range clients {
		s[i] = client.requestsSent()
	}
	testClock.Sleep(time.Second * 10)
	for i, client := range clients {
		diff := client.requestsSent() - s[i]
		fmt.Println(i, " ", diff)
	}
}
