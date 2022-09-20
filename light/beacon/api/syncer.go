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

package api

import (
	"context"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/light/beacon"
	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
)

const (
	headPollFrequency = time.Millisecond * 200
	headPollCount     = 50
)

type CommitteeSyncer struct {
	api *RestApi

	genesisData         beacon.GenesisData
	checkpointPeriod    uint64
	checkpointCommittee []byte
	sct                 *beacon.SyncCommitteeTracker

	updateCache, committeeCache *lru.Cache
	headTriggerCh, closedCh     chan struct{}
}

// genesisData is only needed when light syncing (using GetInitData for bootstrap)
func NewCommitteeSyncer(api *RestApi, genesisData beacon.GenesisData) *CommitteeSyncer {
	updateCache, _ := lru.New(beacon.MaxCommitteeUpdateFetch)
	committeeCache, _ := lru.New(beacon.MaxCommitteeUpdateFetch / beacon.CommitteeCostFactor)
	return &CommitteeSyncer{
		api:            api,
		genesisData:    genesisData,
		headTriggerCh:  make(chan struct{}, 1),
		closedCh:       make(chan struct{}),
		updateCache:    updateCache,
		committeeCache: committeeCache,
	}
}

func (cs *CommitteeSyncer) Start(sct *beacon.SyncCommitteeTracker) {
	cs.sct = sct
	sct.SyncWithPeer(cs, nil)
	go cs.headPollLoop()
}

func (cs *CommitteeSyncer) TriggerNewHead() {
	cs.updateCache.Purge()
	cs.committeeCache.Purge()

	select {
	case cs.headTriggerCh <- struct{}{}:
	default:
	}
}

func (cs *CommitteeSyncer) Stop() {
	cs.sct.Disconnect(cs)
	close(cs.headTriggerCh)
	<-cs.closedCh
}

func (cs *CommitteeSyncer) headPollLoop() {
	//fmt.Println("Started headPollLoop()")
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
			if head, err := cs.api.GetHeadUpdate(); err == nil {
				//fmt.Println(" headPollLoop head update for slot", head.Header.Slot, nextAdvertiseSlot)
				if !head.Equal(&lastHead) {
					//fmt.Println("head poll: new head", head.Header.Slot, nextAdvertiseSlot)
					cs.sct.AddSignedHeads(cs, []beacon.SignedHead{head})
					lastHead = head
					if uint64(head.Header.Slot) >= nextAdvertiseSlot {
						lastPeriod := uint64(head.Header.Slot-1) >> 13
						if cs.advertiseUpdates(lastPeriod) {
							nextAdvertiseSlot = (lastPeriod + 1) << 13
							if uint64(head.Header.Slot) >= nextAdvertiseSlot {
								nextAdvertiseSlot += 8000
							}
						}
					}
				}
				if counter < headPollCount {
					//counter++
					timer.Reset(headPollFrequency)
					timerStarted = true
				}
			} else {
				//fmt.Println(" getHeadUpdate failed:", err)
			}
		case _, ok := <-cs.headTriggerCh:
			if !ok {
				timer.Stop()
				close(cs.closedCh)
				return
			}
			if !timerStarted {
				timer.Reset(headPollFrequency)
				timerStarted = true
			}
			counter = 0
		}
	}
}

func (cs *CommitteeSyncer) advertiseUpdates(lastPeriod uint64) bool {
	nextPeriod := cs.sct.NextPeriod()
	if nextPeriod < 1 {
		nextPeriod = 1 // we are advertising the range starting from nextPeriod-1
	}
	//fmt.Println("advertiseUpdates", nextPeriod, lastPeriod)
	if nextPeriod-1 > lastPeriod {
		return true
	}
	updateInfo := &beacon.UpdateInfo{
		AfterLastPeriod: lastPeriod + 1,
		Scores:          make([]byte, int(lastPeriod+2-nextPeriod)*3),
	}
	var ptr int
	for period := nextPeriod - 1; period <= lastPeriod; period++ {
		if update, err := cs.getBestUpdate(period); err == nil {
			update.CalculateScore().Encode(updateInfo.Scores[ptr : ptr+3])
		} else {
			log.Error("Error retrieving best committee update from API", "period", period, "error", err)
		}
		ptr += 3
	}
	//fmt.Println("advertiseUpdates", lastPeriod, updateInfo, len(updateInfo.Scores)/3)
	cs.sct.SyncWithPeer(cs, updateInfo)
	return true
}

func (cs *CommitteeSyncer) GetBestCommitteeProofs(ctx context.Context, req beacon.CommitteeRequest) (beacon.CommitteeReply, error) {
	reply := beacon.CommitteeReply{
		Updates:    make([]beacon.LightClientUpdate, len(req.UpdatePeriods)),
		Committees: make([][]byte, len(req.CommitteePeriods)),
	}
	var err error
	for i, period := range req.UpdatePeriods {
		if reply.Updates[i], err = cs.getBestUpdate(period); err != nil {
			return beacon.CommitteeReply{}, err
		}
	}
	for i, period := range req.CommitteePeriods {
		if reply.Committees[i], err = cs.getCommittee(period); err != nil {
			return beacon.CommitteeReply{}, err
		}
	}
	return reply, nil
}

func (cs *CommitteeSyncer) getBestUpdate(period uint64) (beacon.LightClientUpdate, error) {
	if c, _ := cs.updateCache.Get(period); c != nil {
		return c.(beacon.LightClientUpdate), nil
	}
	update, _, err := cs.getBestUpdateAndCommittee(period)
	return update, err
}

func (cs *CommitteeSyncer) getCommittee(period uint64) ([]byte, error) {
	if cs.checkpointCommittee != nil && period == cs.checkpointPeriod {
		return cs.checkpointCommittee, nil
	}
	if c, _ := cs.committeeCache.Get(period); c != nil {
		return c.([]byte), nil
	}
	_, committee, err := cs.getBestUpdateAndCommittee(period - 1)
	return committee, err
}

func (cs *CommitteeSyncer) getBestUpdateAndCommittee(period uint64) (beacon.LightClientUpdate, []byte, error) {
	update, committee, err := cs.api.GetBestUpdateAndCommittee(period)
	if err != nil {
		return beacon.LightClientUpdate{}, nil, err
	}
	cs.updateCache.Add(period, update)
	cs.committeeCache.Add(period+1, committee)
	return update, committee, nil
}

func (cs *CommitteeSyncer) GetInitData(ctx context.Context, checkpoint common.Hash) (beacon.Header, beacon.LightClientInitData, error) {
	if cs.genesisData == (beacon.GenesisData{}) {
		return beacon.Header{}, beacon.LightClientInitData{}, errors.New("missing genesis data")
	}
	header, checkpointData, committee, err := cs.api.GetCheckpointData(ctx, checkpoint)
	if err != nil {
		return beacon.Header{}, beacon.LightClientInitData{}, err
	}
	cs.checkpointPeriod, cs.checkpointCommittee = checkpointData.Period, committee
	return header, beacon.LightClientInitData{GenesisData: cs.genesisData, CheckpointData: checkpointData}, nil
}

func (cs *CommitteeSyncer) ClosedChannel() chan struct{} {
	return cs.closedCh
}

func (cs *CommitteeSyncer) WrongReply(description string) {
	log.Error("Beacon node API data source delivered wrong reply", "error", description)
}
