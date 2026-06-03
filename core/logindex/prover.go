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

type tableProver struct {
}

func newTableProver() *tableProver {
	panic("xxx")
	return &tableProver{}
}

func (tp *tableProver) newInstance() *proverInstance {
	panic("xxx")
	return &proverInstance{}
}

type proverInstance struct{}

func (pi *proverInstance) addLeafNode(entryIndex uint64) uint32 {
	panic("xxx")
}

func (pi *proverInstance) addAndNode() uint32 {
	panic("xxx")
}

func (pi *proverInstance) addOrNode() uint32 {
	panic("xxx")
}

func (pi *proverInstance) connect(source, target uint32) {
	panic("xxx")
}
