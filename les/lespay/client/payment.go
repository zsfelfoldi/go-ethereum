// Copyright 2020 The go-ethereum Authors
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

package client

import (
	"io"
	"math"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/les/utils"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nodestate"
)

type DepositController struct {
	ns *nodestate.NodeStateMachine

	maxPaymentRate, minAllowance float64
	sumWeight, spendableWeight   uint64
}

func NewDepositController(ns *nodestate.NodeStateMachine) *DepositController {
}

func (dc *DepositController) updateNode(allowance, weight uint64) int64 {
	if weight == 0 || dc.sumWeight == 0 {
		return 0
	}
	wt := float64(weight) / float64(dc.sumWeight)
	relValue := float64(allowance) / (minAllowance * wt)
	if relValue < 1 {
		relValue -= weightRatioPenalty * (1 - relValue)
	}
}
