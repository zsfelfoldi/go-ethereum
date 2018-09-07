// Copyright 2018 The go-ethereum Authors
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

package les

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/rpc"
)

var (
	ErrInvalidID          = errors.New("extension ID invalid or already in use")
	ErrAuthFailed         = errors.New("extension authentication failed")
	ErrNotMaster          = errors.New("master privilege required")
	ErrLimit              = errors.New("total weight limit exceeded")
	ErrReduceLimit        = errors.New("cannot reduce weight limit")
	ErrUnknownClient      = errors.New("unknown client")
	ErrApiMessageSend     = errors.New("could not send API message")
	ErrApiMessageUnwanted = errors.New("unwanted API message")
)

type clientInfo struct {
	owner  *serverExtension
	weight uint64
}

type serverExtension struct {
	totalWeight, limit uint64
	clients            map[string]*clientInfo
}

type serverAPI struct {
	*PublicLesCommonAPI
	server                 *LesServer
	extensions             map[string]*serverExtension
	clients                map[string]*clientInfo
	master                 map[string]struct{}
	extLimits, globalLimit uint64
}

// NewPublicLesServerAPI creates a new les server API.
func NewLesServerAPI(server *LesServer, commonAPI *PublicLesCommonAPI) *serverAPI {
	api := &serverAPI{
		PublicLesCommonAPI: commonAPI,
		server:             server,
		extensions:         make(map[string]*serverExtension),
		clients:            make(map[string]*clientInfo),
		master:             make(map[string]struct{}),
	}
	commonAPI.connectCallback = api.connect
	commonAPI.registerCallback = api.register
	return api
}

func (api *serverAPI) register(id string) {
	api.extensions[id] = &serverExtension{clients: make(map[string]*clientInfo)}
}

func (api *serverAPI) connect(peerId string, connect bool) {
	if connect {
		api.clients[peerId] = &clientInfo{}
	} else {
		c := api.clients[peerId]
		if c == nil {
			return
		}
		if c.owner != nil {
			delete(c.owner.clients, peerId)
			c.owner.totalWeight -= c.weight
		}
		delete(api.clients, peerId)
	}
}

type (
	PrivateLesServerAPI struct{ *serverAPI }
	PublicLesServerAPI  struct{ *serverAPI }
)

func (api PrivateLesServerAPI) SetMaster(id string) {
	api.lock.Lock()
	defer api.lock.Unlock()

	api.master[id] = struct{}{}
}

func (api PublicLesServerAPI) SetLimit(master, masterKey, target string, newLimit uint64) error {
	api.lock.Lock()
	defer api.lock.Unlock()

	if err := api.authenticate(master, masterKey); err != nil {
		return err
	}
	if _, ok := api.master[master]; !ok {
		return ErrNotMaster
	}
	ext := api.extensions[target]
	if ext == nil {
		return ErrInvalidID
	}

	if newLimit < ext.totalWeight {
		return ErrReduceLimit
	}
	if newLimit != ext.limit {
		if newLimit > api.globalLimit-api.extLimits+ext.limit {
			return ErrLimit
		}
		api.extLimits += newLimit - ext.limit
		ext.limit = newLimit
		api.broadcast(ApiMessageFrom{Msg: "system/limit", Data: msgData{"ext": target, "limit": newLimit}})
	}
	return nil
}

func (api PublicLesServerAPI) SetWeight(id, key, client string, weight uint64) (bool, error) {
	api.lock.Lock()
	defer api.lock.Unlock()

	if err := api.authenticate(id, key); err != nil {
		return false, err
	}
	ext := api.extensions[id]
	c := api.clients[client]
	if c == nil {
		return false, ErrUnknownClient
	}
	if c.owner == ext {
		if weight > ext.limit-ext.totalWeight+c.weight {
			return false, ErrLimit
		}
	} else {
		if weight <= c.weight {
			return false, nil
		}
		if weight > ext.limit-ext.totalWeight {
			return false, ErrLimit
		}
	}
	if c.owner != nil {
		c.owner.totalWeight -= c.weight
	}
	if weight != 0 {
		ext.totalWeight += weight
	} else {
		ext = nil
		id = ""
	}
	c.weight = weight
	if ext != c.owner {
		delete(c.owner.clients, client)
		if ext != nil {
			ext.clients[client] = c
		}
		c.owner = ext
	}
	api.broadcast(ApiMessageFrom{Msg: "system/weight", Data: msgData{"client": client, "ext": id, "weight": weight}})
	return true, nil
}

//------------------------------------------------------------
//------------------------------------------------------------
//------------------------------------------------------------
//------------------------------------------------------------
//------------------------------------------------------------

type (
	msgData map[string]interface{}

	ApiMessage struct {
		Msg  string  `json:"msg"`
		Data msgData `json:"data"`
	}

	ApiMessageFrom struct {
		Msg  string  `json:"msg"`
		Data msgData `json:"data"`
		Peer string  `json:"peer"`
	}
)

type PublicLesCommonAPI struct {
	lock       sync.RWMutex
	keys       map[string]string
	sendRemote func(to string, msg ApiMessage) error
	subs       map[chan ApiMessageFrom]PrefixSet
	allFilters PrefixSet

	connectCallback  func(peerId string, connected bool)
	registerCallback func(extId string)
}

// NewPublicLesServerAPI creates a new les server API.
func NewPublicLesCommonAPI(sendRemote func(to string, msg ApiMessage) error) *PublicLesCommonAPI {
	return &PublicLesCommonAPI{
		sendRemote: sendRemote,
		keys:       make(map[string]string),
		subs:       make(map[chan ApiMessageFrom]PrefixSet),
	}
}

func (api *PublicLesCommonAPI) peerConnect(peerId string, connected bool) {
	api.lock.Lock()
	defer api.lock.Unlock()

	if api.connectCallback != nil {
		api.connectCallback(peerId, connected)
	}
	msg := ApiMessageFrom{Peer: peerId}
	if connected {
		msg.Msg = "system/connect"
	} else {
		msg.Msg = "system/disconnect"
	}
	api.broadcast(msg)
}

func (api *PublicLesCommonAPI) RegisterExtension(id, key string) error {
	api.lock.Lock()
	defer api.lock.Unlock()

	if len(id) == 0 || strings.Contains(id, "/") {
		return ErrInvalidID
	}
	if _, ok := api.keys[id]; ok {
		return ErrInvalidID
	}
	api.keys[id] = key
	if api.registerCallback != nil {
		api.registerCallback(id)
	}
	api.broadcast(ApiMessageFrom{Msg: "system/register", Data: msgData{"id": id}})
	return nil
}

func (api *PublicLesCommonAPI) authenticate(id, key string) error {
	if api.keys[id] != key {
		return ErrAuthFailed
	}
	return nil
}

func (api *PublicLesCommonAPI) getRemoteFilters() PrefixSet {
	api.lock.RLock()
	defer api.lock.RUnlock()

	if api.allFilters == nil {
		list := make([]PrefixSet, len(api.subs))
		pos := 0
		for _, filter := range api.subs {
			list[pos] = filter.StripPrefix("remote/")
			pos++
		}
		api.allFilters = PrefixSetUnion(list)
	}
	return api.allFilters
}

func (api *PublicLesCommonAPI) receivedRemote(msg ApiMessageFrom) error {
	api.lock.RLock()
	defer api.lock.RUnlock()

	msg.Msg = "remote/" + msg.Msg
	return api.broadcast(msg)
}

// internal
func (api *PublicLesCommonAPI) broadcast(msg ApiMessageFrom) error {
	var match bool
	for ch, filter := range api.subs {
		if filter.Match(msg.Msg) {
			match = true
			select {
			case ch <- msg:
			default:
			}
		}
	}
	if match {
		return nil
	} else {
		return ErrApiMessageUnwanted
	}
}

func (api *PublicLesCommonAPI) subscribe(filter PrefixSet, messages chan ApiMessageFrom) func() {
	api.lock.Lock()
	defer api.lock.Unlock()

	api.subs[messages] = filter
	api.allFilters = nil

	return func() {
		api.lock.Lock()
		defer api.lock.Unlock()

		delete(api.subs, messages)
		api.allFilters = nil
		close(messages)
	}
}

func (api *PublicLesCommonAPI) ReceiveMessages(ctx context.Context, filter PrefixSet) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	go func() {
		messages := make(chan ApiMessageFrom, 100)
		unsubscribe := api.subscribe(filter, messages)

		for {
			select {
			case message := <-messages:
				notifier.Notify(rpcSub.ID, message)
			case <-rpcSub.Err():
				unsubscribe()
				return
			case <-notifier.Closed():
				unsubscribe()
				return
			}
		}
	}()

	return rpcSub, nil
}

func (api *PublicLesCommonAPI) SendMessage(from, key, to string, remote bool, message ApiMessage) error {
	if err := api.authenticate(from, key); err != nil {
		return err
	}
	if remote {
		return api.sendRemote(to, ApiMessage{Msg: from + "/" + message.Msg, Data: message.Data})
	} else {
		return api.broadcast(ApiMessageFrom{Msg: "local/" + from + "/" + message.Msg, Data: message.Data})
	}
}

// Note: empty list means "match none", list containing an empty string means "match all".
type PrefixSet []string

func (p PrefixSet) Match(s string) bool {
	a := 0
	b := len(p)
	if b == 0 {
		return false
	}
	for a != b {
		m := (a + b + 1) / 2
		switch strings.Compare(p[m], s) {
		case 0:
			return true
		case -1:
			a = m
		case 1:
			b = m - 1
		}
	}
	return strings.HasPrefix(s, p[a])
}

func PrefixSetUnion(list []PrefixSet) PrefixSet {
	l := 0
	for _, p := range list {
		l += len(p)
	}
	c := make(PrefixSet, l)
	pos := 0
	for _, p := range list {
		lp := len(p)
		copy(c[pos:pos+lp], p)
		pos += lp
	}
	return c.SortAndFilter()
}

// Note: the original prefix set is changed.
func (p PrefixSet) SortAndFilter() PrefixSet {
	lp := len(p)
	if lp < 2 {
		return p
	}
	sort.StringSlice(p).Sort()
	readPos, writePos := 1, 1
	for readPos < lp {
		if !strings.HasPrefix(p[readPos], p[readPos-1]) {
			if writePos != readPos {
				p[writePos] = p[readPos]
				writePos++
			}
		}
		readPos++
	}
	return p[:writePos]
}

func (p PrefixSet) StripPrefix(prefix string) PrefixSet {
	res := make(PrefixSet, len(p))
	pl := len(prefix)
	l := 0
	for _, pp := range p {
		if strings.HasPrefix(pp, prefix) {
			res[l] = pp[pl:]
			l++
		}
	}
	return res[:l]
}
