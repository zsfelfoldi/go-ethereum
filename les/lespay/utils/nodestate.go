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
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

// 2do: size limit

type (
	NodeStateMachine struct {
		lock                         sync.Mutex
		clock                        mclock.Clock
		db                           ethdb.Database
		dbKey                        []byte
		nodes                        map[enode.ID]*nodeInfo
		nodeFieldTypes               []reflect.Type
		nodeFieldMasks               []NodeStateBitMask
		nodeStates                   map[*NodeStateFlag]int
		stateCount                   int
		stateSubs                    []nodeStateSub
		saveImmediately, saveTimeout NodeStateBitMask
		saveTimeoutThreshold         time.Duration
	}

	NodeStateFlag struct {
		name                         string
		saveImmediately, saveTimeout bool
	}

	NodeStateBitMask uint64

	nodeInfo struct {
		state     NodeStateBitMask
		timeouts  []*nodeStateTimeout
		fields    []interface{} //nodeFields
		db, dirty bool
	}

	nodeInfoEnc struct {
		State    NodeStateBitMask
		Timeouts []nodeStateTimeoutEnc
		Fields   [][]byte
	}

	nodeStateSub struct {
		mask     NodeStateBitMask
		callback NodeStateCallback
	}

	NodeStateCallback func(id enode.ID, oldState, newState NodeStateBitMask)

	nodeStateTimeout struct {
		id    enode.ID
		at    mclock.AbsTime
		timer mclock.Timer
		mask  NodeStateBitMask
	}

	nodeStateTimeoutEnc struct {
		At   uint64
		Mask NodeStateBitMask
	}
)

const initState = NodeStateBitMask(1)

func NewNodeStateMachine(db ethdb.Database, dbKey []byte, saveTimeoutThreshold time.Duration, clock mclock.Clock) *NodeStateMachine {
	ns := &NodeStateMachine{
		db:                   db,
		dbKey:                dbKey,
		saveTimeoutThreshold: saveTimeoutThreshold,
		clock:                clock,
		nodes:                make(map[enode.ID]*nodeInfo),
		nodeStates:           make(map[*NodeStateFlag]int),
	}
	ns.GetState(NewNodeStateFlag("init", false, false))
	return ns
}

func NewNodeStateFlag(name string, saveImmediately, saveTimeout bool) *NodeStateFlag {
	return &NodeStateFlag{
		name:            name,
		saveImmediately: saveImmediately,
		saveTimeout:     saveTimeout,
	}
}

// call before starting the state machine
func (ns *NodeStateMachine) AddStateSub(mask NodeStateBitMask, callback NodeStateCallback) {
	ns.stateSubs = append(ns.stateSubs, nodeStateSub{mask, callback})
}

func (ns *NodeStateMachine) newNode() *nodeInfo {
	return &nodeInfo{
		fields: make([]interface{}, len(ns.nodeFieldTypes)),
	}
}

func (ns *NodeStateMachine) LoadFromDb() {
	now := ns.clock.Now()
	it := ns.db.NewIteratorWithPrefix(ns.dbKey)
	for it.Next() {
		fmt.Println("found new db entry")
		var id enode.ID
		if len(it.Key()) != len(id)+len(ns.dbKey) {
			fmt.Println("*** len(it.Key()) != len(id)")
			continue
		}
		copy(id[:], it.Key()[len(ns.dbKey):])
		var enc nodeInfoEnc
		if err := rlp.DecodeBytes(it.Value(), &enc); err != nil {
			fmt.Println("*** decode error", err)
			continue
		}
		node := ns.newNode()
		node.db = true
		fieldCount := len(node.fields)
		if len(enc.Fields) != fieldCount {
			// error
			if len(enc.Fields) < fieldCount {
				fieldCount = len(enc.Fields)
			}
		}
		for i := 0; i < fieldCount; i++ {
			if len(enc.Fields[i]) > 0 {
				fmt.Println("decoding field", i, ns.nodeFieldTypes[i])
				node.fields[i] = reflect.New(ns.nodeFieldTypes[i]).Interface()
				fmt.Println("type before decode", reflect.TypeOf(node.fields[i]))
				if err := rlp.DecodeBytes(enc.Fields[i], node.fields[i]); err != nil {
					fmt.Println("*** field decode error", err)
					continue
				}
				fmt.Println("type after decode", reflect.TypeOf(node.fields[i]))
			}
		}
		ns.nodes[id] = node
		ns.initState(id, node, enc.State)
		for _, et := range enc.Timeouts {
			dt := time.Duration(et.At - uint64(now))
			if dt > 0 {
				ns.addTimeout(id, et.Mask, dt)
			} else {
				ns.updateState(id, 0, et.Mask, 0)()
			}
		}
	}
}

func (ns *NodeStateMachine) saveNode(id enode.ID, node *nodeInfo) {
	saveStates := ns.saveImmediately | ns.saveTimeout
	enc := nodeInfoEnc{
		State:  node.state & saveStates,
		Fields: make([][]byte, len(ns.nodeFieldTypes)),
	}
	fmt.Println("saveNode", id, "state", ns.stateToString(node.state), "savedState", ns.stateToString(node.state&saveStates))
	for _, t := range node.timeouts {
		if mask := t.mask & saveStates; mask != 0 {
			enc.Timeouts = append(enc.Timeouts, nodeStateTimeoutEnc{
				At:   uint64(t.at),
				Mask: mask,
			})
		}
	}
	for i, f := range node.fields {
		var err error
		if enc.Fields[i], err = rlp.EncodeToBytes(f); err != nil {
			// error
		}
	}
	if data, err := rlp.EncodeToBytes(&enc); err == nil {
		ns.db.Put(append(ns.dbKey, id[:]...), data)
	} else {
		// error
	}
	node.dirty = false
	node.db = true
}

func (ns *NodeStateMachine) deleteNode(id enode.ID) {
	ns.db.Delete(append(ns.dbKey, id[:]...))
}

func (ns *NodeStateMachine) SaveToDb() {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	for id, node := range ns.nodes {
		if node.dirty {
			ns.saveNode(id, node)
		}
	}
}

// returns after all resulting immediate changes are processed
func (ns *NodeStateMachine) UpdateState(id enode.ID, set, reset NodeStateBitMask, timeout time.Duration) {
	ns.lock.Lock()
	cb := ns.updateState(id, set, reset, timeout)
	ns.lock.Unlock()
	cb()
}

func (ns *NodeStateMachine) updateState(id enode.ID, set, reset NodeStateBitMask, timeout time.Duration) func() {
	node := ns.nodes[id]
	if node == nil {
		node = ns.newNode()
		ns.nodes[id] = node
		fmt.Println("updateState", id, "newNode")
	}
	fmt.Println("updateState", id, "state", ns.stateToString(node.state), "set", ns.stateToString(set), "reset", ns.stateToString(reset))
	newState := (node.state & (^reset)) | set
	if newState == node.state {
		return func() {}
	}
	oldState := node.state
	changed := oldState ^ newState
	node.state = newState
	// remove timers of reset states
	ns.removeTimeouts(node, oldState&(^newState))
	setStates := newState & (^oldState)
	if timeout != 0 && setStates != 0 {
		ns.addTimeout(id, setStates, timeout)
	}
	if newState == 0 {
		delete(ns.nodes, id)
		if node.db {
			ns.deleteNode(id)
		}
	} else {
		for i, f := range node.fields {
			if f != nil {
				if ns.nodeFieldMasks[i]&newState == 0 {
					node.fields[i] = nil
				}
			}
		}
		if changed&ns.saveTimeout != 0 {
			node.dirty = true
		}
		if changed&ns.saveImmediately != 0 {
			ns.saveNode(id, node)
		}
	}
	return func() {
		// call state update subscription callbacks without holding the mutex
		for _, sub := range ns.stateSubs {
			if changed&sub.mask != 0 {
				sub.callback(id, oldState&sub.mask, newState&sub.mask)
			}
		}
	}
}

func (ns *NodeStateMachine) initState(id enode.ID, node *nodeInfo, state NodeStateBitMask) {
	node.state = state
	for _, sub := range ns.stateSubs {
		if (node.state|initState)&sub.mask != 0 {
			sub.callback(id, initState&sub.mask, node.state&sub.mask)
		}
	}
}

func (ns *NodeStateMachine) AddTimeout(id enode.ID, mask NodeStateBitMask, timeout time.Duration) {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	ns.addTimeout(id, mask, timeout)
}

func (ns *NodeStateMachine) addTimeout(id enode.ID, mask NodeStateBitMask, timeout time.Duration) {
	node := ns.nodes[id]
	if node == nil {
		fmt.Println("addTimeout", id, "unknown")
		return
	}
	fmt.Println("addTimeout", id, "state", ns.stateToString(node.state), "mask", ns.stateToString(mask), "timeout", timeout)
	mask &= node.state
	ns.removeTimeouts(node, mask)
	t := &nodeStateTimeout{
		id:   id,
		at:   ns.clock.Now() + mclock.AbsTime(timeout),
		mask: mask,
	}
	t.timer = ns.clock.AfterFunc(timeout, func() {
		var cb func()
		ns.lock.Lock()
		if t.mask != 0 {
			fmt.Println("timeout", id, "mask", ns.stateToString(t.mask))
			cb = ns.updateState(id, 0, t.mask, 0)
		}
		ns.lock.Unlock()
		if cb != nil {
			cb()
		}
	})
	node.timeouts = append(node.timeouts, t)
	if mask&ns.saveTimeout != 0 {
		if timeout >= ns.saveTimeoutThreshold {
			ns.saveNode(id, node)
		} else {
			node.dirty = true
		}
	}
}

func (ns *NodeStateMachine) removeTimeouts(node *nodeInfo, mask NodeStateBitMask) {
	for i := 0; i < len(node.timeouts); i++ {
		t := node.timeouts[i]
		if match := t.mask & mask; match != 0 {
			t.mask -= match
			if t.mask == 0 {
				t.timer.Stop()
				node.timeouts[i] = node.timeouts[len(node.timeouts)-1]
				node.timeouts = node.timeouts[:len(node.timeouts)-1]
				i--
			}
		}
	}
}

// call before starting the state machine
func (ns *NodeStateMachine) RegisterField(fieldType reflect.Type, fieldMask NodeStateBitMask) int {
	index := len(ns.nodeFieldTypes)
	ns.nodeFieldTypes = append(ns.nodeFieldTypes, fieldType)
	ns.nodeFieldMasks = append(ns.nodeFieldMasks, fieldMask)
	return index
}

func (ns *NodeStateMachine) GetField(id enode.ID, fieldId int) interface{} {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	if node := ns.nodes[id]; node != nil && fieldId < len(node.fields) {
		return node.fields[fieldId]
	} else {
		return nil
	}
}

func (ns *NodeStateMachine) SetField(id enode.ID, fieldId int, value interface{}) {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	node := ns.nodes[id]
	if node == nil {
		node = ns.newNode()
		ns.nodes[id] = node
	}

	if fieldId < len(node.fields) {
		node.fields[fieldId] = value
	} else {
		// error
	}
}

func (ns *NodeStateMachine) GetState(field *NodeStateFlag) NodeStateBitMask {
	if state, ok := ns.nodeStates[field]; ok {
		return NodeStateBitMask(1) << state
	}
	state := ns.stateCount
	mask := NodeStateBitMask(1) << state
	ns.stateCount++
	ns.nodeStates[field] = state
	if field.saveImmediately {
		ns.saveImmediately |= mask
	}
	if field.saveTimeout {
		ns.saveTimeout |= mask
	}
	return mask
}

func (ns *NodeStateMachine) GetStates(fields []*NodeStateFlag) NodeStateBitMask {
	var mask NodeStateBitMask
	for _, field := range fields {
		mask |= ns.GetState(field)
	}
	return mask
}

func (ns *NodeStateMachine) stateToString(states NodeStateBitMask) string {
	s := "["
	comma := false
	for field, index := range ns.nodeStates {
		if states&(NodeStateBitMask(1)<<index) != 0 {
			if comma {
				s = s + ", "
			}
			s = s + field.name
			comma = true
		}
	}
	s = s + "]"
	return s
}
