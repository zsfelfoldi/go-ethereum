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
	"container/heap"
	"math"
	"math/rand"
	"time"
	
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/aristanetworks/goarista/atime"
)

type ticket struct {
	id	NodeID
	topics []Topic
	regTime []uint32   // local absolute time
	pong []byte
	refCnt int
}

const (
	ticketGroupTime = 60
	timeWindow = 30	
)

type topicTickets struct {
	rad topicRadius
	time map[uint32][]*ticket
}

type ticketStore struct {
	topics map[Topic]*topicTickets
	nodes map[NodeID]*ticket
}

func (s *ticketStore) add(localTime uint32, t *ticket) {
	if s.nodes[t] != nil {
		return
	}

	for _, topic := range t.topics {
		if tt, ok := s.topics[topic]; ok && tt.rad.isInRadius(t) {
			tt.rad.adjust(t)
			
			if tt.rad.converged {
				wait := t.regTime[topic] - localTime
				rnd := rand.ExpFloat64()
				if rnd > 10 {
					rnd = 10
				}
				if float64(wait) < keepTicketConst + keepTicketExp * rnd {
					// use the ticket to register this topic
					tgroup := t.regTime[topic] / ticketGroupTime
					tt.time[tgroup] = append(tt.time[tgroup], t)
					t.refCnt++
				}
			}
		}
	}
	
}

func (s *ticketStore) register(localTime uint32) (res []*ticket) {
	m := make(map[*ticket]struct{})
	ltGroup := localTime / ticketGroupTime
	for topic, tt := range s.topics {
		list := tt.time[ltGroup]
		i := 0
		for i < len(list) {
			t := list[i]
			if t.regTime[topic] <= localTime {
				list = append(list[:i], list[i+1:]...)
				if _, ok := m[t]; !ok {		// don't add a ticket twice
					res = append(res, t)
					m[t] = struct{}{}
				}
				t.refCnt--
				if t.refCnt == 0 {
					
				}
			} else {
				i++
			}
		}
	}
	return
}

func (s *ticketStore) needMoreTickets(t Topic) {
	
}

func (s *ticketStore) getNodeTicket(id NodeID) *ticket {
	
}

type topicRadius struct {
	topic Topic
	topicHashPrefix uint64
	radius uint64
	filteredRadius float64 // only for convergence detection
	converged bool
}

const targetWaitTime = 600

func (r *topicRadius) isInRadius(t *ticket) bool {
	nodePrefix := binary.BigEndian.Uint64(t.id[0:8])
	dist := nodePrefix ^ r.topicHashPrefix
	return dist < r.radius
}

func (r *topicRadius) adjust(t *ticket) {
	wait := t.wait[r.topic] - t.currTime
	adjust := (float64(wait) / float64(targetWaitTime) - 1) * 2
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
	if radius > float64(uint64(-1)) {
		r.radius = uint64(-1)
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

func newTopicRadius(t Topic) *topicRadius {
	topicHash := crypto.Keccak256Hash(t)
	topicHashPrefix := binary.BigEndian.Uint64(topicHash[0:8])

	return &topicRadius{
		topic: t,
		topicHashPrefix: topicHashPrefix,
		radius: 	uint64(-1),
		filteredRadius: float64(uint64(-1)),
		converged: false,
	}
}
