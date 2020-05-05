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

package client

import (
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

var (
	sfTest1 = utils.NewNodeStateFlag("test1", false, false)
	sfTest2 = utils.NewNodeStateFlag("test2", false, false)
	sfTest3 = utils.NewNodeStateFlag("test3", false, false)
	sfTest4 = utils.NewNodeStateFlag("test4", false, false)
	sfiEnr  = utils.NewNodeStateField("enr", reflect.TypeOf(&enr.Record{}), []*utils.NodeStateFlag{sfTest1}, false, nil, nil)
)

const iterTestNodeCount = 6

func testNodeID(i int) enode.ID {
	return enode.ID{42, byte(i % 256), byte(i / 256)}
}

func testNodeIndex(id enode.ID) int {
	if id[0] != 42 {
		return -1
	}
	return int(id[1]) + int(id[2])*256
}

func TestQueueIterator(t *testing.T) {
	ns := utils.NewNodeStateMachine(nil, nil, &mclock.Simulated{})
	st1 := ns.MustRegisterState(sfTest1)
	st2 := ns.MustRegisterState(sfTest2)
	st3 := ns.MustRegisterState(sfTest3)
	st4 := ns.MustRegisterState(sfTest4)
	enrField := ns.MustRegisterField(sfiEnr)
	qi := NewQueueIterator(ns, st2, st3, sfTest4, sfiEnr, enode.ValidSchemesForTesting)
	ns.Start()
	for i := 1; i <= iterTestNodeCount; i++ {
		node := enode.SignNull(&enr.Record{}, testNodeID(i))
		ns.UpdateState(node.ID(), st1, 0, 0)
		ns.SetField(node.ID(), enrField, node.Record())
	}
	ch := make(chan *enode.Node)
	go func() {
		for qi.Next() {
			ch <- qi.Node()
		}
		close(ch)
	}()
	next := func() int {
		select {
		case node := <-ch:
			return testNodeIndex(node.ID())
		case <-time.After(time.Millisecond * 200):
			return 0
		}
	}
	exp := func(i int) {
		n := next()
		if n != i {
			t.Errorf("Wrong item returned by iterator (expected %d, got %d)", i, n)
		}
	}
	exp(0)
	ns.UpdateState(testNodeID(1), st2, 0, 0)
	ns.UpdateState(testNodeID(2), st2, 0, 0)
	ns.UpdateState(testNodeID(3), st2, 0, 0)
	exp(1)
	exp(2)
	exp(3)
	exp(0)
	ns.UpdateState(testNodeID(4), st2, 0, 0)
	ns.UpdateState(testNodeID(5), st2, 0, 0)
	ns.UpdateState(testNodeID(6), st2, 0, 0)
	ns.UpdateState(testNodeID(5), st3, 0, 0)
	exp(4)
	exp(6)
	exp(0)
	ns.UpdateState(testNodeID(1), 0, st4, 0)
	ns.UpdateState(testNodeID(2), 0, st4, 0)
	ns.UpdateState(testNodeID(3), 0, st4, 0)
	ns.UpdateState(testNodeID(2), st3, 0, 0)
	ns.UpdateState(testNodeID(2), 0, st3, 0)
	exp(1)
	exp(3)
	exp(2)
	exp(0)
}
