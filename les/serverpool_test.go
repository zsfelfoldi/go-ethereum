package les

import (
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/rlp"
	"math/rand"
	"sync"
	"testing"
)

func TestLoadTrustedNodes(t *testing.T) {
	node := discover.Node{
		ID: discover.NodeID{},
	}
	rand.Read(node.ID[:])

	var wg sync.WaitGroup
	q := make(chan struct{})
	sp := newServerPool(&dbStub{}, q, &wg)
	sp.server = &p2p.Server{}
	sp.server.TrustedNodes = []*discover.Node{
		&node,
	}

	sp.loadNodes()

	if len(sp.entries) == 0 {
		t.Fatal("empty nodes")
	}
	if _, ok := sp.entries[node.ID]; !ok {
		t.Fatal("empty entries")
	}
	if len(sp.trustedQueue.queue) != 1 {
		t.Fatal("incorrect trustedQueue.queue")
	}
	if sp.trustedQueue.queue[sp.entries[node.ID].queueIdx] != sp.entries[node.ID] {
		t.Fatal("not exist")
	}
	if sp.trustedQueue.newPtr != 1 {
		t.Fatal("incorrect ptr")
	}
}

type dbStub struct{}

func (db *dbStub) Put(key []byte, value []byte) error {
	return nil
}
func (db *dbStub) Get(key []byte) ([]byte, error) {
	list := make([]*poolEntry, 0)
	return rlp.EncodeToBytes(&list)
}
func (db *dbStub) Has(key []byte) (bool, error) {
	return false, nil
}
func (db *dbStub) Delete(key []byte) error {
	return nil
}
func (db *dbStub) Close() {

}
func (db *dbStub) NewBatch() ethdb.Batch {
	return nil
}
