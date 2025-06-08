// Copyright 2025 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/filtermaps"
	"github.com/ethereum/go-ethereum/core/types"
)

func main() {
	ds := filtermaps.MakeTestDataset("empty blocks", 10001, func(block uint64) []*types.Log { return nil })
	ds.Run(&filtermaps.TestQuery{
		Description: "partial block range",
		Address:     []common.Address{common.Address{42}},
		FromBlock:   1000,
		ToBlock:     9000,
	}, "data1_query1")
	ds.Run(&filtermaps.TestQuery{
		Description: "full block range",
		Address:     []common.Address{common.Address{42}},
		FromBlock:   0,
		ToBlock:     10000,
	}, "data1_query2")

	ds = filtermaps.MakeTestDataset("address only, all different", 10001, func(block uint64) []*types.Log {
		logs := make([]*types.Log, 100)
		for i := range logs {
			logs[i] = &types.Log{Address: makeAddress(block, uint64(i))}
		}
		return logs
	})
	ds.Run(&filtermaps.TestQuery{
		Description: "single match",
		Address:     []common.Address{makeAddress(1234, 56)},
		FromBlock:   1,
		ToBlock:     9999,
	}, "data2_query1")
	ds.Run(&filtermaps.TestQuery{
		Description: "two matches",
		Address:     []common.Address{makeAddress(1234, 56), makeAddress(4321, 98)},
		FromBlock:   0,
		ToBlock:     10000,
	}, "data2_query2")

	ds = filtermaps.MakeTestDataset("address + 2 topics, all combinations", 10001, func(block uint64) []*types.Log {
		logs := make([]*types.Log, 100)
		for i := range logs {
			logs[i] = &types.Log{
				Address: makeAddress(block/100, 0),
				Topics:  []common.Hash{makeTopic(block%100, 0), makeTopic(uint64(i), 0)},
			}
		}
		return logs
	})
	ds.Run(&filtermaps.TestQuery{
		Description: "single match combination",
		Address:     []common.Address{makeAddress(11, 0)},
		Topics:      [][]common.Hash{{makeTopic(22, 0)}, {makeTopic(33, 0)}},
		FromBlock:   1,
		ToBlock:     9999, //TODO 10000 fails
	}, "data3_query1")
	ds.Run(&filtermaps.TestQuery{
		Description: "8 match combination",
		Address:     []common.Address{makeAddress(11, 0), makeAddress(12, 0)},
		Topics:      [][]common.Hash{{makeTopic(22, 0), makeTopic(23, 0)}, {makeTopic(33, 0), makeTopic(34, 0)}},
		FromBlock:   1,
		ToBlock:     9999, //TODO 10000 fails
	}, "data3_query2")

	ds = filtermaps.MakeTestDataset("topic2 always the same", 10001, func(block uint64) []*types.Log {
		logs := make([]*types.Log, 100)
		for i := range logs {
			logs[i] = &types.Log{
				Address: makeAddress(block, 0),
				Topics:  []common.Hash{makeTopic(uint64(i), 0), makeTopic(42, 0)},
			}
		}
		return logs
	})
	ds.Run(&filtermaps.TestQuery{
		Description: "full combination",
		Address:     []common.Address{makeAddress(5678, 0)},
		Topics:      [][]common.Hash{{makeTopic(77, 0)}, {makeTopic(42, 0)}},
		FromBlock:   1,
		ToBlock:     9999, //TODO 10000 fails
	}, "data4_query1")
	ds.Run(&filtermaps.TestQuery{
		Description: "topic2 not specified",
		Address:     []common.Address{makeAddress(5678, 0)},
		Topics:      [][]common.Hash{{makeTopic(77, 0)}},
		FromBlock:   1,
		ToBlock:     9999, //TODO 10000 fails
	}, "data4_query2")
	ds.Run(&filtermaps.TestQuery{
		Description: "only address specified",
		Address:     []common.Address{makeAddress(5678, 0)},
		FromBlock:   1,
		ToBlock:     9999, //TODO 10000 fails
	}, "data4_query3")

}

/*	ds.Run(&filtermaps.TestQuery{
		Description: "topic2 not specified",
		Address:     []common.Address{makeAddress(11, 0), makeAddress(12, 0)},
		Topics:      [][]common.Hash{{makeTopic(77, 0)}},
		FromBlock:   1,
		ToBlock:     9999, //TODO 10000 fails
	}, "data4_query2")
*/
//TODO fail

func makeAddress(a, b uint64) (addr common.Address) {
	binary.LittleEndian.PutUint64(addr[0:8], a)
	binary.LittleEndian.PutUint64(addr[8:16], b)
	return
}

func makeTopic(a, b uint64) (topic common.Hash) {
	binary.LittleEndian.PutUint64(topic[0:8], a)
	binary.LittleEndian.PutUint64(topic[8:16], b)
	return
}
