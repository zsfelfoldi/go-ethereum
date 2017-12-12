package eth

import (
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
)

const defaultUTCMinTrustedFraction = 75

// ULCConfig is a Ultra Light client options.
type ULCConfig struct {
	TrustedNodes       []string `toml:",omitempty"` // A list of trusted servers
	MinTrustedFraction int      `toml:",omitempty"` // Minimum percentage of connected trusted servers to validate trusted (1-100)
}

// SetULC properties into eth and p2p configs.
func SetULC(ulc *ULCConfig, p2pCfg *p2p.Config) {
	if ulc == nil || len(ulc.TrustedNodes) == 0 {
		return
	}

	if ulc.MinTrustedFraction < 0 || ulc.MinTrustedFraction >= 100 {
		ulc.MinTrustedFraction = defaultUTCMinTrustedFraction
	}

	p2pCfg.TrustedNodes = make([]*discover.Node, 0, len(ulc.TrustedNodes))
	for _, url := range ulc.TrustedNodes {
		node, err := discover.ParseNode(url)
		if err != nil {
			log.Error("Trusted node URL invalid", "enode", url, "err", err)
			continue
		}
		p2pCfg.TrustedNodes = append(p2pCfg.TrustedNodes, node)
	}
}
