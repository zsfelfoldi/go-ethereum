// Copyright 2015 The go-ethereum Authors
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

// Package discover implements the Node Discovery Protocol.
//
// The Node Discovery protocol provides a way to find RLPx nodes that
// can be connected to. It uses a Kademlia-like protocol to maintain a
// distributed database of the IDs and endpoints of all listening
// nodes.
package discover

import (
	"encoding/binary"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

type ticket struct {
	id      NodeID
	topics  []Topic
	regTime []uint64 // local absolute time
	pong    []byte
	refCnt  int
}

const (
	ticketGroupTime = uint64(time.Minute)
	timeWindow      = 30
	keepTicketConst = uint64(time.Minute) * 20
	keepTicketExp   = uint64(time.Minute) * 20
	maxRadius       = 0xffffffffffffffff
	minRadAverage   = 1024
)

type ticketStore struct {
	topics               map[Topic]*topicTickets
	nodes                map[NodeID]*ticket
	lastGroupFetched     uint64
	minRadSum            float64
	minRadCnt, minRadius uint64
}

func (s *ticketStore) add(localTime uint64, t *ticket) {
	if s.nodes[t.id] != nil {
		return
	}

	if s.lastGroupFetched == 0 {
		s.lastGroupFetched = localTime / ticketGroupTime
	}

	for i, topic := range t.topics {
		if tt, ok := s.topics[topic]; ok && tt.isInRadius(t) {
			tt.adjust(localTime, ticketRef{t, i}, s.minRadius)

			if tt.converged {
				wait := t.regTime[i] - localTime
				rnd := rand.ExpFloat64()
				if rnd > 10 {
					rnd = 10
				}
				if float64(wait) < float64(keepTicketConst)+float64(keepTicketExp)*rnd {
					// use the ticket to register this topic
					tgroup := t.regTime[i] / ticketGroupTime
					tt.time[tgroup] = append(tt.time[tgroup], ticketRef{t, i})
					t.refCnt++
				}
			}
		}
	}

	if t.refCnt > 0 {
		s.nodes[t.id] = t
	}
}

func (s *ticketStore) fetch(localTime uint64) (res []*ticket) {
	ltGroup := localTime / ticketGroupTime
	for _, tt := range s.topics {
		for g := s.lastGroupFetched; g <= ltGroup; g++ {
			list := tt.time[g]
			i := 0
			for i < len(list) {
				t := list[i]
				if t.t.regTime[t.idx] <= localTime {
					list = append(list[:i], list[i+1:]...)
					res = append(res, t.t)
					t.t.refCnt--
					if t.t.refCnt == 0 {
						delete(s.nodes, t.t.id)
					}
				} else {
					i++
				}
			}
			if g != ltGroup {
				delete(tt.time, g)
			}
		}
	}
	s.lastGroupFetched = ltGroup
	return
}

func (s *ticketStore) ticketsInWindow(localTime uint64, t Topic) int {
	ltGroup := localTime / ticketGroupTime
	sum := 0
	for g := ltGroup; g < ltGroup+timeWindow; g++ {
		sum += len(s.topics[t].time[g])
	}
	return sum
}

func (s *ticketStore) getNodeTicket(id NodeID) *ticket {
	return s.nodes[id]
}

func (s *ticketStore) adjustMinRadius(target, found NodeID) {
	tp := binary.BigEndian.Uint64(target[0:8])
	fp := binary.BigEndian.Uint64(found[0:8])
	dist := tp ^ fp
	mrAdjust := float64(dist) * 16
	if mrAdjust > maxRadius/2 {
		mrAdjust = maxRadius / 2
	}

	if s.minRadCnt < minRadAverage {
		s.minRadCnt++
	} else {
		s.minRadSum -= s.minRadSum / minRadAverage
	}
	s.minRadSum += mrAdjust
	s.minRadius = uint64(s.minRadSum / float64(s.minRadCnt))
}

type ticketRef struct {
	t   *ticket
	idx int
}

type topicTickets struct {
	topic           Topic
	topicHashPrefix uint64
	radius          uint64
	filteredRadius  float64 // only for convergence detection
	converged       bool

	time map[uint64][]ticketRef
}

const targetWaitTime = uint64(time.Minute) * 10

func (r *topicTickets) isInRadius(t *ticket) bool {
	nodePrefix := binary.BigEndian.Uint64(t.id[0:8])
	dist := nodePrefix ^ r.topicHashPrefix
	return dist < r.radius
}

func (r *topicTickets) newTarget() (target NodeID) {
	rnd := uint64(rand.Int63n(int64(r.radius/2))) * 2
	prefix := r.topicHashPrefix ^ rnd
	binary.BigEndian.PutUint64(target[0:8], prefix)
	return
}

func (r *topicTickets) adjust(localTime uint64, t ticketRef, minRadius uint64) {
	wait := t.t.regTime[t.idx] - localTime
	adjust := (float64(wait)/float64(targetWaitTime) - 1) * 2
	if adjust > 1 {
		adjust = 1
	}
	if adjust < -1 {
		adjust = -1
	}
	if r.converged {
		adjust *= 0.01
	} else {
		adjust *= 0.1
	}

	radius := float64(r.radius) * (1 + adjust)
	if radius > float64(maxRadius) {
		r.radius = maxRadius
		radius = float64(r.radius)
	} else {
		r.radius = uint64(radius)
		if r.radius < minRadius {
			r.radius = minRadius
		}
	}

	if !r.converged {
		if radius >= r.filteredRadius {
			r.converged = true
		} else {
			r.filteredRadius += (radius - r.filteredRadius) * 0.05
		}
	}
}

func newtopicTickets(t Topic) *topicTickets {
	topicHash := crypto.Keccak256Hash([]byte(t))
	topicHashPrefix := binary.BigEndian.Uint64(topicHash[0:8])

	return &topicTickets{
		topic:           t,
		topicHashPrefix: topicHashPrefix,
		radius:          maxRadius,
		filteredRadius:  float64(maxRadius),
		converged:       false,
	}
}
