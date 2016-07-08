// Copyright 2015 The go-ethereum Authors
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

// Package les implements the Light Ethereum Subprotocol.
package les

import (
"fmt"
	"encoding/binary"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/les/flowcontrol"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

type LesServer struct {
	protocolManager *ProtocolManager
	fcManager       *flowcontrol.ClientManager // nil if our node is client only
	fcCostStats     *requestCostStats
	defParams       *flowcontrol.ServerParams
}

func NewLesServer(eth *eth.FullNodeService, config *eth.Config) (*LesServer, error) {
	pm, err := NewProtocolManager(config.ChainConfig, false, config.NetworkId, eth.EventMux(), eth.Pow(), eth.BlockChain(), eth.TxPool(), eth.ChainDb(), nil, nil)
	if err != nil {
		return nil, err
	}
	pm.blockLoop()

	srv := &LesServer{protocolManager: pm}
	pm.server = srv

	srv.defParams = &flowcontrol.ServerParams{
		BufLimit:    300000000,
		MinRecharge: 50000,
	}
	srv.fcManager = flowcontrol.NewClientManager(uint64(config.LightServ), 10, 1000000000)
	srv.fcCostStats = newCostStats(eth.ChainDb())
	return srv, nil
}

func (s *LesServer) Protocols() []p2p.Protocol {
	return s.protocolManager.SubProtocols
}

func (s *LesServer) Start() {
	s.protocolManager.Start()
}

func (s *LesServer) Stop() {
	s.fcCostStats.store()
	s.fcManager.Stop()
	go func() {
		<-s.protocolManager.noMorePeers
	}()
	s.protocolManager.Stop()
}

type requestCosts struct {
	baseCost, reqCost uint64
}

type requestCostTable map[uint64]*requestCosts

type RequestCostList []struct {
	MsgCode, BaseCost, ReqCost uint64
}

func (list RequestCostList) decode() requestCostTable {
	table := make(requestCostTable)
	for _, e := range list {
		table[e.MsgCode] = &requestCosts{
			baseCost: e.BaseCost,
			reqCost:  e.ReqCost,
		}
	}
	return table
}

func (table requestCostTable) encode() RequestCostList {
	list := make(RequestCostList, len(table))
	for idx, code := range reqList {
		list[idx].MsgCode = code
		list[idx].BaseCost = table[code].baseCost
		list[idx].ReqCost = table[code].reqCost
	}
	return list
}

type requestCostStats struct {
	lock     sync.RWMutex
	db       ethdb.Database
	avg      requestCostTable
	baseCost uint64
}

var rcStatsKey = []byte("requestCostStats")

func newCostStats(db ethdb.Database) *requestCostStats {
	table := make(requestCostTable)
	for _, code := range reqList {
		table[code] = &requestCosts{0, 100000}
	}

	/*	if db != nil {
		var cl RequestCostList
		data, err := db.Get(rcStatsKey)
		if err == nil {
			err = rlp.DecodeBytes(data, &cl)
		}
		if err == nil {
			t := cl.decode()
			for code, entry := range t {
				table[code] = entry
			}
		}
	}*/

	return &requestCostStats{
		db:       db,
		avg:      table,
		baseCost: 100000,
	}
}

func (s *requestCostStats) store() {
	s.lock.Lock()
	defer s.lock.Unlock()

	list := s.avg.encode()
	if data, err := rlp.EncodeToBytes(list); err == nil {
		s.db.Put(rcStatsKey, data)
	}
}

func (s *requestCostStats) getCurrentList() RequestCostList {
	s.lock.Lock()
	defer s.lock.Unlock()

	list := make(RequestCostList, len(s.avg))
	for idx, code := range reqList {
		list[idx].MsgCode = code
		list[idx].BaseCost = s.baseCost
		list[idx].ReqCost = s.avg[code].reqCost * 2
	}
	return list
}

func (s *requestCostStats) update(msgCode, reqCnt, cost uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()

	c, ok := s.avg[msgCode]
	if !ok || reqCnt == 0 {
		return
	}
	cost = cost / reqCnt
	if cost > c.reqCost {
		c.reqCost += (cost - c.reqCost) / 10
	} else {
		c.reqCost -= (c.reqCost - cost) / 100
	}
}

func (pm *ProtocolManager) blockLoop() {
	pm.wg.Add(1)
	sub := pm.eventMux.Subscribe(	core.ChainHeadEvent{})
	newCht := make(chan struct{}, 10)
	newCht <- struct{}{}
	go func() {
		for {
			select {
			case ev := <-sub.Chan():
				peers := pm.peers.AllPeers()
				if len(peers) > 0 {
					header := ev.Data.(core.ChainHeadEvent).Block.Header()
					hash := header.Hash()
					number := header.Number.Uint64()
					td := core.GetTd(pm.chainDb, hash, number)
//fmt.Println("BROADCAST", number, hash, td)
					announce := newBlockHashesData{{Hash: hash, Number: number, Td: td}}
					for _, p := range peers {
						go p.SendNewBlockHashes(announce)
					}
				}
				newCht <- struct{}{}
			case <-newCht:
				go func() {
					if makeCht(pm.chainDb) {
						time.Sleep(time.Millisecond * 10)
						newCht <- struct{}{}
					}
				}()
			case <-pm.quitSync:
				sub.Unsubscribe()
				pm.wg.Done()
				return
			}
		}
	}()
}

var (
	lastChtKey = []byte("LastChtNumber") // chtNum (uint64 big endian)
	chtPrefix = []byte("cht") // chtPrefix + chtNum (uint64 big endian) -> trie root hash
	chtConfirmations = light.ChtFrequency / 2
)

func getChtRoot(db ethdb.Database, num uint64) common.Hash {
	var encNumber [8]byte
	binary.BigEndian.PutUint64(encNumber[:], num)
	data, _ := db.Get(append(chtPrefix, encNumber[:]...))
	return common.BytesToHash(data)
}

func storeChtRoot(db ethdb.Database, num uint64, root common.Hash) {
	var encNumber [8]byte
	binary.BigEndian.PutUint64(encNumber[:], num)
	db.Put(append(chtPrefix, encNumber[:]...), root[:])
}

func makeCht(db ethdb.Database) bool {
	headHash := core.GetHeadBlockHash(db)
	headNum := core.GetBlockNumber(db, headHash)

	var newChtNum uint64
	if headNum > chtConfirmations {
		newChtNum = (headNum-chtConfirmations)/light.ChtFrequency
	}
	
	var lastChtNum uint64
	data, _ := db.Get(lastChtKey)
	if len(data) == 8 {
		lastChtNum = binary.BigEndian.Uint64(data[:])
	}
	if newChtNum <= lastChtNum {
		return false
	}
	
	var t *trie.Trie
	if lastChtNum > 0 {
		var err error
		t, err = trie.New(getChtRoot(db, lastChtNum), db)
		if err != nil {
			lastChtNum = 0
		}
	}
	if lastChtNum == 0 {
		t, _ = trie.New(common.Hash{}, db)
	}

	for num := lastChtNum*light.ChtFrequency; num < (lastChtNum+1)*light.ChtFrequency; num++ {
		hash := core.GetCanonicalHash(db, num)
		if hash == (common.Hash{}) {
			panic("Canonical hash not found")
		}
		td := core.GetTd(db, hash, num)
		if td == nil {
			panic("TD not found")
		}
		var encNumber [8]byte
		binary.BigEndian.PutUint64(encNumber[:], num)
		var node light.ChtNode
		node.Hash = hash
		node.Td = td
		data, _ := rlp.EncodeToBytes(node)
		t.Update(encNumber[:], data)
	}

	root, err := t.Commit()
	if err != nil {
		lastChtNum = 0
	} else {
		lastChtNum++
fmt.Printf("CHT %d %064x\n", lastChtNum, root)
		storeChtRoot(db, lastChtNum, root)
		var data [8]byte
		binary.BigEndian.PutUint64(data[:], lastChtNum)
		db.Put(lastChtKey, data[:])
	}
	
	return newChtNum > lastChtNum
}
