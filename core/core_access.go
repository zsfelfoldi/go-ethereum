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

package core

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/access"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/rlp"
)

type BlockAccess struct {
	db        common.Database
	blockHash common.Hash
	rlp		  []byte
}

func (self *BlockAccess) Request(peer *access.Peer) error {
	return peer.GetBlockBodies([]common.Hash{self.blockHash})
}

func (self *BlockAccess) Valid(msg *access.Msg) bool {
	if msg.MsgType != access.MsgBlockBodies {
		return false
	}
	blocks := msg.Obj.([]*types.Block)
	if len(blocks) != 1 {
		return false
	}
	data, err := rlp.EncodeToBytes(blocks[0])
	if err != nil {
		return false
	}
	hash := crypto.Sha3Hash(data)
	valid := bytes.Compare(self.blockHash[:], hash[:]) == 0
	if valid {
		self.rlp = data
	}
	return valid
}

func (self *BlockAccess) DbGet() bool {
	self.rlp, _ = self.db.Get(append(append(blockPrefix, self.blockHash[:]...), bodySuffix...))
	return len(self.rlp) != 0
}

func (self *BlockAccess) DbPut() {
	self.db.Put(append(append(blockPrefix, self.blockHash[:]...), bodySuffix...), self.rlp)
}

type ReceiptAccess struct {
	db      common.Database
	txHash  common.Hash
	receipt *types.Receipt
}

func (self *ReceiptAccess) Request(peer *access.Peer) error {
	return peer.GetReceipts([]common.Hash{self.txHash})
}

func (self *ReceiptAccess) Valid(msg *access.Msg) bool {
	if msg.MsgType != access.MsgReceipts {
		return false
	}
	receipts := msg.Obj.(types.Receipts)
	if len(receipts) != 1 {
		return false
	}
	data, err := rlp.EncodeToBytes(receipts[0])
	if err != nil {
		return false
	}
	hash := crypto.Sha3Hash(data)
	valid := bytes.Compare(self.txHash[:], hash[:]) == 0
	if valid {
		self.receipt = receipts[0]
	}
	return valid
}

func (self *ReceiptAccess) DbGet() bool {
	data, _ := self.db.Get(append(receiptsPre, self.txHash[:]...))
	if len(data) == 0 {
		return false
	}

	var receipt types.Receipt
	err := rlp.DecodeBytes(data, &receipt)
	if err == nil {
		self.receipt = &receipt
		return true
	} else {
		glog.V(logger.Core).Infoln("GetReceipt err:", err)
		return false
	}
}

func (self *ReceiptAccess) DbPut() {
	PutReceipts(self.db, types.Receipts{self.receipt})
}
