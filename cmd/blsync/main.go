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
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/beacon/light"
	"github.com/ethereum/go-ethereum/beacon/light/api"
	"github.com/ethereum/go-ethereum/beacon/light/request"
	"github.com/ethereum/go-ethereum/beacon/light/sync"
	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/beacon/params"
	"github.com/ethereum/go-ethereum/beacon/types"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	ctypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfave/cli/v2"
)

func main() {
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlInfo, log.StreamHandler(os.Stderr, log.TerminalFormat(true))))
	app := flags.NewApp("beacon light syncer tool")
	app.Flags = []cli.Flag{
		utils.BeaconApiFlag,
		utils.BeaconApiHeaderFlag,
		utils.BeaconThresholdFlag,
		utils.BeaconNoFilterFlag,
		utils.BeaconConfigFlag,
		utils.BeaconGenesisRootFlag,
		utils.BeaconGenesisTimeFlag,
		utils.BeaconCheckpointFlag,
		//TODO datadir for optional permanent database
		utils.MainnetFlag,
		utils.SepoliaFlag,
		utils.GoerliFlag,
		utils.BlsyncApiFlag,
		utils.BlsyncJWTSecretFlag,
	}
	app.Action = blsync

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var (
	stateProofFormat    merkle.CompactProofFormat // requested multiproof format
	execBlockIndex      int                       // index of execution block root in proof.Values where proof.Format == stateProofFormat
	finalizedBlockIndex int                       // index of finalized block root in proof.Values where proof.Format == stateProofFormat
)

func blsync(ctx *cli.Context) error {
	if !ctx.IsSet(utils.BeaconApiFlag.Name) {
		utils.Fatalf("Beacon node light client API URL not specified")
	}
	stateProofFormat = merkle.EncodeCompactProofFormat(merkle.NewIndexMapFormat().AddLeaf(params.StateIndexExecHead, nil).AddLeaf(params.StateIndexFinalBlock, nil))
	var (
		stateIndexMap = merkle.ProofFormatIndexMap(stateProofFormat)
		chainConfig   = makeChainConfig(ctx)
		customHeader  = make(map[string]string)
	)
	execBlockIndex = stateIndexMap[params.StateIndexExecHead]
	finalizedBlockIndex = stateIndexMap[params.StateIndexFinalBlock]

	for _, s := range ctx.StringSlice(utils.BeaconApiHeaderFlag.Name) {
		kv := strings.Split(s, ":")
		if len(kv) != 2 {
			utils.Fatalf("Invalid custom API header entry: %s", s)
		}
		customHeader[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}

	// create data structures
	var (
		db              = memorydb.New()
		threshold       = ctx.Int(utils.BeaconThresholdFlag.Name)
		committeeChain  = light.NewCommitteeChain(db, chainConfig.ChainConfig, threshold, !ctx.Bool(utils.BeaconNoFilterFlag.Name), light.BLSVerifier{}, &mclock.System{}, func() int64 { return time.Now().UnixNano() })
		checkpointStore = light.NewCheckpointStore(db, committeeChain)
		headValidator   = light.NewHeadValidator(committeeChain)
		lightChain      = light.NewLightChain(db)
	)

	srv := &api.Server{
		CheckpointStore: checkpointStore,
		CommitteeChain:  committeeChain,
		LightChain:      lightChain,
	}

	headUpdater := sync.NewHeadUpdater(headValidator, committeeChain)
	headTracker := request.NewHeadTracker(headUpdater.NewSignedHead)
	headValidator.Subscribe(threshold, func(signedHead types.SignedHeader) {
		headTracker.SetValidatedHead(signedHead)
	})

	// create sync modules
	checkpointInit := sync.NewCheckpointInit(committeeChain, checkpointStore, chainConfig.Checkpoint)
	forwardSync := sync.NewForwardUpdateSync(committeeChain)
	headerSync := sync.NewHeaderSync(lightChain, false)
	stateSync := sync.NewStateSync(lightChain, stateProofFormat, true)
	beaconBlockSync := newBeaconBlockSyncer(lightChain)
	srv.RecentBlocks = beaconBlockSync.recentBlocks //TODO qqq
	srv.HeadTracker = headTracker
	beaconBlockUpdater := &beaconBlockUpdater{ //TODO constructor
		client:     makeRPCClient(ctx),
		headerSync: headerSync,
		stateSync:  stateSync,
		blockSync:  beaconBlockSync,
		chain:      lightChain,
		server:     srv,
	}

	// set up sync modules and triggers
	scheduler := request.NewScheduler(headTracker, &mclock.System{})
	scheduler.RegisterModule(checkpointInit)
	scheduler.RegisterModule(forwardSync)
	scheduler.RegisterModule(headUpdater)
	scheduler.RegisterModule(beaconBlockSync)
	scheduler.RegisterModule(beaconBlockUpdater)
	scheduler.RegisterModule(stateSync)
	scheduler.RegisterModule(headerSync)
	// start
	scheduler.Start()
	stateSync.SetTailTarget(0)
	// register server(s)
	for _, url := range ctx.StringSlice(utils.BeaconApiFlag.Name) {
		beaconApi := api.NewClient(url, customHeader)
		scheduler.RegisterServer(api.NewSyncServer(beaconApi))
	}

	mux := http.NewServeMux()
	srv.RegisterAt(mux)
	go http.ListenAndServe(":65278", mux)

	// run until stopped
	<-ctx.Done()
	scheduler.Stop()
	return nil
}

func callNewPayloadV2(client *rpc.Client, block *ctypes.Block) (string, error) {
	var resp engine.PayloadStatusV1
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	err := client.CallContext(ctx, &resp, "engine_newPayloadV2", *engine.BlockToExecutableData(block, nil, nil, nil, nil).ExecutionPayload)
	cancel()
	return resp.Status, err
}

func callForkchoiceUpdatedV1(client *rpc.Client, headHash, finalizedHash common.Hash) (string, error) {
	var resp engine.ForkChoiceResponse
	update := engine.ForkchoiceStateV1{
		HeadBlockHash:      headHash,
		SafeBlockHash:      finalizedHash,
		FinalizedBlockHash: finalizedHash,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	err := client.CallContext(ctx, &resp, "engine_forkchoiceUpdatedV1", update, nil)
	cancel()
	return resp.PayloadStatus.Status, err
}
