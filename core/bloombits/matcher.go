package bloombits

import (
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	maxRequestLength = 16
	channelCap       = 100
)

type req struct {
	data      BitVector
	requested bool
	fetched   chan struct{}
}

type fetcher struct {
	reqMap           map[uint64]req
	reqFirst, reqCnt uint64
	reqLock          sync.RWMutex
	newReqCallback   func()
}

func (f *fetcher) fetch(sectionChn chan uint64, stop <-chan struct{}) chan BitVector {
	dataChn := make(chan BitVector)
	returnChn := make(chan uint64)

	go func() {
		defer close(returnChn)

		for {
			select {
			case <-stop:
				return
			case idx, ok := <-sectionChn:
				if !ok {
					return
				}

				f.reqLock.Lock()
				_, ok = f.reqMap[idx]
				if !ok {
					f.reqMap[idx] = req{fetched: make(chan struct{})}
					if idx < f.reqFirst || f.reqCnt == 0 {
						f.reqFirst = idx
					}
					f.reqCnt++
				}
				f.reqLock.Unlock()
				if !ok {
					f.newReqCallback()
				}
				returnChn <- idx
			}
		}
	}()

	go func() {
		defer close(dataChn)

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
					}
				}
				dataChn <- r.data
			}
		}
	}()

	return dataChn
}

func (f *fetcher) nextRequest() (sectionIdxList []uint64) {
	f.reqLock.Lock()
	defer f.reqLock.Unlock()

	for f.reqCnt > 0 && len(sectionIdxList) < maxRequestLength {
		sectionIdxList = append(sectionIdxList, f.reqFirst)
		r := f.reqMap[f.reqFirst]
		r.requested = true
		f.reqMap[f.reqFirst] = r
		f.reqCnt--
		if f.reqCnt > 0 {
		loop:
			for {
				f.reqFirst++
				if r, ok := f.reqMap[f.reqFirst]; ok && !r.requested {
					break loop
				}
			}
		}
	}

	return
}

func (f *fetcher) firstIndex() (uint64, bool) {
	f.reqLock.RLock()
	defer f.reqLock.RUnlock()

	return f.reqFirst, f.reqCnt > 0
}

func (f *fetcher) deliver(sectionIdxList []uint64, data []BitVector) {
	f.reqLock.Lock()
	defer f.reqLock.Unlock()

	for i, idx := range sectionIdxList {
		r := f.reqMap[idx]
		r.data = data[i]
		close(r.fetched)
		r.fetched = nil
		f.reqMap[idx] = r
	}
}

type Matcher struct {
	addresses []types.BloomIndexList
	topics    [][]types.BloomIndexList
	fetchers  map[uint]*fetcher

	wakeupQueue []chan struct{}
	reqCnt      int
	reqLock     sync.Mutex
}

// SetAddresses matches only logs that are generated from addresses that are included
// in the given addresses.
func (m *Matcher) SetAddresses(addr []common.Address) {
	m.addresses = make([]types.BloomIndexList, len(addr))
	for i, b := range addr {
		m.addresses[i] = types.BloomIndexes(b.Bytes())
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
}

func (m *Matcher) match(sectionChn chan uint64, stop <-chan struct{}) (chan uint64, chan BitVector) {
	subIdx := m.topics
	if len(m.addresses) > 0 {
		subIdx = append([][]types.BloomIndexList{m.addresses}, subIdx...)
	}
	m.fetchers = make(map[uint]*fetcher)

	s := sectionChn
	var bv chan BitVector
	for _, idx := range subIdx {
		s, bv = m.subMatch(s, bv, idx, stop)
	}
	return s, bv
}

func (m *Matcher) getOrNewFetcher(idx uint) *fetcher {
	if f, ok := m.fetchers[idx]; ok {
		return f
	}
	f := &fetcher{
		reqMap: make(map[uint64]req),
		newReqCallback: func() {
			fmt.Println("cb 1")
			m.reqLock.Lock()
			fmt.Println("cb 2")
			if m.reqCnt == 0 && len(m.wakeupQueue) > 0 {
				fmt.Println("cb 3")
				close(m.wakeupQueue[0])
				m.wakeupQueue = m.wakeupQueue[1:]
			}
			m.reqCnt++
			m.reqLock.Unlock()
			fmt.Println("cb 4")
		},
	}
	m.fetchers[idx] = f
	return f
}

// andVector == nil
func (m *Matcher) subMatch(sectionChn chan uint64, andVectorChn chan BitVector, idxs []types.BloomIndexList, stop <-chan struct{}) (chan uint64, chan BitVector) {
	// set up fetchers
	fetchIdx := make([][3]chan uint64, len(idxs))
	fetchData := make([][3]chan BitVector, len(idxs))
	for i, idx := range idxs {
		for j, ii := range idx {
			fetchIdx[i][j] = make(chan uint64, channelCap)
			fetchData[i][j] = m.getOrNewFetcher(ii).fetch(fetchIdx[i][j], stop)
		}
	}

	processChn := make(chan uint64, channelCap)
	resIdxChn := make(chan uint64, channelCap)
	resDataChn := make(chan BitVector, channelCap)

	// goroutine for starting retrievals
	go func() {
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

				processChn <- s
				for _, ff := range fetchIdx {
					for _, f := range ff {
						f <- s
					}
				}
			}
		}
	}()

	// goroutine for processing retrieved data
	go func() {
		for {
			select {
			case <-stop:
				return
			case s, ok := <-sectionChn:
				if !ok {
					close(resIdxChn)
					close(resDataChn)
				}

				var orVector BitVector
				for _, ff := range fetchData {
					var andVector BitVector
					for _, f := range ff {
						data := <-f
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
					bvAnd(orVector, <-andVectorChn)
				}
				if bvIsNonZero(orVector) {
					resIdxChn <- s
					resDataChn <- orVector
				}
			}
		}
	}()

	return resIdxChn, resDataChn
}

func (m *Matcher) GetMatches(start, end uint64, stop <-chan struct{}) chan uint64 {
	sectionChn := make(chan uint64, channelCap)
	resultsChn := make(chan uint64, channelCap)

	s, bv := m.match(sectionChn, stop)

	startSection := start / SectionSize
	endSection := end / SectionSize

	go func() {
		defer close(sectionChn)

		for i := startSection; i <= endSection; i++ {
			select {
			case sectionChn <- i:
			case <-stop:
				return
			}
		}
	}()

	go func() {
		defer close(resultsChn)

		for {
			select {
			case idx, ok := <-s:
				if !ok {
					return
				}
				match := <-bv
				for i, b := range match {
					if b != 0 {
						for bit := uint(0); i < 8; i++ {
							if b&(1<<(7-bit)) != 0 {
								resultsChn <- idx*SectionSize + uint64(i)*8 + uint64(bit)
							}
						}
					}
				}
			case <-stop:
				return
			}
		}
	}()

	return resultsChn
}

func (m *Matcher) NextRequest(stop <-chan struct{}) (bitIdx uint, sectionIdxList []uint64) {
	m.reqLock.Lock()
	defer m.reqLock.Unlock()

	fmt.Println("1")
	for {
		fmt.Println("2")
		var (
			firstIdx  uint64
			reqBitIdx uint
			found     bool
		)

		for m.reqCnt == 0 {
			fmt.Println("4")
			c := make(chan struct{})
			m.wakeupQueue = append(m.wakeupQueue, c)
			m.reqLock.Unlock()
			select {
			case <-stop:
				fmt.Println("5")
				return 0, nil
			case <-c:
				fmt.Println("6")
			}
			m.reqLock.Lock()
			fmt.Println("7")
		}

		fmt.Println("8")
		for i, f := range m.fetchers {
			if first, ok := f.firstIndex(); ok && (!found || first < firstIdx) {
				firstIdx = first
				reqBitIdx = i
				found = true
			}
		}
		fmt.Println("9")

		if found {
			fmt.Println("10")
			list := m.fetchers[reqBitIdx].nextRequest()
			if len(list) > 0 {
				fmt.Println("11")
				m.reqCnt -= len(list)
				return reqBitIdx, list
			}
		}
	}
}

func (m *Matcher) Deliver(bitIdx uint, sectionIdxList []uint64, data []BitVector) {
	m.fetchers[bitIdx].deliver(sectionIdxList, data)
}
