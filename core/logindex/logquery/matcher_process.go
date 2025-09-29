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
	"fmt"
	"math"
	"math/big"
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
	chainView             chainView
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
	positions      []logindex.IndexPosition //TODO is this required?
	sectionNodes   []logicNodeID
	allMatches     []*types.Log // including invalid matches where len(log.Topics) < session.minTopicCount
	validMatches   int          // number of valid matches
	blockProofs    map[uint64]*blockProof
	finished       bool
	err            error
	started        mclock.AbsTime
	runTime        time.Duration
	lastHeader     *types.Header
	lastBody       *types.Body
	lastReceipts   types.Receipts
	lastBlockProof *blockProof

	// atomic flags accessed by both threads during processing
	estimatedResults  uint64 // set by worker thread; MSB is "can split" flag
	cumulativeResults uint64 // set by control thread; MSB is "suspend now" flag

	testRegisterHook func(*matcherProcess)
	testHook         chan int
}

const (
	testWaitMatcher = iota // to be sent to testHook
	testWaitDeliver
)

func newMatcherProcess(
	logIndex logIndex,
	chainView chainView,
	matcher matcherInstance,
	session *matcherSession,
	tableReader *logindex.TableReader,
	firstBlock, lastBlock uint64,
) *matcherProcess {
	return &matcherProcess{
		logIndex:    logIndex,
		chainView:   chainView,
		matcher:     matcher,
		session:     session,
		tableReader: tableReader,
		firstBlock:  firstBlock,
		lastBlock:   lastBlock,
		blockProofs: make(map[uint64]*blockProof),
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

	//fmt.Println("matcherProcess", mp.firstBlock, mp.lastBlock, "started")
	//defer fmt.Println("matcherProcess", mp.firstBlock, mp.lastBlock, "stopped")

	for !mp.finished {
		//fmt.Println(" mp.matcher.next()")
		if mp.testHook != nil {
			mp.testHook <- testWaitMatcher
		}
		pos, node, err := mp.matcher.next()
		if err != nil {
			//fmt.Println("matcherProcess", mp.firstBlock, mp.lastBlock, "error (next)", err)
			mp.finished, mp.err = true, err
			//fmt.Println(mp.firstBlock, mp.lastBlock, "*** return next err", err)
			return
		}
		mp.sectionNodes = append(mp.sectionNodes, node)
		if pos == nil {
			//fmt.Println("matcherProcess", mp.firstBlock, mp.lastBlock, "finished")
			mp.finished = true
		} else {
			if err := mp.addMatch(pos); err != nil {
				fmt.Println("matcherProcess", mp.firstBlock, mp.lastBlock, "error (advance)", err)
				mp.finished, mp.err = true, err
				return
			}
			//fmt.Println(" mp.matcher.advance(nil)")
			if mp.testHook != nil {
				mp.testHook <- testWaitMatcher
			}
			if err := mp.matcher.advance(nil); err != nil {
				//fmt.Println("matcherProcess", mp.firstBlock, mp.lastBlock, "error (advance)", err)
				mp.finished, mp.err = true, err
				//fmt.Println(mp.firstBlock, mp.lastBlock, "*** return advance err", err)
				return
			}
		}
		mp.updateEstimatedResults()
		cumulativeResults, suspendNow := mp.getCumulativeResults()
		if suspendNow || cumulativeResults+uint64(mp.validMatches) >= uint64(mp.session.maxResults) {
			//fmt.Println(mp.firstBlock, mp.lastBlock, "*** return suspendNow", suspendNow, "cumulativeResults", cumulativeResults, "mp.validMatches", mp.validMatches, "mp.session.maxResults", mp.session.maxResults)
			return
		}
	}
	//fmt.Println(mp.firstBlock, mp.lastBlock, "*** return finished", mp.finished, "len(mp.allMatches)", len(mp.allMatches), "mp.validMatches", mp.validMatches)
}

func (mp *matcherProcess) updateLastMatchBlock(number uint64) error {
	if mp.lastHeader != nil {
		if mp.lastHeader.Number.Uint64() == number {
			return nil
		}
		if err := mp.finalizeLastMatchBlock(); err != nil {
			return err
		}
	}
	if mp.lastHeader = mp.chainView.Header(number); mp.lastHeader == nil {
		return fmt.Errorf("header of block #%d not found", number)
	}
	if mp.lastBody = mp.chainView.Body(number); mp.lastBody == nil {
		return fmt.Errorf("body of block #%d not found", number)
	}
	if mp.lastReceipts = mp.chainView.RawReceipts(number); mp.lastReceipts == nil {
		return fmt.Errorf("body of block #%d not found", number)
	}
	if mp.tableProver != nil {
		// result found in a block that is not proven yet; initialize new block proof
		if number == mp.tableReader.BlockRange().Last() {
			mp.lastBlockProof = newBlockProof(mp.lastHeader, math.MaxUint64)
		} else {
			blockEntry := logindex.IndexEntry{
				IndexValue: logindex.IndexValue{
					EntryType: logindex.IeBlock,
					Value:     ([32]byte)(mp.lastHeader.Hash()),
				},
				IndexPosition: logindex.IndexPosition{
					BlockNumber: number,
				},
			}
			blockEntryIndex, found, err := mp.tableReader.SeekEntry(&blockEntry)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("could not find block entry index")
			}
			mp.lastBlockProof = newBlockProof(mp.lastHeader, blockEntryIndex)
		}
	}
	return nil
}

func (mp *matcherProcess) finalizeLastMatchBlock() error {
	if mp.lastHeader == nil {
		return nil
	}
	header, body, receipts := mp.lastHeader, mp.lastBody, mp.lastReceipts
	number := header.Number.Uint64()
	var blobGasPrice *big.Int
	if header.ExcessBlobGas != nil {
		blobGasPrice = eip4844.CalcBlobFee(params.MainnetChainConfig, header) //TODO chain config
	}
	if err := receipts.DeriveFields(params.MainnetChainConfig, header.Hash(), header.Number.Uint64(), header.Time, header.BaseFee, blobGasPrice, body.Transactions); err != nil { //TODO chain config
		return err
	}
	if mp.lastBlockProof != nil {
		mp.lastBlockProof.createProof(receipts)
		mp.blockProofs[number] = mp.lastBlockProof
		mp.lastBlockProof = nil
	}
	mp.lastHeader, mp.lastBody, mp.lastReceipts = nil, nil, nil
	return nil
}

func (mp *matcherProcess) addMatch(pos *logindex.IndexPosition) error {
	if err := mp.updateLastMatchBlock(pos.BlockNumber); err != nil {
		return err
	}
	mp.positions = append(mp.positions, *pos)
	header, body, receipts := mp.lastHeader, mp.lastBody, mp.lastReceipts
	if pos.TxIndex >= uint32(len(body.Transactions)) {
		return fmt.Errorf("block #%d transaction #%d is out of range", pos.BlockNumber, pos.TxIndex)
	}
	txHash := body.Transactions[pos.TxIndex].Hash()
	if pos.TxIndex >= uint32(len(receipts)) {
		return fmt.Errorf("block #%d receipt #%d is out of range", pos.BlockNumber, pos.TxIndex)
	}
	receipt := receipts[pos.TxIndex]
	if pos.LogIndex >= uint32(len(receipt.Logs)) {
		return fmt.Errorf("block #%d receipt #%d log #%d is out of range", pos.BlockNumber, pos.TxIndex, pos.LogIndex)
	}
	log := receipt.Logs[pos.LogIndex]
	var logOffset uint
	for i := range pos.TxIndex {
		logOffset += uint(len(receipts[i].Logs)) //TODO different position encoding?
	}
	mp.allMatches = append(mp.allMatches, &types.Log{
		Address:        log.Address,
		Topics:         log.Topics,
		Data:           log.Data,
		BlockNumber:    pos.BlockNumber,
		TxHash:         txHash,
		TxIndex:        uint(pos.TxIndex),
		BlockHash:      header.Hash(),
		BlockTimestamp: header.Time,
		Index:          logOffset + uint(pos.LogIndex),
	})
	if len(log.Topics) >= mp.session.minTopicCount {
		mp.validMatches++
	}
	if mp.tableProver != nil {
		blockProof := mp.lastBlockProof
		if _, ok := blockProof.matchingTxs[pos.TxIndex]; !ok {
			// result found in a transaction that is not proven yet; initialize new transaction proof
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
				return err
			}
			if !found {
				return errors.New("could not find transaction entry index")
			}
			blockProof.matchingTxs[pos.TxIndex] = matchingTx{txEntryIndex: txEntryIndex}
		}
	}
	return nil
}

// called by worker thread
func (mp *matcherProcess) updateEstimatedResults() (uint64, bool) {
	if mp.finished {
		// store final result count
		atomic.StoreUint64(&mp.estimatedResults, uint64(mp.validMatches))
		return uint64(mp.validMatches), false
	}
	var (
		estimatedResults uint64
		canSplit         bool
	)
	if len(mp.positions) > 0 {
		done, _, remaining := mp.getProgress()
		ratio := float64(remaining) / float64(done) // remaining to done ratio; done >= 1
		runTime := mp.runTime
		runTime += time.Duration(max(0, mclock.Now()-mp.started))
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
	v := atomic.LoadUint64(&mp.cumulativeResults)
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
	//fmt.Println(mp.firstBlock, mp.lastBlock, "*** split at", splitAt)
	logicBuilder := mp.tableProver.optimizer.newBuilderInstance()
	mp2 := newMatcherProcess(mp.logIndex, mp.chainView, mp.matcher.split(logicBuilder, splitAt), mp.session, mp.tableReader, mp.firstBlock, mp.lastBlock)
	mp2.setProver(mp.tableProver, logicBuilder)
	mp2.prev, mp2.next = mp, mp.next
	if mp.next != nil {
		mp.next.prev = mp2
	}
	mp.next = mp2
	if mp.session.last == mp {
		mp.session.last = mp2
	}
	if mp.session.reverse {
		mp.lastBlock = splitAt
		mp2.firstBlock = splitAt - 1
	} else {
		mp.lastBlock = splitAt - 1
		mp2.firstBlock = splitAt
	}
	//fmt.Println(" mp:", mp.firstBlock, mp.lastBlock)
	//fmt.Println(" mp2:", mp2.firstBlock, mp2.lastBlock)
	return mp2, nil
}
