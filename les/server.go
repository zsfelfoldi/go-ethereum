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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/ethdb"
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
	pm.broadcastBlockLoop()

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
	MakeCHT(s.protocolManager.chainDb)
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

var (
	lastChtKey = []byte("LastCHT") // num (uint64 big endian) + hash
	chtPrefix = []byte("cht") // chtPrefix + num (uint64 big endian) + hash -> trie root hash
	chtFrequency = uint64(4096)
)

func getCht(db ethdb.Database, hash common.Hash, num uint64) common.Hash {
	var encNumber [8]byte
	binary.BigEndian.PutUint64(encNumber[:], num)
	data, _ := db.Get(append(append(chtPrefix, encNumber[:]...), hash.Bytes()...))
	return common.BytesToHash(data)
}

func (pm *ProtocolManager) makeCht() {
	headHash := core.GetHeadBlockHash(db)
	headNum := core.GetBlockNumber(db, headHash)
	x := (headNum+1)/chtFrequency
	if x < 2 {
		return
	}
	headCht := (x-1)*chtFrequency-1
	
	var lastHash common.Hash
	var lastNum uint64
	data, _ := db.Get(lastChtKey)
	if len(data) == 40 {
		lastNum = binary.BigEndian.Uint64(data[:8])
		lastHash = common.BytesToHash(data[8:])
	}
	if headCht <= lastNum {
		return
	}
	
	var t *trie.Trie
	if lastNum > 0 {
		var err error
		t, err = trie.New(getCht(db, lastHash, lastNum), db)
		if err != nil {
			lastNum = 0
		}
	}
	if lastNum == 0 {
		t, _ = trie.New(common.Hash{}, db)
	}

	ptr := lastNum + 1
	if lastNum == 0 {
		ptr = 0
	}
	for ; ptr <= headCht; ptr++ {
		hash := core.GetCanonicalHash(db, ptr)
		var encNumber [8]byte
		binary.BigEndian.PutUint64(encNumber[:], ptr)
		t.Update(encNumber[:], hash[:])
		if (ptr+1)%chtFrequency == 0 {
			root, err := t.Commit()
			if err != nil {
				return
			}
fmt.Println("CHT", ptr, root)
			db.Put(append(append(chtPrefix, encNumber[:]...), hash.Bytes()...), root.Bytes())
			var data [40]byte
			binary.BigEndian.PutUint64(data[:8], ptr)
			copy(data[8:], hash[:])
			db.Put(lastChtKey, data[:])
		}
	}
	
}

func (pm *ProtocolManager) broadcastBlockLoop() {
	sub := pm.eventMux.Subscribe(	core.ChainHeadEvent{})
	go func() {
		for {
			select {
			case ev := <-sub.Chan():
				MakeCHT(pm.chainDb)
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
			case <-pm.quitSync:
				sub.Unsubscribe()
				return
			}
		}
	}()
}
