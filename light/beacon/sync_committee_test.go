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

package beacon

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/minio/sha256-simd"
)

var (
	testGenesis  = newTestGenesis()
	testGenesis2 = newTestGenesis()

	tfNormal = newTestForks(testGenesis, Forks{
		Fork{Epoch: 0, Version: []byte{0}},
	})
	tfAlternative = newTestForks(testGenesis, Forks{
		Fork{Epoch: 0, Version: []byte{0}},
		Fork{Epoch: 0x700, Version: []byte{1}},
	})
	tfAnotherGenesis = newTestForks(testGenesis2, Forks{
		Fork{Epoch: 0, Version: []byte{0}},
	})

	tcBase             = newTestChain(nil, testGenesis, tfNormal, true, 0, 9, 200, 400)
	tcLowParticipation = newTestChain(newTestChain(tcBase, testGenesis, tfNormal, true, 8, 14, 1000, 257), testGenesis, tfNormal, true, 15, 19, 1000, 100)
	tcFork             = newTestChain(tcBase, testGenesis, tfAlternative, true, 7, 9, 200, 400)
	tcAnotherGenesis   = newTestChain(nil, testGenesis2, tfAnotherGenesis, true, 0, 9, 200, 450)
	tcBetterUpdates2   = newTestChain(tcBase, testGenesis, tfNormal, false, 5, 7, 1000, 450)                // better signer participation from period 5 to 7
	tcBetterUpdates    = newTestChain(tcBase, testGenesis, tfNormal, false, 5, 7, finalizedTestUpdate, 400) // finalized updates from period 5 to 7 (stronger than the one above)
)

type sctTestCase []sctTestStep

type sctTestStep struct {
	periodTime float64 // slotTime uint64
	trackers   []sctTestTrackerStep
	sync       []sctTestTrackerSync
}

type sctTestTrackerSync struct {
	sourceTc       *testChain // nil if target is synced from another source tracker
	source, target int        // tracker index in the test setup; source is -1 if the target is synced from a testChain
	expFail        bool
}

type sctTestTrackerStep struct {
	forks           Forks
	signerThreshold int
	newTracker      bool // should always be true at first step and whenever forks/signerThreshold is changed
	// constraint
	constraintsTc                                                 *testChain
	constraintsFirst, constraintsAfterFixed, constraintsAfterLast uint64
	// exp result
	expTc                  *testChain
	expFirst, expAfterLast uint64
}

func TestSyncCommitteeTrackerConstraints(t *testing.T) {
	runSctTest(t, sctTestCase{
		{7.5, []sctTestTrackerStep{{tfNormal, 257, true, tcBase, 0, 9, 9, tcBase, 0, 8}, {tfNormal, 257, true, tcBase, 5, 6, 1000, tcBase, 5, 8}}, []sctTestTrackerSync{{tcBase, -1, 0, false}, {nil, 0, 1, false}}},
		{8.5, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 10, 10, tcBase, 0, 9}, {tfNormal, 257, false, tcBase, 5, 6, 1000, tcBase, 5, 9}}, []sctTestTrackerSync{{tcBase, -1, 0, false}, {nil, 0, 1, false}}},
		{9.5, []sctTestTrackerStep{{tfNormal, 257, true, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfNormal, 257, true, tcBase, 5, 6, 1000, tcBase, 5, 10}}, []sctTestTrackerSync{{tcBase, -1, 0, false}, {nil, 0, 1, false}}},
		{9.6, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfNormal, 257, false, tcBase, 0, 6, 1000, tcBase, 0, 10}}, []sctTestTrackerSync{{nil, 0, 1, false}}},
	})
}

func TestSyncCommitteeTrackerLowParticipation(t *testing.T) {
	runSctTest(t, sctTestCase{
		{9.5, []sctTestTrackerStep{{tfNormal, 257, true, tcLowParticipation, 0, 9, 9, tcLowParticipation, 0, 8}, {tfNormal, 300, true, tcBase, 5, 6, 1000, tcLowParticipation, 5, 8}}, []sctTestTrackerSync{{tcLowParticipation, -1, 0, false}, {nil, 0, 1, false}}},
		{11.5, []sctTestTrackerStep{{tfNormal, 257, false, tcLowParticipation, 0, 13, 13, tcLowParticipation, 0, 12}, {tfNormal, 300, false, tcBase, 5, 6, 1000, tcLowParticipation, 5, 8}}, []sctTestTrackerSync{{tcLowParticipation, -1, 0, false}, {nil, 0, 1, false}}},
		{11.6, []sctTestTrackerStep{{tfNormal, 257, false, tcLowParticipation, 0, 13, 13, tcLowParticipation, 0, 12}, {tfNormal, 257, true, tcBase, 5, 6, 1000, tcLowParticipation, 5, 12}}, []sctTestTrackerSync{{nil, 0, 1, false}}},
		{13.5, []sctTestTrackerStep{{tfNormal, 257, false, tcLowParticipation, 0, 16, 16, tcLowParticipation, 0, 14}, {tfNormal, 257, false, tcBase, 5, 6, 1000, tcLowParticipation, 5, 14}}, []sctTestTrackerSync{{tcLowParticipation, -1, 0, true}, {nil, 0, 1, false}}},
		{14.5, []sctTestTrackerStep{{tfNormal, 257, false, tcLowParticipation, 0, 16, 16, tcLowParticipation, 0, 15}, {tfNormal, 257, false, tcBase, 5, 6, 1000, tcLowParticipation, 5, 15}}, []sctTestTrackerSync{{tcLowParticipation, -1, 0, false}, {nil, 0, 1, false}}},
		{19.5, []sctTestTrackerStep{{tfNormal, 257, false, tcLowParticipation, 0, 21, 21, tcLowParticipation, 0, 15}, {tfNormal, 257, false, tcBase, 5, 6, 1000, tcLowParticipation, 5, 15}}, []sctTestTrackerSync{{tcLowParticipation, -1, 0, false}, {nil, 0, 1, false}}},
		{19.6, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfNormal, 257, false, tcBase, 5, 6, 1000, tcBase, 5, 10}}, []sctTestTrackerSync{{tcBase, -1, 0, false}, {nil, 0, 1, false}}},
	})
}

func TestSyncCommitteeTrackerFork(t *testing.T) {
	runSctTest(t, sctTestCase{
		{9.5, []sctTestTrackerStep{{tfNormal, 257, true, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfAlternative, 257, true, tcFork, 0, 11, 11, tcFork, 0, 10}, {tfNormal, 257, true, tcBase, 5, 6, 1000, tcBase, 5, 7}}, []sctTestTrackerSync{{tcBase, -1, 0, false}, {tcFork, -1, 1, false}, {nil, 1, 2, true}}},
		{9.6, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfAlternative, 257, false, tcFork, 0, 11, 11, tcFork, 0, 10}, {tfNormal, 257, false, tcBase, 5, 6, 1000, tcBase, 5, 10}}, []sctTestTrackerSync{{nil, 0, 2, false}}},
		{9.7, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfAlternative, 257, false, tcFork, 0, 11, 11, tcFork, 0, 10}, {tfAlternative, 257, true, tcFork, 5, 6, 1000, tcFork, 5, 7}}, []sctTestTrackerSync{}},
		{9.8, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfAlternative, 257, false, tcFork, 0, 11, 11, tcFork, 0, 10}, {tfAlternative, 257, true, tcFork, 5, 6, 1000, tcFork, 5, 10}}, []sctTestTrackerSync{{nil, 1, 2, false}}},
	})
}

func TestSyncCommitteeTrackerAnotherGenesis(t *testing.T) {
	runSctTest(t, sctTestCase{
		{9.5, []sctTestTrackerStep{{tfNormal, 257, true, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfAnotherGenesis, 257, true, tcAnotherGenesis, 0, 11, 11, tcAnotherGenesis, 0, 10}, {tfNormal, 257, true, tcBase, 5, 6, 1000, tcBase, 1, 0}}, []sctTestTrackerSync{{tcBase, -1, 0, false}, {tcAnotherGenesis, -1, 1, false}, {nil, 1, 0, true}, {nil, 1, 2, true}}},
		{9.6, []sctTestTrackerStep{{tfNormal, 257, true, tcBase, 0, 11, 11, tcBase, 0, 10}, {tfAnotherGenesis, 257, true, tcAnotherGenesis, 0, 11, 11, tcAnotherGenesis, 0, 10}, {tfNormal, 257, true, tcBase, 5, 6, 1000, tcBase, 5, 10}}, []sctTestTrackerSync{{nil, 0, 2, false}}},
	})
}

func TestSyncCommitteeTrackerBetterUpdates(t *testing.T) {
	runSctTest(t, sctTestCase{
		{9.5, []sctTestTrackerStep{{tfNormal, 257, true, tcBase, 2, 11, 11, tcBase, 2, 10}, {tfNormal, 257, true, tcBase, 0, 9, 9, tcBetterUpdates, 0, 8}, {tfNormal, 257, true, tcBase, 0, 9, 9, tcBetterUpdates2, 0, 8}}, []sctTestTrackerSync{{tcBase, -1, 0, false}, {tcBetterUpdates, -1, 1, false}, {tcBetterUpdates2, -1, 2, false}}},
		{9.6, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 11, 11, tcBetterUpdates, 0, 10}, {tfNormal, 257, false, tcBase, 0, 11, 11, tcBetterUpdates, 0, 10}, {tfNormal, 257, false, tcBase, 0, 11, 11, tcBetterUpdates2, 0, 10}}, []sctTestTrackerSync{{tcBetterUpdates, -1, 1, false}, {nil, 1, 0, false}, {nil, 0, 2, false}}},
		{9.7, []sctTestTrackerStep{{tfNormal, 257, false, tcBase, 0, 11, 11, tcBetterUpdates2, 0, 10}, {tfNormal, 257, false, tcBase, 0, 11, 11, tcBetterUpdates2, 0, 10}, {tfNormal, 257, false, tcBase, 0, 11, 11, tcBetterUpdates2, 0, 10}}, []sctTestTrackerSync{{nil, 2, 0, false}, {nil, 2, 1, false}}},
	})
}

func runSctTest(t *testing.T, testCase sctTestCase) {
	count := len(testCase[0].trackers)
	dbs := make([]*memorydb.Database, count)
	trackers := make([]*SyncCommitteeTracker, count)
	constraints := make([]*testConstraints, count)
	for i := range dbs {
		dbs[i] = memorydb.New()
	}
	clock := &mclock.Simulated{}
	var lastTime time.Duration
	for stepIndex, step := range testCase {
		tm := time.Duration(float64(time.Second*12*8192) * step.periodTime)
		clock.Run(tm - lastTime)
		lastTime = tm
		for i, ts := range step.trackers {
			if ts.newTracker {
				if trackers[i] != nil {
					trackers[i].Stop()
				}
				constraints[i] = &testConstraints{}
				trackers[i] = NewSyncCommitteeTracker(dbs[i], ts.forks, constraints[i], ts.signerThreshold, true, dummyVerifier{}, clock, func() int64 { return int64(clock.Now()) })
			}
			constraints[i].setRoots(ts.constraintsTc, ts.constraintsFirst, ts.constraintsAfterFixed, ts.constraintsAfterLast)
		}
		for syncIndex, ss := range step.sync {
			var failed bool
			if ss.sourceTc != nil {
				s := &tcSyncer{tc: ss.sourceTc}
				s.syncTracker(trackers[ss.target])
				failed = s.failed
			} else {
				s := &sctSyncer{sct: trackers[ss.source]}
				s.syncTracker(trackers[ss.target])
				failed = s.failed
			}
			if failed != ss.expFail {
				t.Errorf("Step %d sync %d result mismatch (got %v, expected %v)", stepIndex, syncIndex, failed, ss.expFail)
			}
		}
		// check resulting tracker state
		for i, ts := range step.trackers {
			sct := trackers[i]
			if ts.expFirst > 0 {
				if sct.GetBestUpdate(ts.expFirst-1) != nil {
					t.Errorf("Step %d tracker %d: update found in synced chain before the expected range (period %d)", stepIndex, i, ts.expFirst-1)
				}
			}
			for period := ts.expFirst; period < ts.expAfterLast; period++ {
				if update := sct.GetBestUpdate(period); update == nil {
					t.Errorf("Step %d tracker %d: update missing from synced chain (period %d)", stepIndex, i, period)
				} else if update.CalculateScore() != ts.expTc.periods[period].update.CalculateScore() {
					t.Errorf("Step %d tracker %d: wrong update found in synced chain (period %d)", stepIndex, i, period)
				}
			}
			for period := ts.expFirst; period <= ts.expAfterLast; period++ {
				if sct.GetSyncCommitteeRoot(period) != ts.expTc.periods[period].committeeRoot {
					t.Errorf("Step %d tracker %d: committee root mismatch in synced chain (period %d)", stepIndex, i, period)
				}
			}
			if sct.GetBestUpdate(ts.expAfterLast) != nil {
				t.Errorf("Step %d tracker %d: update found in synced chain after the expected range (period %d)", stepIndex, i, ts.expAfterLast)
			}
		}
	}
	for _, sct := range trackers {
		if sct != nil {
			sct.Stop()
		}
	}
}

func newTestGenesis() GenesisData {
	var genesisData GenesisData
	rand.Read(genesisData.GenesisValidatorsRoot[:])
	return genesisData
}

func newTestForks(genesisData GenesisData, forks Forks) Forks {
	forks.computeDomains(genesisData.GenesisValidatorsRoot)
	return forks
}

func newTestChain(parent *testChain, genesisData GenesisData, forks Forks, newCommittees bool, begin, end int, subPeriodIndex uint64, signerCount int) *testChain {
	tc := &testChain{
		genesisData: genesisData,
		forks:       forks,
	}
	if parent != nil {
		tc.periods = make([]testPeriod, len(parent.periods))
		copy(tc.periods, parent.periods)
	}
	if newCommittees {
		if begin == 0 {
			tc.fillCommittees(begin, end+1)
		} else {
			tc.fillCommittees(begin+1, end+1)
		}
	}
	tc.fillUpdates(begin, end, subPeriodIndex, signerCount)
	return tc
}

func makeTestHeaderWithSingleProof(slot, index uint64, value MerkleValue) (Header, MerkleValues) {
	var branch MerkleValues
	hasher := sha256.New()
	for index > 1 {
		var proofHash MerkleValue
		rand.Read(proofHash[:])
		hasher.Reset()
		if index&1 == 0 {
			hasher.Write(value[:])
			hasher.Write(proofHash[:])
		} else {
			hasher.Write(proofHash[:])
			hasher.Write(value[:])
		}
		hasher.Sum(value[:0])
		index /= 2
		branch = append(branch, proofHash)
	}
	return Header{Slot: slot, StateRoot: common.Hash(value)}, branch
}

func makeBitmask(signerCount int) []byte {
	bitmask := make([]byte, 64)
	for i := 0; i < 512; i++ {
		if rand.Intn(512-i) < signerCount {
			bitmask[i/8] += byte(1) << (i & 7)
			signerCount--
		}
	}
	return bitmask
}

type testPeriod struct {
	committee     dummySyncCommittee
	committeeRoot common.Hash
	update        LightClientUpdate
}

type testChain struct {
	periods     []testPeriod
	forks       Forks
	genesisData GenesisData
}

func (tc *testChain) makeTestSignedHead(header Header, signerCount int) SignedHead {
	bitmask := makeBitmask(signerCount)
	return SignedHead{
		Header:    header,
		BitMask:   bitmask,
		Signature: makeDummySignature(tc.periods[PeriodOfSlot(header.Slot+1)].committee, tc.forks.signingRoot(header), bitmask),
	}
}

const finalizedTestUpdate = 8191 // if subPeriodIndex == finalizedTestUpdate then a finalized update is generated

func (tc *testChain) makeTestUpdate(period, subPeriodIndex uint64, signerCount int) LightClientUpdate {
	var update LightClientUpdate
	update.NextSyncCommitteeRoot = tc.periods[period+1].committeeRoot
	if subPeriodIndex == finalizedTestUpdate {
		update.FinalizedHeader, update.NextSyncCommitteeBranch = makeTestHeaderWithSingleProof(PeriodStart(period)+100, BsiNextSyncCommittee, MerkleValue(update.NextSyncCommitteeRoot))
		update.Header, update.FinalityBranch = makeTestHeaderWithSingleProof(PeriodStart(period)+200, BsiFinalBlock, MerkleValue(update.FinalizedHeader.Hash()))
	} else {
		update.Header, update.NextSyncCommitteeBranch = makeTestHeaderWithSingleProof(PeriodStart(period)+subPeriodIndex, BsiNextSyncCommittee, MerkleValue(update.NextSyncCommitteeRoot))
	}
	signedHead := tc.makeTestSignedHead(update.Header, signerCount)
	update.SyncCommitteeBits, update.SyncCommitteeSignature = signedHead.BitMask, signedHead.Signature
	return update
}

func (tc *testChain) fillCommittees(begin, end int) {
	if len(tc.periods) <= end {
		tc.periods = append(tc.periods, make([]testPeriod, end+1-len(tc.periods))...)
	}
	for i := begin; i <= end; i++ {
		tc.periods[i].committee = randomDummySyncCommittee()
		tc.periods[i].committeeRoot = SerializedCommitteeRoot(serializeDummySyncCommittee(tc.periods[i].committee))
	}
}

func (tc *testChain) fillUpdates(begin, end int, subPeriodIndex uint64, signerCount int) {
	for i := begin; i <= end; i++ {
		tc.periods[i].update = tc.makeTestUpdate(uint64(i), subPeriodIndex, signerCount)
	}
}

type tcSyncer struct {
	tc     *testChain
	failed bool
}

func (s *tcSyncer) CanRequest(updateCount, committeeCount int) bool { return true }

func (s *tcSyncer) GetBestCommitteeProofs(ctx context.Context, req CommitteeRequest) (CommitteeReply, error) {
	reply := CommitteeReply{
		Updates:    make([]LightClientUpdate, len(req.UpdatePeriods)),
		Committees: make([][]byte, len(req.CommitteePeriods)),
	}
	for i, period := range req.UpdatePeriods {
		reply.Updates[i] = s.tc.periods[period].update
	}
	for i, period := range req.CommitteePeriods {
		reply.Committees[i] = serializeDummySyncCommittee(s.tc.periods[period].committee)
	}
	return reply, nil
}

func (s *tcSyncer) WrongReply(description string) {
	s.failed = true
}

func (tc *testChain) makeUpdateInfo(firstPeriod int) *UpdateInfo {
	u := &UpdateInfo{
		AfterLastPeriod: uint64(len(tc.periods) - 1),
		Scores:          make(UpdateScores, len(tc.periods)-firstPeriod-1),
	}
	for i := range u.Scores {
		u.Scores[i] = tc.periods[firstPeriod+i].update.CalculateScore()
	}
	return u
}

func (s *tcSyncer) syncTracker(sct *SyncCommitteeTracker) {
	<-sct.SyncWithPeer(s, s.tc.makeUpdateInfo(0))
}

type sctSyncer struct {
	sct    *SyncCommitteeTracker
	failed bool
}

func (s *sctSyncer) CanRequest(updateCount, committeeCount int) bool { return true }

func (s *sctSyncer) GetBestCommitteeProofs(ctx context.Context, req CommitteeRequest) (CommitteeReply, error) {
	reply := CommitteeReply{
		Updates:    make([]LightClientUpdate, len(req.UpdatePeriods)),
		Committees: make([][]byte, len(req.CommitteePeriods)),
	}
	for i, period := range req.UpdatePeriods {
		if u := s.sct.GetBestUpdate(period); u != nil {
			reply.Updates[i] = *u
		}
	}
	for i, period := range req.CommitteePeriods {
		reply.Committees[i] = s.sct.GetSerializedSyncCommittee(period, s.sct.GetSyncCommitteeRoot(period))
	}
	return reply, nil
}

func (s *sctSyncer) WrongReply(description string) {
	s.failed = true
}

func (s *sctSyncer) syncTracker(sct *SyncCommitteeTracker) {
	<-sct.SyncWithPeer(s, s.sct.GetUpdateInfo())
}

type testConstraints struct {
	committeeRoots   []common.Hash
	first, afterLast uint64

	genesisData    GenesisData
	initCallback   func(GenesisData)
	updateCallback func()
}

func (tcs *testConstraints) PeriodRange() (first, afterFixed, afterLast uint64) {
	return tcs.first, tcs.first + uint64(len(tcs.committeeRoots)), tcs.afterLast
}

func (tcs *testConstraints) CommitteeRoot(period uint64) (root common.Hash, matchAll bool) {
	if period < tcs.first || period >= tcs.afterLast {
		return common.Hash{}, false
	}
	if period >= tcs.first+uint64(len(tcs.committeeRoots)) {
		return common.Hash{}, true
	}
	return tcs.committeeRoots[period-tcs.first], false
}

func (tcs *testConstraints) SetCallbacks(initCallback func(GenesisData), updateCallback func()) {
	if tcs.genesisData == (GenesisData{}) {
		tcs.initCallback = initCallback
	} else {
		initCallback(tcs.genesisData)
	}
	tcs.updateCallback = updateCallback
}

func (tcs *testConstraints) setRoots(tc *testChain, first, afterFixed, afterLast uint64) {
	tcs.first, tcs.afterLast = first, afterLast
	tcs.committeeRoots = make([]common.Hash, int(afterFixed-first))
	for i := range tcs.committeeRoots {
		tcs.committeeRoots[i] = tc.periods[first+uint64(i)].committeeRoot
	}
	if tcs.genesisData == (GenesisData{}) {
		tcs.genesisData = tc.genesisData
		if tcs.initCallback != nil {
			tcs.initCallback(tcs.genesisData)
		}
	}
	if tcs.updateCallback != nil {
		tcs.updateCallback()
	}
}
