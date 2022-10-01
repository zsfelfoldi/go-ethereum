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
	"os"
	"time"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	cbeacon "github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/light/beacon/api"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
	"github.com/urfave/cli/v2"
)

func main() {
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlInfo, log.StreamHandler(os.Stderr, log.TerminalFormat(true))))
	app := flags.NewApp("beacon light syncer tool")
	app.Flags = []cli.Flag{
		utils.BeaconApiFlag,
		utils.BeaconThresholdFlag,
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
	stateProofFormat beacon.ProofFormat
	stateIndexMap    map[uint64]int
)

func blsync(ctx *cli.Context) error {
	format := beacon.NewIndexMapFormat()
	format.AddLeaf(beacon.BsiExecHead, nil)
	format.AddLeaf(beacon.BsiFinalBlock, nil)
	stateProofFormat = format
	stateIndexMap = beacon.ProofIndexMap(stateProofFormat)

	chainConfig := makeChainConfig(ctx)

	if !ctx.IsSet(utils.BeaconApiFlag.Name) {
		utils.Fatalf("Beacon node light client API URL not specified")
	}
	beaconApi := &api.RestApi{Url: ctx.String(utils.BeaconApiFlag.Name)}
	committeeSyncer := api.NewCommitteeSyncer(beaconApi, chainConfig.GenesisData)
	db := memorydb.New() //TODO or real db
	syncCommitteeCheckpoint := beacon.NewWeakSubjectivityCheckpoint(db, committeeSyncer, chainConfig.Checkpoint, nil)
	if syncCommitteeCheckpoint == nil {
		utils.Fatalf("No beacon chain checkpoint")
	}
	syncCommitteeTracker := beacon.NewSyncCommitteeTracker(db, chainConfig.Forks, syncCommitteeCheckpoint, ctx.Int(utils.BeaconThresholdFlag.Name), beacon.BLSVerifier{}, &mclock.System{})
	cache, _ := lru.New(1000)
	execSyncer := &execSyncer{
		api:           beaconApi,
		client:        makeRPCClient(ctx),
		execRootCache: cache,
	}
	syncCommitteeTracker.SubscribeToNewHeads(execSyncer.newHead)
	committeeSyncer.Start(syncCommitteeTracker)
	syncCommitteeCheckpoint.TriggerFetch()

	<-ctx.Done()
	return nil
}

func callNewPayloadV1(client *rpc.Client, block *types.Block) (string, error) {
	var resp cbeacon.PayloadStatusV1
	ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
	err := client.CallContext(ctx, &resp, "engine_newPayloadV1", *cbeacon.BlockToExecutableData(block))
	return resp.Status, err
}

func callForkchoiceUpdatedV1(client *rpc.Client, headHash, finalizedHash common.Hash) (string, error) {
	var resp cbeacon.ForkChoiceResponse
	update := cbeacon.ForkchoiceStateV1{
		HeadBlockHash:      headHash,
		SafeBlockHash:      finalizedHash,
		FinalizedBlockHash: finalizedHash,
	}
	ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
	err := client.CallContext(ctx, &resp, "engine_forkchoiceUpdatedV1", update, nil)
	return resp.PayloadStatus.Status, err
}

type execSyncer struct {
	api           *api.RestApi
	client        *rpc.Client
	execRootCache *lru.Cache // beacon block root -> execution block root
}

var statePaths = []string{
	"[\"finalizedCheckpoint\",\"root\"]",
	"[\"latestExecutionPayloadHeader\",\"blockHash\"]",
}

func (e *execSyncer) newHead(head beacon.Header) {
	log.Info("Received new beacon head", "slot", head.Slot, "blockRoot", head.Hash())
	if e.client == nil {
		return
	}
	var (
		blocks         []*types.Block
		finalizedRoots []common.Hash
		parentFound    bool
	)
	count := 16
	for {
		proof, err := e.api.GetStateProof(head.StateRoot, statePaths, stateProofFormat)
		if err != nil {
			log.Error("Error fetching state proof from beacon API", "error", err)
			break
		}
		execBlockRoot := common.Hash(proof.Values[stateIndexMap[beacon.BsiExecHead]])
		finalizedBeaconRoot := common.Hash(proof.Values[stateIndexMap[beacon.BsiFinalBlock]])

		beaconRoot := head.Hash()
		block, err := e.api.GetExecutionPayload(beaconRoot, execBlockRoot)
		if err != nil {
			log.Error("Error fetching execution payload from beacon API", "error", err)
			break
		}
		blocks = append(blocks, block)
		finalizedRoots = append(finalizedRoots, finalizedBeaconRoot)
		e.execRootCache.Add(beaconRoot, block.Hash())

		if _, ok := e.execRootCache.Get(head.ParentRoot); ok {
			parentFound = true
			break
		}
		count--
		if count == 0 {
			break
		}

		if head, err = e.api.GetHeader(head.ParentRoot); err != nil {
			log.Error("Error fetching header from beacon API", "error", err)
			break
		}
	}
	if blocks == nil {
		return
	}
	if !parentFound {
		blocks = blocks[:1]
		finalizedRoots = finalizedRoots[:1]
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		blockRoot := blocks[i].Hash()
		if status, err := callNewPayloadV1(e.client, blocks[i]); err == nil {
			log.Info("Successful NewPayload", "number", blocks[i].NumberU64(), "blockRoot", blockRoot, "status", status)
		} else {
			log.Error("Failed NewPayload", "number", blocks[i].NumberU64(), "blockRoot", blockRoot, "error", err)
		}
		var finalizedExecRoot common.Hash
		if f, ok := e.execRootCache.Get(finalizedRoots[i]); ok {
			finalizedExecRoot = f.(common.Hash)
		}
		if status, err := callForkchoiceUpdatedV1(e.client, blockRoot, finalizedExecRoot); err == nil {
			log.Info("Successful ForkchoiceUpdated", "head", blockRoot, "finalized", finalizedExecRoot, "status", status)
		} else {
			log.Error("Failed ForkchoiceUpdated", "head", blockRoot, "finalized", finalizedExecRoot, "error", err)
		}
	}
}
