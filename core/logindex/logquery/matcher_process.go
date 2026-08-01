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
	"errors"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/logindex"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

var uint64msb = uint64(1) << 63

const (
	mpInactive    = iota // not scheduled for running because no more results will be needed according to estimates
	mpSuspended          // scheduled for running
	mpRunning            // passed to a worker thread, included in Matcher.processing
	mpFinished           // all results found or hit an error
	mpFinishedAll        // all previous processes have the same state; either found all results or enough to reach maxResults
)

type matcherProcess struct {
	logIndex              logIndex
	matcher               matcherInstance
	session               *matcherSession
	tableReader           *logindex.TableReader
	tableProver           *tableProver
	logicBuilder          *logicBuilder
	prev, next            *matcherProcess
	firstBlock, lastBlock uint64 // can be a subset of TableReader.blockRange after split; firstBlock > lastblock when doing reverse search

	// accessed by control thread only
	status  int
	pqIndex int

	// accessed only by worker thread while status == mpRunning
	positions                    []logindex.IndexPosition
	sectionNodes                 []logicNodeID
	completeUntil, completeValid int
	finished, matcherFinished    bool
	err                          error
	runTime                      time.Duration
	started                      mclock.AbsTime

	// accessed by block data delivery thread
	blockDataLock  sync.Mutex
	deliverCh      chan struct{}
	blockInResults map[uint64]int
	allMatches     []*types.Log // including nil at invalid matches where len(log.Topics) < len(query.Topics)
	validMatches   int          // number of valid matches
	blockProofs    map[uint64]*blockProof
	deliveryErr    error

	// atomic flags accessed by both threads during processing
	estimatedResults  uint64 // set by worker thread; MSB is "can split" flag
	cumulativeResults uint64 // set by control thread; MSB is "suspend now" flag

	testRegisterHook func(*matcherProcess)
	testHook         chan bool
}

func newMatcherProcess(
	logIndex logIndex,
	matcher matcherInstance,
	session *matcherSession,
	tableReader *logindex.TableReader,
	firstBlock, lastBlock uint64,
) *matcherProcess {
	return &matcherProcess{
		logIndex:       logIndex,
		matcher:        matcher,
		session:        session,
		tableReader:    tableReader,
		firstBlock:     firstBlock,
		lastBlock:      lastBlock,
		blockInResults: make(map[uint64]int),
		blockProofs:    make(map[uint64]*blockProof),
		deliverCh:      make(chan struct{}, 1),
	}
}

func (mp *matcherProcess) setProver(tableProver *tableProver, logicBuilder *logicBuilder) {
	mp.tableProver = tableProver
	mp.logicBuilder = logicBuilder
}

func (mp *matcherProcess) run() {
	if mp.testRegisterHook != nil {
		mp.testRegisterHook(mp)
	}
	mp.started = mclock.Now()
	defer func() {
		mp.runTime += time.Duration(mclock.Now() - mp.started)
		if mp.testHook != nil {
			close(mp.testHook)
		}
	}()

	//fmt.Println("matcherProcess", mp.blockRange, "started")
	//defer fmt.Println("matcherProcess", mp.blockRange, "stopped")

	for !mp.finished {
		if !mp.matcherFinished && len(mp.positions) < mp.completeUntil+maxIncompleteResults {
			//fmt.Println(" mp.matcher.next()")
			if mp.testHook != nil {
				mp.testHook <- true
			}
			pos, node, err := mp.matcher.next()
			if err != nil {
				//fmt.Println("matcherProcess", mp.blockRange, "error (next)", err)
				mp.finished, mp.err = true, err
				return
			}
			mp.sectionNodes = append(mp.sectionNodes, node)
			if pos == nil {
				//fmt.Println("matcherProcess", mp.blockRange, "finished")
				mp.matcherFinished = true
			} else {
				//fmt.Println("matcherProcess", mp.blockRange, "found", *pos)
				mp.positions = append(mp.positions, *pos)
				//fmt.Println(" mp.matcher.advance(nil)")
				if mp.testHook != nil {
					mp.testHook <- true
				}
				if err := mp.matcher.advance(nil); err != nil {
					//fmt.Println("matcherProcess", mp.blockRange, "error (advance)", err)
					mp.finished, mp.err = true, err
					return
				}
			}
		} else {
			//fmt.Println(" <-mp.deliverCh")
			if mp.testHook != nil {
				mp.testHook <- false
			}
			select {
			case <-mp.deliverCh:
			case <-mp.session.ctx.Done():
				return
			}
		}
		mp.updateEstimatedResults(true)
		cumulativeResults, suspendNow := mp.getCumulativeResults()
		mp.blockDataLock.Lock()
		var requestBlocks []uint64
		if mp.deliveryErr != nil {
			mp.finished, mp.err = true, mp.deliveryErr
		} else {
			for len(mp.positions) > len(mp.allMatches) {
				pos := mp.positions[len(mp.allMatches)]
				if _, ok := mp.blockInResults[pos.BlockNumber]; !ok {
					mp.blockInResults[pos.BlockNumber] = len(mp.allMatches)
					//fmt.Println("*** getBlockData", pos.BlockNumber)
					//fmt.Println(" len(mp.positions)", len(mp.positions), "len(mp.logs)", len(mp.logs), "blockInResults[", pos.BlockNumber, "] = ", len(mp.logs))
					requestBlocks = append(requestBlocks, pos.BlockNumber)
				}
				mp.allMatches = append(mp.allMatches, nil)
			}
			for mp.completeUntil < len(mp.allMatches) && mp.allMatches[mp.completeUntil] != nil {
				if len(mp.allMatches[mp.completeUntil].Topics) >= mp.session.minTopicCount {
					mp.completeValid++
				}
				mp.completeUntil++
			}
			if mp.matcherFinished && mp.completeUntil == len(mp.allMatches) {
				mp.finished = true
			}
		}
		mp.blockDataLock.Unlock()
		for _, blockNumber := range requestBlocks {
			// start requests outside blockDataLock to avoid wrong locking order
			mp.logIndex.RequestBlock(mp.session.refBlockHash, blockNumber, mp.deliverBlockData)
		}
		if suspendNow || cumulativeResults+uint64(mp.completeValid) >= uint64(mp.session.maxResults) {
			return
		}
	}
}

func (mp *matcherProcess) deliverBlockData(req logindex.BlockRequest, header *types.Header, body *types.Body, receipts types.Receipts) {
	//fmt.Println("*** deliverBlockData", req.Number)
	mp.blockDataLock.Lock()
	defer mp.blockDataLock.Unlock()

	firstInResults, ok := mp.blockInResults[req.Number]
	if ok {
		delete(mp.blockInResults, req.Number)
	} else {
		return
	}
	if header == nil || body == nil || receipts == nil {
		mp.deliveryErr = errors.New("block data not delivered")
		return
	}
	select {
	case mp.deliverCh <- struct{}{}:
	default:
	}

	var blockProof *blockProof
	if mp.tableProver != nil {
		blockProof = mp.blockProofs[req.Number]
		if blockProof == nil {
			if req.Number == mp.tableReader.BlockRange().Last() {
				blockProof = newBlockProof(header, math.MaxUint64)
			} else {
				blockEntry := logindex.IndexEntry{
					IndexValue: logindex.IndexValue{
						EntryType: logindex.IeBlock,
						Value:     ([32]byte)(header.Hash()),
					},
					IndexPosition: logindex.IndexPosition{
						BlockNumber: req.Number,
					},
				}
				blockEntryIndex, found, err := mp.tableReader.SeekEntry(&blockEntry)
				if err != nil {
					mp.deliveryErr = err
					return
				}
				if !found {
					mp.deliveryErr = errors.New("could not find block entry index")
					return
				}
				blockProof = newBlockProof(header, blockEntryIndex)
			}
			mp.blockProofs[req.Number] = blockProof
		}
	}

loop:
	for ; firstInResults < len(mp.allMatches); firstInResults++ {
		pos := mp.positions[firstInResults]
		if pos.BlockNumber != req.Number || uint32(len(receipts)) <= pos.TxIndex || uint32(len(receipts[pos.TxIndex].Logs)) <= pos.LogIndex {
			break loop
		}
		var logOffset uint
		for i := range pos.TxIndex {
			logOffset += uint(len(receipts[i].Logs)) //TODO different position encoding?
		}
		txHash := body.Transactions[pos.TxIndex].Hash()
		l := receipts[pos.TxIndex].Logs[pos.LogIndex]
		if len(l.Topics) >= mp.session.minTopicCount {
			mp.validMatches++
		}
		mp.allMatches[firstInResults] = &types.Log{
			Address:        l.Address,
			Topics:         l.Topics,
			Data:           l.Data,
			BlockNumber:    pos.BlockNumber,
			TxHash:         txHash,
			TxIndex:        uint(pos.TxIndex),
			BlockHash:      header.Hash(),
			BlockTimestamp: header.Time,
			Index:          logOffset + uint(pos.LogIndex),
		}
		if blockProof != nil {
			txEntry := logindex.IndexEntry{
				IndexValue: logindex.IndexValue{
					EntryType: logindex.IeTransaction,
					Value:     ([32]byte)(txHash),
				},
				IndexPosition: logindex.IndexPosition{
					BlockNumber: pos.BlockNumber,
					TxIndex:     pos.TxIndex,
					LogIndex:    uint32(logOffset),
				},
			}
			txEntryIndex, found, err := mp.tableReader.SeekEntry(&txEntry)
			if err != nil {
				mp.deliveryErr = err
				return
			}
			if !found {
				mp.deliveryErr = errors.New("could not find transaction entry index")
				return
			}
			blockProof.addMatchingTx(pos.TxIndex, txEntryIndex)
		}
	}
	if blockProof != nil {
		// Compute effective blob gas price.
		var blobGasPrice *big.Int
		if header.ExcessBlobGas != nil {
			blobGasPrice = eip4844.CalcBlobFee(params.MainnetChainConfig, header) //TODO chain config
		}
		if err := receipts.DeriveFields(params.MainnetChainConfig, header.Hash(), header.Number.Uint64(), header.Time, header.BaseFee, blobGasPrice, body.Transactions); err != nil { //TODO chain config
			mp.deliveryErr = err
			return
		}
		blockProof.createProof(receipts)
	}
}

// called by worker thread
func (mp *matcherProcess) updateEstimatedResults(running bool) (uint64, bool) {
	var (
		estimatedResults uint64
		canSplit         bool
	)
	if len(mp.positions) > 0 {
		done, _, remaining := mp.getProgress()
		ratio := float64(remaining) / float64(done) // remaining to done ratio; done >= 1
		runTime := mp.runTime
		if running {
			runTime += time.Duration(max(0, mclock.Now()-mp.started))
		}
		remainingResults := uint64(float64(mp.validMatches) * ratio)
		estimatedResults = uint64(mp.validMatches) + remainingResults
		if runTime >= splitAfter {
			remainingTime := time.Duration(float64(runTime) * ratio)
			canSplit = remainingTime >= splitThreshold
		}
	}
	if canSplit {
		atomic.StoreUint64(&mp.estimatedResults, estimatedResults+uint64msb)
	} else {
		atomic.StoreUint64(&mp.estimatedResults, estimatedResults)
	}
	return estimatedResults, canSplit
}

// estimated result count, "can split" flag
func (mp *matcherProcess) getEstimatedResults() (uint64, bool) {
	v := atomic.LoadUint64(&mp.estimatedResults)
	return v & (uint64msb - 1), (v & uint64msb) != 0
}

// split after returned block number; always called while not running
func (mp *matcherProcess) getSplitBlock() (uint64, bool) {
	done, lastBlock, remaining := mp.getProgress()
	s := float64(done) * float64(splitTarget) / float64(max(mp.runTime, 1))
	if s >= float64(remaining)/2 {
		return 0, false
	}
	splitAfter := uint64(s)
	if mp.session.reverse {
		return max(lastBlock, mp.lastBlock+splitAfter) - splitAfter, true
	} else {
		return min(lastBlock+splitAfter+1, mp.lastBlock), true
	}
}

func (mp *matcherProcess) getProgress() (done, lastBlock, remaining uint64) {
	if len(mp.positions) > 0 {
		lastBlock = mp.positions[len(mp.positions)-1].BlockNumber
	} else {
		lastBlock = mp.firstBlock
	}
	if mp.session.reverse {
		done, remaining = mp.firstBlock+1-lastBlock, lastBlock-mp.lastBlock
	} else {
		done, remaining = lastBlock+1-mp.firstBlock, mp.lastBlock-lastBlock
	}
	return
}

func (mp *matcherProcess) getCumulativeResults() (uint64, bool) {
	v := atomic.LoadUint64(&mp.estimatedResults)
	return v & (uint64msb - 1), (v & uint64msb) != 0
}

func (mp *matcherProcess) setCumulativeResults(cumulativeResults uint64, suspendNow bool) {
	if suspendNow {
		atomic.StoreUint64(&mp.cumulativeResults, cumulativeResults+uint64msb)
	} else {
		atomic.StoreUint64(&mp.cumulativeResults, cumulativeResults)
	}
}

func (mp *matcherProcess) split() (*matcherProcess, error) {
	splitAt, ok := mp.getSplitBlock()
	if !ok {
		return nil, nil
	}
	logicBuilder := mp.tableProver.optimizer.newBuilderInstance()
	mp2 := newMatcherProcess(mp.logIndex, mp.matcher.split(logicBuilder, splitAt), mp.session, mp.tableReader, mp.firstBlock, mp.lastBlock)
	mp2.setProver(mp.tableProver, logicBuilder)
	if mp.next != nil {
		mp.next.prev = mp2
	}
	mp.next = mp2
	if mp.session.last == mp {
		mp.session.last = mp2
	}
	if mp.firstBlock <= mp.lastBlock {
		mp.lastBlock = splitAt - 1
		mp2.firstBlock = splitAt
	} else {
		mp.lastBlock = splitAt
		mp2.firstBlock = splitAt - 1
	}
	return mp2, nil
}
