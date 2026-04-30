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

package logindex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"slices"

	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/rlp"
)

func (ix *Indexer) mergeLoop() {
	defer ix.mergeWg.Done()

	for {
		<-ix.updateMergeCh
		ix.lock.Lock()
		currentOp, shutdown := ix.currentOp, ix.shutdown
		ix.lock.Unlock()
		for !shutdown && currentOp.operation == opNone {
			<-ix.updateMergeCh
			ix.lock.Lock()
			currentOp, shutdown := ix.currentOp, ix.shutdown
			ix.lock.Unlock()
		}
		if shutdown {
			return
		}
		switch currentOp.operation {
		case opDelete:
		case opMerge:
		}
	}
}
