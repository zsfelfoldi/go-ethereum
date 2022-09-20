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
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/common/mclock"
	cbeacon "github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/light/beacon/api"
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
	stateProofFormat    beacon.ProofFormat // requested multiproof format
	execBlockIndex      int                // index of execution block root in proof.Values where proof.Format == stateProofFormat
	finalizedBlockIndex int                // index of finalized block root in proof.Values where proof.Format == stateProofFormat
)

func blsync(ctx *cli.Context) error {
	if !ctx.IsSet(utils.BeaconApiFlag.Name) {
		utils.Fatalf("Beacon node light client API URL not specified")
	}
	stateProofFormat = beacon.NewIndexMapFormat().AddLeaf(beacon.BsiExecHead, nil).AddLeaf(beacon.BsiFinalBlock, nil)
	var (
		stateIndexMap = beacon.ProofFormatIndexMap(stateProofFormat)
		chainConfig   = makeChainConfig(ctx)
		customHeader  = make(map[string]string)
	)
	execBlockIndex = stateIndexMap[beacon.BsiExecHead]
	finalizedBlockIndex = stateIndexMap[beacon.BsiFinalBlock]

	for _, s := range utils.SplitAndTrim(ctx.String(utils.BeaconApiHeaderFlag.Name)) {
		kv := strings.Split(s, ":")
		if len(kv) != 2 {
			utils.Fatalf("Invalid custom API header entry: %s", s)
		}
		customHeader[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
	}

	var (
		beaconApi               = api.NewBeaconLightApi(ctx.String(utils.BeaconApiFlag.Name), customHeader)
		committeeSyncer         = api.NewCommitteeSyncer(beaconApi, chainConfig.GenesisData, false)
		db                      = memorydb.New()
		syncCommitteeCheckpoint = beacon.NewWeakSubjectivityCheckpoint(db, committeeSyncer, chainConfig.Checkpoint, nil)
	)
	if syncCommitteeCheckpoint == nil {
		utils.Fatalf("No beacon chain checkpoint")
	}
	syncCommitteeTracker := beacon.NewSyncCommitteeTracker(db, chainConfig.Forks, syncCommitteeCheckpoint, ctx.Int(utils.BeaconThresholdFlag.Name), !ctx.Bool(utils.BeaconNoFilterFlag.Name), beacon.BLSVerifier{}, &mclock.System{}, func() int64 { return time.Now().UnixNano() })
	execSyncer := &execSyncer{
		api:           beaconApi,
		client:        makeRPCClient(ctx),
		execRootCache: lru.NewCache[common.Hash, common.Hash](1000),
	}
	syncCommitteeTracker.SubscribeToNewHeads(execSyncer.newHead)
	committeeSyncer.Start(syncCommitteeTracker)
	syncCommitteeCheckpoint.TriggerFetch()
	<-ctx.Done()
	return nil
}

func callNewPayloadV1(client *rpc.Client, block *types.Block) (string, error) {
	var resp cbeacon.PayloadStatusV1
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	err := client.CallContext(ctx, &resp, "engine_newPayloadV1", *cbeacon.BlockToExecutableData(block))
	cancel()
	return resp.Status, err
}

func callForkchoiceUpdatedV1(client *rpc.Client, headHash, finalizedHash common.Hash) (string, error) {
	var resp cbeacon.ForkChoiceResponse
	update := cbeacon.ForkchoiceStateV1{
		HeadBlockHash:      headHash,
		SafeBlockHash:      finalizedHash,
		FinalizedBlockHash: finalizedHash,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	err := client.CallContext(ctx, &resp, "engine_forkchoiceUpdatedV1", update, nil)
	cancel()
	return resp.PayloadStatus.Status, err
}

type execSyncer struct {
	api           *api.BeaconLightApi
	client        *rpc.Client
	execRootCache *lru.Cache[common.Hash, common.Hash] // beacon block root -> execution block root
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
		maxBlockFetch  = 4
	)
	for {
		proof, err := e.api.GetStateProof(head.StateRoot, statePaths, stateProofFormat)
		if err != nil {
			log.Error("Error fetching state proof from beacon API", "error", err)
			break
		}
		var (
			execBlockRoot       = common.Hash(proof.Values[execBlockIndex])
			finalizedBeaconRoot = common.Hash(proof.Values[finalizedBlockIndex])
			beaconRoot          = head.Hash()
		)
		block, err := e.api.GetExecutionPayload(beaconRoot, execBlockRoot)
		if err != nil {
			log.Error("Error fetching execution payload from beacon API", "error", err)
			break
		}
		blocks = append(blocks, block)
		finalizedRoots = append(finalizedRoots, finalizedBeaconRoot)
		e.execRootCache.Add(beaconRoot, execBlockRoot)

		if _, ok := e.execRootCache.Get(head.ParentRoot); ok {
			parentFound = true
			break
		}
		maxBlockFetch--
		if maxBlockFetch == 0 {
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
	if !parentFound && maxBlockFetch == 0 {
		blocks = blocks[:1]
		finalizedRoots = finalizedRoots[:1]
		// iterate further back as far as possible (or max 256 steps) just to collect beacon -> exec root associations
		maxFinalizedFetch := 256
		for {
			proof, err := e.api.GetStateProof(head.StateRoot, statePaths, stateProofFormat)
			if err != nil {
				// exit silently because we expect running into an error
				break
			}
			execBlockRoot := common.Hash(proof.Values[execBlockIndex])
			beaconRoot := head.Hash()
			e.execRootCache.Add(beaconRoot, execBlockRoot)
			if beaconRoot == finalizedRoots[0] {
				break // this is what we were looking for, earlier associations won't be needed
			}
			if _, ok := e.execRootCache.Get(head.ParentRoot); ok {
				break
			}
			maxFinalizedFetch--
			if maxFinalizedFetch == 0 {
				break
			}
			if head, err = e.api.GetHeader(head.ParentRoot); err != nil {
				break
			}
		}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		blockRoot := blocks[i].Hash()
		if status, err := callNewPayloadV1(e.client, blocks[i]); err == nil {
			log.Info("Successful NewPayload", "number", blocks[i].NumberU64(), "blockRoot", blockRoot, "status", status)
		} else {
			log.Error("Failed NewPayload", "number", blocks[i].NumberU64(), "blockRoot", blockRoot, "error", err)
		}
		finalizedExecRoot, _ := e.execRootCache.Get(finalizedRoots[i])
		if status, err := callForkchoiceUpdatedV1(e.client, blockRoot, finalizedExecRoot); err == nil {
			log.Info("Successful ForkchoiceUpdated", "head", blockRoot, "finalized", finalizedExecRoot, "status", status)
		} else {
			log.Error("Failed ForkchoiceUpdated", "head", blockRoot, "finalized", finalizedExecRoot, "error", err)
		}
	}
}
