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
	"encoding/binary"
	"math"
	"time"

	"github.com/ethereum/go-ethereum/log"
)

const (
	MaxUpdateInfoLength     = 128
	broadcastFrequencyLimit = time.Millisecond * 200
	advertiseDelay          = time.Second * 10
)

// CommitteeRequest represents a request for fetching updates and committees at the given periods
type CommitteeRequest struct {
	UpdatePeriods    []uint64 // list of periods where LightClientUpdates are requested (not including full sync committee, only the root)
	CommitteePeriods []uint64 // list of periods where sync committees are requested
}

// empty returns true if the request does not request anything
func (req CommitteeRequest) empty() bool {
	return req.UpdatePeriods == nil && req.CommitteePeriods == nil
}

// CommitteeReply is an answer to a CommitteeRequest, contains the updates and committees corresponding
// to the period numbers in the request in the same order
type CommitteeReply struct {
	Updates    []LightClientUpdate // list of requested LightClientUpdates
	Committees [][]byte            // list of requested sync committees in serialized form
}

// sctClient represents a peer that SyncCommitteeTracker sends signed heads and sync committee advertisements to
type sctClient interface {
	SendSignedHeads(heads []SignedHead)
	SendUpdateInfo(updateInfo *UpdateInfo)
}

// sctServer represents a peer that SyncCommitteeTracker can request sync committee update proofs from
type sctServer interface {
	GetBestCommitteeProofs(ctx context.Context, req CommitteeRequest) (CommitteeReply, error)
	CanRequest(updateCount, committeeCount int) bool
	WrongReply(description string)
}

// UpdateInfo contains scores for an advertised update chain. Note that the most recent updates are always
// advertised but earliest ones might not because of length limitation.
type UpdateInfo struct {
	AfterLastPeriod uint64       // first period not covered by Scores
	Scores          UpdateScores // Scores[i] is the UpdateScore of period AfterLastPeriod-len(Scores)+i
}

// UpdateScore allows the comparison between updates at the same period in order to find the best
// update chain that provides the strongest proof of being canonical.
//
// UpdateScores have a tightly packed binary encoding format for efficient p2p protocol transmission:
// Each UpdateScore is encoded in 3 bytes. When interpreted as a 24 bit little indian unsigned integer:
//  - the lowest 10 bits contain the number of signers in the header signature aggregate
//  - the next 13 bits contain the "sub-period index" which is he signed header's slot modulo 8192 (which
//    is correlated with the risk of the chain being re-orged before the previous period boundary in case
//    of non-finalized updates)
//  - the highest bit is set when the update is finalized (meaning that the finality header referenced by
//    the signed header is in the same period as the signed header, making reorgs before the period
//    boundart impossible
type UpdateScore struct {
	signerCount     uint32 // number of signers in the header signature aggregate
	subPeriodIndex  uint32 // signed header's slot modulo 8192
	finalizedHeader bool   // update is considered finalized if has finalized header from the same period and 2/3 signatures
}

type UpdateScores []UpdateScore

// isFinalized returns true if the update has a header signed by at least 2/3 of the committee,
// referring to a finalized header that refers to the next sync committee. This condition is a
// close approximation of the actual finality condition that can only be verified by full beacon nodes.
func (u *UpdateScore) isFinalized() bool {
	return u.finalizedHeader && u.signerCount > 341
}

// reorgRiskPenalty returns a modifier that approximates the risk of a non-finalized update header being reorged.
// It should be subtracted from the signer participation count when comparing non-finaized updates.
// This risk is assumed to be dropping exponentially as the update header is further away from the beginning of the period.
func (u *UpdateScore) reorgRiskPenalty() uint32 {
	return uint32(math.Pow(2, 10-float64(u.subPeriodIndex)/32))
}

// betterThan returns true if update u is considered better than w.
func (u *UpdateScore) betterThan(w UpdateScore) bool {
	uFinalized, wFinalized := u.isFinalized(), w.isFinalized()
	if uFinalized || wFinalized {
		// finalized update is always better than non-finalized; only signer count matters when both are finalized
		if uFinalized && wFinalized {
			return u.signerCount > w.signerCount
		}
		return uFinalized
	}
	// take reorg risk into account when comparing two non-finalized updates
	return u.signerCount+w.reorgRiskPenalty() > w.signerCount+u.reorgRiskPenalty()
}

// Encode encodes the update score data in a compact form for in-protocol advertisement.
func (u *UpdateScore) Encode(data []byte) {
	v := u.signerCount + u.subPeriodIndex<<10
	if u.finalizedHeader {
		v += 0x800000
	}
	var enc [4]byte
	binary.LittleEndian.PutUint32(enc[:], v)
	copy(data, enc[:3])
}

// Decode decodes the update score data
func (u *UpdateScore) Decode(data []byte) {
	var enc [4]byte
	copy(enc[:3], data)
	v := binary.LittleEndian.Uint32(enc[:])
	u.signerCount = v & 0x3ff
	u.subPeriodIndex = (v >> 10) & 0x1fff
	u.finalizedHeader = (v & 0x800000) != 0
}

// SyncWithPeer starts or updates the syncing process with a given peer, based on the advertised
// update scores.
// Note that calling with remoteInfo == nil does not start syncing but allows attempting the init
// process with the given peer if not initialized yet.
func (s *SyncCommitteeTracker) SyncWithPeer(peer sctServer, remoteInfo *UpdateInfo) chan struct{} {
	s.lock.Lock()
	sp := s.connected[peer]
	if sp == nil {
		sp = &sctPeerInfo{peer: peer}
		s.connected[peer] = sp
	}
	if remoteInfo != nil {
		sp.remoteInfo = *remoteInfo
		sp.forkPeriod = remoteInfo.AfterLastPeriod
		if !sp.queued && !sp.requesting {
			s.requestQueue = append(s.requestQueue, sp)
			sp.queued = true
			sp.doneSyncing = make(chan struct{})
			select {
			case s.triggerCh <- struct{}{}:
			default:
			}
		}
	}
	doneSyncing := sp.doneSyncing
	s.lock.Unlock()
	return doneSyncing
}

// Disconnect notifies the tracker about a peer being disconnected
func (s *SyncCommitteeTracker) Disconnect(peer sctServer) {
	s.lock.Lock()
	delete(s.connected, peer)
	s.lock.Unlock()
}

// retrySyncAllPeers re-triggers the syncing process (check if there is something new to request)
// with all connected peers. Should be called when constraints are updated and might allow syncing further.
func (s *SyncCommitteeTracker) retrySyncAllPeers() {
	for _, sp := range s.connected {
		if !sp.queued && !sp.requesting {
			s.requestQueue = append(s.requestQueue, sp)
			sp.queued = true
			sp.doneSyncing = make(chan struct{})
		}
	}
	select {
	case s.triggerCh <- struct{}{}:
	default:
	}
}

// Stop stops the syncing/propagation process and shuts down the tracker
func (s *SyncCommitteeTracker) Stop() {
	close(s.stopCh)
}

// sctPeerInfo is the state of the syncing process from an individual server peer
type sctPeerInfo struct {
	peer               sctServer
	remoteInfo         UpdateInfo
	requesting, queued bool
	forkPeriod         uint64
	deferredHeads      []SignedHead
	doneSyncing        chan struct{}
}

// syncLoop is the global syncing loop starting requests to all peers where there is
// something to sync according to the most recent advertisement.
func (s *SyncCommitteeTracker) syncLoop() {
	s.lock.Lock()
	for {
		if len(s.requestQueue) > 0 {
			sp := s.requestQueue[0]
			s.requestQueue = s.requestQueue[1:]
			if len(s.requestQueue) == 0 {
				s.requestQueue = nil
			}
			sp.queued = false
			if s.startRequest(sp) {
				s.lock.Unlock()
				select {
				case <-s.triggerCh:
				case <-s.clock.After(time.Second):
				case <-s.stopCh:
					return
				}
				s.lock.Lock()
			}
		} else {
			s.lock.Unlock()
			select {
			case <-s.triggerCh:
			case <-s.stopCh:
				return
			}
			s.lock.Lock()
		}
	}
}

// startRequest sends a new request to the given peer if there is anything to request; finishes the syncing
// otherwise (processes deferred signed head advertisements and closes the doneSyncing channel).
// Returns true if a new request has been sent.
func (s *SyncCommitteeTracker) startRequest(sp *sctPeerInfo) bool {
	req := s.nextRequest(sp)
	if req.empty() {
		if sp.deferredHeads != nil {
			s.addSignedHeads(sp.peer, sp.deferredHeads)
			sp.deferredHeads = nil
		}
		close(sp.doneSyncing)
		sp.doneSyncing = nil
		return false
	}
	sp.requesting = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*20)
		reply, err := sp.peer.GetBestCommitteeProofs(ctx, req) // expected to return with error in case of shutdown
		cancel()
		if err != nil {
			s.lock.Lock()
			sp.requesting = false
			close(sp.doneSyncing)
			sp.doneSyncing = nil
			select {
			case s.triggerCh <- struct{}{}: // trigger next request
			default:
			}
			s.lock.Unlock()
			return
		}
		s.lock.Lock()
		sp.requesting = false
		if s.processReply(sp, req, reply) {
			s.requestQueue = append(s.requestQueue, sp)
			sp.queued = true
		} else {
			sp.peer.WrongReply("Invalid committee proof")
			close(sp.doneSyncing)
			sp.doneSyncing = nil
		}
		select {
		case s.triggerCh <- struct{}{}: // trigger next request
		default:
		}
		s.lock.Unlock()
	}()
	return true
}

// nextRequest creates the next request to be sent to the given peer, based on the difference between the
// remote advertised and the local update chains.
func (s *SyncCommitteeTracker) nextRequest(sp *sctPeerInfo) CommitteeRequest {
	if sp.remoteInfo.AfterLastPeriod < uint64(len(sp.remoteInfo.Scores)) {
		return CommitteeRequest{}
	}
	localInfo := s.getUpdateInfo()
	remoteFirst, remoteAfterLast := sp.remoteInfo.AfterLastPeriod-uint64(len(sp.remoteInfo.Scores)), sp.remoteInfo.AfterLastPeriod
	constraintsFirst, constraintsAfterFixed, constraintsAfterLast := s.constraints.PeriodRange()

	if constraintsAfterLast <= constraintsFirst || constraintsAfterFixed <= constraintsFirst {
		return CommitteeRequest{}
	}
	if s.chainInit && (constraintsAfterFixed <= s.firstPeriod || constraintsFirst > s.nextPeriod) {
		log.Error("Gap between local updates and fixed committee constraints, cannot sync", "local first", s.firstPeriod, "local afterLast", s.nextPeriod, "constraints first", constraintsFirst, "constraints afterFixed", constraintsAfterFixed)
		return CommitteeRequest{}
	}

	// committee period range -> update period range
	syncFirst, syncAfterFixed, syncAfterLast := constraintsFirst, constraintsAfterFixed-1, constraintsAfterLast-1
	// find intersection with remote advertised update range
	if remoteFirst > syncFirst {
		syncFirst = remoteFirst
	}
	if remoteAfterLast < syncAfterFixed {
		syncAfterFixed = remoteAfterLast
	}
	if remoteAfterLast < syncAfterLast {
		syncAfterLast = remoteAfterLast
	}
	if syncAfterLast < syncFirst {
		return CommitteeRequest{}
	}

	var request CommitteeRequest
	localFirst, localAfterLast := s.firstPeriod, s.nextPeriod
	if sp.forkPeriod < localAfterLast {
		localAfterLast = sp.forkPeriod
	}
	localInfoFirst := localInfo.AfterLastPeriod - uint64(len(localInfo.Scores))
	if !s.chainInit {
		request.CommitteePeriods = []uint64{syncAfterFixed}
		localFirst, localAfterLast = syncAfterFixed, syncAfterFixed
	}

	// shared range: both local and remote chain has updates; fetch only if remote score is better
	sharedFirst, sharedAfterLast := localFirst, localAfterLast
	if localInfoFirst > sharedFirst {
		sharedFirst = localInfoFirst
	}
	if remoteFirst > sharedFirst {
		sharedFirst = remoteFirst
	}
	if syncAfterLast < sharedAfterLast {
		sharedAfterLast = syncAfterLast
	}
	for period := sharedFirst; period < sharedAfterLast; period++ {
		if !sp.peer.CanRequest(len(request.UpdatePeriods)+1, len(request.CommitteePeriods)) {
			break
		}
		localScore := localInfo.Scores[period-localFirst]
		remoteScore := sp.remoteInfo.Scores[period-remoteFirst]
		if remoteScore.betterThan(localScore) {
			request.UpdatePeriods = append(request.UpdatePeriods, period)
		}
	}

	// future range: fetch update and next committee as long as remote score reaches required minimum
	for period := sharedAfterLast; period < syncAfterLast; period++ {
		if !sp.peer.CanRequest(len(request.UpdatePeriods)+1, len(request.CommitteePeriods)+1) {
			break // cannot fetch update + committee any more
		}
		// Note: we might try syncing before remote advertised range here is local known chain head is older than that; in this case we skip
		// score check here and hope for the best (will be checked by processReply later; we drop the peer as useless if it cannot serve us)
		if period >= remoteFirst {
			remoteScore := sp.remoteInfo.Scores[period-remoteFirst]
			if s.minimumUpdateScore.betterThan(remoteScore) {
				break // do not sync further if advertised score is less than our minimum requirement
			}
		}
		request.UpdatePeriods = append(request.UpdatePeriods, period)
		request.CommitteePeriods = append(request.CommitteePeriods, period+1)
	}

	// past range: fetch update and committee for periods before the locally stored range that are covered by the constraints (known committee roots)
	for nextPeriod := localFirst; nextPeriod > constraintsFirst && nextPeriod > remoteFirst; nextPeriod-- { // loop variable is nextPeriod == period+1 to avoid uint64 underflow
		if !sp.peer.CanRequest(len(request.UpdatePeriods)+1, len(request.CommitteePeriods)+1) {
			break // cannot fetch update + committee any more
		}
		period := nextPeriod - 1
		if period > remoteAfterLast {
			break
		}
		remoteScore := sp.remoteInfo.Scores[period-remoteFirst]
		if s.minimumUpdateScore.betterThan(remoteScore) {
			break // do not sync further if advertised score is less than our minimum requirement
		}
		// Note: updates are available from localFirst to localAfterLast-1 while committees are available from localFirst to localAfterLast
		// so we extend backwards by requesting updates and committees for the same period (committee for localFirst should be available or
		// requested here already so update for localFirst-1 can always be inserted if it matches our chain)
		request.UpdatePeriods = append(request.UpdatePeriods, period)
		request.CommitteePeriods = append(request.CommitteePeriods, period)
	}
	return request
}

// processReply processes the reply to a previous request, verifying received updates and committees
// and extending/improving the local update chain if possible.
func (s *SyncCommitteeTracker) processReply(sp *sctPeerInfo, sentRequest CommitteeRequest, reply CommitteeReply) bool {
	if len(reply.Updates) != len(sentRequest.UpdatePeriods) || len(reply.Committees) != len(sentRequest.CommitteePeriods) {
		return false
	}

	futureCommittees := make(map[uint64][]byte)
	var (
		storedCommittee     bool
		lastStoredCommittee uint64
	)
	for i, c := range reply.Committees {
		if len(c) != 513*48 {
			return false
		}
		period := sentRequest.CommitteePeriods[i]
		if len(sentRequest.UpdatePeriods) == 0 || period <= sentRequest.UpdatePeriods[0] {
			if root := SerializedCommitteeRoot(c); root != s.getSyncCommitteeRoot(period) {
				return false
			} else {
				s.storeSerializedSyncCommittee(period, root, c)
				if !storedCommittee || period > lastStoredCommittee {
					storedCommittee, lastStoredCommittee = true, period
				}
			}
		} else {
			futureCommittees[period] = c
		}
	}

	if !s.chainInit {
		// chain not initialized
		if storedCommittee {
			s.firstPeriod, s.nextPeriod, s.chainInit = lastStoredCommittee, lastStoredCommittee, true
			s.updateInfoChanged()
		} else {
			return false
		}
	}

	firstPeriod := sp.remoteInfo.AfterLastPeriod - uint64(len(sp.remoteInfo.Scores))
	for i, update := range reply.Updates {
		update := update // updates are cached by reference, do not overwrite
		period := update.Header.SyncPeriod()
		if period != sentRequest.UpdatePeriods[i] {
			return false
		}
		if period > s.nextPeriod { // a previous insertUpdate could have reduced nextPeriod since the request was created
			continue // skip but do not fail because it is not the remote side's fault; retry with new request
		}

		var remoteInfoScore UpdateScore
		if period >= firstPeriod {
			remoteInfoScore = sp.remoteInfo.Scores[period-firstPeriod]
		} else {
			remoteInfoScore = s.minimumUpdateScore
		}
		update.CalculateScore()
		if remoteInfoScore.betterThan(update.score) {
			return false // remote did not deliver an update with the promised score
		}

		switch s.insertUpdate(&update, futureCommittees[period+1]) {
		case sciSuccess:
			if sp.forkPeriod == period {
				// if local chain is successfully updated to the remote fork then remote is not on a different fork anymore
				sp.forkPeriod = sp.remoteInfo.AfterLastPeriod
			}
		case sciWrongUpdate:
			return false
		case sciNeedCommittee:
			// remember that remote is on a different and more valuable fork; do not fail but construct next request accordingly
			sp.forkPeriod = period
			return true //continue
		case sciUnexpectedError:
			// local error, insertUpdate has already printed an error log
			return false // though not the remote's fault, fail here to avoid infinite retries
		}
	}
	return true
}

// NextPeriod returns the next update period to be synced (the period after the last update if there
// are updates or the first period fixed by the constraints if there are no updates yet)
func (s *SyncCommitteeTracker) NextPeriod() uint64 {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.nextPeriod == 0 {
		first, _, _ := s.constraints.PeriodRange()
		return first
	}
	return s.nextPeriod
}

// GetUpdateInfo returns and UpdateInfo based on the current local update chain (tracker mutex locked).
func (s *SyncCommitteeTracker) GetUpdateInfo() *UpdateInfo {
	s.lock.Lock()
	defer s.lock.Unlock()

	return s.getUpdateInfo()
}

// getUpdateInfo returns and UpdateInfo based on the current local update chain (tracker mutex expected).
func (s *SyncCommitteeTracker) getUpdateInfo() *UpdateInfo {
	if s.updateInfo != nil {
		return s.updateInfo
	}
	l := s.nextPeriod - s.firstPeriod
	if l > MaxUpdateInfoLength {
		l = MaxUpdateInfoLength
	}
	firstPeriod := s.nextPeriod - l

	u := &UpdateInfo{
		AfterLastPeriod: s.nextPeriod,
		Scores:          make(UpdateScores, int(l)),
	}

	for period := firstPeriod; period < s.nextPeriod; period++ {
		if update := s.GetBestUpdate(period); update != nil {
			u.Scores[period-firstPeriod] = update.score
		} else {
			log.Error("Update missing from database", "period", period)
		}
	}

	s.updateInfo = u
	return u
}

// updateInfoChanged should be called whenever the committee update chain is changed. It schedules a call to
// advertiseCommitteesNow in the near future (after advertiseDelay) unless it is already scheduled. This delay
// ensures that advertisements are not sent too frequently.
func (s *SyncCommitteeTracker) updateInfoChanged() {
	s.updateInfo = nil
	if s.advertiseScheduled {
		return
	}
	s.advertiseScheduled = true
	s.advertisedTo = nil

	s.clock.AfterFunc(advertiseDelay, func() {
		s.lock.Lock()
		s.advertiseCommitteesNow()
		s.advertiseScheduled = false
		s.lock.Unlock()
	})
}

// advertiseCommitteesNow sends committee update chain advertisements to all active peers.
func (s *SyncCommitteeTracker) advertiseCommitteesNow() {
	info := s.getUpdateInfo()
	if s.advertisedTo == nil {
		s.advertisedTo = make(map[sctClient]struct{})
	}
	for peer := range s.broadcastTo {
		if _, ok := s.advertisedTo[peer]; !ok {
			peer.SendUpdateInfo(info)
			s.advertisedTo[peer] = struct{}{}
		}
	}
}
