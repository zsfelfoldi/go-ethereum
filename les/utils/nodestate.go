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
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

type (
	// NodeStateMachine connects different system components operating on subsets of
	// network nodes. Node states are represented by 64 bit vectors with each bit assigned
	// to a state flag. Each state flag has a descriptor structure and the mapping is
	// created automatically. It is possible to subscribe to subsets of state flags and
	// receive a callback if one of the nodes has a relevant state flag changed.
	// Callbacks can also modify further flags of the same node or other nodes. State
	// updates only return after all immediate effects throughout the system have happened
	// (deadlocks should be avoided by design of the implemented state logic). The caller
	// can also add timeouts assigned to a certain node and a subset of state flags.
	// If the timeout elapses, the flags are reset. If all relevant flags are reset then
	// the timer is dropped. The state flags and the associated timers are persisted in
	// the database if the flag descriptor enables saving.
	//
	// Extra node fields can also be registered so system components can also store more
	// complex state for each node that is relevant to them, without creating a custom
	// peer set. Fields can be shared across multiple components if they all know the
	// field ID. Fields are also assigned to a set of state flags and the contents of
	// each field is only retained for a certain node as long as at least one of these
	// flags is set (meaning that the node is still interesting to the component(s) which
	// are interested in the given field). Persistent fields should implement rlp.Encoder
	// and rlp.Decoder.
	NodeStateMachine struct {
		inited                  uint32
		lock                    sync.Mutex
		clock                   mclock.Clock
		db                      ethdb.Database
		dbMappingKey, dbNodeKey []byte
		mappings                []nsMapping
		currentMapping          int
		nodes                   map[enode.ID]*nodeInfo

		// Registered state flags or fields. Modifications are allowed
		// only when the node state machine has not been initialized.
		nodeStates                      map[*NodeStateFlag]int
		nodeStateNameMap                map[string]int
		nodeFields                      []*NodeStateField
		nodeFieldMasks                  []NodeStateBitMask
		nodeFieldMap                    map[*NodeStateField]int
		nodeFieldNameMap                map[string]int
		stateCount                      int
		stateLimit                      int
		saveImmediately, saveAtShutdown NodeStateBitMask

		// Installed callbacks. Modifications are allowed only when the
		// node state machine has not been initialized.
		stateSubs []nodeStateSub

		// Testing hooks, only for testing purposes.
		saveNodeHook func(*nodeInfo)
	}

	// NodeStateFlag describes a node state flag. Each registered instance is automatically
	// mapped to a bit of the 64 bit node states.
	// If saveImmediately is true then the node is saved each time the flag is switched on
	// or off. If saveAtShutdown is true then the node is saved when state machine is shutdown.
	NodeStateFlag struct {
		name                            string
		saveImmediately, saveAtShutdown bool
	}

	// NodeStateField describes an optional node field of the given type. The contents
	// of the field are only retained for each node as long as at least one of the
	// specified flags is set. If all relevant flags are reset then the field is removed
	// after all callbacks of the state change are processed.
	NodeStateField struct {
		name  string
		ftype reflect.Type
		flags []*NodeStateFlag
	}

	// nsMapping describes an index mapping of node state flags and fields. Mapping is
	// determined during startup, before loading node data from the database. The used
	// mapping is saved for each node and is converted upon loading if it has changed
	// since the last time it was saved.
	nsMapping struct {
		States, Fields []string
	}

	// NodeStateBitMask describes a node state or state mask. It represents a subset
	// of node flags with each bit assigned to a flag index (LSB represents flag 0).
	NodeStateBitMask uint64

	// NodeStateCallback is a subscription callback which is called when one of the
	// state flags that is included in the subscription state mask is changed.
	// Note: oldState and newState are also masked with the subscription mask so only
	// the relevant bits are included.
	NodeStateCallback func(id enode.ID, oldState, newState NodeStateBitMask)

	// nodeInfo contains node state, fields and state timeouts
	nodeInfo struct {
		state          NodeStateBitMask
		timeouts       []*nodeStateTimeout
		fields         []interface{}
		fieldGcCounter int
		db, dirty      bool
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

var (
	errAlreadyInited = errors.New("state machine is already initialized")
	errStateOverflow = errors.New("registered state flag exceeds the limit")
	errNameCollision = errors.New("state flag or node field name collision")
	errOutOfBound    = errors.New("out of bound")
	errInvalidField  = errors.New("invalid field")
)

// initState is a special state that is assumed to be set before a node is loaded from
// the database. After loading initState is reset and the saved state flags are set.
const initState = NodeStateBitMask(1)

// NewNodeStateMachine creates a new node state machine.
// Note: if state timers are persisted in the database then clock value should also
// be persisted at shutdown and resumed at next startup.
func NewNodeStateMachine(db ethdb.Database, dbKey []byte, clock mclock.Clock) *NodeStateMachine {
	ns := &NodeStateMachine{
		db:               db,
		dbMappingKey:     append(dbKey, []byte("mapping")...),
		dbNodeKey:        append(dbKey, []byte("node")...),
		clock:            clock,
		nodes:            make(map[enode.ID]*nodeInfo),
		nodeStates:       make(map[*NodeStateFlag]int),
		nodeStateNameMap: make(map[string]int),
		nodeFieldMap:     make(map[*NodeStateField]int),
		nodeFieldNameMap: make(map[string]int),
		stateLimit:       8 * int(unsafe.Sizeof(NodeStateBitMask(0))),
	}
	// init flag is always mapped to index 0 (initState bit mask)
	ns.RegisterState(NewNodeStateFlag("init", false, false))
	return ns
}

// NewNodeStateFlag creates a new node state flag. Mapping happens when it is first passed
// to NodeStateMachine.RegisterState
func NewNodeStateFlag(name string, saveImmediately, saveAtShutdown bool) *NodeStateFlag {
	return &NodeStateFlag{
		name:            name,
		saveImmediately: saveImmediately,
		saveAtShutdown:  saveAtShutdown,
	}
}

// NewNodeStateField creates a new node state field. Mapping happens when it is first passed
// to NodeStateMachine.RegisterField
func NewNodeStateField(name string, ftype reflect.Type, flags []*NodeStateFlag) *NodeStateField {
	return &NodeStateField{
		name:  name,
		ftype: ftype,
		flags: flags,
	}
}

// AddStateSub adds a node state subscription. The callback is called while the state
// machine mutex is not held and it is allowed to make further state updates. All immediate
// changes throughout the system are processed in the same thread/goroutine. It is the
// responsibility of the implemented state logic to avoid deadlocks caused by the callbacks,
// infinite toggling of flags or hazardous/non-deterministic state changes.
// State subscriptions should be installed before loading the node database or making the
// first state update.
func (ns *NodeStateMachine) AddStateSub(mask NodeStateBitMask, callback NodeStateCallback) error {
	if atomic.LoadUint32(&ns.inited) == 1 {
		return errAlreadyInited
	}
	ns.stateSubs = append(ns.stateSubs, nodeStateSub{mask, callback})
	return nil
}

// newNode creates a new nodeInfo
func (ns *NodeStateMachine) newNode() *nodeInfo {
	return &nodeInfo{fields: make([]interface{}, len(ns.nodeFields))}
}

// init marks the node state machine as initialized. After this no more
// state registration or callback installation is allowed.
func (ns *NodeStateMachine) init() {
	atomic.StoreUint32(&ns.inited, 1)
}

// LoadFromDb loads persisted node states from the database
func (ns *NodeStateMachine) LoadFromDb() {
	ns.init()
	if enc, err := ns.db.Get(ns.dbMappingKey); err == nil {
		if err := rlp.DecodeBytes(enc, &ns.mappings); err != nil {
			log.Error("Failed to decode scheme", "error", err)
			return
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

		// Scheme should be persisted properly, otherwise
		// all persisted data can't be resolved next time.
		enc, err := rlp.EncodeToBytes(ns.mappings)
		if err != nil {
			panic("Failed to encode scheme")
		}
		if err := ns.db.Put(ns.dbMappingKey, enc); err != nil {
			panic("Failed to save scheme")
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
		ns.decodeNode(id, it.Value(), now)
	}
}

// decodeNode decodes a node database entry and adds it to the node set if successful
func (ns *NodeStateMachine) decodeNode(id enode.ID, data []byte, now mclock.AbsTime) {
	var enc nodeInfoEnc
	if err := rlp.DecodeBytes(data, &enc); err != nil {
		log.Error("Failed to decode node info", "id", id, "error", err)
		return
	}
	node := ns.newNode()
	node.db = true

	if int(enc.Mapping) >= len(ns.mappings) {
		log.Error("Unknown scheme", "id", id, "index", enc.Mapping, "len", len(ns.mappings))
		return
	}
	encMapping := ns.mappings[int(enc.Mapping)]
	if len(enc.Fields) != len(encMapping.Fields) {
		log.Error("Invalid node field count", "id", id, "stored", len(enc.Fields), "mapping", len(encMapping.Fields))
		return
	}
	// convertMask converts a old format state/mask to the latest version.
	convertMask := func(schemeID int, scheme []string, state NodeStateBitMask) (NodeStateBitMask, bool) {
		if schemeID == ns.currentMapping {
			return state, true // Nothing need to be changed
		}
		var converted NodeStateBitMask
		for i, name := range scheme {
			if (state & (NodeStateBitMask(1) << i)) != 0 {
				if index, ok := ns.nodeStateNameMap[name]; ok {
					converted |= NodeStateBitMask(1) << index
				} else {
					return NodeStateBitMask(0), false // unkonwn flag
				}
			}
		}
		return converted, true
	}
	// Resolve persisted node fields
	for i, encField := range enc.Fields {
		if len(encField) == 0 {
			continue
		}
		index := i
		if int(enc.Mapping) != ns.currentMapping {
			name := encMapping.Fields[i]
			var ok bool
			if index, ok = ns.nodeFieldNameMap[name]; !ok {
				log.Error("Dropped unknown node field", "id", id, "field name", name)
				return
			}
		}
		node.fields[index] = reflect.New(ns.nodeFields[index].ftype).Interface()
		if err := rlp.DecodeBytes(encField, node.fields[index]); err != nil {
			log.Error("Failed to decode node field", "id", id, "field index", index, "error", err)
			return
		}
	}
	// Resolve node state
	state, success := convertMask(int(enc.Mapping), encMapping.States, enc.State)
	if !success {
		return
	}
	var masks []NodeStateBitMask
	for _, et := range enc.Timeouts {
		mask, success := convertMask(int(enc.Mapping), encMapping.States, et.Mask)
		if !success {
			return
		}
		masks = append(masks, mask)
	}
	// It's a compatible node record, add it to set.
	ns.nodes[id] = node
	ns.initState(id, node, state)
	for index, et := range enc.Timeouts {
		dt := time.Duration(et.At - uint64(now))
		if dt > 0 {
			ns.addTimeout(id, masks[index], dt)
		} else {
			if cb := ns.updateState(id, 0, masks[index], 0); cb != nil {
				cb()
			}
		}
	}
	log.Debug("Loaded node state", "id", id, "state", ns.stateToString(enc.State))
}

// saveNode saves the given node info to the database
func (ns *NodeStateMachine) saveNode(id enode.ID, node *nodeInfo) error {
	saveStates := ns.saveImmediately | ns.saveAtShutdown
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
		if f == nil {
			continue
		}
		blob, err := rlp.EncodeToBytes(f)
		if err != nil {
			return err
		}
		enc.Fields[i] = blob
	}
	data, err := rlp.EncodeToBytes(&enc)
	if err != nil {
		return err
	}
	if err := ns.db.Put(append(ns.dbNodeKey, id[:]...), data); err != nil {
		return err
	}
	node.dirty, node.db = false, true

	if ns.saveNodeHook != nil {
		ns.saveNodeHook(node)
	}
	return nil
}

// deleteNode removes a node info from the database
func (ns *NodeStateMachine) deleteNode(id enode.ID) {
	ns.db.Delete(append(ns.dbNodeKey, id[:]...))
}

// SaveToDb saves all nodes that have been changed but not saved immediately
func (ns *NodeStateMachine) SaveToDb() {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	for id, node := range ns.nodes {
		if node.dirty {
			err := ns.saveNode(id, node)
			if err != nil {
				log.Error("Failed to save node", "id", id, "error", err)
			}
		}
	}
}

// UpdateState updates the given node state flags and processes all resulting callbacks.
// It only returns after all subsequent immediate changes (including those changed by the
// callbacks) have been processed.
func (ns *NodeStateMachine) UpdateState(id enode.ID, set, reset NodeStateBitMask, timeout time.Duration) {
	ns.lock.Lock()
	ns.init()
	cb := ns.updateState(id, set, reset, timeout)
	ns.lock.Unlock()
	if cb != nil {
		cb()
	}
}

// updateState performs a node state update and returns a function that processes state
// subscription callbacks and should be called while the mutex is not held.
//
// If the timeout is specified, it means the set states will be reset after the specified
// time interval.
func (ns *NodeStateMachine) updateState(id enode.ID, set, reset NodeStateBitMask, timeout time.Duration) func() {
	node := ns.nodes[id]
	if node == nil {
		node = ns.newNode()
		ns.nodes[id] = node
	}
	newState := (node.state & (^reset)) | set
	if newState == node.state {
		return nil
	}
	oldState := node.state
	changed := oldState ^ newState
	node.state = newState

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
		if changed&ns.saveImmediately != 0 {
			err := ns.saveNode(id, node)
			if err != nil {
				log.Error("Failed to save node", "id", id, "error", err)
			}
		} else if changed&ns.saveAtShutdown != 0 {
			node.dirty = true
		}
		node.fieldGcCounter++
	}
	return func() {
		// call state update subscription callbacks without holding the mutex
		for _, sub := range ns.stateSubs {
			if changed&sub.mask != 0 {
				sub.callback(id, oldState&sub.mask, newState&sub.mask)
			}
		}
		if newState != 0 {
			ns.lock.Lock()
			node.fieldGcCounter--
			if node.fieldGcCounter == 0 {
				// remove all fields that are not needed any more after reaching the final state
				for i, f := range node.fields {
					if f != nil {
						if ns.nodeFieldMasks[i]&node.state == 0 {
							node.fields[i] = nil
						}
					}
				}
			}
			ns.lock.Unlock()
		}
	}
}

// initState performs the first state update after loading a node from the database
func (ns *NodeStateMachine) initState(id enode.ID, node *nodeInfo, state NodeStateBitMask) {
	node.state = state
	for _, sub := range ns.stateSubs {
		if (node.state|initState)&sub.mask != 0 {
			sub.callback(id, initState&sub.mask, node.state&sub.mask)
		}
	}
}

// AddTimeout adds a node state timeout associated to the given state flag(s).
// After the specified time interval, the relevant states will be reset.
func (ns *NodeStateMachine) AddTimeout(id enode.ID, mask NodeStateBitMask, timeout time.Duration) {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	ns.init()
	ns.addTimeout(id, mask, timeout)
}

// addTimeout adds a node state timeout associated to the given state flag(s).
func (ns *NodeStateMachine) addTimeout(id enode.ID, mask NodeStateBitMask, timeout time.Duration) {
	node := ns.nodes[id]
	if node == nil {
		return
	}
	mask &= node.state
	if mask == 0 {
		return
	}
	ns.removeTimeouts(node, mask)
	t := &nodeStateTimeout{
		id:   id,
		at:   ns.clock.Now() + mclock.AbsTime(timeout),
		mask: mask,
	}
	t.timer = ns.clock.AfterFunc(timeout, func() {
		ns.lock.Lock()
		cb := ns.updateState(id, 0, t.mask, 0)
		ns.lock.Unlock()
		if cb != nil {
			cb()
		}
	})
	node.timeouts = append(node.timeouts, t)
	if mask&ns.saveAtShutdown != 0 {
		node.dirty = true
	}
}

// removeTimeout removes node state timeouts associated to the given state flag(s).
// If a timeout was associated to multiple flags which are not all included in the
// specified remove mask then only the included flags are de-associated and the timer
// stays active.
func (ns *NodeStateMachine) removeTimeouts(node *nodeInfo, mask NodeStateBitMask) {
	for i := 0; i < len(node.timeouts); i++ {
		t := node.timeouts[i]
		match := t.mask & mask
		if match == 0 {
			continue
		}
		t.mask -= match
		if t.mask != 0 {
			continue
		}
		t.timer.Stop()
		node.timeouts[i] = node.timeouts[len(node.timeouts)-1]
		node.timeouts = node.timeouts[:len(node.timeouts)-1]
		i--
	}
}

// RegisterField adds the node field if it has not been added before and returns the field index.
// Node fields should be mapped before loading the node database or making the first state update.
func (ns *NodeStateMachine) RegisterField(field *NodeStateField) (int, error) {
	// Short circuit if it's already registered.
	if index, ok := ns.nodeFieldMap[field]; ok {
		return index, nil
	}
	// Ensure the registration time window is still opened.
	if atomic.LoadUint32(&ns.inited) == 1 {
		return 0, errAlreadyInited
	}
	// Ensure the field name is still avaliable.
	if _, ok := ns.nodeFieldNameMap[field.name]; ok {
		return 0, errNameCollision
	}
	index := len(ns.nodeFields)
	ns.nodeFields = append(ns.nodeFields, field)
	ns.nodeFieldMasks = append(ns.nodeFieldMasks, ns.StatesMask(field.flags))
	ns.nodeFieldMap[field] = index
	ns.nodeFieldNameMap[field.name] = index
	return index, nil
}

// GetField retrieves the given field of the given node
func (ns *NodeStateMachine) GetField(id enode.ID, fieldId int) interface{} {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	if node := ns.nodes[id]; node != nil && fieldId < len(node.fields) {
		return node.fields[fieldId]
	}
	return nil
}

// SetField sets the given field of the given node
func (ns *NodeStateMachine) SetField(id enode.ID, fieldId int, value interface{}) error {
	ns.lock.Lock()
	defer ns.lock.Unlock()

	// Allocate the node if it's non-existent
	node := ns.nodes[id]
	if node == nil {
		node = ns.newNode()
	}
	// Refuse to set field if it's unknown or the relevant state is unset.
	if fieldId >= len(ns.nodeFields) {
		return errOutOfBound
	}
	if reflect.TypeOf(value) != ns.nodeFields[fieldId].ftype {
		return errInvalidField
	}
	fieldMask := ns.nodeFieldMasks[fieldId]
	if fieldMask&node.state == 0 {
		return nil
	}
	node.fields[fieldId] = value
	ns.nodes[id] = node

	// Persist node after change if necessary
	if fieldMask&ns.saveImmediately != 0 {
		err := ns.saveNode(id, node)
		if err != nil {
			log.Error("Failed to save node", "id", id, "error", err)
		}
	} else if fieldMask&ns.saveAtShutdown != 0 {
		node.dirty = true
	}
	return nil
}

// RegisterState assigns a bit index to the given flag if it has not been mapped
// before and returns the node state bit mask. State flags should be mapped before
// loading the node database or making the first state update.
func (ns *NodeStateMachine) RegisterState(flag *NodeStateFlag) (NodeStateBitMask, error) {
	// Short circuit if it's already registered.
	if state, ok := ns.nodeStates[flag]; ok {
		return NodeStateBitMask(1) << state, nil
	}
	// Ensure the registration time window is still opened.
	if atomic.LoadUint32(&ns.inited) == 1 {
		return NodeStateBitMask(0), errAlreadyInited
	}
	// Ensure the registered states is under the limitation.
	if len(ns.nodeStates) >= ns.stateLimit {
		return NodeStateBitMask(0), errStateOverflow
	}
	// Ensure the flag name is still avaliable.
	if _, ok := ns.nodeStateNameMap[flag.name]; ok {
		return NodeStateBitMask(0), errNameCollision
	}
	// Pass all checking, register it now
	index := ns.stateCount
	mask := NodeStateBitMask(1) << index
	ns.stateCount++
	ns.nodeStates[flag] = index
	ns.nodeStateNameMap[flag.name] = index

	if flag.saveImmediately {
		ns.saveImmediately |= mask
	}
	if flag.saveAtShutdown {
		ns.saveAtShutdown |= mask
	}
	return mask, nil
}

// StateMask returns the state mask associated with given flag.
func (ns *NodeStateMachine) StateMask(flag *NodeStateFlag) NodeStateBitMask {
	if state, ok := ns.nodeStates[flag]; ok {
		return NodeStateBitMask(1) << state
	}
	return NodeStateBitMask(0)
}

// StatesMask assigns a bit index to the given flags if they have not been mapped before and
// returns the node state bit mask.
// State flags should be mapped before loading the node database or making the first state update.
func (ns *NodeStateMachine) StatesMask(flags []*NodeStateFlag) NodeStateBitMask {
	var mask NodeStateBitMask
	for _, flag := range flags {
		mask |= ns.StateMask(flag)
	}
	return mask
}

// stateToString returns a list of the names of the flags specified in the bit mask
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

// String returns the 2-based format to better represent "bits"
func (mask NodeStateBitMask) String() string {
	return fmt.Sprintf("%b", mask)
}
