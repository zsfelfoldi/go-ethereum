// Copyright 2026 The go-ethereum Authors
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

// Package core implements the Ethereum consensus protocol.
package core

import (
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
)

type indexStats struct {
	total                  indexStat
	readBlockData          [priorityLevels]indexStat
	indexerProcessing      [priorityLevels]indexStat
	totalWait              indexStat
	waitForRequest         [priorityLevels]indexStat
	systemPriorityTooHigh  [priorityLevels]indexStat
	indexerPriorityTooHigh [priorityLevels]indexStat
	deliverQueueFull       [priorityLevels]indexStat
}

type indexStat struct {
	lock       sync.Mutex
	counting   bool
	started    mclock.AbsTime
	totalTime  time.Duration
	totalCount uint64
}

func (s *indexStat) set(now mclock.AbsTime, counting bool) {
	s.lock.Lock()
	if s.counting == counting {
		s.lock.Unlock()
		return
	}
	if counting {
		s.started = now
	} else {
		if now > s.started {
			s.totalTime += time.Duration(now - s.started)
		}
		s.totalCount++
	}
	s.counting = counting
	s.lock.Unlock()
}

func (s *indexStat) getTotalAndReset(now mclock.AbsTime) (time.Duration, uint64) {
	s.lock.Lock()
	totalTime, totalCount := s.totalTime, s.totalCount
	s.totalTime, s.totalCount = 0, 0
	if s.counting && now > s.started {
		totalTime += time.Duration(now - s.started)
		s.started = now
	}
	s.lock.Unlock()
	return totalTime, totalCount
}

const printDelimiter = "+-------------------------------------+-----------------+----------+-----------------+"

func (s *indexStat) printAndReset(now mclock.AbsTime, name string) {
	totalTime, totalCount := s.getTotalAndReset(now)
	fmt.Printf("| %-35s | %15v | %8d | %15v |\n", name, totalTime, totalCount, totalTime/time.Duration(max(totalCount, 1)))
}

func (s *indexStats) setWaiting(now mclock.AbsTime, systemPriority, indexerPriority, fullQueue int, counting bool) {
	s.totalWait.set(now, counting)
	for p := range priorityLevels {
		if systemPriority < p {
			s.systemPriorityTooHigh[p].set(now, counting)
		}
		if indexerPriority < p {
			s.indexerPriorityTooHigh[p].set(now, counting)
		}
		if fullQueue <= p {
			s.deliverQueueFull[p].set(now, counting)
		}
		if min(systemPriority+1, indexerPriority+1, fullQueue) > p {
			s.waitForRequest[p].set(now, counting)
		}
	}
}

func (s *indexStats) printAndReset() {
	now := mclock.Now()
	fmt.Println(printDelimiter)

	s.total.printAndReset(now, "Total time")
	for p := range priorityLevels {
		s.readBlockData[p].printAndReset(now, fmt.Sprintf(" Read block data @P%d", p))
	}
	for p := range priorityLevels {
		s.indexerProcessing[p].printAndReset(now, fmt.Sprintf(" Indexer processing block data @P%d", p))
	}
	fmt.Println(printDelimiter)
	s.totalWait.printAndReset(now, "Total wait time in read loop")
	for p := range priorityLevels {
		s.waitForRequest[p].printAndReset(now, fmt.Sprintf(" Waiting for request @P%d", p))
	}
	for p := 1; p < priorityLevels; p++ {
		s.systemPriorityTooHigh[p].printAndReset(now, fmt.Sprintf(" Blocked by system priority @P%d", p))
	}
	for p := 1; p < priorityLevels; p++ {
		s.indexerPriorityTooHigh[p].printAndReset(now, fmt.Sprintf(" Blocked by indexer priority @P%d", p))
	}
	for p := range priorityLevels {
		s.deliverQueueFull[p].printAndReset(now, fmt.Sprintf(" Blocked by full delivery queue @P%d", p))
	}
	fmt.Println(printDelimiter)
}
