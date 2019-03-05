// Copyright 2019 The go-ethereum Authors
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

package csvlogger

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
)

type Logger struct {
	file     *os.File
	started  mclock.AbsTime
	channels []*Channel
	period   time.Duration
	stopCh   chan chan struct{}
	storeCh  chan struct{}
}

func NewLogger(fileName string, period time.Duration) *Logger {
	f, err := os.Create(fileName)
	if err != nil {
		log.Error("Error creating log file", "name", fileName, "error", err)
		return nil
	}
	return &Logger{
		file:    f,
		period:  period,
		stopCh:  make(chan chan struct{}),
		storeCh: make(chan struct{}, 1),
	}
}

func (l *Logger) NewChannel(name string, threshold float64) *Channel {
	if l == nil {
		return nil
	}
	c := &Channel{
		logger:    l,
		name:      name,
		threshold: threshold,
	}
	l.channels = append(l.channels, c)
	return c
}

func (l *Logger) NewMinMaxChannel(name string, zeroDefault bool) *Channel {
	if l == nil {
		return nil
	}
	c := &Channel{
		logger:        l,
		name:          name,
		minmax:        true,
		mmZeroDefault: zeroDefault,
	}
	l.channels = append(l.channels, c)
	return c
}

func (l *Logger) store() {
	s := fmt.Sprintf("%g", float64(mclock.Now()-l.started)/1000000000)
	for _, ch := range l.channels {
		s += ", " + ch.store()
	}
	l.file.WriteString(s + "\n")
	fmt.Println(s)
}

func (l *Logger) Start() {
	if l == nil {
		return
	}
	l.started = mclock.Now()
	s := "Time"
	for _, ch := range l.channels {
		s += ", " + ch.header()
	}
	l.file.WriteString(s + "\n")
	fmt.Println(s)
	go func() {
		timer := time.NewTimer(l.period)
		for {
			select {
			case <-timer.C:
				l.store()
				timer.Reset(l.period)
			case <-l.storeCh:
				l.store()
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(l.period)
			case stop := <-l.stopCh:
				close(stop)
				return
			}
		}
	}()
}

func (l *Logger) Stop() {
	if l == nil {
		return
	}
	stop := make(chan struct{})
	l.stopCh <- stop
	<-stop
	l.file.Close()
}

type Channel struct {
	logger                                             *Logger
	lock                                               sync.Mutex
	name                                               string
	threshold, storeMin, storeMax, lastValue, min, max float64
	minmax, mmSet, mmZeroDefault                       bool
}

func (lc *Channel) Update(value float64) {
	if lc == nil {
		return
	}
	lc.lock.Lock()
	defer lc.lock.Unlock()

	lc.lastValue = value
	if lc.minmax {
		if value > lc.max || !lc.mmSet {
			lc.max = value
		}
		if value < lc.min || !lc.mmSet {
			lc.min = value
		}
		lc.mmSet = true
	} else {
		if value < lc.storeMin || value > lc.storeMax {
			select {
			case lc.logger.storeCh <- struct{}{}:
			default:
			}
		}
	}
}

func (lc *Channel) store() (s string) {
	lc.lock.Lock()
	defer lc.lock.Unlock()

	if lc.minmax {
		s = fmt.Sprintf("%g, %g", lc.min, lc.max)
		lc.mmSet = false
		if lc.mmZeroDefault {
			lc.min = 0
		} else {
			lc.min = lc.lastValue
		}
		lc.max = lc.min
	} else {
		s = fmt.Sprintf("%g", lc.lastValue)
		lc.storeMin = lc.lastValue * (1 - lc.threshold)
		lc.storeMax = lc.lastValue * (1 + lc.threshold)
		if lc.lastValue < 0 {
			lc.storeMin, lc.storeMax = lc.storeMax, lc.storeMin
		}
	}
	return
}

func (lc *Channel) header() string {
	if lc.minmax {
		return lc.name + " (min), " + lc.name + " (max)"
	}
	return lc.name
}
