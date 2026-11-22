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
	total             indexStat
	readBlockData     indexStat
	indexerProcessing indexStat
	totalWait         indexStat
	waitForRequest    indexStat
	systemSuspended   indexStat
	indexerSuspended  indexStat
	deliverQueueFull  indexStat
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

func (s *indexStats) setWaiting(now mclock.AbsTime, systemSuspend, indexerSuspend, fullQueue, counting bool) {
	s.totalWait.set(now, counting)
	if systemSuspend {
		s.systemSuspended.set(now, counting)
	}
	if indexerSuspend {
		s.indexerSuspended.set(now, counting)
	}
	if fullQueue {
		s.deliverQueueFull.set(now, counting)
	}
	if !(systemSuspend || indexerSuspend || fullQueue) {
		s.waitForRequest.set(now, counting)
	}
}

func (s *indexStats) printAndReset() {
	now := mclock.Now()
	fmt.Println(printDelimiter)

	s.total.printAndReset(now, "Total time")
	s.readBlockData.printAndReset(now, fmt.Sprintf(" Read block data"))
	s.indexerProcessing.printAndReset(now, fmt.Sprintf(" Indexer processing block data"))
	fmt.Println(printDelimiter)
	s.totalWait.printAndReset(now, "Total wait time in read loop")
	s.waitForRequest.printAndReset(now, fmt.Sprintf(" Waiting for request"))
	s.systemSuspended.printAndReset(now, fmt.Sprintf(" Blocked by system suspend"))
	s.indexerSuspended.printAndReset(now, fmt.Sprintf(" Blocked by indexer suspend"))
	s.deliverQueueFull.printAndReset(now, fmt.Sprintf(" Blocked by full delivery queue"))
	fmt.Println(printDelimiter)
}
