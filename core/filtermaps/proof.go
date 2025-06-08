// Copyright 2025 The go-ethereum Authors
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

package filtermaps

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type prover interface {
	potentialMatches(reader mapReader, mapIndex uint32) mapRangeSubset
	proofSubset(reader mapReader, mapIndex uint32, rangeSubset mapRangeSubset) mapRowSubset
}

type mapRowSubset rangeSet[uint64] // higher 48 bits: mapRowIndex; lower 16 bits: list index

const mapRowShift = 16

var mapRowMult = uint64(1) << mapRowShift

func mapRowUnion(a []mapRowSubset) mapRowSubset {
	var l int
	for _, r := range a {
		l += len(r)
	}
	u := make(mapRowSubset, 0, l)
	for _, r := range a {
		u = append(u, r...)
	}
	(*rangeSet[uint64])(&u).normalize()
	return u
}

func (a mapRowSubset) intersection(b mapRowSubset) mapRowSubset {
	return mapRowSubset(rangeSet[uint64](a).intersection(rangeSet[uint64](b)))
}

func (a mapRowSubset) rowSubset(mapRowIndex uint64) (rowSubset, bool) {
	s := a.intersection(mapRowSubset{common.NewRange[uint64](mapRowIndex<<mapRowShift, mapRowMult)})
	if len(s) == 0 || s[0].First()%mapRowMult != 0 {
		return nil, false
	}
	if s[0].AfterLast()%mapRowMult == 1 {
		s = s[1:]
	} else {
		s[0].SetFirst(mapRowIndex<<mapRowShift + 1)
	}
	res := make(rowSubset, len(s))
	for i, r := range s {
		res[i] = common.NewRange[uint32](uint32(r.First()%mapRowMult)-1, uint32(r.Count()))
	}
	return res, true
}

type rowSubset rangeSet[uint32]

func (a rowSubset) intersection(b rowSubset) rowSubset {
	return rowSubset(rangeSet[uint32](a).intersection(rangeSet[uint32](b)))
}

var rowCountIndex = uint32(math.MaxUint32)

type mapRangeSubset rangeSet[uint32]

func mapRangeUnion(a []mapRangeSubset) mapRangeSubset {
	var l int
	for _, r := range a {
		l += len(r)
	}
	u := make(mapRangeSubset, 0, l)
	for _, r := range a {
		u = append(u, r...)
	}
	(*rangeSet[uint32])(&u).normalize()
	return u
}

func (a mapRangeSubset) intersection(b mapRangeSubset) mapRangeSubset {
	return mapRangeSubset(rangeSet[uint32](a).intersection(rangeSet[uint32](b)))
}

func (a mapRangeSubset) shiftLeft(shift, limit uint32) mapRangeSubset {
	if shift == 0 {
		return a
	}
	b := slices.Clone(a)
	for i, r := range b {
		first := max(r.First(), limit+shift) - shift
		afterLast := max(r.AfterLast(), limit+shift) - shift
		b[i] = common.NewRange[uint32](first, afterLast-first)
	}
	return b
}

func (a mapRangeSubset) shiftRight(shift, limit uint32) mapRangeSubset {
	if shift == 0 {
		return a
	}
	b := slices.Clone(a)
	for i, r := range b {
		first := min(r.First(), limit-shift) + shift
		afterLast := min(r.AfterLast(), limit-shift) + shift
		b[i] = common.NewRange[uint32](first, afterLast-first)
	}
	return b
}

type mapReader interface {
	// if entries beyond the row length are requested then it might return extra zeroes and/or a shorter slice
	getRowData(mapIndex, rowIndex uint32, subset rowSubset) (rowSubset, [][]uint32, uint32, bool)
}

type limitedMapReader struct {
	params *Params
	reader mapReader
	mask   mapRowSubset
}

func (l limitedMapReader) getRowData(mapIndex, rowIndex uint32, subset rowSubset) (rowSubset, [][]uint32, uint32, bool) {
	rowSubset, exists := l.mask.rowSubset(l.params.mapRowIndex(mapIndex, rowIndex))
	if !exists {
		return nil, nil, 0, false
	}
	return l.reader.getRowData(mapIndex, rowIndex, rowSubset.intersection(subset))
}

type singleProver struct {
	params *Params
	value  common.Hash
}

// rangeSubset is expected to be normalized
func (p *singleProver) proofSubset(reader mapReader, mapIndex uint32, rangeSubset mapRangeSubset) (proof mapRowSubset) {
	//fmt.Println("+++ sp.proofSubset  range:", rangeSubset)
	if p.params.progListHeightFirst != 0 {
		panic("progListHeightFirst is assumed to be zero")
	}
	addRange := func(mapRowIndex uint64, first, last uint32) {
		//fmt.Println("addRange", first, last)
		firstChunk := first / 8
		if firstChunk != 0 && (firstChunk&1 == 0) {
			firstChunk--
		}
		lastChunk := last / 8
		if lastChunk&1 == 1 {
			lastChunk++
		}
		proof = append(proof, common.NewRange[uint64](mapRowIndex<<mapRowShift+uint64(firstChunk*8+1), uint64(lastChunk+1-firstChunk)*8))
	}
	var skipOlderThan uint32
	for layerIndex := uint32(0); len(rangeSubset) > 0; layerIndex++ {
		//fmt.Println(" layer", layerIndex)
		rowIndex := p.params.rowIndex(mapIndex, layerIndex, p.value)
		mapRowIndex := p.params.mapRowIndex(mapIndex, rowIndex)
		maxLen := p.params.maxRowLength(layerIndex)
		//fmt.Println("prove getRowData", mapIndex, rowIndex, maxLen)
		rowSubset, rowEntries, rowCount, rowExists := reader.getRowData(mapIndex, rowIndex, rowSubset{common.NewRange[uint32](0, maxLen)})
		//fmt.Println("  res", rowSubset, rowCount, rowExists)
		if !rowExists {
			//fmt.Println(mapIndex, rowIndex, rowSubset)
			panic("layer row data not available")
		}
		proof = append(proof, common.NewRange[uint64](mapRowIndex<<mapRowShift, 1)) // always prove at least the count (even if 0)
		if rowCount == 0 {
			break
		}
		if len(rowSubset) != 1 {
			panic("layer row data not fully available")
		}
		entries := rowEntries[0]
		entryCount := uint32(len(entries))
		if entryCount != min(rowCount, maxLen) {
			//fmt.Println(entryCount, rowCount, maxLen)
			panic("layer row data not fully available")
		}
		var ptr uint32
		for len(rangeSubset) > 0 && ptr < entryCount {
			//fmt.Println(" len(rangeSubset)", len(rangeSubset), "ptr", ptr, "entryCount", entryCount)
			afterFirst := max((rangeSubset[0].First()+1)<<(p.params.logMapWidth-p.params.logValuesPerMap), skipOlderThan)
			last := rangeSubset[0].Last() << (p.params.logMapWidth - p.params.logValuesPerMap)
			for ; ptr < entryCount && entries[ptr] < afterFirst; ptr++ {
			}
			proveFirst := max(ptr, 1) - 1
			for ; ptr < entryCount && entries[ptr] < last; ptr++ {
			}
			proveLast := min(ptr, rowCount-1)
			addRange(mapRowIndex, proveFirst, proveLast)
			if entries[proveLast] >= last {
				rangeSubset = rangeSubset[1:]
			}
		}
		if rowCount < maxLen {
			break
		}
		addRange(mapRowIndex, maxLen-1, maxLen-1)
		skipOlderThan = max(skipOlderThan, ((entries[rowCount-1]>>(p.params.logMapWidth-p.params.logValuesPerMap))+1)<<(p.params.logMapWidth-p.params.logValuesPerMap))
	}
	//fmt.Println("proof before normalize", proof)
	(*rangeSet[uint64])(&proof).normalize()
	//fmt.Println("proof after normalize", proof)
	return proof
}

func (p *singleProver) potentialMatches(reader mapReader, mapIndex uint32) (matches mapRangeSubset) {
	//fmt.Println("+++ sp.potentialMatches")
	mapFirst := uint64(mapIndex) << p.params.logValuesPerMap
	var (
		lastLayer, afterLastPosition, afterLastSubIndex uint32
		lastEntryKnown                                  bool
	)
	for layerIndex := uint32(0); ; layerIndex++ {
		rowIndex := p.params.rowIndex(mapIndex, layerIndex, p.value)
		maxLen := p.params.maxRowLength(layerIndex)
		//fmt.Println("pm getRowData", mapIndex, rowIndex, maxLen)
		rowSubset, rowEntries, rowCount, rowExists := reader.getRowData(mapIndex, rowIndex, rowSubset{common.NewRange[uint32](0, maxLen)})
		//fmt.Println("  res", rowSubset, rowCount, rowExists)
		if !rowExists {
			// entire mapping layer is missing, we cannot even determine whether
			// we should proceed to the next layer; assume that anything from here
			// is a potential match
			break
		}
		for i, section := range rowSubset {
			entries := rowEntries[i]
			var skippedOlderEntry bool
			for i, entry := range entries {
				position := section.First() + uint32(i)
				subIndex := entry >> (p.params.logMapWidth - p.params.logValuesPerMap)
				if subIndex < afterLastSubIndex {
					skippedOlderEntry = true
					////fmt.Println("skip")
					continue
				}
				if (layerIndex != lastLayer || position > afterLastPosition) && (!skippedOlderEntry || layerIndex != lastLayer+1) {
					matches = append(matches, common.NewRange[uint32](afterLastSubIndex, subIndex-afterLastSubIndex))
				}
				if entry == p.params.columnIndex(mapFirst+uint64(subIndex), &p.value) {
					matches = append(matches, common.NewRange[uint32](subIndex, 1))
				}
				skippedOlderEntry = false
				////fmt.Println(" row iter", i, entry)
				lastLayer, afterLastPosition, afterLastSubIndex = layerIndex, position+1, subIndex+1
			}
		}
		////fmt.Println("row end", afterLastPosition, maxLen, rowCount)
		if afterLastPosition < maxLen {
			lastEntryKnown = afterLastPosition >= rowCount
			break
		}
	}
	if !lastEntryKnown {
		matches = append(matches, common.NewRange[uint32](afterLastSubIndex, uint32(p.params.valuesPerMap)-afterLastSubIndex))
	}
	(*rangeSet[uint32])(&matches).normalize()
	return matches
}

type matchAnyProver []prover

func (p matchAnyProver) proofSubset(reader mapReader, mapIndex uint32, rangeSubset mapRangeSubset) mapRowSubset {
	union := make([]mapRowSubset, len(p))
	for i, prover := range p {
		union[i] = prover.proofSubset(reader, mapIndex, rangeSubset)
	}
	return mapRowUnion(union)
}

func (p matchAnyProver) potentialMatches(reader mapReader, mapIndex uint32) mapRangeSubset {
	union := make([]mapRangeSubset, len(p))
	for i, prover := range p {
		union[i] = prover.potentialMatches(reader, mapIndex)
	}
	return mapRangeUnion(union)
}

type matchSequenceProver struct {
	params   *Params
	children []prover
}

func (p *matchSequenceProver) proofSubset(reader mapReader, mapIndex uint32, proveRange mapRangeSubset) mapRowSubset {
	matchCount := make([]uint32, len(p.children))
	order := make([]int, len(p.children))
	for i := range order {
		order[i] = i
	}
	rowSubset := make([]mapRowSubset, len(p.children))
	rangeSubset := make([]mapRangeSubset, len(p.children)+1)
	for i, prover := range p.children {
		matchCount[i] = rangeSet[uint32](prover.potentialMatches(reader, mapIndex)).totalCount()
	}
	//fmt.Println("*** matchCount", matchCount)
	sort.Slice(order, func(i, j int) bool { return matchCount[order[i]] < matchCount[order[j]] })
	//fmt.Println("*** order", order)
	rangeSubset[0] = proveRange
	//fmt.Println("*** rangeSubset", 0, proveRange)
	for i, j := range order {
		rowSubset[i] = p.children[j].proofSubset(reader, mapIndex, rangeSubset[i].shiftRight(uint32(j), uint32(p.params.valuesPerMap)))
		//fmt.Println("*** rowSubset", i, rowSubset[i])
		matches := p.children[j].potentialMatches(limitedMapReader{p.params, reader, rowSubset[i]}, mapIndex).shiftLeft(uint32(j), 0)
		//fmt.Println("*** matches", matches)
		rangeSubset[i+1] = rangeSubset[i].intersection(matches)
		//fmt.Println("*** rangeSubset", i+1, rangeSubset[i+1])
	}
	//TODO
	/*lastRange := rangeSubset[len(p.children)]
	for i := len(order) - 2; i >= 0; i-- {
		j := order[i]
		rowSubset[i] = p.children[j].proofSubset(reader, mapIndex, rangeSubset[i].intersection(lastRange).shiftRight(uint32(j), uint32(p.params.valuesPerMap)))
		//fmt.Println("*** rowSubset", i, rowSubset[i])
		matches := p.children[j].potentialMatches(limitedMapReader{p.params, reader, rowSubset[i]}, mapIndex).shiftLeft(uint32(j), 0)
		//fmt.Println("*** matches", matches)
		lastRange = lastRange.intersection(matches)
		//fmt.Println("*** lastRange", lastRange)
	}*/
	return mapRowUnion(rowSubset)
}

func (p *matchSequenceProver) potentialMatches(reader mapReader, mapIndex uint32) mapRangeSubset {
	//fmt.Println("msp.potentialMatches")
	var res mapRangeSubset
	for i, prover := range p.children {
		if i == 0 {
			res = prover.potentialMatches(reader, mapIndex)
			//fmt.Println(" child 0", res)
		} else {
			next := prover.potentialMatches(reader, mapIndex).shiftLeft(uint32(i), 0)
			//fmt.Println(" child", i, next)
			res = res.intersection(next)
			//fmt.Println("  intersection", res)
		}
	}
	return res
}

type treeReader interface {
	get(treeIndex) TreeNode
	tryGet(treeIndex) (TreeNode, bool, uint)
	isLeaf(treeIndex) bool
}

/*type treeReadWriter interface {
	treeReader
	set(treeIndex, TreeNode)
}*/

func (params *Params) proveMapSubset(source treeReader, proof *memTreeView, subset mapRowSubset) {
	for _, rowRange := range subset {
		mapRowIndex := rowRange.First() >> mapRowShift
		if mapRowIndex != rowRange.Last()>>mapRowShift {
			//fmt.Println("xxx ", rowRange.First()>>mapRowShift, rowRange.Last()>>mapRowShift)
			panic("proven row range should not cross row boundaries")
		}
		epoch := uint32(mapRowIndex >> (params.logMapsPerEpoch + params.logMapHeight))
		mapRowSubIndex := mapRowIndex % (uint64(params.mapsPerEpoch) * uint64(params.mapHeight))
		filterMapsRootIndex := params.gtiEpochRoot(epoch).child(gtiFilterMaps)
		mapRowRootIndex := filterMapsRootIndex.append(mapRowSubIndex, params.logMapsPerEpoch+params.logMapHeight)

		first, last := uint32(rowRange.First()%mapRowMult), uint32(rowRange.Last()%mapRowMult)
		if first == 0 {
			countIndex := mapRowRootIndex.child(gtiProgListCount)
			proof.set(countIndex, source.get(countIndex))
			first++
		}
		if first <= last {
			var pl progListIndex
			pl.init(params, mapRowRootIndex)
			firstChunk, lastChunk := (first-1)/8, (last-1)/8
			for listIndex := firstChunk; listIndex <= lastChunk; listIndex++ {
				leafIndex, _, _, _ := pl.getLeaf(uint64(listIndex))
				proof.set(leafIndex, source.get(leafIndex))
				//TODO prove empty rows
			}
		}
	}
}

type proofReader struct {
	params *Params
	reader treeReader
}

func dumpSubtree(tree treeReader, root, sub treeIndex) {
	_, known, lb := tree.tryGet(root.child(sub))
	if lb != 0 {
		return
	}
	fmt.Printf("  %032b  %v\n", uint32(sub.lo), known)
	dumpSubtree(tree, root, sub.append(0, 1))
	dumpSubtree(tree, root, sub.append(1, 1))
}

func (p proofReader) getRowData(mapIndex, rowIndex uint32, wantSubset rowSubset) (haveSubset rowSubset, haveEntries [][]uint32, rowCount uint32, rowExists bool) {
	mapRowIndex := p.params.mapRowIndex(mapIndex, rowIndex)
	epoch := uint32(mapRowIndex >> (p.params.logMapsPerEpoch + p.params.logMapHeight)) //TODO func
	mapRowSubIndex := mapRowIndex % (uint64(p.params.mapsPerEpoch) * uint64(p.params.mapHeight))
	filterMapsRootIndex := p.params.gtiEpochRoot(epoch).child(gtiFilterMaps)
	mapRowRootIndex := filterMapsRootIndex.append(mapRowSubIndex, p.params.logMapsPerEpoch+p.params.logMapHeight)
	//dumpSubtree(p.reader, mapRowRootIndex, rootIndex)
	node, known, levelsBelow := p.reader.tryGet(mapRowRootIndex.child(gtiProgListCount))
	if levelsBelow != 0 || !known {
		return nil, nil, 0, false
	}
	rowCount, rowExists = uint32(nodeToUint64(node)), true
	//fmt.Println(" rowCount", rowCount)
	wantSubset = wantSubset.intersection(rowSubset{common.NewRange[uint32](0, rowCount)})
	var pl progListIndex
	pl.init(p.params, mapRowRootIndex)
	lastChunkIndex := uint32(math.MaxUint32)
	nextEntryIndex := uint32(math.MaxUint32)
	for _, rowRange := range wantSubset {
		for entryIndex := rowRange.First(); entryIndex < rowRange.AfterLast(); {
			chunkIndex := entryIndex / 8
			if chunkIndex != lastChunkIndex {
				leafIndex, _, subtreeIndex, subtreeHeight := pl.getLeaf(uint64(chunkIndex))
				node, known, levelsBelow = p.reader.tryGet(leafIndex)
				if levelsBelow == 0 {
					if !known {
						//fmt.Println(" chunk", chunkIndex)
						panic("unknown row data leaf")
					}
					lastChunkIndex = chunkIndex
				} else {
					// entry is unavailable; advance to entry after missing subtree
					entryIndex += min(uint32(1)<<levelsBelow, (uint32(1)<<subtreeHeight)-uint32(subtreeIndex))
				}
			}
			if chunkIndex == lastChunkIndex {
				// entry is available; add to results and advance to next entry
				entry := binary.LittleEndian.Uint32(node[(entryIndex%8)*4 : (entryIndex%8)*4+4])
				if entryIndex != nextEntryIndex {
					haveSubset = append(haveSubset, common.NewRange[uint32](entryIndex, 0))
					haveEntries = append(haveEntries, nil)
				}
				last := len(haveSubset) - 1
				haveSubset[last].SetLast(entryIndex)
				haveEntries[last] = append(haveEntries[last], entry)
				entryIndex++
				nextEntryIndex = entryIndex
			}
		}
	}
	return
}

func (params *Params) gtiLogEntryRoot(lvIndex uint64) treeIndex {
	logTreeHeight := params.logMapsPerEpoch + params.logValuesPerMap
	return params.gtiEpochRoot(uint32(lvIndex>>logTreeHeight)).child(gtiLogEntries).append(lvIndex&(uint64(1)<<logTreeHeight-1), logTreeHeight)
}

func (params *Params) getOrProveLog(reader treeReader, proof *memTreeView, lvIndex uint64) (*types.Log, bool) {
	logRoot := params.gtiLogEntryRoot(lvIndex)
	rootNode, known, lb := reader.tryGet(logRoot)
	if lb != 0 {
		//fmt.Println("gpl 1")
		return nil, false
	}
	if known && rootNode == (TreeNode{}) { // no log there, prove false positive
		if proof != nil {
			proof.set(logRoot, rootNode)
		}
		return nil, true
	}
	var fail bool
	getNode := func(childIndex treeIndex) TreeNode {
		index := logRoot.child(childIndex)
		node, known, lb := reader.tryGet(index)
		if lb != 0 || !known {
			fail = true
			return TreeNode{}
		}
		if proof != nil {
			proof.set(index, node)
		}
		return node
	}
	delimiterDummy := nodeToUint64(getNode(gtiDelimiterMetaDummy))
	if delimiterDummy == math.MaxUint64 {
		return nil, true // block delimiter, not relevant here
	}

	log := new(types.Log)
	addr := getNode(gtiLogAddress)
	copy(log.Address[:], addr[:len(log.Address)])
	topicCount := nodeToUint64(getNode(gtiLogTopicsLength))
	if topicCount > 4 {
		//fmt.Println("gpl 2")
		return nil, false //TODO log error?
	}
	log.Topics = make([]common.Hash, topicCount)
	for i := range topicCount {
		log.Topics[i] = common.Hash(getNode(gtiLogTopicsRoot.append(i, 2)))
	}
	if fail {
		//fmt.Println("gpl 3")
		return nil, false
	}
	var pl progListIndex
	pl.init(params, gtiLogData)
	chunkIndex := uint64(0)
	dataLen := nodeToUint64(getNode(pl.countIndex))
	if dataLen > 10000000 {
		//fmt.Println("gpl 4")
		return nil, false //TODO log error?
	}
	log.Data = make([]byte, dataLen)
	for ptr := uint64(0); ptr < dataLen; {
		leafIndex, _, _, _ := pl.getLeaf(chunkIndex)
		node := getNode(leafIndex)
		if fail {
			//fmt.Println("gpl 5")
			return nil, false
		}
		end := min(ptr+32, dataLen)
		copy(log.Data[ptr:end], node[:end-ptr])
		ptr = end
		chunkIndex++
	}
	log.BlockNumber = nodeToUint64(getNode(gtiLogMetaBlockNumber))
	log.TxHash = common.Hash(getNode(gtiLogMetaTxHash))
	log.TxIndex = uint(nodeToUint64(getNode(gtiLogMetaTxIndex)))
	log.Index = uint(delimiterDummy) // equals to nodeToUint64(getNode(gtiLogMetaLogIndex))
	//TODO prove block hash with next delimiter
	if fail {
		//fmt.Println("gpl 6")
		return nil, false //TODO allow partially proven false positives
	}
	return log, true
}

func (params *Params) getOrProveDelimiter(reader treeReader, proof *memTreeView, lvIndex uint64) (uint64, bool) {
	delimiterRoot := params.gtiLogEntryRoot(lvIndex)
	var fail bool
	getNode := func(childIndex treeIndex) TreeNode {
		index := delimiterRoot.child(childIndex)
		node, known, lb := reader.tryGet(index)
		if lb != 0 || !known {
			fail = true
			return TreeNode{}
		}
		if proof != nil {
			proof.set(index, node)
		}
		return node
	}
	blockNumber := nodeToUint64(getNode(gtiDelimiterMetaBlockNumber))
	delimiterDummy := nodeToUint64(getNode(gtiDelimiterMetaDummy))
	if fail || delimiterDummy != math.MaxUint64 {
		return 0, false
	}
	return blockNumber, true
}

func findBoundary(tree treeReader, index treeIndex, height uint, dir uint64) (treeIndex, uint64) {
	var subIndex uint64
	for range height {
		if !tree.isLeaf(index.append(dir, 1)) {
			index = index.append(dir, 1)
			subIndex = subIndex*2 + dir
		} else if !tree.isLeaf(index.append(1-dir, 1)) {
			index = index.append(1-dir, 1)
			subIndex = subIndex*2 + 1 - dir
		} else {
			panic("could not find boundary vector item")
		}
	}
	return index, subIndex
}

func (params *Params) findBoundaryIndex(tree treeReader, dir uint64) uint64 {
	epochRoot, epochIndex := findBoundary(tree, gtiEpochs, params.logEpochHistory, dir)
	_, subIndex := findBoundary(tree, epochRoot.child(gtiLogEntries), params.logMapsPerEpoch+params.logValuesPerMap, dir)
	return epochIndex<<(params.logMapsPerEpoch+params.logValuesPerMap) + subIndex
}

type filterQuery struct {
	firstBlock, lastBlock uint64
	addresses             []common.Address
	topics                [][]common.Hash
}

func (fq *filterQuery) match(log *types.Log) bool {
	if len(fq.addresses) > 0 && !slices.Contains(fq.addresses, log.Address) {
		return false
	}
	// If the to filtered topics is greater than the amount of topics in logs, skip.
	if len(fq.topics) > len(log.Topics) {
		return false
	}
	for i, sub := range fq.topics {
		if len(sub) == 0 {
			continue // empty rule set == wildcard
		}
		if !slices.Contains(sub, log.Topics[i]) {
			return false
		}
	}
	return true
}

func (params *Params) constructProver(query *filterQuery) prover {
	provers := make([]prover, len(query.topics)+1)
	proveAddress := make(matchAnyProver, len(query.addresses))
	for i, address := range query.addresses {
		proveAddress[i] = &singleProver{params: params, value: addressValue(address)}
	}
	provers[0] = proveAddress
	for i, topicList := range query.topics {
		proveTopic := make(matchAnyProver, len(topicList))
		for j, topic := range topicList {
			proveTopic[j] = &singleProver{params: params, value: topicValue(topic)}
		}
		provers[i+1] = proveTopic
	}
	return &matchSequenceProver{params: params, children: provers}
}

func (params *Params) verifyProof(mp *multiProof, query *filterQuery, root common.Hash, headBlock uint64) (logs []*types.Log, err error) {
	tree := createProofTree(mp)
	if root != tree.rootHash() {
		return nil, errors.New("root hash mismatch")
	}
	firstIndex := params.findBoundaryIndex(tree, 0)
	lastIndex := params.findBoundaryIndex(tree, 1)
	if query.firstBlock == 0 {
		if firstIndex != 0 {
			return nil, errors.New("query range mismatch")
		}
	} else {
		blockNumber, ok := params.getOrProveDelimiter(tree, nil, firstIndex)
		if !ok || blockNumber+1 != query.firstBlock {
			return nil, errors.New("query range mismatch")
		}
	}
	if query.lastBlock == headBlock {
		nextIndex, ok, _ := tree.tryGet(gtiNextIndex)
		if !ok || nodeToUint64(nextIndex) != lastIndex+1 {
			return nil, errors.New("query range mismatch")
		}
	} else {
		blockNumber, ok := params.getOrProveDelimiter(tree, nil, lastIndex)
		if !ok || blockNumber != query.lastBlock {
			return nil, errors.New("query range mismatch")
		}
	}
	prover := params.constructProver(query)
	reader := proofReader{params, tree}
	firstMap, lastMap := uint32(firstIndex>>params.logValuesPerMap), uint32(lastIndex>>params.logValuesPerMap)
	for mapIndex := firstMap; mapIndex <= lastMap; mapIndex++ {
		mapMatches := prover.potentialMatches(reader, mapIndex)
		for _, mapRange := range mapMatches {
			for subIndex := range mapRange.Iter() {
				lvIndex := uint64(mapIndex)<<params.logValuesPerMap + uint64(subIndex)
				if lvIndex >= firstIndex && lvIndex <= lastIndex {
					//fmt.Println("verifier: potential match", lvIndex)
					log, ok := params.getOrProveLog(tree, nil, lvIndex)
					if !ok {
						return nil, errors.New("potential match not proven")
					}
					if log != nil && query.match(log) {
						logs = append(logs, log)
					}
				}
			}
		}
	}
	return logs, nil
}

type multiProof struct {
	leaves, proof []TreeNode
	leafIndices   []treeIndex
}

func createMultiProof(source treeReader, proofTree *memTreeView) *multiProof {
	mp := new(multiProof)
	createMultiProofSubtree(source, proofTree, mp, rootIndex)
	slices.Reverse(mp.proof)
	return mp
}

func createMultiProofSubtree(source treeReader, proofTree *memTreeView, multiProof *multiProof, index treeIndex) {
	if !proofTree.isLeaf(index) {
		index = index.shiftLeft(1)
		createMultiProofSubtree(source, proofTree, multiProof, index)
		createMultiProofSubtree(source, proofTree, multiProof, index.addInt(1))
		return
	}
	if proofTree.isKnown(index) {
		multiProof.leafIndices = append(multiProof.leafIndices, index)
		/*lz := index.leadingZeros()
		shi := index.shiftLeft(lz + 1).or(rootIndex.shiftLeft(lz))
		fmt.Printf("leaf  %016x%016x\n", shi.hi, shi.lo)*/
		multiProof.leaves = append(multiProof.leaves, proofTree.get(index))
	} else {
		multiProof.proof = append(multiProof.proof, source.get(index))
	}
}

func createProofTree(mp *multiProof) *memTreeView {
	mt := &memTree{roots: make(map[uint64]uint32)}
	proofTree := mt.newWriter(0)
	if len(mp.leafIndices) != len(mp.leaves) {
		panic("invalid multiproof")
	}
	for i, index := range mp.leafIndices {
		proofTree.set(index, mp.leaves[i])
	}
	var ptr int
	createProofSubtree(mp, proofTree, rootIndex, &ptr)
	if ptr != len(mp.proof) {
		panic("invalid number of proof nodes")
	}
	return proofTree
}

func createProofSubtree(mp *multiProof, proofTree *memTreeView, index treeIndex, ptr *int) {
	if !proofTree.isLeaf(index) {
		index = index.shiftLeft(1)
		createProofSubtree(mp, proofTree, index.addInt(1), ptr)
		createProofSubtree(mp, proofTree, index, ptr)
		return
	}
	if !proofTree.isKnown(index) {
		proofTree.set(index, mp.proof[*ptr])
		(*ptr)++
	}
}

func (params *Params) proveQuery(tree treeReader, query *filterQuery, firstIndex, lastIndex uint64) *multiProof {
	mt := &memTree{roots: make(map[uint64]uint32)}
	proof := mt.newWriter(0)
	prover := params.constructProver(query)
	reader := proofReader{params, tree}
	firstMap, lastMap := uint32(firstIndex>>params.logValuesPerMap), uint32(lastIndex>>params.logValuesPerMap)
	//fmt.Println("map range", firstMap, lastMap)
	proveSubsets := make([]mapRowSubset, lastMap+1-firstMap)
	for mapIndex := firstMap; mapIndex <= lastMap; mapIndex++ {
		first, last := uint32(0), uint32(params.valuesPerMap-1)
		if mapIndex == firstMap {
			first = uint32(firstIndex % params.valuesPerMap)
		}
		if mapIndex == lastMap {
			last = uint32(lastIndex % params.valuesPerMap)
		}
		//fmt.Println("prove", mapIndex, first, last)
		proveSubsets[mapIndex-firstMap] = prover.proofSubset(reader, mapIndex, mapRangeSubset{common.NewRange[uint32](first, last+1-first)})
	}
	//fmt.Println("proveSubsets", proveSubsets)
	proveSubset := mapRowUnion(proveSubsets)
	//fmt.Println("proveSubset", proveSubset)
	params.proveMapSubset(tree, proof, proveSubset)
	//proofReader := proofReader{params, proof}
	if firstIndex > 0 {
		if _, ok := params.getOrProveDelimiter(tree, proof, firstIndex); !ok {
			panic("failed to prove block delimiter")
		}
	} else {
		if _, ok := params.getOrProveLog(tree, proof, 0); !ok {
			panic("failed to prove first log entry")
		}
	}
	nextIndex := nodeToUint64(tree.get(gtiNextIndex))
	if lastIndex < nextIndex {
		if _, ok := params.getOrProveDelimiter(tree, proof, lastIndex); !ok {
			panic("failed to prove block delimiter")
		}
	} else {
		if _, ok := params.getOrProveLog(tree, proof, nextIndex-1); !ok {
			panic("failed to prove last log entry")
		}
	}
	for mapIndex := firstMap; mapIndex <= lastMap; mapIndex++ {
		mapMatches := prover.potentialMatches(reader, mapIndex) //TODO
		//mapMatches := prover.potentialMatches(proofReader, mapIndex) //TODO
		for _, mapRange := range mapMatches {
			for subIndex := range mapRange.Iter() {
				lvIndex := uint64(mapIndex)<<params.logValuesPerMap + uint64(subIndex)
				if lvIndex >= firstIndex && lvIndex <= lastIndex {
					//fmt.Println("potential match:", lvIndex)
					if _, ok := params.getOrProveLog(tree, proof, lvIndex); !ok {
						//return nil, errors.New("failed to prove potential match")
						panic("failed to prove potential match")
					}
				}
			}
		}
	}
	return createMultiProof(tree, proof)
}
