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

package triedb

import (
	"errors"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/triedb/database"
)

func NewProofReader(proofNodes map[common.Hash][]byte, config *Config) *Database {
	return &Database{
		config:  config,
		backend: proofBackend{proofNodes: proofNodes},
	}
}

func (db *Database) NewProofWriter(proofNodes map[common.Hash][]byte) *Database {
	return &Database{
		disk:       db.disk,
		config:     db.config,
		backend:    proofBackend{parent: db.backend, proofNodes: proofNodes},
		proofNodes: proofNodes,
	}
}

type proofBackend struct {
	parent     backend // reader if nil, writer if not nil
	proofNodes map[common.Hash][]byte
}

func (pb proofBackend) NodeReader(root common.Hash) (database.NodeReader, error) {
	if pb.parent == nil {
		return proofNodeReader{proofNodes: pb.proofNodes}, nil // reader
	}
	reader, err := pb.parent.NodeReader(root)
	if err != nil {
		return nil, err
	}
	return proofNodeReader{parent: reader, proofNodes: pb.proofNodes}, nil // writer
}

func (pb proofBackend) StateReader(root common.Hash) (database.StateReader, error) {
	return nil, errors.New("not implemented")
}
func (pb proofBackend) Size() (common.StorageSize, common.StorageSize) {
	return 0, 0
}

func (pb proofBackend) Commit(root common.Hash, report bool) error {
	return errors.New("not implemented")
}

func (pb proofBackend) Close() error {
	return errors.New("not implemented")
}

type proofNodeReader struct {
	parent     database.NodeReader
	proofNodes map[common.Hash][]byte
}

func (pnr proofNodeReader) Node(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	if pnr.parent == nil {
		if node, ok := pnr.proofNodes[hash]; ok {
			//fmt.Printf("Retrieved node:  owner %x  hash %x  path  %x\n", owner, hash, path)
			return node, nil
		}
		//fmt.Printf("Node not found:  owner %x  hash %x  path  %x\n", owner, hash, path)
		return nil, errors.New("not found")
	}
	node, err := pnr.parent.Node(owner, path, hash)
	if err != nil {
		return nil, err
	}
	if _, ok := pnr.proofNodes[hash]; !ok {
		//fmt.Printf("Store node:  owner %x  hash %x  path  %x\n", owner, hash, path)
		pnr.proofNodes[hash] = slices.Clone(node)
	}
	return node, nil
}
