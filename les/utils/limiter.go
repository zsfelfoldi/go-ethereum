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
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

const (
	minRelCost      = 0.001
	maxSelectWeight = 1000000000
)

type Limiter struct {
	lock       sync.Mutex
	cond       *sync.Cond
	clock      mclock.Clock
	quit       bool
	process    func(interface{}) float64
	costFilter *CostFilter

	nodes                      map[enode.ID]*nodeQueue
	addresses                  map[string]*addressGroup
	addressSelect, valueSelect *WeightedRandomSelect
	maxValue, sleepFactor      float64
}

type nodeQueue struct {
	queue                   []cmd
	id                      enode.ID
	address                 string
	value                   float64
	flatWeight, valueWeight uint64
	groupIndex              int
}

type addressGroup struct {
	nodes                      []*nodeQueue
	nodeSelect                 *WeightedRandomSelect
	sumFlatWeight, groupWeight uint64
}

func flatWeight(item interface{}) uint64 { return item.(*nodeQueue).flatWeight }

func (ag *addressGroup) add(nq *nodeQueue) {
	l := len(ag.nodes)
	if l == 1 {
		ag.nodeSelect = NewWeightedRandomSelect(flatWeight)
		ag.nodeSelect.Update(ag.nodes[0])
	}
	nq.groupIndex = l
	ag.nodes = append(ag.nodes, nq)
	ag.sumFlatWeight += nq.flatWeight
	ag.groupWeight = ag.sumFlatWeight / uint64(l+1)
	if l >= 1 {
		ag.nodeSelect.Update(ag.nodes[l])
	}
}

func (ag *addressGroup) update(nq *nodeQueue, weight uint64) {
	ag.sumFlatWeight += weight - nq.flatWeight
	nq.flatWeight = weight
	ag.groupWeight = ag.sumFlatWeight / uint64(len(ag.nodes))
	if ag.nodeSelect != nil {
		ag.nodeSelect.Update(nq)
	}
}

func (ag *addressGroup) remove(nq *nodeQueue) {
	l := len(ag.nodes) - 1
	if nq.groupIndex != l {
		ag.nodes[nq.groupIndex] = ag.nodes[l]
		ag.nodes[nq.groupIndex].groupIndex = nq.groupIndex
	}
	ag.nodes = ag.nodes[:l]
	ag.sumFlatWeight -= nq.flatWeight
	ag.groupWeight = ag.sumFlatWeight / uint64(l)
	if l == 1 {
		ag.nodeSelect = nil
	}
}

func (ag *addressGroup) choose() *nodeQueue {
	if ag.nodeSelect == nil {
		// nodes list should never be empty here
		return ag.nodes[0]
	}
	return ag.nodeSelect.Choose().(*nodeQueue)
}

type cmd struct {
	data        interface{}
	priorWeight float64 // <= 1
}

func NewLimiter(process func(interface{}) float64, costFilter *CostFilter, sleepFactor float64, clock mclock.Clock) *Limiter {
	l := &Limiter{
		process:       process,
		costFilter:    costFilter,
		clock:         clock,
		addressSelect: NewWeightedRandomSelect(func(item interface{}) uint64 { return item.(*addressGroup).groupWeight }),
		valueSelect:   NewWeightedRandomSelect(func(item interface{}) uint64 { return item.(*nodeQueue).valueWeight }),
		nodes:         make(map[enode.ID]*nodeQueue),
		addresses:     make(map[string]*addressGroup),
		sleepFactor:   sleepFactor,
	}
	l.cond = sync.NewCond(&l.lock)
	return l
}

func selectionWeights(relCost, value float64) (flatWeight, valueWeight uint64) {
	var f float64
	if relCost <= minRelCost {
		f = 1
	} else {
		f = minRelCost / relCost
	}
	f *= maxSelectWeight
	flatWeight, valueWeight = uint64(f), uint64(f*value)
	if flatWeight == 0 {
		flatWeight = 1
	}
	return
}

// Note: priorWeight <= 1
func (l *Limiter) Add(id enode.ID, address string, value, priorWeight float64, data interface{}) {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.quit {
		return
	}
	if value > l.maxValue {
		l.maxValue = value
	}
	if value > 0 {
		// normalize value to <= 1
		value /= l.maxValue
	}

	if nq, ok := l.nodes[id]; ok {
		nq.queue = append(nq.queue, cmd{data, priorWeight})
	} else {
		nq := &nodeQueue{
			queue:       []cmd{{data, priorWeight}},
			id:          id,
			address:     address,
			value:       value,
			flatWeight:  uint64(priorWeight * maxSelectWeight),
			valueWeight: uint64(value * priorWeight * maxSelectWeight),
		}
		nq.flatWeight, nq.valueWeight = selectionWeights(priorWeight, value)
		l.nodes[id] = nq
		if nq.valueWeight != 0 {
			l.valueSelect.Update(nq)
		}
		ag := l.addresses[address]
		if ag == nil {
			ag = &addressGroup{}
			l.addresses[address] = ag
		}
		ag.add(nq)
		l.addressSelect.Update(ag)
	}
}

func (l *Limiter) update(nq *nodeQueue, relCost float64) {
	flatWeight, valueWeight := selectionWeights(relCost, nq.value)
	ag := l.addresses[nq.address]
	ag.update(nq, flatWeight)
	l.addressSelect.Update(ag)
	if valueWeight != 0 {
		nq.valueWeight = valueWeight
		l.valueSelect.Update(nq)
	}
}

func (l *Limiter) remove(nq *nodeQueue) {
	ag := l.addresses[nq.address]
	ag.remove(nq)
	if len(ag.nodes) == 0 {
		delete(l.addresses, nq.address)
	}
	l.addressSelect.Remove(ag)
	if nq.valueWeight != 0 {
		l.valueSelect.Remove(nq)
	}
	delete(l.nodes, nq.id)
}

func (l *Limiter) choose() *nodeQueue {
	if l.valueSelect.IsEmpty() || rand.Intn(2) == 0 {
		if ag, ok := l.addressSelect.Choose().(*addressGroup); ok {
			return ag.choose()
		}
	}
	nq, _ := l.valueSelect.Choose().(*nodeQueue)
	return nq
}

func (l *Limiter) processLoop() {
	l.lock.Lock()
	defer l.lock.Unlock()

	for {
		if l.quit {
			return
		}
		nq := l.choose()
		if nq == nil {
			l.cond.Wait()
			continue
		}
		if len(nq.queue) > 0 {
			cmd := nq.queue[0]
			nq.queue = nq.queue[1:]
			l.lock.Unlock()
			cost := l.process(cmd.data)
			fcost, limit := l.costFilter.Filter(cost, cmd.priorWeight)
			var relCost float64
			if limit > fcost {
				relCost = fcost / limit
			} else {
				relCost = 1
			}
			l.clock.Sleep(time.Duration(fcost * l.sleepFactor))
			l.lock.Lock()
			l.update(nq, relCost)
		} else {
			l.remove(nq)
		}
	}
}

func (l *Limiter) Stop() {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.quit = true
	l.cond.Signal()
}
