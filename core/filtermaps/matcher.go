package filtermaps

import (
	"context"
	"math"

	//"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type Backend interface {
	GetBlockLvPointer(ctx context.Context, blockNumber uint64) (uint64, error) // returns head lv pointer if blockNumber > head number
	GetFilterMapRow(ctx context.Context, mapIndex, rowIndex uint32) (FilterRow, error)
	GetLogByLvIndex(ctx context.Context, lvIndex uint64) (*types.Log, error)
}

func GetPotentialMatches(ctx context.Context, backend Backend, firstBlock, lastBlock uint64, addresses []common.Address, topics [][]common.Hash) ([]*types.Log, error) {
	firstIndex, err := backend.GetBlockLvPointer(ctx, firstBlock)
	if err != nil {
		return nil, err
	}
	lastIndex, err := backend.GetBlockLvPointer(ctx, lastBlock+1)
	if err != nil {
		return nil, err
	}
	firstMap, lastMap := uint32(firstIndex>>logValuesPerMap), uint32(lastIndex>>logValuesPerMap)
	firstEpoch, lastEpoch := firstMap>>logMapsPerEpoch, lastMap>>logMapsPerEpoch

	matcher := make(matchSequence, len(topics)+1)
	matchAddress := make(matchAny, len(addresses))
	for i, address := range addresses {
		matchAddress[i] = &singleMatcher{backend: backend, value: addressValue(address)}
	}
	matcher[0] = matchAddress //newCachedMatcher(matchAddress, firstMap)
	for i, topicList := range topics {
		matchTopic := make(matchAny, len(topicList))
		for j, topic := range topicList {
			matchTopic[j] = &singleMatcher{backend: backend, value: topicValue(topic)}
		}
		matcher[i+1] = matchTopic //newCachedMatcher(matchTopic, firstMap)
	}

	var logs []*types.Log
	for epochIndex := firstEpoch; epochIndex <= lastEpoch; epochIndex++ {
		fm, lm := epochIndex<<logMapsPerEpoch, (epochIndex+1)<<logMapsPerEpoch-1
		if fm < firstMap {
			fm = firstMap
		}
		if lm > lastMap {
			lm = lastMap
		}
		mapIndices := make([]uint32, lm+1-fm)
		for i := range mapIndices {
			mapIndices[i] = fm + uint32(i)
		}

		//TODO parallelize, check total data size
		matches, err := matcher.getMatches(ctx, mapIndices)
		if err != nil {
			return logs, err
		}
		matcher.clearUntil(mapIndices[len(mapIndices)-1])
		for _, m := range matches {
			mlogs, err := getLogsFromMatches(ctx, backend, firstIndex, lastIndex, m)
			if err != nil {
				return logs, err
			}
			logs = append(logs, mlogs...)
		}
	}
	return logs, nil
}

type matcher interface {
	getMatches(ctx context.Context, mapIndices []uint32) ([]potentialMatches, error)
	clearUntil(mapIndex uint32)
}

func getLogsFromMatches(ctx context.Context, backend Backend, firstIndex, lastIndex uint64, matches potentialMatches) ([]*types.Log, error) {
	var logs []*types.Log
	for _, match := range matches {
		if match < firstIndex || match > lastIndex {
			continue
		}
		log, err := backend.GetLogByLvIndex(ctx, match)
		if err != nil {
			return logs, err
		}
		logs = append(logs, log)
	}
	return logs, nil
}

// singleMatcher implements matcher
type singleMatcher struct {
	backend Backend
	value   common.Hash
}

func (s *singleMatcher) getMatches(ctx context.Context, mapIndices []uint32) ([]potentialMatches, error) {
	results := make([]potentialMatches, len(mapIndices))
	for i, mapIndex := range mapIndices {
		filterRow, err := s.backend.GetFilterMapRow(ctx, mapIndex, rowIndex(mapIndex>>logMapsPerEpoch, s.value))
		if err != nil {
			return nil, err
		}
		results[i] = filterRow.potentialMatches(mapIndex, s.value)
	}
	return results, nil
}

func (s *singleMatcher) clearUntil(mapIndex uint32) {}

// matchAny implements matcher
type matchAny []matcher

func (m matchAny) getMatches(ctx context.Context, mapIndices []uint32) ([]potentialMatches, error) {
	if len(m) == 0 {
		return make([]potentialMatches, len(mapIndices)), nil
	}
	if len(m) == 1 {
		return m[0].getMatches(ctx, mapIndices)
	}
	matches := make([][]potentialMatches, len(m))
	for i, matcher := range m {
		var err error
		if matches[i], err = matcher.getMatches(ctx, mapIndices); err != nil {
			return nil, err
		}
	}
	results := make([]potentialMatches, len(mapIndices))
	merge := make([]potentialMatches, len(m))
	for i := range results {
		for j := range merge {
			merge[j] = matches[j][i]
		}
		results[i] = mergeResults(merge)
	}
	return results, nil
}

func (m matchAny) clearUntil(mapIndex uint32) {
	for _, matcher := range m {
		matcher.clearUntil(mapIndex)
	}
}

func mergeResults(results []potentialMatches) potentialMatches {
	if len(results) == 0 {
		return nil
	}
	var sumLen int
	for _, res := range results {
		if res == nil {
			// nil is a wild card; all indices in map range are potential matches
			return nil
		}
		sumLen += len(res)
	}
	merged := make(potentialMatches, 0, sumLen)
	for {
		best := -1
		for i, res := range results {
			if len(res) == 0 {
				continue
			}
			if best < 0 || res[0] < results[best][0] {
				best = i
			}
		}
		if best < 0 {
			return merged
		}
		if len(merged) == 0 || results[best][0] > merged[len(merged)-1] {
			merged = append(merged, results[best][0])
		}
		results[best] = results[best][1:]
	}
}

// matchSequence implements matcher
type matchSequence []matcher

func (m matchSequence) getMatches(ctx context.Context, mapIndices []uint32) ([]potentialMatches, error) {
	if len(m) == 0 {
		return make([]potentialMatches, len(mapIndices)), nil
	}
	if len(m) == 1 {
		return m[0].getMatches(ctx, mapIndices)
	}
	base, next, offset := m[:len(m)-1], m[len(m)-1], uint64(len(m)-1)
	baseRes, err := base.getMatches(ctx, mapIndices)
	if err != nil {
		return nil, err
	}
	nextIndices := make([]uint32, 0, len(mapIndices)*3/2)
	lastAdded := uint32(math.MaxUint32)
	for i, mapIndex := range mapIndices {
		if baseRes[i] != nil && len(baseRes[i]) == 0 {
			// do not request map index from next matcher if no results from base matcher
			continue
		}
		if lastAdded != mapIndex {
			nextIndices = append(nextIndices, mapIndex)
			lastAdded = mapIndex
		}
		if baseRes[i] == nil || baseRes[i][len(baseRes[i])-1] >= (uint64(mapIndex+1)<<logValuesPerMap)-offset {
			nextIndices = append(nextIndices, mapIndex+1)
			lastAdded = mapIndex + 1
		}
	}

	if len(nextIndices) == 0 {
		return baseRes, nil
	}
	nextRes, err := next.getMatches(ctx, nextIndices)
	if err != nil {
		return nil, err
	}
	var nextPtr int
	nextResult := func(mapIndex uint32) potentialMatches {
		for nextPtr < len(nextIndices) && nextIndices[nextPtr] <= mapIndex {
			if nextIndices[nextPtr] == mapIndex {
				//fmt.Println("nr +", mapIndex, nextPtr)
				return nextRes[nextPtr]
			}
			nextPtr++
		}
		//fmt.Println("nr -", mapIndex, nextPtr)
		return noMatches
	}
	results := make([]potentialMatches, len(mapIndices))
	for i, mapIndex := range mapIndices {
		results[i] = matchResults(mapIndex, offset, baseRes[i], nextResult(mapIndex), nextResult(mapIndex+1))
	}
	//fmt.Println("ms", len(m), "base indices", mapIndices, "next indices", nextIndices)
	//fmt.Println("ms", len(m), "base lengths", pmlen(baseRes), "next lengths", pmlen(nextRes))
	//fmt.Println("ms", len(m), "results lengths", pmlen(results))
	return results, nil
}

/*func pmlen(pm []potentialMatches) []int {
	l := make([]int, len(pm))
	for i, p := range pm {
		l[i] = len(p)
	}
	return l
}*/

func (m matchSequence) clearUntil(mapIndex uint32) {
	for _, matcher := range m {
		matcher.clearUntil(mapIndex)
	}
}

func matchResults(mapIndex uint32, offset uint64, baseRes, nextRes, nextNextRes potentialMatches) potentialMatches {
	if nextRes == nil || (baseRes != nil && len(baseRes) == 0) {
		return baseRes
	}
	if len(nextRes) > 0 {
		start := 0
		for start < len(nextRes) && nextRes[start] < uint64(mapIndex)<<logValuesPerMap+offset {
			start++
		}
		nextRes = nextRes[start:]
	}
	if len(nextNextRes) > 0 {
		stop := 0
		for stop < len(nextNextRes) && nextNextRes[stop] < uint64(mapIndex+1)<<logValuesPerMap+offset {
			stop++
		}
		nextNextRes = nextNextRes[:stop]
	}
	maxLen := len(nextRes) + len(nextNextRes)
	if maxLen == 0 {
		return nextRes
	}
	if len(baseRes) < maxLen {
		maxLen = len(baseRes)
	}
	matchedRes := make(potentialMatches, 0, maxLen)
	for _, res := range []potentialMatches{nextRes, nextNextRes} {
		if baseRes != nil {
			for len(res) > 0 && len(baseRes) > 0 {
				if res[0] > baseRes[0]+offset {
					baseRes = baseRes[1:]
				} else if res[0] < baseRes[0]+offset {
					res = res[1:]
				} else {
					matchedRes = append(matchedRes, baseRes[0])
					res = res[1:]
					baseRes = baseRes[1:]
				}
			}
		} else {
			for len(res) > 0 {
				matchedRes = append(matchedRes, res[0]-offset)
				res = res[1:]
			}
		}
	}
	return matchedRes
}

/*type cachedMatcher struct {
	lock          sync.Mutex
	matcher       matcher
	cache         map[uint32]cachedMatches
	clearedBefore uint32
}

func newCachedMatcher(m matcher, start uint32) *cachedMatcher {
	return &cachedMatcher{
		matcher:       m,
		cache:         make(map[uint32]cachedMatches),
		clearedBefore: start,
	}
}

type cachedMatches struct {
	matches potentialMatches
	reqCh   chan struct{}
}

func (c *cachedMatcher) getMatches(ctx context.Context, mapIndices []uint32) ([]potentialMatches, error) {
	c.lock.Lock()
	if mapIndex < c.clearedBefore {
		panic("invalid cachedMatcher access")
	}
	cm, existed := c.cache[mapIndex]
	if !existed {
		cm = cachedMatches{reqCh: make(chan struct{})}
		c.cache[mapIndex] = cm
	}
	c.lock.Unlock()
	if existed {
		if cm.reqCh == nil {
			return cm.matches, nil
		}
		select {
		case <-cm.reqCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.lock.Lock()
		cm = c.cache[mapIndex]
		c.lock.Unlock()
		if cm.matches == nil {
			panic("cached matches missing")
		}
		return cm.matches, nil
	}
	matches, err := c.matcher.getMatches(ctx, mapIndex)
	if err != nil {
		return nil, err
	}
	c.lock.Lock()
	c.cache[mapIndex] = cachedMatches{matches: matches}
	c.lock.Unlock()
	close(cm.reqCh)
	return matches, nil
}

func (c *cachedMatcher) clearUntil(mapIndex uint32) {
	c.lock.Lock()
	for c.clearedBefore <= mapIndex {
		delete(c.cache, c.clearedBefore)
		c.clearedBefore++
	}
	c.lock.Unlock()
}
*/
