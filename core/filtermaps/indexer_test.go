// Copyright 2024 The go-ethereum Authors
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
	"crypto/sha256"
	"fmt"
	"math/big"
	"math/rand"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
)

var testParams = Params{
	logMapHeight:    2,
	logMapsPerEpoch: 4,
	logValuesPerMap: 4,
}

func TestIndexerSetHistory(t *testing.T) {
	ts := newTestSetup(t)
	ts.setHistory(0, false)
	ts.chain.addBlocks(1000, 5, 2, 4, false) // 50 log values per block
	ts.runUntilWait()
	ts.checkLvRange(50)
	ts.setHistory(100, false)
	ts.runUntil(func() bool {
		l := ts.lastRange.headLvPointer - ts.lastRange.tailLvPointer
		return l > 44000 && l < 45000
	})
	ts.setHistory(200, false)
	ts.runUntilWait()
	ts.checkLvRange(50)
	ts.setHistory(0, false)
	ts.runUntilWait()
	ts.checkLvRange(50)
}

func TestIndexerRandomSetHistory(t *testing.T) {
	ts := newTestSetup(t)
	ts.chain.addBlocks(100, 5, 2, 4, false) // 50 log values per block
	for i := 0; i < 3000; i++ {
		ts.setHistory(uint64(rand.Intn(1001)), false)
		ts.nextEvent()
		for rand.Intn(20) != 0 && ts.lastEvent != testHookWait {
			ts.nextEvent()
		}
		if ts.lastEvent == testHookWait {
			ts.checkLvRange(50)
		}
	}
	ts.setHistory(0, false)
	ts.runUntilWait()
	ts.checkLvRange(50)
}

func TestIndexerDbEquality(t *testing.T) {
	ts := newTestSetup(t)
	ts.setHistory(0, false)
	for i := 0; i < 10; i++ {
		ts.chain.addBlocks(100, 10, 3, 4, true)
		ts.runUntilWait()
	}
	hash1 := ts.fmDbHash()
	fmt.Println(hash1)
	ts.setHistory(500, false)
	ts.runUntilWait()
	hash2 := ts.fmDbHash()
	fmt.Println(hash2)
	ts.setHistory(0, false)
	ts.runUntilWait()
	hash3 := ts.fmDbHash()
	fmt.Println(hash3)
}

type testSetup struct {
	t         *testing.T
	fm        *FilterMaps
	db        ethdb.Database
	chain     *testChain
	params    Params
	eventCh   chan int
	resumeCh  chan struct{}
	lastEvent int
	lastRange filterMapsRange
}

func newTestSetup(t *testing.T) *testSetup {
	params := testParams
	params.deriveFields()
	return &testSetup{
		t:        t,
		chain:    newTestChain(),
		db:       rawdb.NewMemoryDatabase(),
		params:   params,
		eventCh:  make(chan int),
		resumeCh: make(chan struct{}),
	}
}

func (ts *testSetup) runUntil(stop func() bool) {
	for !stop() {
		ts.nextEvent()
		for ts.lastEvent == testHookWait {
			ts.t.Fatalf("Indexer in waiting state before runUntil condition is met")
		}
	}
}

func (ts *testSetup) runUntilWait() {
	for {
		ts.nextEvent()
		for ts.lastEvent != testHookWait {
			ts.nextEvent()
		}
		if ts.fm.getRange().headBlockHash == ts.chain.CurrentBlock().Hash() {
			return
		}
	}
}

func (ts *testSetup) checkLvRange(lvPerBlock uint64) {
	expBlockCount := uint64(len(ts.chain.canonical) - 1)
	if ts.fm.history != 0 && ts.fm.history < expBlockCount {
		expBlockCount = ts.fm.history
	}
	if ts.lastRange.headLvPointer-ts.lastRange.tailBlockLvPointer != expBlockCount*lvPerBlock {
		ts.t.Fatalf("Invalid number of log values (expected %d, got %d)", expBlockCount*lvPerBlock, ts.lastRange.headLvPointer-ts.lastRange.tailLvPointer)
	}
	if ts.lastRange.tailBlockLvPointer-ts.lastRange.tailLvPointer >= ts.params.valuesPerMap {
		ts.t.Fatalf("Invalid number of leftover tail log values (expected < %d, got %d)", ts.params.valuesPerMap, ts.lastRange.tailBlockLvPointer-ts.lastRange.tailLvPointer)
	}
}

func (ts *testSetup) setHistory(history uint64, noHistory bool) {
	if ts.fm != nil {
		ts.stopFm()
	}
	ts.fm = NewFilterMaps(ts.db, ts.chain, ts.params, history, noHistory)
	ts.fm.testHook = ts.testHook
	ts.fm.Start()
	ts.lastEvent = <-ts.eventCh
}

func (ts *testSetup) testHook(event int) {
	ts.eventCh <- event
	<-ts.resumeCh
}

func (ts *testSetup) nextEvent() {
	ts.resumeCh <- struct{}{}
	ts.lastEvent = <-ts.eventCh
	ts.lastRange = ts.fm.getRange()
}

func (ts *testSetup) stopFm() {
	close(ts.fm.closeCh)
	for {
		ts.nextEvent()
		if ts.lastEvent == testHookStop {
			break
		}
	}
	ts.resumeCh <- struct{}{}
	ts.fm.closeWg.Wait()
}

func (ts *testSetup) fmDbHash() common.Hash {
	hasher := sha256.New()
	it := ts.db.NewIterator(nil, nil)
	for it.Next() {
		hasher.Write(it.Key())
		hasher.Write(it.Value())
	}
	it.Release()
	var result common.Hash
	hasher.Sum(result[:0])
	return result
}

func (ts *testSetup) close() {
	ts.stopFm()
	ts.db.Close()
	ts.chain.db.Close()
}

type testChain struct {
	db            ethdb.Database
	lock          sync.RWMutex
	canonical     []common.Hash
	chainHeadFeed event.Feed
	blocks        map[common.Hash]*types.Block
	receipts      map[common.Hash]types.Receipts
}

func newTestChain() *testChain {
	return &testChain{
		blocks:   make(map[common.Hash]*types.Block),
		receipts: make(map[common.Hash]types.Receipts),
	}
}

func (tc *testChain) CurrentBlock() *types.Header {
	tc.lock.RLock()
	defer tc.lock.RUnlock()

	if len(tc.canonical) == 0 {
		return nil
	}
	return tc.blocks[tc.canonical[len(tc.canonical)-1]].Header()
}

func (tc *testChain) SubscribeChainEvent(ch chan<- core.ChainEvent) event.Subscription {
	return tc.chainHeadFeed.Subscribe(ch)
}

func (tc *testChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	tc.lock.RLock()
	defer tc.lock.RUnlock()

	return tc.blocks[hash].Header()
}

func (tc *testChain) GetCanonicalHash(number uint64) common.Hash {
	tc.lock.RLock()
	defer tc.lock.RUnlock()

	if uint64(len(tc.canonical)) <= number {
		return common.Hash{}
	}
	return tc.canonical[number]
}

func (tc *testChain) GetReceiptsByHash(hash common.Hash) types.Receipts {
	tc.lock.RLock()
	defer tc.lock.RUnlock()

	return tc.receipts[hash]
}

func (tc *testChain) addBlocks(count, maxTxPerBlock, maxLogsPerReceipt, maxTopicsPerLog int, random bool) {
	tc.lock.Lock()
	defer tc.lock.Unlock()

	blockGen := func(i int, gen *core.BlockGen) {
		var txCount int
		if random {
			txCount = rand.Intn(maxTxPerBlock + 1)
		} else {
			txCount = maxTxPerBlock
		}
		for k := txCount; k > 0; k-- {
			receipt := types.NewReceipt(nil, false, 0)
			var logCount int
			if random {
				logCount = rand.Intn(maxLogsPerReceipt + 1)
			} else {
				logCount = maxLogsPerReceipt
			}
			receipt.Logs = make([]*types.Log, logCount)
			for i := range receipt.Logs {
				log := &types.Log{}
				receipt.Logs[i] = log
				rand.Read(log.Address[:])
				var topicCount int
				if random {
					topicCount = rand.Intn(maxTopicsPerLog + 1)
				} else {
					topicCount = maxTopicsPerLog
				}
				log.Topics = make([]common.Hash, topicCount)
				for j := range log.Topics {
					rand.Read(log.Topics[j][:])
				}
			}
			gen.AddUncheckedReceipt(receipt)
			gen.AddUncheckedTx(types.NewTransaction(999, common.HexToAddress("0x999"), big.NewInt(999), 999, gen.BaseFee(), nil))
		}
	}

	var (
		blocks   []*types.Block
		receipts []types.Receipts
		engine   = ethash.NewFaker()
	)

	if len(tc.canonical) == 0 {
		gspec := &core.Genesis{
			Alloc:   types.GenesisAlloc{},
			BaseFee: big.NewInt(params.InitialBaseFee),
			Config:  params.TestChainConfig,
		}
		tc.db, blocks, receipts = core.GenerateChainWithGenesis(gspec, engine, count, blockGen)
		gblock := gspec.ToBlock()
		ghash := gblock.Hash()
		tc.canonical = []common.Hash{ghash}
		tc.blocks[ghash] = gblock
		tc.receipts[ghash] = types.Receipts{}
	} else {
		blocks, receipts = core.GenerateChain(params.TestChainConfig, tc.blocks[tc.canonical[len(tc.canonical)-1]], engine, tc.db, count, blockGen)
	}

	for i, block := range blocks {
		num, hash := int(block.NumberU64()), block.Hash()
		if len(tc.canonical) != num {
			panic(nil)
		}
		tc.canonical = append(tc.canonical, hash)
		tc.blocks[hash] = block
		if receipts[i] != nil {
			tc.receipts[hash] = receipts[i]
		} else {
			tc.receipts[hash] = types.Receipts{}
		}
	}
	tc.chainHeadFeed.Send(core.ChainEvent{Block: tc.blocks[tc.canonical[len(tc.canonical)-1]]})
}

func (tc *testChain) setHead(headNum int) {
	tc.lock.Lock()
	defer tc.lock.Unlock()

	tc.canonical = tc.canonical[:headNum+1]
	tc.chainHeadFeed.Send(core.ChainEvent{Block: tc.blocks[tc.canonical[len(tc.canonical)-1]]})
}

func (tc *testChain) getCanonicalChain() []common.Hash {
	tc.lock.RLock()
	defer tc.lock.RUnlock()

	cc := make([]common.Hash, len(tc.canonical))
	copy(cc, tc.canonical)
	return cc
}

// restore an earlier state of the chain
func (tc *testChain) setCanonicalChain(cc []common.Hash) {
	tc.lock.Lock()
	defer tc.lock.Unlock()

	tc.canonical = make([]common.Hash, len(cc))
	copy(tc.canonical, cc)
	tc.chainHeadFeed.Send(core.ChainEvent{Block: tc.blocks[tc.canonical[len(tc.canonical)-1]]})
}
