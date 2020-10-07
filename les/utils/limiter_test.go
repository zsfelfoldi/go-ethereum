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

package utils

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

type (
	ltNode struct {
		addr, id   byte
		value, exp float64
		pw         uint64

		served, dropped int
	}

	limTest struct {
		limiter         *Limiter
		wg              sync.WaitGroup
		stop, processed chan struct{}
		nodes           []*ltNode
	}
)

func (lt *limTest) start(n *ltNode) {
	lt.wg.Add(1)
	go func() {
		address := string([]byte{n.addr})
		id := enode.ID{n.id}
		for {
			cch := lt.limiter.Add(id, address, n.value, n.pw)
			select {
			case ch := <-cch:
				if ch != nil {
					n.served++
					ch <- float64(n.pw * 1000000)
				} else {
					n.dropped++
				}
				lt.processed <- struct{}{}
			case <-lt.stop:
				lt.wg.Done()
				return
			}
		}
	}()
}

func TestLimiter(t *testing.T) {
	lt := &limTest{
		limiter:   NewLimiter(0, 1000, NewCostFilter(0, 1), &mclock.Simulated{}),
		stop:      make(chan struct{}),
		processed: make(chan struct{}),
		nodes: []*ltNode{
			//{addr: 0, id: 0, value: 0, pw: 1, exp: 0.5},
			{addr: 1, id: 1, value: 0, pw: 1, exp: 0.5},
			{addr: 1, id: 2, value: 0, pw: 1, exp: 0.5},
		},
	}
	for _, n := range lt.nodes {
		lt.start(n)
	}
	for i := 0; i < 1000000; i++ {
		<-lt.processed
	}
	close(lt.stop)
	lt.wg.Wait()
	for _, n := range lt.nodes {
		fmt.Println(n.served)
	}
}

type (
	cfPoint struct {
		cost, pw float64
	}
	cfTest struct {
		cutRatio, expLimit float64
		period             []cfPoint
	}
)

func TestCostFilter(t *testing.T) {
	tests := []cfTest{
		cfTest{0.5, 50, []cfPoint{{100, 1}}},
		cfTest{0.1, 800, []cfPoint{{100, 1}, {900, 1}}},
		cfTest{0.1, 900, []cfPoint{{0, 0.1}, {150, 0.1}, {950, 1}}},
	}

	for _, test := range tests {
		cf := NewCostFilter(test.cutRatio, 0.01)
		var (
			index                int
			fc, limit, sum, fsum float64
		)
		for i := 0; i < 1000000; i++ {
			c := test.period[index]
			fc, limit = cf.Filter(c.cost, c.pw)
			sum += c.cost
			fsum += fc
			index++
			if index == len(test.period) {
				index = 0
			}
		}
		expfsum := sum * (1 - test.cutRatio)
		if fsum < expfsum*0.99 || fsum > expfsum*1.01 {
			t.Fatalf("Filtered sum is incorrect (got %f, expected %f)", fsum, expfsum)
		}
		if limit < test.expLimit*0.99 || limit > test.expLimit*1.01 {
			t.Fatalf("Limit is incorrect (got %f, expected %f)", limit, test.expLimit)
		}
	}
}
