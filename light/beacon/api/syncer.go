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
	maxRequest        = 8
)

// CommitteeSyncer syncs committee updates and signed heads from BeaconLightApi to SyncCommitteeTracker
type CommitteeSyncer struct {
	api *BeaconLightApi

	genesisData         beacon.GenesisData
	checkpointPeriod    uint64
	checkpointCommittee []byte
	sct                 *beacon.SyncCommitteeTracker

	updateCache, committeeCache *lru.Cache
	headTriggerCh, closedCh     chan struct{}
}

// NewCommitteeSyncer creates a new CommitteeSyncer
// Note: genesisData is only needed when light syncing (using GetInitData for bootstrap)
func NewCommitteeSyncer(api *BeaconLightApi, genesisData beacon.GenesisData) *CommitteeSyncer {
	updateCache, _ := lru.New(maxRequest)
	committeeCache, _ := lru.New(maxRequest)
	return &CommitteeSyncer{
		api:            api,
		genesisData:    genesisData,
		headTriggerCh:  make(chan struct{}, 1),
		closedCh:       make(chan struct{}),
		updateCache:    updateCache,
		committeeCache: committeeCache,
	}
}

// Start starts the syncing of the given SyncCommitteeTracker
func (cs *CommitteeSyncer) Start(sct *beacon.SyncCommitteeTracker) {
	cs.sct = sct
	sct.SyncWithPeer(cs, nil)
	go cs.headPollLoop()
}

// Stop stops the syncing process
func (cs *CommitteeSyncer) Stop() {
	cs.sct.Disconnect(cs)
	close(cs.headTriggerCh)
	<-cs.closedCh
}

// headPollLoop polls the signed head update endpoint and feeds signed heads into the tracker.
// Twice each sync period (not long before the end of the period and after the end of each period)
// it queries the best available committee update and advertises it to the tracker.
func (cs *CommitteeSyncer) headPollLoop() {
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
			if head, err := cs.api.GetHeadUpdate(); err == nil {
				if !head.Equal(&lastHead) {
					cs.updateCache.Purge()
					cs.committeeCache.Purge()
					if cs.sct.NextPeriod() > head.Header.SyncPeriod() {
						cs.sct.AddSignedHeads(cs, []beacon.SignedHead{head})
					}
					lastHead = head
					if head.Header.Slot >= nextAdvertiseSlot {
						lastPeriod := beacon.PeriodOfSlot(head.Header.Slot - 1)
						if cs.advertiseUpdates(lastPeriod) {
							nextAdvertiseSlot = beacon.PeriodStart(lastPeriod + 1)
							if head.Header.Slot >= nextAdvertiseSlot {
								nextAdvertiseSlot += 8000
							}
						}
					}
				}
				if counter < headPollCount {
					timer.Reset(headPollFrequency)
					timerStarted = true
				}
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

// advertiseUpdates queries committee updates that the tracker does not have or might have
// improved since the last query and advertises them to the tracker. The tracker might then
// fetch the actual updates and committees via GetBestCommitteeProofs.
func (cs *CommitteeSyncer) advertiseUpdates(lastPeriod uint64) bool {
	nextPeriod := cs.sct.NextPeriod()
	if nextPeriod < 1 {
		nextPeriod = 1 // we are advertising the range starting from nextPeriod-1
	}
	if nextPeriod-1 > lastPeriod {
		return true
	}
	updateInfo := &beacon.UpdateInfo{
		AfterLastPeriod: lastPeriod + 1,
		Scores:          make(beacon.UpdateScores, int(lastPeriod+2-nextPeriod)),
	}
	ptr := len(updateInfo.Scores)
	for period := lastPeriod; period+1 >= nextPeriod; period-- {
		if update, err := cs.getBestUpdate(period); err == nil {
			ptr--
			updateInfo.Scores[ptr] = update.CalculateScore()
		} else {
			break
		}
		if period == 0 {
			break
		}
	}
	updateInfo.Scores = updateInfo.Scores[ptr:]
	if len(updateInfo.Scores) == 0 {
		log.Error("Could not fetch last committee update")
		return false
	}
	cs.sct.SyncWithPeer(cs, updateInfo)
	return true
}

// GetBestCommitteeProofs fetches updates and committees for the specified periods
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

// CanRequest returns true if a request for the given amount of items can be processed
func (cs *CommitteeSyncer) CanRequest(updateCount, committeeCount int) bool {
	return updateCount <= maxRequest && committeeCount <= maxRequest
}

// getBestUpdate returns the best update for the given period
func (cs *CommitteeSyncer) getBestUpdate(period uint64) (beacon.LightClientUpdate, error) {
	if c, _ := cs.updateCache.Get(period); c != nil {
		return c.(beacon.LightClientUpdate), nil
	}
	update, _, err := cs.getBestUpdateAndCommittee(period)
	return update, err
}

// getCommittee returns the committee for the given period
// Note: cannot return committee altair fork period; this is always same as the committee of the next period
func (cs *CommitteeSyncer) getCommittee(period uint64) ([]byte, error) {
	if period == 0 {
		return nil, errors.New("no committee available for period 0")
	}
	if cs.checkpointCommittee != nil && period == cs.checkpointPeriod {
		return cs.checkpointCommittee, nil
	}
	if c, _ := cs.committeeCache.Get(period); c != nil {
		return c.([]byte), nil
	}
	_, committee, err := cs.getBestUpdateAndCommittee(period - 1)
	return committee, err
}

// getBestUpdateAndCommittee fetches the best update for period and corresponding committee
// for period+1 and caches the results until a new head is received by headPollLoop
func (cs *CommitteeSyncer) getBestUpdateAndCommittee(period uint64) (beacon.LightClientUpdate, []byte, error) {
	update, committee, err := cs.api.GetBestUpdateAndCommittee(period)
	if err != nil {
		return beacon.LightClientUpdate{}, nil, err
	}
	cs.updateCache.Add(period, update)
	cs.committeeCache.Add(period+1, committee)
	return update, committee, nil
}

// GetInitData fetches the bootstrap data and returns LightClientInitData (the corresponding
// committee is stored so that a subsequent GetBestCommitteeProofs can return it when requested)
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

// WrongReply is called by the tracker when the BeaconLightApi has provided wrong committee updates or signed heads
func (cs *CommitteeSyncer) WrongReply(description string) {
	log.Error("Beacon node API data source delivered wrong reply", "error", description)
}
