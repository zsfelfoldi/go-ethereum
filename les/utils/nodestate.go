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
	"reflect"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

type (
	NodeStateMachine struct {
		lock                         sync.Mutex
		clock                        mclock.Clock
		db                           ethdb.Database
		dbMappingKey, dbNodeKey      []byte
		mappings                     []nsMapping
		currentMapping               int
		nodes                        map[enode.ID]*nodeInfo
		nodeStates                   map[*NodeStateFlag]int
		nodeStateNameMap             map[string]int
		nodeFields                   []*NodeStateField
		nodeFieldMasks               []NodeStateBitMask
		nodeFieldMap                 map[*NodeStateField]int
		nodeFieldNameMap             map[string]int
		stateCount                   int
		stateSubs                    []nodeStateSub
		saveImmediately, saveTimeout NodeStateBitMask
		saveTimeoutThreshold         time.Duration
	}

	NodeStateFlag struct {
		name                         string
		saveImmediately, saveTimeout bool
	}

	NodeStateField struct {
		name  string
		ftype reflect.Type
		flags []*NodeStateFlag
	}

	nsMapping struct {
		States, Fields []string
	}

	NodeStateBitMask uint64

	nodeInfo struct {
		state     NodeStateBitMask
		timeouts  []*nodeStateTimeout
		fields    []interface{} //nodeFields
		db, dirty bool
	}

	nodeInfoEnc struct {
		Mapping  uint
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
		dbMappingKey:         append(dbKey, []byte("mapping")...),
		dbNodeKey:            append(dbKey, []byte("node")...),
		saveTimeoutThreshold: saveTimeoutThreshold,
		clock:                clock,
		nodes:                make(map[enode.ID]*nodeInfo),
		nodeStates:           make(map[*NodeStateFlag]int),
		nodeStateNameMap:     make(map[string]int),
		nodeFieldMap:         make(map[*NodeStateField]int),
		nodeFieldNameMap:     make(map[string]int),
	}
	ns.StateMask(NewNodeStateFlag("init", false, false))
	return ns
}

func NewNodeStateFlag(name string, saveImmediately, saveTimeout bool) *NodeStateFlag {
	return &NodeStateFlag{
		name:            name,
		saveImmediately: saveImmediately,
		saveTimeout:     saveTimeout,
	}
}

func NewNodeStateField(name string, ftype reflect.Type, flags []*NodeStateFlag) *NodeStateField {
	return &NodeStateField{
		name:  name,
		ftype: ftype,
		flags: flags,
	}
}

// call before starting the state machine
func (ns *NodeStateMachine) AddStateSub(mask NodeStateBitMask, callback NodeStateCallback) {
	ns.stateSubs = append(ns.stateSubs, nodeStateSub{mask, callback})
}

func (ns *NodeStateMachine) newNode() *nodeInfo {
	return &nodeInfo{
		fields: make([]interface{}, len(ns.nodeFields)),
	}
}

func (ns *NodeStateMachine) LoadFromDb() {
	if enc, err := ns.db.Get(ns.dbMappingKey); err == nil {
		if err := rlp.DecodeBytes(enc, &ns.mappings); err != nil {
			log.Error("Failed to decode node state and field mappings", "error", err)
		}
	}
	mapping := nsMapping{
		States: make([]string, len(ns.nodeStates)),
		Fields: make([]string, len(ns.nodeFields)),
	}
	for flag, index := range ns.nodeStates {
		mapping.States[index] = flag.name
	}
	for index, field := range ns.nodeFields {
		mapping.Fields[index] = field.name
	}
	ns.currentMapping = -1
loop:
	for i, m := range ns.mappings {
		if len(m.States) != len(mapping.States) {
			continue loop
		}
		if len(m.Fields) != len(mapping.Fields) {
			continue loop
		}
		for j, s := range mapping.States {
			if m.States[j] != s {
				continue loop
			}
		}
		for j, s := range mapping.Fields {
			if m.Fields[j] != s {
				continue loop
			}
		}
		ns.currentMapping = i
		break
	}
	if ns.currentMapping == -1 {
		ns.currentMapping = len(ns.mappings)
		ns.mappings = append(ns.mappings, mapping)
		if enc, err := rlp.EncodeToBytes(ns.mappings); err == nil {
			if err := ns.db.Put(ns.dbMappingKey, enc); err != nil {
				log.Error("Failed to save node state and field mappings", "error", err)
			}
		} else {
			log.Error("Failed to encode node state and field mappings", "error", err)
		}
	}

	now := ns.clock.Now()
	it := ns.db.NewIteratorWithPrefix(ns.dbNodeKey)
	for it.Next() {
		var id enode.ID
		if len(it.Key()) != len(ns.dbNodeKey)+len(id) {
			log.Error("Node state db entry with invalid length", "found", len(it.Key()), "expected", len(ns.dbNodeKey)+len(id))
			continue
		}
		copy(id[:], it.Key()[len(ns.dbNodeKey):])
		ns.loadNode(id, it.Value(), now)
	}
}

func (ns *NodeStateMachine) loadNode(id enode.ID, data []byte, now mclock.AbsTime) {
	var enc nodeInfoEnc
	if err := rlp.DecodeBytes(data, &enc); err != nil {
		log.Error("Failed to decode node info", "id", id, "error", err)
		return
	}
	node := ns.newNode()
	node.db = true

	if int(enc.Mapping) >= len(ns.mappings) {
		log.Error("Unknown node state and field mapping", "id", id, "index", enc.Mapping, "len", len(ns.mappings))
		return
	}
	encMapping := ns.mappings[int(enc.Mapping)]
	if len(enc.Fields) != len(encMapping.Fields) {
		log.Error("Invalid node field count", "id", id, "stored", len(enc.Fields), "mapping", len(encMapping.Fields))
		return
	}
loop:
	for i, encField := range enc.Fields {
		if len(encField) > 0 {
			index := i
			if int(enc.Mapping) != ns.currentMapping {
				// convert field mapping
				name := encMapping.Fields[i]
				var ok bool
				if index, ok = ns.nodeFieldNameMap[name]; !ok {
					log.Debug("Dropped unknown node field", "id", id, "field name", name)
					continue loop
				}
			}
			node.fields[index] = reflect.New(ns.nodeFields[index].ftype).Interface()
			if err := rlp.DecodeBytes(encField, node.fields[index]); err != nil {
				log.Error("Failed to decode node field", "id", id, "field index", index, "error", err)
				return
			}
		}
	}
	ns.nodes[id] = node
	state := enc.State
	if int(enc.Mapping) != ns.currentMapping {
		// convert state flag mapping
		state = 0
		for i, name := range encMapping.States {
			if (enc.State & (NodeStateBitMask(1) << i)) != 0 {
				if index, ok := ns.nodeStateNameMap[name]; ok {
					state |= NodeStateBitMask(1) << index
				} else {
					log.Debug("Dropped unknown node state flag", "id", id, "flag name", name)
				}
			}
		}
	}
	ns.initState(id, node, state)
	for _, et := range enc.Timeouts {
		dt := time.Duration(et.At - uint64(now))
		if dt > 0 {
			ns.addTimeout(id, et.Mask, dt)
		} else {
			ns.updateState(id, 0, et.Mask, 0)()
		}
	}
	log.Debug("Loaded node state", "id", id, "state", ns.stateToString(enc.State))
}

func (ns *NodeStateMachine) saveNode(id enode.ID, node *nodeInfo) {
	saveStates := ns.saveImmediately | ns.saveTimeout
	enc := nodeInfoEnc{
		Mapping: uint(ns.currentMapping),
		State:   node.state & saveStates,
		Fields:  make([][]byte, len(ns.nodeFields)),
	}
	log.Debug("Saved node state", "id", id, "state", ns.stateToString(enc.State))
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
			log.Error("Failed to encode node field", "id", id, "fieldIndex", i, "error", err)
		}
	}
	if data, err := rlp.EncodeToBytes(&enc); err == nil {
		if err := ns.db.Put(append(ns.dbNodeKey, id[:]...), data); err != nil {
			log.Error("Failed to save node info", "id", id, "error", err)
		}
	} else {
		log.Error("Failed to encode node info", "id", id, "error", err)
	}
	node.dirty = false
	node.db = true
}

func (ns *NodeStateMachine) deleteNode(id enode.ID) {
	ns.db.Delete(append(ns.dbNodeKey, id[:]...))
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
	}
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
		return
	}
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
func (ns *NodeStateMachine) FieldIndex(field *NodeStateField) int {
	if index, ok := ns.nodeFieldMap[field]; ok {
		return index
	}
	index := len(ns.nodeFields)
	ns.nodeFields = append(ns.nodeFields, field)
	ns.nodeFieldMasks = append(ns.nodeFieldMasks, ns.StatesMask(field.flags))
	ns.nodeFieldMap[field] = index
	if _, ok := ns.nodeFieldNameMap[field.name]; ok {
		log.Error("Node field name collision", "name", field.name)
	}
	ns.nodeFieldNameMap[field.name] = index
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
		log.Error("Invalid node field index", "index", fieldId, "len", len(node.fields))
	}
}

func (ns *NodeStateMachine) StateMask(flag *NodeStateFlag) NodeStateBitMask {
	if state, ok := ns.nodeStates[flag]; ok {
		return NodeStateBitMask(1) << state
	}
	index := ns.stateCount
	mask := NodeStateBitMask(1) << index
	ns.stateCount++
	ns.nodeStates[flag] = index
	if _, ok := ns.nodeStateNameMap[flag.name]; ok {
		log.Error("Node state flag name collision", "name", flag.name)
	}
	ns.nodeStateNameMap[flag.name] = index
	if flag.saveImmediately {
		ns.saveImmediately |= mask
	}
	if flag.saveTimeout {
		ns.saveTimeout |= mask
	}
	return mask
}

func (ns *NodeStateMachine) StatesMask(flags []*NodeStateFlag) NodeStateBitMask {
	var mask NodeStateBitMask
	for _, flag := range flags {
		mask |= ns.StateMask(flag)
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
