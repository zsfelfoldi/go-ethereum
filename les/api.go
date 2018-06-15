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
	ErrApiMessageSend     = errors.New("could not send API message")
	ErrApiMessageUnwanted = errors.New("unwanted API message")
)

// PublicLesServerAPI  provides an API to access the les server.
// It offers only methods that operate on public data that is freely available to anyone.
type PublicLesServerAPI struct {
	server *LesServer
}

// NewPublicLesServerAPI creates a new les server API.
func NewPublicLesServerAPI(server *LesServer) *PublicLesServerAPI {
	return &PublicLesServerAPI{
		server: server,
	}
}

type ApiMessage struct {
	Id  string `json:"id"`
	Msg string `json:"msg"`
}

type PublicLesMessageAPI struct {
	send       func(to, message string) error
	subs       map[chan ApiMessage]PrefixSet
	lock       sync.RWMutex
	allFilters PrefixSet
}

// NewPublicLesServerAPI creates a new les server API.
func NewPublicLesMessageAPI(send func(to, message string) error) *PublicLesMessageAPI {
	return &PublicLesMessageAPI{
		send: send,
		subs: make(map[chan ApiMessage]PrefixSet),
	}
}

func (api *PublicLesMessageAPI) getAllFilters() PrefixSet {
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

func (api *PublicLesMessageAPI) received(from, message string) error {
	api.lock.RLock()
	defer api.lock.RUnlock()

	msg := ApiMessage{from, message}
	var match bool
	for ch, filter := range api.subs {
		if filter.Match(message) {
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

func (api *PublicLesMessageAPI) subscribe(filter PrefixSet, messages chan ApiMessage) func() {
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

func (api *PublicLesMessageAPI) Receive(ctx context.Context, filter PrefixSet) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	go func() {
		messages := make(chan ApiMessage, 100)
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

func (api *PublicLesMessageAPI) Send(to, message string) error {
	return api.send(to, message)
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
