package les

import (
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/light"
	"testing"
	"time"
)

func TestUlc(t *testing.T) {
	testULC(t, 2)
}

func testULC(t *testing.T, protocol int) {
	// Assemble the test environment
	peers := newPeerSet()
	dist := newRequestDistributor(peers, make(chan struct{}))
	rm := newRetrieveManager(peers, dist, nil)

	db, _ := ethdb.NewMemDatabase()
	ldb, _ := ethdb.NewMemDatabase()

	odr := NewLesOdr(ldb, light.NewChtIndexer(db, true), light.NewBloomTrieIndexer(db, true), eth.NewBloomIndexer(db, light.BloomTrieFrequency), rm)

	pm := newTestProtocolManagerMust(t, false, 4, testChainGen, nil, nil, db, nil)
	lpm := newTestProtocolManagerMust(t, true, 0, nil, peers, odr, ldb, nil)

	_, err1, lpeer, err2 := newTestPeerPair("peer", protocol, pm, lpm)
	select {
	case <-time.After(time.Millisecond * 100):
	case err := <-err1:
		t.Fatalf("peer 1 handshake error: %v", err)
	case err := <-err2:
		t.Fatalf("peer 1 handshake error: %v", err)
	}

	lpm.synchronise(lpeer)
}
