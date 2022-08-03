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

package les

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"sync"

	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	lru "github.com/hashicorp/golang-lru"
)

const (
	headPollFrequency = time.Millisecond * 200
	headPollCount     = 50
)

type beaconNodeApiSource struct {
	chain *beacon.BeaconChain
	sct   *beacon.SyncCommitteeTracker
	url   string

	updateCache, committeeCache *lru.Cache
	headTriggerCh               chan chan bool
	closedCh                    chan struct{}

	syncedLock sync.Mutex
	syncedCh   chan bool
}

func (bn *beaconNodeApiSource) start() {
	bn.headTriggerCh = make(chan chan bool, 1)
	bn.closedCh = make(chan struct{})
	bn.updateCache, _ = lru.New(MaxCommitteeUpdateFetch)
	bn.committeeCache, _ = lru.New(MaxCommitteeUpdateFetch / CommitteeCostFactor)
	bn.sct.SyncWithPeer(bn, nil)
	go bn.headPollLoop()
}

func (bn *beaconNodeApiSource) newHead() {
	bn.updateCache.Purge()
	bn.committeeCache.Purge()

	bn.syncedLock.Lock()
	oldSyncedCh := bn.syncedCh
	newSyncedCh := make(chan bool, 1)
	bn.syncedCh = newSyncedCh
	bn.syncedLock.Unlock()

	if oldSyncedCh != nil {
		<-oldSyncedCh
	}
	//bn.headTriggerCh <- newSyncedCh

	select {
	case bn.headTriggerCh <- newSyncedCh:
	default:
		newSyncedCh <- false
	}
}

func (bn *beaconNodeApiSource) stop() {
	bn.sct.Disconnect(bn)
	close(bn.headTriggerCh)
	<-bn.closedCh
}

func (bn *beaconNodeApiSource) headPollLoop() {
	fmt.Println("Started headPollLoop()")
	timer := time.NewTimer(0)
	var (
		counter      int
		lastHead     beacon.SignedHead
		timerStarted bool
	)
	nextAdvertiseSlot := uint64(1)

	for {
		select {
		case <-timer.C:
			timerStarted = false
			//fmt.Println(" headPollLoop timer")
			if head, err := bn.getHeadUpdate(); err == nil {
				//fmt.Println(" headPollLoop head update for slot", head.Header.Slot, nextAdvertiseSlot)
				if !head.Equal(&lastHead) {
					fmt.Println("head poll: new head", head.Header.Slot, nextAdvertiseSlot)
					bn.sct.AddSignedHeads(bn, []beacon.SignedHead{head})
					lastHead = head
					if uint64(head.Header.Slot) >= nextAdvertiseSlot {
						lastPeriod := uint64(head.Header.Slot-1) >> 13
						if bn.advertiseUpdates(lastPeriod) {
							nextAdvertiseSlot = (lastPeriod + 1) << 13
							if uint64(head.Header.Slot) >= nextAdvertiseSlot {
								nextAdvertiseSlot += 8000
							}
						}
					}
				}
				if counter < headPollCount {
					counter++
					timer.Reset(headPollFrequency)
					timerStarted = true
				}
			} else {
				fmt.Println(" getHeadUpdate failed:", err)
			}
		case syncedCh := <-bn.headTriggerCh:
			fmt.Println(" headTriggerCh")
			if syncedCh == nil {
				close(bn.closedCh)
				return
			}
			if head, err := bn.getHeader( /*ctx,*/ common.Hash{}); err == nil {
				bn.chain.SyncToHead(head, syncedCh)
			} else {
				log.Error("Could not fetch header from beacon node API", "error", err)
				syncedCh <- false
			}
			if !timerStarted {
				timer.Reset(headPollFrequency)
				timerStarted = true
			}
			counter = 0
		}
	}
}

func (bn *beaconNodeApiSource) advertiseUpdates(lastPeriod uint64) bool {
	fmt.Println("advertiseUpdates", lastPeriod)
	nextPeriod := bn.sct.NextPeriod()
	if nextPeriod < 2 {
		nextPeriod = 2 //TODO ???
	}
	/*if nextPeriod == 0 {
		return false
	}
	if nextPeriod-1 > lastPeriod {
		return true
	}*/
	updateInfo := &beacon.UpdateInfo{
		AfterLastPeriod: lastPeriod + 1,
		Scores:          make([]byte, int(lastPeriod+2-nextPeriod)*3),
	}
	var ptr int
	for period := nextPeriod - 1; period <= lastPeriod; period++ {
		if update, err := bn.getBestUpdate(period); err == nil {
			update.CalculateScore().Encode(updateInfo.Scores[ptr : ptr+3])
		} else {
			log.Error("Error retrieving best committee update from API", "period", period)
		}
		ptr += 3
	}
	fmt.Println("advertiseUpdates", lastPeriod, updateInfo, len(updateInfo.Scores)/3)
	bn.sct.SyncWithPeer(bn, updateInfo)
	return true
}

func (bn *beaconNodeApiSource) GetBestCommitteeProofs(ctx context.Context, req beacon.CommitteeRequest) (beacon.CommitteeReply, error) {
	reply := beacon.CommitteeReply{
		Updates:    make([]beacon.LightClientUpdate, len(req.UpdatePeriods)),
		Committees: make([][]byte, len(req.CommitteePeriods)),
	}
	var err error
	for i, period := range req.UpdatePeriods {
		if reply.Updates[i], err = bn.getBestUpdate(period); err != nil {
			return beacon.CommitteeReply{}, err
		}
	}
	for i, period := range req.CommitteePeriods {
		if reply.Committees[i], err = bn.getCommittee(period); err != nil {
			return beacon.CommitteeReply{}, err
		}
	}
	return reply, nil
}

func (bn *beaconNodeApiSource) getBestUpdate(period uint64) (beacon.LightClientUpdate, error) {
	if c, _ := bn.updateCache.Get(period); c != nil {
		return c.(beacon.LightClientUpdate), nil
	}
	update, _, err := bn.getBestUpdateAndCommittee(period)
	return update, err
}

func (bn *beaconNodeApiSource) getCommittee(period uint64) ([]byte, error) {
	if c, _ := bn.committeeCache.Get(period); c != nil {
		return c.([]byte), nil
	}
	_, committee, err := bn.getBestUpdateAndCommittee(period - 1)
	return committee, err
}

func (bn *beaconNodeApiSource) getBestUpdateAndCommittee(period uint64) (beacon.LightClientUpdate, []byte, error) {
	c, err := bn.getCommitteeUpdate(period)
	if err != nil {
		return beacon.LightClientUpdate{}, nil, err
	}
	committee, ok := c.NextSyncCommittee.serialize()
	if !ok {
		return beacon.LightClientUpdate{}, nil, errors.New("invalid sync committee")
	}
	update := beacon.LightClientUpdate{
		Header:                  c.Header,
		NextSyncCommitteeRoot:   beacon.SerializedCommitteeRoot(committee),
		NextSyncCommitteeBranch: c.NextSyncCommitteeBranch,
		FinalizedHeader:         c.FinalizedHeader,
		FinalityBranch:          c.FinalityBranch,
		SyncCommitteeBits:       c.Aggregate.BitMask,
		SyncCommitteeSignature:  c.Aggregate.Signature,
		ForkVersion:             c.ForkVersion,
	}
	bn.updateCache.Add(period, update)
	bn.committeeCache.Add(period+1, committee)
	return update, committee, nil
}

type syncAggregate struct {
	BitMask   hexutil.Bytes `json:"sync_committee_bits"`
	Signature hexutil.Bytes `json:"sync_committee_signature"`
}

func (bn *beaconNodeApiSource) getHeadUpdate() (beacon.SignedHead, error) {
	resp, err := http.Get(bn.url + "/eth/v1/light_client/optimistic_update/")
	if err != nil {
		return beacon.SignedHead{}, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return beacon.SignedHead{}, err
	}

	var data struct {
		Data struct {
			Aggregate syncAggregate `json:"sync_aggregate"`
			Header    beacon.Header `json:"attested_header"`
		} `json:"data"`
	}
	//fmt.Println("getHeadUpdate:", string(body))
	if err := json.Unmarshal(body, &data); err != nil {
		return beacon.SignedHead{}, err
	}
	//fmt.Println(" data:", data)
	if len(data.Data.Aggregate.BitMask) != 64 {
		return beacon.SignedHead{}, errors.New("invalid sync_committee_bits length")
	}
	if len(data.Data.Aggregate.Signature) != 96 {
		return beacon.SignedHead{}, errors.New("invalid sync_committee_signature length")
	}
	return beacon.SignedHead{
		Header:    data.Data.Header,
		BitMask:   data.Data.Aggregate.BitMask,
		Signature: data.Data.Aggregate.Signature,
	}, nil
}

type syncCommitteeJson struct {
	Pubkeys   []hexutil.Bytes `json:"pubkeys"`
	Aggregate hexutil.Bytes   `json:"aggregate_pubkey"`
}

func (s *syncCommitteeJson) serialize() ([]byte, bool) {
	if len(s.Pubkeys) != 512 {
		return nil, false
	}
	sk := make([]byte, 513*48)
	for i, key := range s.Pubkeys {
		if len(key) != 48 {
			return nil, false
		}
		copy(sk[i*48:(i+1)*48], key[:])
	}
	if len(s.Aggregate) != 48 {
		return nil, false
	}
	copy(sk[512*48:], s.Aggregate[:])
	return sk, true
}

type committeeUpdate struct {
	Header                  beacon.Header       `json:"attested_header"`
	NextSyncCommittee       syncCommitteeJson   `json:"next_sync_committee"`
	NextSyncCommitteeBranch beacon.MerkleValues `json:"next_sync_committee_branch"`
	FinalizedHeader         beacon.Header       `json:"finalized_header"`
	FinalityBranch          beacon.MerkleValues `json:"finality_branch"`
	Aggregate               syncAggregate       `json:"sync_aggregate"`
	ForkVersion             hexutil.Bytes       `json:"fork_version"`
}

func (bn *beaconNodeApiSource) getCommitteeUpdate(period uint64) (committeeUpdate, error) {
	periodStr := strconv.Itoa(int(period))
	resp, err := http.Get(bn.url + "/eth/v1/light_client/updates?start_period=" + periodStr + "&count=1")
	if err != nil {
		return committeeUpdate{}, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return committeeUpdate{}, err
	}

	var data struct {
		Data []committeeUpdate `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return committeeUpdate{}, err
	}
	if len(data.Data) != 1 {
		return committeeUpdate{}, errors.New("invalid number of committee updates")
	}
	update := data.Data[0]
	if len(update.NextSyncCommittee.Pubkeys) != 512 {
		return committeeUpdate{}, errors.New("invalid number of pubkeys in next_sync_committee")
	}
	return update, nil
}

func (bn *beaconNodeApiSource) ClosedChannel() chan struct{} {
	return bn.closedCh
}

func (bn *beaconNodeApiSource) WrongReply(description string) {
	log.Error("Beacon node API data source delivered wrong reply", "error", description)
}

// null hash -> current head
func (bn *beaconNodeApiSource) getHeader(blockRoot common.Hash) (beacon.Header, error) {
	url := bn.url + "/eth/v1/beacon/headers/"
	if blockRoot == (common.Hash{}) {
		url += "head"
	} else {
		url += blockRoot.Hex()
	}
	resp, err := http.Get(url)
	if err != nil {
		return beacon.Header{}, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return beacon.Header{}, err
	}

	var data struct {
		Data struct {
			Root      common.Hash `json:"root"`
			Canonical bool        `json:"canonical"`
			Header    struct {
				Message   beacon.Header `json:"message"`
				Signature hexutil.Bytes `json:"signature"`
			} `json:"header"`
		} `json:"data"`
	}
	fmt.Println("header json", string(body), "url", url)
	if err := json.Unmarshal(body, &data); err != nil {
		fmt.Println("header unmarshal err", err)
		return beacon.Header{}, err
	}
	header := data.Data.Header.Message
	if blockRoot == (common.Hash{}) {
		blockRoot = data.Data.Root
	}
	if header.Hash() != blockRoot {
		return beacon.Header{}, errors.New("retrieved beacon header root does not match")
	}
	return header, nil
}

func statePaths(id, id2 string, index int) string {
	s := "[\"" + id + "\""
	if id2 != "" {
		s += ",\"" + id2 + "\""
	}
	if index >= 0 {
		s += "," + strconv.Itoa(index)
	}
	return s + "]"
}

//
var statePathsBase = []string{
	statePaths("fork", "", -1),                       //TODO ???
	statePaths("latestBlockHeader", "stateRoot", -1), // stateRoot is not needed but this is currently the cheapest way to get the header root
	statePaths("finalizedCheckpoint", "root", -1),
	statePaths("latestExecutionPayloadHeader", "blockHash", -1),
}

var statePathsInit = []string{
	statePaths("genesisTime", "", -1),
	statePaths("genesisValidatorsRoot", "", -1),
	statePaths("currentSyncCommittee", "aggregatePubkey", -1), // aggregatePubkey is not needed but this is currently the cheapest way to get the committee root
	statePaths("nextSyncCommittee", "aggregatePubkey", -1),
}

func blockStatePaths(proofFormat byte, slot, parentSlotDiff uint64) []string {
	init := proofFormat&beacon.HspInitData != 0
	period := int((slot - parentSlotDiff) >> 13)
	rootCount := 1
	if parentSlotDiff > 2 {
		rootCount = int(parentSlotDiff - 1)
	}
	rootsPos := len(statePathsBase)
	if init {
		rootsPos += len(statePathsInit)
	}
	paths := make([]string, rootsPos+rootCount+1)
	copy(paths[:len(statePathsBase)], statePathsBase)
	if init {
		copy(paths[len(statePathsBase):rootsPos], statePathsInit)
	}
	for rootCount > 0 {
		slot--
		//paths[rootsPos] = statePaths("blockRoots", "", int(slot&0x1fff)) //TODO request all block roots?
		//rootsPos++
		paths[rootsPos] = statePaths("stateRoots", "", int(slot&0x1fff))
		rootsPos++
		rootCount--
	}
	paths[rootsPos] = statePaths("historicalRoots", "", period)
	/*paths[rootsPos] = statePaths("historicalRoots", "", period*2)
	rootsPos++
	paths[rootsPos] = statePaths("historicalRoots", "", period*2+1)*/
	return paths
}

func (bn *beaconNodeApiSource) getStateProof(stateRoot common.Hash, paths []string) (beacon.MultiProof, error) {
	url := bn.url + "/eth/v1/light_client/proof/" + stateRoot.Hex() + "?paths=" + paths[0]
	for i := 1; i < len(paths); i++ {
		url += "&paths=" + paths[i]
	}
	resp, err := http.Get(url)
	if err != nil {
		return beacon.MultiProof{}, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return beacon.MultiProof{}, err
	}
	//fmt.Println("paths", paths, "proof length", len(body))
	return beacon.ParseMultiProof(body)
}

const multiProofGroupSize = 64

func (bn *beaconNodeApiSource) getMultiStateProof(stateRoot common.Hash, pathCount int, pathFn func(int) string) (beacon.ProofReader, error) {
	var reader beacon.MergedReader
	for i := 0; i < pathCount; {
		paths := make([]string, 0, multiProofGroupSize)
		for {
			paths = append(paths, pathFn(i))
			i++
			if i == pathCount || len(paths) == multiProofGroupSize {
				break
			}
		}
		if proof, err := bn.getStateProof(stateRoot, paths); err == nil {
			reader = append(reader, proof.Reader(nil))
		} else {
			return nil, err
		}
	}
	return reader, nil
}

// assumes that ParentSlotDiff is already initialized
func (bn *beaconNodeApiSource) getBlockState(block *beacon.BlockData) error {
	proof, err := bn.getStateProof(block.StateRoot, blockStatePaths(block.ProofFormat, block.Header.Slot, block.ParentSlotDiff))
	if err != nil {
		return err
	}
	var stateDiffWriter, historicDiffWriter beacon.ProofWriter
	if block.ParentSlotDiff >= 2 {
		fmt.Println("StateRootsDiff", block.Header.Slot, block.ParentSlotDiff)
		firstDiffSlot := block.Header.Slot - block.ParentSlotDiff + 1
		block.StateRootDiffs = make(beacon.MerkleValues, block.ParentSlotDiff-1)
		stateDiffWriter = beacon.NewValueWriter(beacon.StateRootsRangeFormat(firstDiffSlot, block.Header.Slot-1, nil), block.StateRootDiffs, func(index uint64) int {
			if index < 0x2000 {
				return -1
			}
			i := int((index - firstDiffSlot) & 0x1fff)
			if i < int(block.ParentSlotDiff-1) {
				return i
			}
			return -1
		})
	}
	writer := beacon.ProofWriter(beacon.NewMultiProofWriter(beacon.StateProofFormats[block.ProofFormat], &block.StateProof, func(index uint64) beacon.ProofWriter {
		if index == beacon.BsiStateRoots {
			return stateDiffWriter
		}
		if index == beacon.BsiHistoricRoots {
			return historicDiffWriter
		}
		return nil
	}))
	fmt.Print("received format:")
	//printIndices(proof.format, 1)
	fmt.Print("expected format ", block.ProofFormat, ":")
	//printIndices(beacon.StateProofFormats[block.ProofFormat], 1)
	if root, ok := beacon.TraverseProof(proof.Reader(nil), writer); !ok || root != block.StateRoot {
		fmt.Println("invalid block state proof", ok, root, block.StateRoot, block.ProofFormat)
		return errors.New("invalid state proof")
	}
	fmt.Println("DIFFS", block.StateRootDiffs)
	return nil
}

func (bn *beaconNodeApiSource) GetBlocksFromHead(ctx context.Context, header beacon.Header, amount uint64) (blocks []*beacon.BlockData, err error) {
	blocks = make([]*beacon.BlockData, int(amount))
	blockPtr := int(amount)
	if err != nil {
		return nil, err
	}
	blockRoot := header.Hash()
	var firstSlot uint64
	if uint64(header.Slot) >= amount {
		firstSlot = uint64(header.Slot) + 1 - amount
	}
	for {
		blockPtr--
		block := &beacon.BlockData{
			Header: beacon.HeaderWithoutState{
				Slot:          uint64(header.Slot),
				ProposerIndex: uint(header.ProposerIndex),
				BodyRoot:      header.BodyRoot,
				ParentRoot:    header.ParentRoot,
			},
			StateRoot: header.StateRoot,
			BlockRoot: blockRoot,
		}
		blocks[blockPtr] = block
		if parentHeader, err := bn.getHeader(header.ParentRoot); err == nil {
			block.ParentSlotDiff = block.Header.Slot - uint64(parentHeader.Slot)
			block.ProofFormat = beacon.ProofFormatForBlock(block)
			if err := bn.getBlockState(block); err != nil {
				return nil, err
			}
			fmt.Println("GetBlocksFromHead", block.Header.Slot, block.ParentSlotDiff, block.StateRootDiffs)
			blockRoot = header.ParentRoot
			header = parentHeader
		} else {
			block.ProofFormat = beacon.HspFormatCount - 1
			if err := bn.getBlockState(block); err != nil {
				return nil, err
			}
			break
		}
		if uint64(header.Slot) < firstSlot {
			break
		}
	}
	blocks = blocks[blockPtr:]
	return
}

func (bn *beaconNodeApiSource) GetRootsProof(ctx context.Context, block *beacon.BlockData) (beacon.MultiProof, beacon.MultiProof, error) {
	blockRootsProof, err := bn.getSingleRootsProof(block, beacon.BsiBlockRoots, "blockRoots")
	if err != nil {
		return beacon.MultiProof{}, beacon.MultiProof{}, err
	}
	stateRootsProof, err := bn.getSingleRootsProof(block, beacon.BsiStateRoots, "stateRoots")
	if err != nil {
		return beacon.MultiProof{}, beacon.MultiProof{}, err
	}
	return blockRootsProof, stateRootsProof, nil
}

func (bn *beaconNodeApiSource) getSingleRootsProof(block *beacon.BlockData, leafIndex uint64, id string) (beacon.MultiProof, error) {
	reader, err := bn.getMultiStateProof(block.StateRoot, 0x2000, func(i int) string {
		return statePaths(id, "", i)
	})
	if err != nil {
		return beacon.MultiProof{}, err
	}
	var values, mv beacon.MerkleValues
	format := beacon.NewRangeFormat(0x2000, 0x3fff, nil)
	pw := beacon.NewMultiProofWriter(format, &values, nil)
	writer := beacon.NewMultiProofWriter(beacon.NewIndexMapFormat().AddLeaf(leafIndex, nil), &mv, func(index uint64) beacon.ProofWriter {
		if index == leafIndex {
			return pw
		}
		return nil
	})
	if root, ok := beacon.TraverseProof(reader, writer); !ok || root != block.StateRoot {
		fmt.Println("invalid state roots state proof", ok, root, block.StateRoot)
		return beacon.MultiProof{}, errors.New("invalid state proof")
	}
	return beacon.MultiProof{Format: format, Values: values}, nil
}

func (bn *beaconNodeApiSource) GetHistoricRootsProof(ctx context.Context, block *beacon.BlockData, period uint64) (beacon.MultiProof, error) {
	//proof, err := bn.getStateProof(block.StateRoot, []string{statePaths("historicalRoots", "", int(period*2)), statePaths("historicalRoots", "", int(period*2+1))})
	proof, err := bn.getStateProof(block.StateRoot, []string{statePaths("historicalRoots", "", int(period))})
	if err != nil {
		return beacon.MultiProof{}, err
	}
	var values, mv beacon.MerkleValues
	format := beacon.NewRangeFormat(0x2000000+period, 0x2000000+period, nil)
	pw := beacon.NewMultiProofWriter(format, &values, nil)
	writer := beacon.NewMultiProofWriter(beacon.NewIndexMapFormat().AddLeaf(beacon.BsiHistoricRoots, nil), &mv, func(index uint64) beacon.ProofWriter {
		if index == beacon.BsiHistoricRoots {
			return pw
		}
		return nil
	})
	if root, ok := beacon.TraverseProof(proof.Reader(nil), writer); !ok || root != block.StateRoot {
		fmt.Println("invalid historic roots state proof", ok, root, block.StateRoot)
		return beacon.MultiProof{}, errors.New("invalid state proof")
	}
	return beacon.MultiProof{Format: format, Values: values}, nil
}

func (bn *beaconNodeApiSource) getSyncCommittee(block *beacon.BlockData, leafIndex uint64, id string) ([]byte, error) {
	reader, err := bn.getMultiStateProof(block.StateRoot, 513, func(i int) string {
		if i == 512 {
			return statePaths(id, "aggregatePubkey", -1)
		}
		return statePaths(id, "pubkeys", i)
	})
	if err != nil {
		return nil, err
	}
	values := make(beacon.MerkleValues, 1026)
	format := beacon.MergedFormat{beacon.NewRangeFormat(0x800, 0xbff, nil), beacon.NewRangeFormat(6, 7, nil)}
	vw := beacon.NewValueWriter(format, values, func(index uint64) int {
		if index >= 6 && index <= 7 {
			return int(index + 1024 - 6)
		}
		if index < 0x800 {
			return -1
		}
		return int(index - 0x800)
	})
	mv := new(beacon.MerkleValues)
	writer := beacon.ProofWriter(beacon.NewMultiProofWriter(beacon.NewIndexMapFormat().AddLeaf(leafIndex, nil), mv, func(index uint64) beacon.ProofWriter {
		if index == leafIndex {
			return vw
		}
		return nil
	}))
	if root, ok := beacon.TraverseProof(reader, writer); !ok || root != block.StateRoot {
		fmt.Println("invalid sync committee state proof", ok, root, block.StateRoot)
		return nil, errors.New("invalid state proof")
	}
	committee := make([]byte, 513*48)
	for i, v := range values {
		fmt.Println("committee", i, v)
		i2 := i / 2
		if i&1 == 0 {
			copy(committee[i2*48:i2*48+32], v[:])
		} else {
			copy(committee[i2*48+32:i2*48+48], v[:16])
		}
	}
	return committee, nil
}

// checkpoint should be a recent block (no need to be on epoch boundary)
// empty hash: latest head
func (bn *beaconNodeApiSource) GetInitBlock(ctx context.Context, checkpoint common.Hash) (*beacon.BlockData, error) {
	header, err := bn.getHeader(checkpoint)
	if err != nil {
		return nil, err
	}
	block := &beacon.BlockData{
		Header: beacon.HeaderWithoutState{
			Slot:          uint64(header.Slot),
			ProposerIndex: uint(header.ProposerIndex),
			BodyRoot:      header.BodyRoot,
			ParentRoot:    header.ParentRoot,
		},
		StateRoot: header.StateRoot,
		BlockRoot: checkpoint,
	}
	block.ProofFormat = beacon.HspFormatCount - 1
	if err := bn.getBlockState(block); err != nil {
		return nil, err
	}
	return block, nil

	/*blocks, err := bn.GetBlocksFromHead(ctx, header, 1)
	if err != nil {
		return nil, err
	}
		committee, err := bn.getSyncCommittee(blocks[0], beacon.BsiSyncCommittee, "currentSyncCommittee")
		if err != nil {
			return nil, nil, nil, err
		}
		nextCommittee, err := bn.getSyncCommittee(blocks[0], beacon.BsiNextSyncCommittee, "nextSyncCommittee")
		if err != nil {
			return nil, nil, nil, err
		}
	return blocks[0], committee, nextCommittee, nil*/
}

type odrDataSource LightEthereum

func (od *odrDataSource) GetBlocks(ctx context.Context, head beacon.Header, lastSlot, amount uint64) (blocks []*beacon.BlockData, err error) {
	req := &light.BeaconDataRequest{
		Header:   head,
		LastSlot: lastSlot,
		Length:   amount,
		//TODO TailShortTerm
	}
	if err := od.odr.Retrieve(ctx, req); err != nil {
		return nil, err
	}
	return req.Blocks, nil
}

func (od *odrDataSource) GetBlocksFromHead(ctx context.Context, head beacon.Header, amount uint64) (blocks []*beacon.BlockData, err error) {
	req := &light.BeaconDataRequest{
		Header:   head,
		LastSlot: uint64(head.Slot),
		Length:   amount,
		//TODO TailShortTerm
	}
	if err := od.odr.Retrieve(ctx, req); err != nil {
		return nil, err
	}
	return req.Blocks, nil
}

func (od *odrDataSource) GetInitBlock(ctx context.Context, checkpoint common.Hash) (block *beacon.BlockData, err error) {
	req := &light.BeaconInitRequest{
		Checkpoint: checkpoint,
	}
	if err := od.odr.Retrieve(ctx, req); err != nil {
		return nil, err
	}
	return req.Block, nil
}

type sctServerPeer struct {
	peer      *peer
	retriever *retrieveManager
}

func (sp sctServerPeer) GetBestCommitteeProofs(ctx context.Context, req beacon.CommitteeRequest) (beacon.CommitteeReply, error) {
	reqID := rand.Uint64()
	var reply beacon.CommitteeReply
	r := &distReq{
		getCost: func(dp distPeer) uint64 {
			peer := dp.(*peer)
			return peer.getRequestCost(GetCommitteeProofsMsg, len(req.UpdatePeriods)+len(req.CommitteePeriods)*CommitteeCostFactor)
		},
		canSend: func(dp distPeer) bool {
			return dp.(*peer) == sp.peer
		},
		request: func(dp distPeer) func() {
			peer := dp.(*peer)
			cost := peer.getRequestCost(GetCommitteeProofsMsg, len(req.UpdatePeriods)+len(req.CommitteePeriods)*CommitteeCostFactor)
			peer.fcServer.QueuedRequest(reqID, cost)
			return func() {
				peer.requestCommitteeProofs(reqID, beacon.CommitteeRequest{
					UpdatePeriods:    req.UpdatePeriods,
					CommitteePeriods: req.CommitteePeriods,
				})
			}
		},
	}
	valFn := func(peer distPeer, msg *Msg) error {
		log.Debug("Validating committee proofs", "updatePeriods", req.UpdatePeriods, "committeePeriods", req.CommitteePeriods)
		if msg.MsgType != MsgCommitteeProofs {
			return errInvalidMessageType
		}
		reply = msg.Obj.(beacon.CommitteeReply)
		return nil
	}
	if err := sp.retriever.retrieve(ctx, reqID, r, valFn, nil); err == nil {
		return reply, nil
	} else {
		return beacon.CommitteeReply{}, err
	}
}

func (sp sctServerPeer) ClosedChannel() chan struct{} {
	return sp.peer.closeCh
}

func (sp sctServerPeer) WrongReply(description string) {
	if val := sp.peer.bumpInvalid(); val > maxResponseErrors {
		sp.peer.Peer.Disconnect(p2p.DiscProtocolError)
	}
}
