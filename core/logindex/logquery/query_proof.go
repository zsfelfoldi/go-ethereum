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

package logquery

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"
	"math/bits"
	"sort"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/logindex"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb/database"
)

type QueryProof struct {
	Query                                  FilterQuery
	RefHeader                              types.Header
	IndexContracts                         []common.Address
	ContractProofNodes, ContractProofCodes [][]byte
	TableQueryProofs                       []tableQueryProof

	contractProofNodes, contractProofCodes map[common.Hash][]byte
}

type tableQueryProof struct {
	IndexContract           uint32
	FirstBlock, TableSize   uint64
	ProvenEntries           logindex.EntriesForStorage
	EntryIndices            []uint64
	EntryCount, ResultCount uint64
	ProofHashes             []merkle.Value
	BlockResults            []blockResults

	entries logindex.IndexEntries
}

type blockResults struct {
	Header         types.Header
	ProvenReceipts []uint32
	ReceiptsProof  [][]byte // receipts trie nodes in ascending order of node hash

	proofMap map[common.Hash][]byte
}

func (qp *QueryProof) addOrGetIndexContract(address common.Address) uint32 {
	for i, addr := range qp.IndexContracts {
		if addr == address {
			return uint32(i)
		}
	}
	qp.IndexContracts = append(qp.IndexContracts, address)
	return uint32(len(qp.IndexContracts) - 1)
}

func (qp *QueryProof) printStats() {
	fmt.Println("* Reference block number:", qp.RefHeader.Number.Uint64())
	fmt.Println("* Unique index contracts:", len(qp.IndexContracts))
	fmt.Println("* Table root MPT proof nodes:", len(qp.ContractProofNodes))
	fmt.Println("* Table root MPT proof codes:", len(qp.ContractProofCodes))
	for i, tqp := range qp.TableQueryProofs {
		fmt.Println("*** Table query proof", i)
		fmt.Println("  * Contract index:", tqp.IndexContract)
		fmt.Println("  * First block:", tqp.FirstBlock)
		fmt.Println("  * Table size:", tqp.TableSize)
		fmt.Println("  * Number of proven entries:", len(tqp.ProvenEntries))
		fmt.Println("  * Number of IXT proof hashes:", len(tqp.ProofHashes))
		fmt.Println("  * Number of individual results:", tqp.ResultCount)
		fmt.Println("  * Number of block results:", len(tqp.BlockResults))
		for j, br := range tqp.BlockResults {
			fmt.Println("  *** Block result", j)
			fmt.Println("    * Block number:", br.Header.Number.Uint64())
			fmt.Println("    * Number of proven receipts:", len(br.ProvenReceipts))
			fmt.Println("    * Number of receipt MPT proof nodes:", len(br.ReceiptsProof))
		}
	}
}

func (qp *QueryProof) Verify(backend contractVerifierBackend) ([]*types.Log, error) {
	//fmt.Println("* qp.Verify")
	//TODO check index contract whitelist
	var resultCount, nextTableFirst uint64
	if len(qp.TableQueryProofs) == 0 {
		return nil, errors.New("no table proofs available")
	}
	for i, tqp := range qp.TableQueryProofs {
		resultCount += tqp.ResultCount
		if i > 0 && tqp.FirstBlock != nextTableFirst {
			return nil, errors.New("non-adjacent or overlapping table proofs")
		}
		nextTableFirst = tqp.FirstBlock + tqp.TableSize
	}
	//fmt.Println("* no gap, no overlap")
	if (qp.TableQueryProofs[0].FirstBlock > qp.Query.FirstBlock && (resultCount < qp.Query.MaxResults || !qp.Query.Reverse)) ||
		(qp.TableQueryProofs[len(qp.TableQueryProofs)-1].FirstBlock+qp.TableQueryProofs[len(qp.TableQueryProofs)-1].TableSize <= qp.Query.LastBlock &&
			(resultCount < qp.Query.MaxResults || qp.Query.Reverse)) { //TODO make this nicer
		return nil, errors.New("table proofs do not cover query range")
	}
	//fmt.Println("* fully covered")
	if resultCount > qp.Query.MaxResults {
		return nil, errors.New("query result count limit exceeded")
	}
	//fmt.Println("* resultCount", resultCount)
	limitedTableProof := -1
	if resultCount == qp.Query.MaxResults {
		if qp.Query.Reverse {
			limitedTableProof = 0
		} else {
			limitedTableProof = len(qp.TableQueryProofs) - 1
		}
		if qp.TableQueryProofs[limitedTableProof].ResultCount == 0 {
			return nil, errors.New("useless table proof for limited query results")
		}
	}
	//fmt.Println("* limitedTableProof", limitedTableProof)
	var results []*types.Log
	qp.contractProofNodes = makeProofMap(qp.ContractProofNodes)
	qp.contractProofCodes = makeProofMap(qp.ContractProofCodes)
	var reqParentBlockHash common.Hash
	for i, tqp := range qp.TableQueryProofs {
		provenTableRoot, err := qp.getProvenTableRoot(backend, qp.IndexContracts[tqp.IndexContract], tqp.FirstBlock, tqp.TableSize)
		if err != nil {
			return nil, err
		}
		results, reqParentBlockHash, err = tqp.verify(&qp.Query, provenTableRoot, i == limitedTableProof, results, reqParentBlockHash)
		if err != nil {
			return nil, err
		}
	}
	if reqParentBlockHash != (common.Hash{}) {
		tqp := qp.TableQueryProofs[len(qp.TableQueryProofs)-1]
		if qp.RefHeader.Number.Uint64() != tqp.FirstBlock+tqp.TableSize-1 {
			return nil, errors.New("required last block of last proven table not proven by reference head")
		} else if qp.RefHeader.Hash() != reqParentBlockHash {
			return nil, errors.New("required last block of last proven table does not match reference head")
		}
	}
	return results, nil
}

func (qp *QueryProof) getProvenTableRoot(backend contractVerifierBackend, indexContract common.Address, firstBlock, tableSize uint64) (common.Hash, error) {
	return getProvenTableRoot(backend, &qp.RefHeader, indexContract, firstBlock, tableSize, qp.contractProofNodes, qp.contractProofCodes)
}

// if total result count equals MaxResults then resultsLimited is true for the
// first/last tableQueryProof (depending on direction)
func (tqp *tableQueryProof) verify(query *FilterQuery, provenTableRoot common.Hash, resultsLimited bool, results []*types.Log, reqParentBlockHash common.Hash) ([]*types.Log, common.Hash, error) {
	fmt.Println("** tqp.verify", tqp.FirstBlock, tqp.TableSize)
	tqp.entries = tqp.ProvenEntries.ToEntries()
	ctr, err := tqp.calculateTableRoot()
	if err != nil {
		return nil, common.Hash{}, err
	}
	fmt.Println(" * calc table root", common.Hash(ctr), "proven table root", provenTableRoot)
	if common.Hash(ctr) != provenTableRoot {
		return nil, common.Hash{}, errors.New("table root mismatch")
	}
	oldCount := len(results)
	results, inclusionProven, validResult, reqLastBlockHash, err := tqp.getProvenEntries(query, results, reqParentBlockHash)
	blockRange := common.NewRange[uint64](tqp.FirstBlock, tqp.TableSize).Intersection(common.NewRange[uint64](query.FirstBlock, query.LastBlock+1-query.FirstBlock))
	if blockRange.IsEmpty() {
		return results, reqLastBlockHash, nil
		//return nil, common.Hash{}, errors.New("useful block range is empty")
	}
	begin := logindex.IndexPosition{BlockNumber: blockRange.First()}
	end := logindex.IndexPosition{BlockNumber: blockRange.Last(), TxIndex: math.MaxUint32, LogIndex: math.MaxUint32}
	//fmt.Println("getProvenEntries oldCount:", oldCount, "results:", len(results), "inclusionProven", len(inclusionProven))
	if err != nil {
		return nil, common.Hash{}, err
	}
	count := len(results) - oldCount
	if resultsLimited && uint64(count) >= tqp.ResultCount {
		trimResults := count - int(tqp.ResultCount)
		if query.Reverse {
			// trim extra results from beginning of result list
			results = results[trimResults:]
			var trimPos, validPos int
			for validPos < trimResults {
				if validResult[trimPos] {
					validPos++
				}
				trimPos++
			}
			for trimPos < len(validResult) && !validResult[trimPos] {
				trimPos++
			}
			inclusionProven = inclusionProven[trimPos:]
			begin = inclusionProven[0]
		} else {
			// trim extra results from end of result list
			results = results[:len(results)-trimResults]
			trimPos := len(inclusionProven) - 1
			var validPos int
			for validPos < trimResults {
				if validResult[trimPos] {
					validPos++
				}
				trimPos--
			}
			for trimPos >= 0 && !validResult[trimPos] {
				trimPos--
			}
			inclusionProven = inclusionProven[:trimPos+1]
			end = inclusionProven[trimPos]
		}
		count = int(tqp.ResultCount)
	}
	//fmt.Println(" * begin", begin, "end", end)
	//fmt.Println("after trim count:", count, "expected:", tqp.ResultCount)
	if uint64(count) != tqp.ResultCount {
		//fmt.Println("count", count, "tqp.ResultCount", tqp.ResultCount, "len(inclusionProven)", len(inclusionProven), "len(validResult)", len(validResult), "validResult", validResult)
		panic("xxxxx")
		return nil, common.Hash{}, errors.New("invalid result count")
	}
	//fmt.Println(" * result count", count)
	potentialMatches := tqp.getPotentialMatches(query, begin, end)
	//fmt.Println(" * inclusion proven", inclusionProven)
	//fmt.Println(" * potential matches", potentialMatches)
	/*fmt.Println("--- query", query)
	fmt.Println("--- range", begin, end)
	for i, pos := range inclusionProven {
		fmt.Println("--- ip", i, pos)
	}
	var ii int
	for pos := range potentialMatches.iter() {
		if ii >= len(inclusionProven)+10 {
			break
		}
		fmt.Println("--- pm", ii, pos)
		ii++
	}*/
	var i int
	for pos := range potentialMatches.iter() {
		if i >= len(inclusionProven) || inclusionProven[i] != pos {
			return nil, common.Hash{}, errors.New("inclusion and exclusion proofs do not match")
		}
		i++
	}
	if i != len(inclusionProven) {
		return nil, common.Hash{}, errors.New("inclusion and exclusion proofs do not match")
	}
	//fmt.Println(" * inclusion/exclusion proof match")
	return results, reqLastBlockHash, nil
}

// getProvenEntries returns all logs matching the filter criteria from proven
// transaction receipts. Note that the number of added results might exceed
// ResultCount because entire receipts are proven and filtered here regardless
// of whether a part of multiple results in a receipt were dropped because of
// the count limit.
// Also note that the inclusionProven position list might be longer than the
// number of added results in case a log satisfies the matchSpecified but not
// the matchLength criteria.
func (tqp *tableQueryProof) getProvenEntries(query *FilterQuery, results []*types.Log, reqParentBlockHash common.Hash) ([]*types.Log, []logindex.IndexPosition, []bool, common.Hash, error) {
	type txPosition struct {
		blockNumber uint64
		txIndex     uint32
	}
	type txInfo struct {
		txHash             common.Hash
		cumulativeLogIndex uint32
	}
	provenBlockEntries := make(map[uint64]common.Hash)
	provenTxEntries := make(map[txPosition]txInfo)
	var (
		inclusionProven  []logindex.IndexPosition
		validResult      []bool
		reqLastBlockHash common.Hash
	)
	//fmt.Println("getProvenEntries", tqp.FirstBlock, tqp.TableSize)
loop:
	for _, entry := range tqp.entries {
		switch entry.EntryType {
		case logindex.IeBlock:
			//fmt.Println(" block entry", entry.BlockNumber, common.Hash(entry.value))
			provenBlockEntries[entry.BlockNumber] = entry.Value
		case logindex.IeTransaction:
			provenTxEntries[txPosition{blockNumber: entry.BlockNumber, txIndex: entry.TxIndex}] = txInfo{txHash: entry.Value, cumulativeLogIndex: entry.LogIndex}
		default:
			break loop // block/tx entries come first in the sorted list
		}
	}
	if reqParentBlockHash != (common.Hash{}) {
		//fmt.Println(" required parent block entry", tqp.FirstBlock-1, common.Hash(reqParentBlockHash))
		if pbh, ok := provenBlockEntries[tqp.FirstBlock-1]; !ok {
			return nil, nil, nil, common.Hash{}, errors.New("required proven parent block hash is missing")
		} else if pbh != reqParentBlockHash {
			return nil, nil, nil, common.Hash{}, errors.New("required proven parent block hash does not match")
		}
	}
	var lastNumber uint64
	for i, br := range tqp.BlockResults {
		number := br.Header.Number.Uint64()
		if number < tqp.FirstBlock || number >= tqp.FirstBlock+tqp.TableSize || number < query.FirstBlock || number > query.LastBlock {
			return nil, nil, nil, common.Hash{}, errors.New("invalid block proof")
		}
		if i > 0 && number <= lastNumber {
			return nil, nil, nil, common.Hash{}, errors.New("invalid block proof order")
		}
		lastNumber = number
		blockHash := br.Header.Hash()
		if number < tqp.FirstBlock+tqp.TableSize-1 {
			if hash, ok := provenBlockEntries[number]; !ok {
				return nil, nil, nil, common.Hash{}, errors.New("block entry missing")
			} else if hash != blockHash {
				return nil, nil, nil, common.Hash{}, errors.New("block hash mismatch")
			}
		} else {
			reqLastBlockHash = blockHash
		}
		br.proofMap = makeProofMap(br.ReceiptsProof)
		//fmt.Println("::: makeProofMap", number, len(br.ReceiptsProof), len(br.proofMap))
		for _, txIndex := range br.ProvenReceipts {
			txInfo, ok := provenTxEntries[txPosition{blockNumber: number, txIndex: txIndex}]
			if !ok {
				return nil, nil, nil, common.Hash{}, errors.New("transaction entry missing")
			}
			receipt, err := br.getProvenReceipt(txIndex)
			//fmt.Println("::: getProvenReceipt", number, txIndex, err)
			if err != nil {
				return nil, nil, nil, common.Hash{}, err
			}
			for i, log := range receipt.Logs {
				if query.matchSpecified(log) {
					inclusionProven = append(inclusionProven, logindex.IndexPosition{BlockNumber: number, TxIndex: txIndex, LogIndex: uint32(i)})
					valid := query.matchLength(log)
					validResult = append(validResult, valid)
					if valid {
						rlog := &types.Log{
							Address:        log.Address,
							Topics:         log.Topics,
							Data:           log.Data,
							BlockNumber:    number,
							TxHash:         txInfo.txHash,
							BlockHash:      blockHash,
							BlockTimestamp: br.Header.Time,
							Index:          uint(txInfo.cumulativeLogIndex) + uint(i),
						}
						results = append(results, rlog)
					}
				}
			}
		}
	}
	return results, inclusionProven, validResult, reqLastBlockHash, nil
}

func (tqp *tableQueryProof) calculateTableRoot() (merkle.Value, error) {
	if len(tqp.ProvenEntries) != len(tqp.EntryIndices) {
		return merkle.Value{}, errors.New("entry count and entry index count mismatch")
	}
	treeHeight := 64 - bits.LeadingZeros64(max(tqp.EntryCount, 1)-1)
	leafOffset := uint64(1) << treeHeight
	hashes := make(map[uint64]merkle.Value)
	lastIndex := uint64(math.MaxUint64)

	var proofHashPtr int
	loadProofHashes := func(a, b uint64) error {
		return iterateProofIndices(treeHeight, a, b, func(gti uint64) error {
			if proofHashPtr >= len(tqp.ProofHashes) {
				return errors.New("not enough proof hashes")
			}
			hashes[gti] = tqp.ProofHashes[proofHashPtr]
			//fmt.Println("node", gti, "hash", common.Hash(hashes[gti]))
			proofHashPtr++
			return nil
		})
	}

	for i, entryIndex := range tqp.EntryIndices {
		if (i > 0 && entryIndex <= lastIndex) || entryIndex >= tqp.EntryCount {
			return merkle.Value{}, errors.New("invalid entry index")
		}
		hashes[leafOffset+entryIndex] = tqp.entries[i].Hash()
		//fmt.Println("entry", entryIndex, "hash", common.Hash(hashes[leafOffset+entryIndex]))
		if err := loadProofHashes(lastIndex, entryIndex); err != nil {
			return merkle.Value{}, err
		}
		lastIndex = entryIndex
	}
	if err := loadProofHashes(lastIndex, uint64(math.MaxUint64)); err != nil {
		return merkle.Value{}, err
	}
	if proofHashPtr != len(tqp.ProofHashes) {
		return merkle.Value{}, errors.New("too many proof hashes")
	}

	var getHash func(gti uint64) merkle.Value
	getHash = func(gti uint64) (result merkle.Value) {
		if hash, ok := hashes[gti]; ok {
			return hash
		}
		if gti >= uint64(1)<<63 {
			panic("cannot reconstruct table root")
		}
		left, right := getHash(gti*2), getHash(gti*2+1)
		hasher := sha256.New()
		hasher.Write(left[:])
		hasher.Write(right[:])
		hasher.Sum(result[:0])
		return
	}
	var tableRoot, entryCountNode merkle.Value
	binary.LittleEndian.PutUint64(entryCountNode[:8], tqp.EntryCount)
	treeRoot := getHash(1)
	//fmt.Println("tree root", common.Hash(treeRoot))
	hasher := sha256.New()
	hasher.Write(treeRoot[:])
	hasher.Write(entryCountNode[:])
	hasher.Sum(tableRoot[:0])
	return tableRoot, nil
}

func (tqp *tableQueryProof) getPotentialMatches(query *FilterQuery, begin, end logindex.IndexPosition) indexPositionSet {
	matchAll := make([]indexPositionSet, 0, len(query.Topics)+1)
	addressMatch := make([]indexPositionSet, len(query.Addresses))
	for i, address := range query.Addresses {
		var addressValue [32]byte
		copy(addressValue[12:], address.Bytes())
		addressMatch[i] = tqp.getValueMatches(logindex.IndexValue{EntryType: logindex.IeAddress, Value: addressValue}, begin, end)
	}
	if len(addressMatch) > 0 {
		matchAll = append(matchAll, ipsUnion(addressMatch))
	}
	for j, topics := range query.Topics {
		topicMatch := make([]indexPositionSet, len(topics))
		for i, topic := range topics {
			topicMatch[i] = tqp.getValueMatches(logindex.IndexValue{EntryType: logindex.IeTopic0 + uint32(j), Value: topic}, begin, end)
		}
		if len(topicMatch) > 0 {
			matchAll = append(matchAll, ipsUnion(topicMatch))
		}
	}
	return ipsIntersection(matchAll)
}

func (tqp *tableQueryProof) excludeBefore(i int) bool {
	if len(tqp.EntryIndices) == 0 {
		return false
	}
	if i == 0 {
		return tqp.EntryIndices[0] == 0
	}
	if i == len(tqp.EntryIndices) {
		return tqp.EntryIndices[len(tqp.EntryIndices)-1] == tqp.EntryCount-1
	}
	return tqp.EntryIndices[i] == tqp.EntryIndices[i-1]+1
}

// assumes entries, entryCount present
func (tqp *tableQueryProof) getValueMatches(value logindex.IndexValue, begin, end logindex.IndexPosition) indexPositionSet {
	//fmt.Println("getValueMatches value", value, "begin", begin, "end", end, "entries", len(tqp.EntryIndices))
	/*for i, idx := range tqp.EntryIndices {
		fmt.Println(" ", idx, tqp.entries[i])
	}*/
	boundary := logindex.IndexEntry{
		IndexValue:    value,
		IndexPosition: begin,
	}
	firstInRange, _ := tqp.entries.Find(&boundary)
	boundary.IndexPosition = end
	afterLastInRange, lastFound := tqp.entries.Find(&boundary)
	if lastFound {
		afterLastInRange++
	}
	//fmt.Println("firstInRange", firstInRange, "afterLastInRange", afterLastInRange, "lastFound", lastFound)
	var ips indexPositionSet
	if tqp.excludeBefore(firstInRange) {
		if firstInRange < afterLastInRange {
			ips = append(ips, indexPositionBoundary{p: tqp.entries[firstInRange].IndexPosition, d: 1})
		}
	} else {
		ips = append(ips, indexPositionBoundary{p: begin, d: 1})
	}
	for i := firstInRange + 1; i < afterLastInRange; i++ {
		if tqp.excludeBefore(i) {
			ips = append(ips,
				indexPositionBoundary{p: tqp.entries[i-1].IndexPosition, d: -1},
				indexPositionBoundary{p: tqp.entries[i].IndexPosition, d: 1})
		}
	}
	if tqp.excludeBefore(afterLastInRange) {
		if firstInRange < afterLastInRange {
			ips = append(ips, indexPositionBoundary{p: tqp.entries[afterLastInRange-1].IndexPosition, d: -1})
		}
	} else {
		ips = append(ips, indexPositionBoundary{p: end, d: -1})
	}
	//fmt.Println("matches", ips)
	return ips
}

// all boundaries are inclusive
type indexPositionBoundary struct {
	p logindex.IndexPosition
	d int
}

type indexPositionSet []indexPositionBoundary

func (ips indexPositionSet) iter() iter.Seq[logindex.IndexPosition] {
	return func(yield func(logindex.IndexPosition) bool) {
		var (
			last logindex.IndexPosition
			sum  int
		)
		for _, bound := range ips {
			if sum > 0 {
				for last != bound.p {
					if !yield(last) {
						return
					}
					last.LogIndex++
				}
			} else {
				last = bound.p
			}
			sum += bound.d
			if sum == 0 && bound.d < 0 && !yield(last) {
				return
			}
		}
	}
}

func (ips indexPositionSet) filter(threshold int) indexPositionSet {
	sort.Slice(ips, func(i, j int) bool {
		switch ips[i].p.Compare(&ips[j].p) {
		case -1:
			return true
		case 1:
			return false
		default:
			return ips[i].d > ips[j].d
		}
	})
	var sum, j int
	for _, pb := range ips {
		cmpBefore := sum >= threshold
		sum += pb.d
		if sum < 0 {
			panic("invalid indexPositionSet: sum < 0")
		}
		cmpAfter := sum >= threshold
		if cmpAfter != cmpBefore {
			ips[j].p = pb.p
			if cmpAfter {
				ips[j].d = 1
			} else {
				ips[j].d = -1
			}
			j++
		}
	}
	if sum != 0 {
		panic("invalid indexPositionSet: sum != 0")
	}
	return ips[:j]
}

func ipsMerge(sets []indexPositionSet) indexPositionSet {
	if len(sets) == 0 {
		return nil
	}
	if len(sets) == 1 {
		return sets[0]
	}
	var size, start int
	for _, ips := range sets {
		size += len(ips)
	}
	res := make(indexPositionSet, size)
	for _, ips := range sets {
		copy(res[start:start+len(ips)], ips)
		start += len(ips)
	}
	return res
}

func ipsUnion(sets []indexPositionSet) indexPositionSet {
	return ipsMerge(sets).filter(1)
}

func ipsIntersection(sets []indexPositionSet) indexPositionSet {
	return ipsMerge(sets).filter(len(sets))
}

func makeProofMap(storageProof [][]byte) map[common.Hash][]byte {
	proofMap := make(map[common.Hash][]byte)
	for _, node := range storageProof {
		hash := crypto.Keccak256Hash(node)
		//fmt.Println(" hash", hash, "node", node)
		proofMap[hash] = node
	}
	return proofMap
}

func (br *blockResults) getProvenReceipt(txIndex uint32) (*types.Receipt, error) {
	trie, err := trie.New(trie.TrieID(br.Header.ReceiptHash), br)
	if err != nil {
		return nil, err
	}
	key := rlp.AppendUint64(nil, uint64(txIndex))
	//fmt.Println("receipt trie get key:", key)
	receiptEnc, err := trie.Get(key)
	if err != nil {
		return nil, err
	}
	//fmt.Println("receiptEnc:", receiptEnc)
	receipt := new(types.Receipt)
	//if err := rlp.DecodeBytes(receiptEnc, receipt); err != nil {
	if err := receipt.UnmarshalBinary(receiptEnc); err != nil {
		return nil, err
	}
	return receipt, nil
}

// implements database.NodeDatabase
func (br *blockResults) NodeReader(stateRoot common.Hash) (database.NodeReader, error) {
	return br, nil
}

// implements database.NodeReader
func (br *blockResults) Node(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	if node, ok := br.proofMap[hash]; ok {
		//fmt.Println("receipt trie node found:", hash, "node data:", node)
		return node, nil
	}
	//fmt.Println("receipt trie node not found:", hash)
	return nil, errors.New("trie node missing from receipts proof")
}
