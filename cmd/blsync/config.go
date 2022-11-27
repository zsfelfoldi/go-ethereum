// Copyright 2022 The go-ethereum Authors
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

package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"hash/crc32"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfave/cli/v2"
)

var (
	MainnetConfig = beacon.ChainConfig{
		GenesisData: beacon.GenesisData{
			GenesisValidatorsRoot: common.HexToHash("0x4b363db94e286120d76eb905340fdd4e54bfe9f06bf33ff6cf5ad27f511bfe95"),
			GenesisTime:           1606824023,
		},
		Forks: beacon.Forks{
			beacon.Fork{
				Epoch:   0,
				Name:    "GENESIS",
				Version: []byte{0, 0, 0, 0},
			},
			beacon.Fork{
				Epoch:   74240,
				Name:    "ALTAIR",
				Version: []byte{1, 0, 0, 0},
			},
			beacon.Fork{
				Epoch:   144896,
				Name:    "BELLATRIX",
				Version: []byte{2, 0, 0, 0},
			},
		},
		Checkpoint: common.HexToHash("0x388be41594ec7d6a6894f18c73f3469f07e2c19a803de4755d335817ed8e2e5a"),
	}

	SepoliaConfig = beacon.ChainConfig{
		GenesisData: beacon.GenesisData{
			GenesisValidatorsRoot: common.HexToHash("0xd8ea171f3c94aea21ebc42a1ed61052acf3f9209c00e4efbaaddac09ed9b8078"),
			GenesisTime:           1655733600,
		},
		Forks: beacon.Forks{
			beacon.Fork{
				Epoch:   0,
				Name:    "GENESIS",
				Version: []byte{144, 0, 0, 105},
			},
			beacon.Fork{
				Epoch:   50,
				Name:    "ALTAIR",
				Version: []byte{144, 0, 0, 112},
			},
			beacon.Fork{
				Epoch:   100,
				Name:    "BELLATRIX",
				Version: []byte{144, 0, 0, 113},
			},
		},
		Checkpoint: common.HexToHash("0x1005a6d9175e96bfbce4d35b80f468e9bff0b674e1e861d16e09e10005a58e81"),
	}

	GoerliConfig = beacon.ChainConfig{
		GenesisData: beacon.GenesisData{
			GenesisValidatorsRoot: common.HexToHash("0x043db0d9a83813551ee2f33450d23797757d430911a9320530ad8a0eabc43efb"),
			GenesisTime:           1614588812,
		},
		Forks: beacon.Forks{
			beacon.Fork{
				Epoch:   0,
				Name:    "GENESIS",
				Version: []byte{0, 0, 16, 32},
			},
			beacon.Fork{
				Epoch:   36660,
				Name:    "ALTAIR",
				Version: []byte{1, 0, 16, 32},
			},
			beacon.Fork{
				Epoch:   112260,
				Name:    "BELLATRIX",
				Version: []byte{2, 0, 16, 32},
			},
		},
		Checkpoint: common.HexToHash("0x53a0f4f0a378e2c4ae0a9ee97407eb69d0d737d8d8cd0a5fb1093f42f7b81c49"),
	}
)

func makeChainConfig(ctx *cli.Context) beacon.ChainConfig {
	utils.CheckExclusive(ctx, utils.MainnetFlag, utils.GoerliFlag, utils.SepoliaFlag)
	customConfig := ctx.IsSet(utils.BeaconConfigFlag.Name) || ctx.IsSet(utils.BeaconGenesisRootFlag.Name) || ctx.IsSet(utils.BeaconGenesisTimeFlag.Name)
	var config beacon.ChainConfig
	switch {
	case ctx.Bool(utils.MainnetFlag.Name):
		config = MainnetConfig
	case ctx.Bool(utils.SepoliaFlag.Name):
		config = SepoliaConfig
	case ctx.Bool(utils.GoerliFlag.Name):
		config = GoerliConfig
	default:
		if !customConfig {
			config = MainnetConfig
		}
	}
	if customConfig && config.Forks != nil {
		utils.Fatalf("Cannot use custom beacon chain config flags in combination with pre-defined network config")
	}
	if ctx.IsSet(utils.BeaconConfigFlag.Name) {
		forks, err := beacon.LoadForks(ctx.String(utils.BeaconConfigFlag.Name))
		if err != nil {
			utils.Fatalf("Could not load beacon chain config file", "file name", ctx.String(utils.BeaconConfigFlag.Name), "error", err)
		}
		config.Forks = forks
	}
	if ctx.IsSet(utils.BeaconGenesisRootFlag.Name) {
		if c, err := hexutil.Decode(ctx.String(utils.BeaconGenesisRootFlag.Name)); err == nil && len(c) <= 32 {
			copy(config.GenesisValidatorsRoot[:len(c)], c)
		} else {
			utils.Fatalf("Invalid hex string", "beacon.genesis.gvroot", ctx.String(utils.BeaconGenesisRootFlag.Name), "error", err)
		}
	}
	if ctx.IsSet(utils.BeaconGenesisTimeFlag.Name) {
		config.GenesisTime = ctx.Uint64(utils.BeaconGenesisTimeFlag.Name)
	}
	if ctx.IsSet(utils.BeaconCheckpointFlag.Name) {
		if c, err := hexutil.Decode(ctx.String(utils.BeaconCheckpointFlag.Name)); err == nil && len(c) <= 32 {
			copy(config.Checkpoint[:len(c)], c)
		} else {
			utils.Fatalf("Invalid hex string", "beacon.checkpoint", ctx.String(utils.BeaconCheckpointFlag.Name), "error", err)
		}
	}
	return config
}

func makeRPCClient(ctx *cli.Context) *rpc.Client {
	if !ctx.IsSet(utils.BlsyncApiFlag.Name) {
		log.Warn("No engine API target specified, performing a dry run")
		return nil
	}
	if !ctx.IsSet(utils.BlsyncJWTSecretFlag.Name) {
		utils.Fatalf("JWT secret parameter missing") //TODO use default if datadir is specified
	}

	engineApiUrl, jwtFileName := ctx.String(utils.BlsyncApiFlag.Name), ctx.String(utils.BlsyncJWTSecretFlag.Name)
	jwtSecret := obtainJWTSecret(jwtFileName)
	auth := node.NewJWTAuth(jwtSecret)
	cl, err := rpc.DialOptions(context.Background(), engineApiUrl, rpc.WithHTTPAuth(auth))
	if err != nil {
		utils.Fatalf("Could not create RPC client: %v", err)
	}
	return cl
}

func obtainJWTSecret(fileName string) (jwtSecret [32]byte) {
	if hexData, err := os.ReadFile(fileName); err == nil {
		data := common.FromHex(strings.TrimSpace(string(hexData)))
		if len(data) == 32 {
			copy(jwtSecret[:], data)
			log.Info("Loaded JWT secret file", "path", fileName, "crc32", fmt.Sprintf("%#x", crc32.ChecksumIEEE(data)))
			return
		}
		log.Error("Invalid JWT secret", "path", fileName, "length", len(data))
		utils.Fatalf("invalid JWT secret")
	}
	// Need to generate one
	crand.Read(jwtSecret[:])
	if err := os.WriteFile(fileName, []byte(hexutil.Encode(jwtSecret[:])), 0600); err != nil {
		utils.Fatalf("Could not write JWT secret file: %v", err)
	}
	log.Info("Generated JWT secret", "path", fileName)
	return
}
