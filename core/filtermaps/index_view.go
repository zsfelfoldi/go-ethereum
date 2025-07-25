// Copyright 2025 The go-ethereum Authors
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
	"slices"

	"github.com/ethereum/go-ethereum/common"
)

type indexView struct {
	headMapIndex uint32
	headMap      *memoryMap
	parent       *mapTreeNode
	mapStorage   *mapStorage
}

type mapTreeNode struct {
	mapIndex, refCount uint32
	parent             *mapTreeNode
	mapData            *finishedMap
}

func (iv *indexView) clone() *indexView {

}

func (f *FilterMaps) registerindexView(id common.Hash, indexView *indexView) {}

func (f *FilterMaps) getindexView(id common.Hash) *indexView {}
