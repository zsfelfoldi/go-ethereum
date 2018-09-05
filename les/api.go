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
	ErrApiMessageSend     = errors.New("could not send API message")
	ErrApiMessageUnwanted = errors.New("unwanted API message")
)

type clientInfo struct {
	owner         string // extensionID
	weight        uint64
	offeredWeight map[string]uint64 // extensionID -> weight
}

type serverExtension struct {
	master             bool
	totalWeight, limit uint64
	clients            map[string]*clientInfo
}

type serverAPI struct {
	*PublicLesCommonAPI
	server     *LesServer
	extensions map[string]*serverExtension
	clients    map[string]*clientInfo
}

// NewPublicLesServerAPI creates a new les server API.
func NewLesServerAPI(server *LesServer, commonAPI *PublicLesCommonAPI) *serverAPI {
	return &serverAPI{
		PublicLesCommonAPI: commonAPI,
		server:             server,
		extensions:         make(map[string]*serverExtension),
		clients:            make(map[string]*clientInfo),
	}
}

func (api *serverAPI) getOrNewExtension(id string) *serverExtension {
	if res, ok := api.extensions[id]; ok {
		return res
	} else {
		res = &serverExtension{clients: make(map[string]*clientInfo)}
		api.extensions[id] = res
		return res
	}
}

type (
	PrivateLesServerAPI struct{ *serverAPI }
	PublicLesServerAPI  struct{ *serverAPI }
)

func (api PrivateLesServerAPI) SetMaster(extension string) {
	api.lock.Lock()
	defer api.lock.Unlock()

	api.getOrNewExtension(extension).master = true
}

func (api PublicLesServerAPI) SetLimit(master, masterKey, target string, newLimit uint64) error {
	api.lock.Lock()
	defer api.lock.Unlock()

	if err := api.authenticate(master, masterKey); err != nil {
		return err
	}
	if m, ok := api.extensions[master]; !ok || !m.master {
		return ErrNotMaster
	}
	o := api.getOrNewExtension(target)
	if newLimit < o.totalWeight {

	}
	o.limit = newLimit
	data := make(map[string]interface{})
	data["id"] = target
	data["limit"] = newLimit
	api.broadcast(ApiMessageFrom{Msg: "system/limitChanged", Data: data})
	return nil
}

//------------------------------------------------------------
//------------------------------------------------------------
//------------------------------------------------------------
//------------------------------------------------------------
//------------------------------------------------------------

type ApiMessage struct {
	Msg  string                 `json:"msg"`
	Data map[string]interface{} `json:"data"`
}

type ApiMessageFrom struct {
	Msg  string                 `json:"msg"`
	Data map[string]interface{} `json:"data"`
	Peer string                 `json:"peer"`
}

type PublicLesCommonAPI struct {
	lock       sync.RWMutex
	keys       map[string]string
	sendRemote func(to string, msg ApiMessage) error
	subs       map[chan ApiMessageFrom]PrefixSet
	allFilters PrefixSet
}

// NewPublicLesServerAPI creates a new les server API.
func NewPublicLesCommonAPI(sendRemote func(to string, msg ApiMessage) error) *PublicLesCommonAPI {
	return &PublicLesCommonAPI{
		sendRemote: sendRemote,
		keys:       make(map[string]string),
		subs:       make(map[chan ApiMessageFrom]PrefixSet),
	}
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
	return nil
}

func (api *PublicLesCommonAPI) authenticate(id, key string) error {
	if api.keys[id] != key {
		return ErrAuthFailed
	}
	return nil
}

func (api *PublicLesCommonAPI) getAllFilters() PrefixSet {
	api.lock.RLock()
	defer api.lock.RUnlock()

	if api.allFilters == nil {
		list := make([]PrefixSet, len(api.subs))
		pos := 0
		for _, filter := range api.subs {
			list[pos] = filter
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

func (api *PublicLesCommonAPI) systemBroadcast(msg ApiMessageFrom) error {
	api.lock.RLock()
	defer api.lock.RUnlock()

	msg.Msg = "system/" + msg.Msg
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
