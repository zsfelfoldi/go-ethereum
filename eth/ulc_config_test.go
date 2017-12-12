package eth

import (
	"crypto/rand"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"testing"
)

func TestSetULC_Success(t *testing.T) {
	node := &discover.Node{}
	rand.Read(node.ID[:])
	ulcConf := &ULCConfig{TrustedNodes: []string{node.String(), "incorrect node"}, MinTrustedFraction: 50}
	p2pConf := &p2p.Config{}
	SetULC(ulcConf, p2pConf)

	if len(p2pConf.TrustedNodes) != 1 {
		t.Fatal("Incorrect num of trusted nodes")
	}
	if p2pConf.TrustedNodes[0].ID != node.ID {
		t.Fatal("Incorrect trusted node id")
	}
	if ulcConf.MinTrustedFraction == defaultUTCMinTrustedFraction {
		t.Fatal("MinTrustedFraction changed to default")
	}
}

func TestSetULC_MinTrustedFractionChangedToDefault(t *testing.T) {
	const incorrectTrustedFraction = 110 //percents
	node := &discover.Node{}
	rand.Read(node.ID[:])
	ulcConf := &ULCConfig{TrustedNodes: []string{node.String()}, MinTrustedFraction: incorrectTrustedFraction}
	p2pConf := &p2p.Config{}
	SetULC(ulcConf, p2pConf)

	if ulcConf.MinTrustedFraction != defaultUTCMinTrustedFraction {
		t.Fatal("MinTrustedFraction has not default value")
	}
}

func TestSetULC_NotPanicOnNilUlcConfig(t *testing.T) {
	p2pConf := &p2p.Config{}
	SetULC(nil, p2pConf)
	if len(p2pConf.TrustedNodes) > 0 {
		t.FailNow()
	}
}
