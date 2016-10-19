// Copyright 2015 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

// bootnode runs a bootstrap node for the Ethereum Discovery Protocol.
package main

import (
	"flag"
	"fmt"
	"strconv"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/p2p/discv5"
	"github.com/ethereum/go-ethereum/p2p/nat"
)

func main() {
	var (
		listenPort = flag.Int("addr", 31000, "beginning of listening port range")
		natdesc    = flag.String("nat", "none", "port mapping mechanism (any|none|upnp|pmp|extip:<IP>)")
		count      = flag.Int("count", 100, "number of v5 topic discovery test nodes (adds default bootnodes to form a test network)")
	)
	flag.Var(glog.GetVerbosity(), "verbosity", "log verbosity (0-9)")
	flag.Var(glog.GetVModule(), "vmodule", "log verbosity pattern")
	glog.SetToStderr(true)
	flag.Parse()

	natm, err := nat.Parse(*natdesc)
	if err != nil {
		utils.Fatalf("-nat: %v", err)
	}

	for i := 0; i < *count; i++ {
		listenAddr := ":" + strconv.Itoa(*listenPort+i)

		nodeKey, err := crypto.GenerateKey()
		if err != nil {
			utils.Fatalf("could not generate key: %v", err)
		}

		if ntab, err := discv5.ListenUDP(nodeKey, listenAddr, natm, ""); err != nil {
			utils.Fatalf("%v", err)
		} else {
			if err := ntab.SetFallbackNodes(discv5.BootNodes); err != nil {
				utils.Fatalf("%v", err)
			}
		}
		fmt.Printf("Started test node #%d with public key %v\n", i, discv5.PubkeyID(&nodeKey.PublicKey))
	}

	select {}
}
