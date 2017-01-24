// Copyright 2017 The go-ethereum Authors
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
package bloombits

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	maxRequestLength = 16
	channelCap       = 100
)

type req struct {
	data    BitVector
	queued  bool
	fetched chan struct{}
}

type distReq struct {
	bitIdx     uint
	sectionIdx uint64
}

type fetcher struct {
	bitIdx, ii uint
	reqMap     map[uint64]req
	reqLock    sync.RWMutex
}

func (f *fetcher) fetch(sectionChn chan uint64, distChn chan distReq, stop chan struct{}, wg *sync.WaitGroup) chan BitVector {
	dataChn := make(chan BitVector, channelCap)
	returnChn := make(chan uint64, channelCap)
	wg.Add(2)

	go func() {
		defer func() {
			close(returnChn)
			wg.Done()
		}()

		for {
			select {
			case <-stop:
				return
			case idx, ok := <-sectionChn:
				if !ok {
					return
				}

				req := false
				f.reqLock.Lock()
				r := f.reqMap[idx]
				if r.data == nil {
					req = !r.queued
					r.queued = true
					if r.fetched == nil {
						r.fetched = make(chan struct{})
					}
					f.reqMap[idx] = r
				}
				f.reqLock.Unlock()
				if req {
					distChn <- distReq{bitIdx: f.bitIdx, sectionIdx: idx} // success is guaranteed, distibuteRequests shuts down after fetch
				}
				select {
				case <-stop:
					return
				case returnChn <- idx:
				}
			}
		}
	}()

	go func() {
		defer func() {
			close(dataChn)
			wg.Done()
		}()

		for {
			select {
			case <-stop:
				return
			case idx, ok := <-returnChn:
				if !ok {
					return
				}

				f.reqLock.RLock()
				r := f.reqMap[idx]
				f.reqLock.RUnlock()

				if r.data == nil {
					select {
					case <-stop:
						return
					case <-r.fetched:
						f.reqLock.RLock()
						r = f.reqMap[idx]
						f.reqLock.RUnlock()
					}
				}
				select {
				case <-stop:
					return
				case dataChn <- r.data:
				}
			}
		}
	}()

	return dataChn
}

func (f *fetcher) deliver(sectionIdxList []uint64, data []BitVector) {
	//fmt.Println("deliver", f.bitIdx, sectionIdxList, data != nil)
	f.reqLock.Lock()
	defer f.reqLock.Unlock()

	for i, idx := range sectionIdxList {
		r := f.reqMap[idx]
		if r.data != nil {
			panic("BloomBits section data delivered twice")
		}
		r.data = data[i]
		close(r.fetched)
		f.reqMap[idx] = r
	}
}

type Matcher struct {
	addresses []types.BloomIndexList
	topics    [][]types.BloomIndexList
	fetchers  map[uint]*fetcher

	distChn       chan distReq
	reqs          map[uint][]uint64
	getNextReqChn chan chan nextRequests
	wg, distWg    sync.WaitGroup
}

func NewMatcher() *Matcher {

	return &Matcher{fetchers: make(map[uint]*fetcher), reqs: make(map[uint][]uint64), distChn: make(chan distReq, channelCap)}
}

// SetAddresses matches only logs that are generated from addresses that are included
// in the given addresses.
func (m *Matcher) SetAddresses(addr []common.Address) {
	m.addresses = make([]types.BloomIndexList, len(addr))
	for i, b := range addr {
		m.addresses[i] = types.BloomIndexes(b.Bytes())
	}

	for _, idxs := range m.addresses {
		for _, idx := range idxs {
			m.newFetcher(idx)
		}
	}
}

// SetTopics matches only logs that have topics matching the given topics.
func (m *Matcher) SetTopics(topics [][]common.Hash) {
	m.topics = nil
loop:
	for _, topicList := range topics {
		t := make([]types.BloomIndexList, len(topicList))
		for i, b := range topicList {
			if (b == common.Hash{}) {
				continue loop
			}
			t[i] = types.BloomIndexes(b.Bytes())
		}
		m.topics = append(m.topics, t)
	}

	for _, idxss := range m.topics {
		for _, idxs := range idxss {
			for _, idx := range idxs {
				m.newFetcher(idx)
			}
		}
	}
}

func (m *Matcher) match(sectionChn chan uint64, stop chan struct{}) (chan uint64, chan BitVector) {
	subIdx := m.topics
	if len(m.addresses) > 0 {
		subIdx = append([][]types.BloomIndexList{m.addresses}, subIdx...)
	}
	//fmt.Println("idx", subIdx)
	m.getNextReqChn = make(chan chan nextRequests) // should be a blocking channel
	m.distributeRequests(stop)

	s := sectionChn
	var bv chan BitVector
	for _, idx := range subIdx {
		s, bv = m.subMatch(s, bv, idx, stop)
	}
	return s, bv
}

func (m *Matcher) newFetcher(idx uint) {
	if _, ok := m.fetchers[idx]; ok {
		return
	}
	f := &fetcher{
		bitIdx: idx,
		reqMap: make(map[uint64]req),
	}
	m.fetchers[idx] = f
}

// andVector == nil
func (m *Matcher) subMatch(sectionChn chan uint64, andVectorChn chan BitVector, idxs []types.BloomIndexList, stop chan struct{}) (chan uint64, chan BitVector) {
	// set up fetchers
	fetchIdx := make([][3]chan uint64, len(idxs))
	fetchData := make([][3]chan BitVector, len(idxs))
	for i, idx := range idxs {
		for j, ii := range idx {
			fetchIdx[i][j] = make(chan uint64, channelCap)
			fetchData[i][j] = m.fetchers[ii].fetch(fetchIdx[i][j], m.distChn, stop, &m.wg)
		}
	}

	processChn := make(chan uint64, channelCap)
	resIdxChn := make(chan uint64, channelCap)
	resDataChn := make(chan BitVector, channelCap)

	m.wg.Add(2)
	// goroutine for starting retrievals
	go func() {
		defer m.wg.Done()

		for {
			select {
			case <-stop:
				return
			case s, ok := <-sectionChn:
				if !ok {
					close(processChn)
					for _, ff := range fetchIdx {
						for _, f := range ff {
							close(f)
						}
					}
					return
				}

				select {
				case <-stop:
					return
				case processChn <- s:
				}
				for _, ff := range fetchIdx {
					for _, f := range ff {
						select {
						case <-stop:
							return
						case f <- s:
						}
					}
				}
			}
		}
	}()

	// goroutine for processing retrieved data
	go func() {
		defer m.wg.Done()

		for {
			select {
			case <-stop:
				return
			case s, ok := <-processChn:
				if !ok {
					close(resIdxChn)
					close(resDataChn)
					return
				}

				var orVector BitVector
				for _, ff := range fetchData {
					var andVector BitVector
					for _, f := range ff {
						var data BitVector
						select {
						case <-stop:
							return
						case data = <-f:
						}
						if andVector == nil {
							andVector = bvCopy(data)
						} else {
							bvAnd(andVector, data)
						}
					}
					if orVector == nil {
						orVector = andVector
					} else {
						bvOr(orVector, andVector)
					}
				}

				if orVector == nil {
					orVector = bvZero()
				}
				if andVectorChn != nil {
					select {
					case <-stop:
						return
					case andVector := <-andVectorChn:
						bvAnd(orVector, andVector)
					}
				}
				if bvIsNonZero(orVector) {
					select {
					case <-stop:
						return
					case resIdxChn <- s:
					}
					select {
					case <-stop:
						return
					case resDataChn <- orVector:
					}
				}
			}
		}
	}()

	return resIdxChn, resDataChn
}

func (m *Matcher) GetMatches(start, end uint64, stop chan struct{}) chan uint64 {
	m.distWg.Wait()

	sectionChn := make(chan uint64, channelCap)
	resultsChn := make(chan uint64, channelCap)

	s, bv := m.match(sectionChn, stop)

	startSection := start / SectionSize
	endSection := end / SectionSize

	m.wg.Add(2)
	go func() {
		defer func() {
			close(sectionChn)
			m.wg.Done()
		}()

		for i := startSection; i <= endSection; i++ {
			select {
			case sectionChn <- i:
			case <-stop:
				return
			}
		}
	}()

	go func() {
		defer func() {
			close(resultsChn)
			m.wg.Done()
		}()

		for {
			select {
			case idx, ok := <-s:
				if !ok {
					return
				}
				var match BitVector
				select {
				case <-stop:
					return
				case match = <-bv:
				}
				sectionStart := idx * SectionSize
				s := sectionStart
				if start > s {
					s = start
				}
				e := sectionStart + SectionSize - 1
				if end < e {
					e = end
				}
				for i := s; i <= e; i++ {
					b := match[(i-sectionStart)/8]
					bit := 7 - i%8
					if b != 0 {
						if b&(1<<bit) != 0 {
							select {
							case <-stop:
								return
							case resultsChn <- i:
							}
						}
					} else {
						i += bit
					}
				}

			case <-stop:
				return
			}
		}
	}()

	return resultsChn
}

type nextRequests struct {
	bitIdx         uint
	sectionIdxList []uint64
}

func (m *Matcher) distributeRequests(stop chan struct{}) {
	m.distWg.Add(1)
	stopDist := make(chan struct{})
	go func() {
		<-stop
		m.wg.Wait()
		close(stopDist)
	}()

	go func() {
		defer m.distWg.Done()

		reqCnt := 0
		for _, s := range m.reqs {
			reqCnt += len(s)
		}

		storeReq := func(r distReq) {
			queue := m.reqs[r.bitIdx]
			i := 0
			for i < len(queue) && r.sectionIdx > queue[i] {
				i++
			}
			queue = append(queue, 0)
			copy(queue[i+1:], queue[i:len(queue)-1])
			queue[i] = r.sectionIdx

			m.reqs[r.bitIdx] = queue
			reqCnt++
		}

		storeReqs := func(r distReq) {
			storeReq(r)
			timeout := time.After(time.Microsecond)
			for {
				select {
				case <-timeout:
					return
				case r := <-m.distChn:
					storeReq(r)
				case <-stopDist:
					return
				}
			}
		}

		for {
			if reqCnt == 0 {
				select {
				case r := <-m.distChn:
					storeReqs(r)
				case <-stopDist:
					return
				}
			} else {
				select {
				case r := <-m.distChn:
					storeReqs(r)
				case <-stopDist:
					return
				case c := <-m.getNextReqChn:
					var (
						found       bool
						bestBit     uint
						bestSection uint64
					)

					for bitIdx, queue := range m.reqs {
						if len(queue) > 0 && (!found || queue[0] < bestSection) {
							found = true
							bestBit = bitIdx
							bestSection = queue[0]
						}
					}
					if !found {
						panic(nil)
					}

					bestQueue := m.reqs[bestBit]
					cnt := len(bestQueue)
					if cnt > maxRequestLength {
						cnt = maxRequestLength
					}
					res := nextRequests{bestBit, bestQueue[:cnt]}
					m.reqs[bestBit] = bestQueue[cnt:]
					reqCnt -= cnt

					c <- res
				}
			}
		}
	}()
}

func (m *Matcher) NextRequest(stop chan struct{}) (bitIdx uint, sectionIdxList []uint64) {
	c := make(chan nextRequests)
	select {
	case m.getNextReqChn <- c:
		r := <-c
		return r.bitIdx, r.sectionIdxList
	case <-stop:
		return 0, nil
	}
}

// It is possible to deliver data even after GetMatches has been stopped. Once a vector has been
// requested, the next call to GetMatches will keep waiting for delivery.
// If retrieval has been cancelled, call Deliver with data == nil. In this case the next call to
// GetMatches will re-request it.
func (m *Matcher) Deliver(bitIdx uint, sectionIdxList []uint64, data []BitVector) {
	m.fetchers[bitIdx].deliver(sectionIdxList, data)
}
