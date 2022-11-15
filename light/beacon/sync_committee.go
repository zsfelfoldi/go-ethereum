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
	"bufio"
	"bytes"
	"context"
	"encoding/binary"

	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	lru "github.com/hashicorp/golang-lru"
	"github.com/minio/sha256-simd"
)

var (
	initDataKey      = []byte("sct.init") // RLP(LightClientInitData)
	bestUpdateKey    = []byte("sct.bu-")  // bigEndian64(syncPeriod) -> RLP(LightClientUpdate)  (nextCommittee only referenced by root hash)
	syncCommitteeKey = []byte("sct.sc-")  // bigEndian64(syncPeriod) + committee root hash -> serialized committee
)

const (
	maxUpdateInfoLength     = 128
	MaxCommitteeUpdateFetch = 128
	CommitteeCostFactor     = 16
	broadcastFrequencyLimit = time.Millisecond * 200
	advertiseDelay          = time.Second * 10
)

// SignedHead represents a beacon header signed by a sync committee
type SignedHead struct {
	BitMask   []byte
	Signature []byte
	Header    Header
}

// signerCount returns the number of individual signers in the signature aggregate
func (s *SignedHead) signerCount() int {
	if len(s.BitMask) != 64 {
		return 0 // signature check will filter it out later but we calculate this before sig check
	}
	var signerCount int
	for _, v := range s.BitMask {
		signerCount += bits.OnesCount8(v)
	}
	return signerCount
}

// Equal returns true if both the headers and the signer sets are the same
func (s *SignedHead) Equal(s2 *SignedHead) bool {
	return s.Header == s2.Header && bytes.Equal(s.BitMask, s2.BitMask) && bytes.Equal(s.Signature, s2.Signature)
}

// sctClient represents a peer that SyncCommitteeTracker sends signed heads and sync committee advertisements to
type sctClient interface {
	SendSignedHeads(heads []SignedHead)
	SendUpdateInfo(updateInfo *UpdateInfo)
}

// sctServer represents a peer that SyncCommitteeTracker can request sync committee update proofs from
type sctServer interface {
	GetBestCommitteeProofs(ctx context.Context, req CommitteeRequest) (CommitteeReply, error)
	ClosedChannel() chan struct{} //TODO ???
	WrongReply(description string)
}

const lastProcessedCount = 4

// SyncCommitteeTracker maintains a chain of sync committee updates and a small set of best known signed heads.
// It is used in all client configurations operating on a beacon chain. It can sync its update chain and receive
// signed heads from either an ODR or beacon node API backend and propagate/serve this data to subscribed peers.
// Received signed heads are validated based on the known sync committee chain and added to the local set if
// valid or placed in a deferred queue if the committees are not synced up to the period of the new head yet.
// Sync committee chain is either initialized from a weak subjectivity checkpoint or controlled by a BeaconChain
// that is driven by a trusted source (beacon node API).
type SyncCommitteeTracker struct {
	lock                     sync.RWMutex
	db                       ethdb.KeyValueStore
	sigVerifier              committeeSigVerifier
	clock                    mclock.Clock
	bestUpdateCache          *lru.Cache
	serializedCommitteeCache *lru.Cache
	syncCommitteeCache       *lru.Cache
	committeeRootCache       *lru.Cache
	unixNano                 func() int64

	forks              Forks
	constraints        SctConstraints
	signerThreshold    int
	minimumUpdateScore UpdateScore
	enforceTime        bool

	genesisInit bool   // genesis data initialized (signature check possible)
	genesisTime uint64 // unix time (seconds)
	chainInit   bool   // update and committee chain initialized
	// if chain is initialized then best updates for periods between firstPeriod to nextPeriod-1
	// and committees for periods between firstPeriod to nextPeriod are available
	firstPeriod, nextPeriod uint64

	updateInfo                             *UpdateInfo
	connected                              map[sctServer]*sctPeerInfo
	requestQueue                           []*sctPeerInfo
	broadcastTo, advertisedTo              map[sctClient]struct{}
	lastBroadcast                          mclock.AbsTime
	advertiseScheduled, broadcastScheduled bool
	triggerCh, stopCh                      chan struct{}
	acceptedList, processedList            headList //TODO add pendingList
	lastProcessed                          [lastProcessedCount]common.Hash
	lastProcessedIndex                     int

	headSubs []func(Header)
}

// NewSyncCommitteeTracker creates a new SyncCommitteeTracker
func NewSyncCommitteeTracker(db ethdb.KeyValueStore, forks Forks, constraints SctConstraints, signerThreshold int, enforceTime bool, sigVerifier committeeSigVerifier, clock mclock.Clock, unixNano func() int64) *SyncCommitteeTracker {
	s := &SyncCommitteeTracker{
		db:              db,
		sigVerifier:     sigVerifier,
		clock:           clock,
		unixNano:        unixNano,
		forks:           forks,
		constraints:     constraints,
		signerThreshold: signerThreshold,
		enforceTime:     enforceTime,
		minimumUpdateScore: UpdateScore{
			signerCount:    uint32(signerThreshold),
			subPeriodIndex: 512,
		},
		connected:     make(map[sctServer]*sctPeerInfo),
		broadcastTo:   make(map[sctClient]struct{}),
		triggerCh:     make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
		acceptedList:  newHeadList(4),
		processedList: newHeadList(2),
	}
	s.bestUpdateCache, _ = lru.New(1000)
	s.serializedCommitteeCache, _ = lru.New(100)
	s.syncCommitteeCache, _ = lru.New(100)
	s.committeeRootCache, _ = lru.New(100)

	iter := s.db.NewIterator(bestUpdateKey, nil)
	kl := len(bestUpdateKey)
	// iterate through them all for simplicity; at most a few hundred items
	for iter.Next() {
		period := binary.BigEndian.Uint64(iter.Key()[kl : kl+8])
		if !s.chainInit {
			s.chainInit = true
			s.firstPeriod = period
		} else if s.nextPeriod != period {
			break // continuity guaranteed
		}
		s.nextPeriod = period + 1
	}
	iter.Release()
	constraints.SetCallbacks(s.Init, s.EnforceForksAndConstraints)
	return s
}

// Stop stops the syncing/propagation process and shuts down the tracker
func (s *SyncCommitteeTracker) Stop() {
	close(s.stopCh)
}

// getBestUpdateKey returns the database key for the canonical sync committee update at the given period
func getBestUpdateKey(period uint64) []byte {
	kl := len(bestUpdateKey)
	key := make([]byte, kl+8)
	copy(key[:kl], bestUpdateKey)
	binary.BigEndian.PutUint64(key[kl:], period)
	return key
}

// GetBestUpdate returns the best known canonical sync committee update at the given period
func (s *SyncCommitteeTracker) GetBestUpdate(period uint64) *LightClientUpdate {
	if v, ok := s.bestUpdateCache.Get(period); ok {
		update, _ := v.(*LightClientUpdate)
		if update != nil {
			if uint64(update.Header.Slot>>13) != period {
				log.Error("Best update from wrong period found in cache")
			}
		}
		return update
	}
	if updateEnc, err := s.db.Get(getBestUpdateKey(period)); err == nil {
		update := new(LightClientUpdate)
		if err := rlp.DecodeBytes(updateEnc, update); err == nil {
			update.CalculateScore()
			s.bestUpdateCache.Add(period, update)
			if uint64(update.Header.Slot>>13) != period {
				log.Error("Best update from wrong period found in database")
			}
			return update
		} else {
			log.Error("Error decoding best update", "error", err)
		}
	}
	s.bestUpdateCache.Add(period, nil)
	return nil
}

// storeBestUpdate stores a sync committee update in the canonical update chain
func (s *SyncCommitteeTracker) storeBestUpdate(update *LightClientUpdate) {
	period := uint64(update.Header.Slot) >> 13
	updateEnc, err := rlp.EncodeToBytes(update)
	if err != nil {
		log.Error("Error encoding LightClientUpdate", "error", err)
		return
	}
	s.bestUpdateCache.Add(period, update)
	s.db.Put(getBestUpdateKey(period), updateEnc)
	s.committeeRootCache.Remove(period + 1)
	s.updateInfoChanged()
}

// deleteBestUpdate deletes a sync committee update from the canonical update chain
func (s *SyncCommitteeTracker) deleteBestUpdate(period uint64) {
	s.db.Delete(getBestUpdateKey(period))
	s.bestUpdateCache.Remove(period)
	s.committeeRootCache.Remove(period + 1)
	s.updateInfoChanged()
}

// verifyUpdate checks whether the header signature is correct and the update fits into the specified constraints
// (assumes that the update has been successfully validated previously)
func (s *SyncCommitteeTracker) verifyUpdate(update *LightClientUpdate) bool {
	if !s.checkForksAndConstraints(update) {
		return false
	}
	ok, age := s.verifySignature(SignedHead{Header: update.Header, Signature: update.SyncCommitteeSignature, BitMask: update.SyncCommitteeBits})
	if age < 0 {
		log.Warn("Future committee update received", "age", age)
	}
	return ok
}

const (
	sciSuccess = iota
	sciNeedCommittee
	sciWrongUpdate
	sciUnexpectedError
)

// insertUpdate verifies the update and stores it in the update chain if possible. The serialized version
// of the next committee should also be supplied if it is not already stored in the database.
func (s *SyncCommitteeTracker) insertUpdate(update *LightClientUpdate, nextCommittee []byte) int {
	period := uint64(update.Header.Slot) >> 13
	if !s.verifyUpdate(update) {
		return sciWrongUpdate
	}

	if !s.chainInit || period > s.nextPeriod || period+1 < s.firstPeriod {
		log.Error("Unexpected insertUpdate", "period", period, "firstPeriod", s.firstPeriod, "nextPeriod", s.nextPeriod)
		return sciUnexpectedError
	}
	update.CalculateScore()
	var rollback bool
	if period+1 == s.firstPeriod {
		if update.NextSyncCommitteeRoot != s.getSyncCommitteeRoot(period+1) { //TODO is this check needed here?
			return sciWrongUpdate
		}
	} else if period < s.nextPeriod {
		// update should already exist
		oldUpdate := s.GetBestUpdate(period)
		if oldUpdate == nil {
			log.Error("Update expected to exist but missing from db")
			return sciUnexpectedError
		}
		if !update.score.betterThan(oldUpdate.score) {
			// not better that existing one, nothing to do
			return sciSuccess
		}
		rollback = update.NextSyncCommitteeRoot != oldUpdate.NextSyncCommitteeRoot
	}

	if (period == s.nextPeriod || rollback) && s.GetSerializedSyncCommittee(period+1, update.NextSyncCommitteeRoot) == nil {
		// committee is not yet stored in db
		if nextCommittee == nil {
			return sciNeedCommittee
		}
		if SerializedCommitteeRoot(nextCommittee) != update.NextSyncCommitteeRoot {
			return sciWrongUpdate
		}
		s.storeSerializedSyncCommittee(period+1, update.NextSyncCommitteeRoot, nextCommittee)
	}

	if rollback {
		for p := s.nextPeriod - 1; p >= period; p-- {
			s.deleteBestUpdate(p)
		}
		s.nextPeriod = period
	}
	s.storeBestUpdate(update)
	if period == s.nextPeriod {
		s.nextPeriod++
	}
	if period+1 == s.firstPeriod {
		s.firstPeriod--
	}
	log.Info("Synced new committee update", "period", period, "nextCommitteeRoot", update.NextSyncCommitteeRoot)
	return sciSuccess
}

// getSyncCommitteeKey returns the database key for the specified sync committee
func getSyncCommitteeKey(period uint64, committeeRoot common.Hash) []byte {
	kl := len(syncCommitteeKey)
	key := make([]byte, kl+8+32)
	copy(key[:kl], syncCommitteeKey)
	binary.BigEndian.PutUint64(key[kl:kl+8], period)
	copy(key[kl+8:], committeeRoot[:])
	return key
}

// GetSerializedSyncCommittee fetches the serialized version of a sync committee from cache or database
func (s *SyncCommitteeTracker) GetSerializedSyncCommittee(period uint64, committeeRoot common.Hash) []byte {
	key := getSyncCommitteeKey(period, committeeRoot)
	if v, ok := s.serializedCommitteeCache.Get(string(key)); ok {
		committee, _ := v.([]byte)
		if len(committee) == 513*48 {
			return committee
		} else {
			log.Error("Serialized committee with invalid size found in cache")
		}
	}
	if committee, err := s.db.Get(key); err == nil {
		if len(committee) == 513*48 {
			s.serializedCommitteeCache.Add(string(key), committee)
			return committee
		} else {
			log.Error("Serialized committee with invalid size found in database")
		}
	}
	return nil
}

// storeSerializedSyncCommittee stores the serialized version of a sync committee to cache and database
func (s *SyncCommitteeTracker) storeSerializedSyncCommittee(period uint64, committeeRoot common.Hash, committee []byte) {
	key := getSyncCommitteeKey(period, committeeRoot)
	s.serializedCommitteeCache.Add(string(key), committee)
	s.syncCommitteeCache.Remove(string(key)) // a nil entry for "not found" might have been stored here earlier
	s.db.Put(key, committee)
}

// verifySignature returns true if the given signed head has a valid signature according to the local
// committee chain. The caller should ensure that the committees advertised by the same source where
// the signed head came from are synced before verifying the signature.
// The age of the header is also returned (the time elapsed since the beginning of the given slot,
// according to the local system clock). If enforceTime is true then negative age (future) headers
// are rejected.
func (s *SyncCommitteeTracker) verifySignature(head SignedHead) (bool, time.Duration) {
	slotTime := int64(time.Second) * int64(s.genesisTime+uint64(head.Header.Slot)*12)
	age := time.Duration(s.unixNano() - slotTime)
	if s.enforceTime && age < 0 {
		return false, age
	}
	committee := s.getSyncCommitteeLocked(uint64(head.Header.Slot+1) >> 13) // signed with the next slot's committee
	if committee == nil {
		return false, age
	}
	return s.sigVerifier.verifySignature(committee, s.forks.signingRoot(head.Header), head.BitMask, head.Signature), age
}

// computeDomain returns the signature domain based on the given fork version and genesis validator set root
func computeDomain(forkVersion []byte, genesisValidatorsRoot common.Hash) MerkleValue {
	var forkVersion32, forkDataRoot, domain MerkleValue
	hasher := sha256.New()
	copy(forkVersion32[:len(forkVersion)], forkVersion)
	hasher.Write(forkVersion32[:])
	hasher.Write(genesisValidatorsRoot[:])
	hasher.Sum(forkDataRoot[:0])
	domain[0] = 7
	copy(domain[4:], forkDataRoot[:28])
	return domain
}

// SerializedCommitteeRoot calculates the root hash of the binary tree representation
// of a sync committee provided in serialized format
func SerializedCommitteeRoot(enc []byte) common.Hash {
	if len(enc) != 513*48 {
		return common.Hash{}
	}
	hasher := sha256.New()
	var (
		padding [16]byte
		data    [512]common.Hash
	)
	for i := range data {
		hasher.Reset()
		hasher.Write(enc[i*48 : (i+1)*48])
		hasher.Write(padding[:])
		hasher.Sum(data[i][:0])
	}
	l := 512
	for l > 1 {
		for i := 0; i < l/2; i++ {
			hasher.Reset()
			hasher.Write(data[i*2][:])
			hasher.Write(data[i*2+1][:])
			hasher.Sum(data[i][:0])
		}
		l /= 2
	}
	hasher.Reset()
	hasher.Write(enc[512*48 : 513*48])
	hasher.Write(padding[:])
	hasher.Sum(data[1][:0])
	hasher.Reset()
	hasher.Write(data[0][:])
	hasher.Write(data[1][:])
	hasher.Sum(data[0][:0])
	return data[0]
}

// getSyncCommitteeRoot returns the sync committee root at the given period of the current
// local committee root constraints or update chain (tracker mutex lock expected).
func (s *SyncCommitteeTracker) getSyncCommitteeRoot(period uint64) (root common.Hash) {
	if v, ok := s.committeeRootCache.Get(period); ok {
		return v.(common.Hash)
	}
	defer func() {
		s.committeeRootCache.Add(period, root)
	}()

	if r, matchAll := s.constraints.CommitteeRoot(period); !matchAll {
		return r
	}
	if !s.chainInit || period <= s.firstPeriod || period > s.nextPeriod {
		return common.Hash{}
	}
	if update := s.GetBestUpdate(period - 1); update != nil {
		return update.NextSyncCommitteeRoot
	}
	return common.Hash{}
}

// GetSyncCommitteeRoot returns the sync committee root at the given period of the current
// local committee root constraints or update chain (tracker mutex locked).
func (s *SyncCommitteeTracker) GetSyncCommitteeRoot(period uint64) common.Hash {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.getSyncCommitteeRoot(period)
}

// getSyncCommitteeLocked returns the deserialized sync committee at the given period of the
// current local committee chain (tracker mutex lock expected).
func (s *SyncCommitteeTracker) getSyncCommitteeLocked(period uint64) syncCommittee {
	if committeeRoot := s.getSyncCommitteeRoot(period); committeeRoot != (common.Hash{}) {
		key := string(getSyncCommitteeKey(period, committeeRoot))
		if v, ok := s.syncCommitteeCache.Get(key); ok {
			sc, _ := v.(syncCommittee)
			return sc
		}
		if sc := s.GetSerializedSyncCommittee(period, committeeRoot); sc != nil {
			c := s.sigVerifier.deserializeSyncCommittee(sc)
			s.syncCommitteeCache.Add(key, c)
			return c
		} else {
			log.Error("Missing serialized sync committee", "period", period, "committeeRoot", committeeRoot)
		}
	}
	return nil
}

// getSyncCommittee returns the deserialized sync committee at the given period of the
// current local committee chain (tracker mutex locked).
func (s *SyncCommitteeTracker) getSyncCommittee(period uint64) syncCommittee {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.getSyncCommitteeLocked(period)
}

// UpdateScore allows the comparison between updates at the same period in order to find the best
// update chain that provides the strongest proof of being canonical.
type UpdateScore struct {
	signerCount, subPeriodIndex uint32
	finalizedHeader             bool // update is considered finalized if has finalized header and 2/3 signatures
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

// Encode encodes the update score data in a compact form for in-protocol advertisement
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

// UpdateInfo contains scores for an advertised update chain. Note that the most recent updates are always
// advertised but earliest ones might not because of length limitation.
type UpdateInfo struct {
	AfterLastPeriod uint64
	Scores          UpdateScores
}

// NextPeriod returns the period after the last update
func (s *SyncCommitteeTracker) NextPeriod() uint64 {
	s.lock.Lock()
	defer s.lock.Unlock()

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
	if l > maxUpdateInfoLength {
		l = maxUpdateInfoLength
	}
	firstPeriod := s.nextPeriod - l

	u := &UpdateInfo{
		AfterLastPeriod: s.nextPeriod,
		Scores:          make(UpdateScores, int(l)),
	}

	for period := firstPeriod; period < s.nextPeriod; period++ {
		if update := s.GetBestUpdate(uint64(period)); update != nil {
			u.Scores[period-firstPeriod] = update.score
		} else {
			log.Error("Update missing from database", "period", period)
		}
	}

	s.updateInfo = u
	return u
}

// CommitteeRequest represents a request for fetching updates and committees at the given periods
type CommitteeRequest struct {
	UpdatePeriods, CommitteePeriods []uint64
}

// empty returns true if the request does not request anything
func (req CommitteeRequest) empty() bool {
	return req.UpdatePeriods == nil && req.CommitteePeriods == nil
}

// CommitteeReply is an answer to a CommitteeRequest, contains the updates and committees corresponding
// to the period numbers in the request in the same order
type CommitteeReply struct {
	Updates    []LightClientUpdate
	Committees [][]byte
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
		ctx, _ := context.WithTimeout(context.Background(), time.Second*20)
		reply, err := sp.peer.GetBestCommitteeProofs(ctx, req) // expected to return with error in case of shutdown
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

	var (
		request  CommitteeRequest
		reqCount int
	)
	localFirst, localAfterLast := s.firstPeriod, s.nextPeriod
	if sp.forkPeriod < localAfterLast {
		localAfterLast = sp.forkPeriod
	}
	localInfoFirst := localInfo.AfterLastPeriod - uint64(len(localInfo.Scores))
	if !s.chainInit {
		request.CommitteePeriods = []uint64{syncAfterFixed}
		reqCount = CommitteeCostFactor
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
		if reqCount >= MaxCommitteeUpdateFetch {
			break
		}
		localScore := localInfo.Scores[period-localFirst]
		remoteScore := sp.remoteInfo.Scores[period-remoteFirst]
		if remoteScore.betterThan(localScore) {
			request.UpdatePeriods = append(request.UpdatePeriods, period)
			reqCount++
		}
	}

	// future range: fetch update and next committee as long as remote score reaches required minimum
	for period := sharedAfterLast; period < syncAfterLast; period++ {
		if reqCount+CommitteeCostFactor >= MaxCommitteeUpdateFetch {
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
		reqCount += CommitteeCostFactor + 1
	}

	// past range: fetch update and committee for periods before the locally stored range that are covered by the constraints (known committee roots)
	for nextPeriod := localFirst; nextPeriod > constraintsFirst && nextPeriod > remoteFirst; nextPeriod-- { // loop variable is nextPeriod == period+1 to avoid uint64 underflow
		if reqCount+CommitteeCostFactor >= MaxCommitteeUpdateFetch {
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
		reqCount += CommitteeCostFactor + 1
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
		period := uint64(update.Header.Slot) >> 13
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

// LightClientUpdate is a proof of the next sync committee root based on a header signed by the sync committee
// of the given period. Optionally the update can prove quasi-finality by the signed header referring to a previous,
// finalized header from the same period, and the finalized header referring to the next sync committee root.
type LightClientUpdate struct {
	Header                  Header
	NextSyncCommitteeRoot   common.Hash
	NextSyncCommitteeBranch MerkleValues
	FinalizedHeader         Header
	FinalityBranch          MerkleValues
	SyncCommitteeBits       []byte
	SyncCommitteeSignature  []byte
	ForkVersion             []byte
	score                   UpdateScore
}

// Validate verifies the validity of the update
func (update *LightClientUpdate) Validate() error {
	if update.Header.Slot&0x1fff == 0x1fff {
		// last slot of each period is not suitable for an update because it is signed by the next period's sync committee, proves the same committee it is signed by
		return errors.New("Last slot of period")
	}
	var checkRoot common.Hash
	if update.hasFinalizedHeader() {
		if update.FinalizedHeader.Slot>>13 != update.Header.Slot>>13 {
			return errors.New("FinalizedHeader is from previous period") // proves the same committee it is signed by
		}
		if root, ok := VerifySingleProof(update.FinalityBranch, BsiFinalBlock, MerkleValue(update.FinalizedHeader.Hash()), 0); !ok || root != update.Header.StateRoot {
			return errors.New("Invalid FinalizedHeader merkle proof")
		}
		checkRoot = update.FinalizedHeader.StateRoot
	} else {
		checkRoot = update.Header.StateRoot
	}
	if root, ok := VerifySingleProof(update.NextSyncCommitteeBranch, BsiNextSyncCommittee, MerkleValue(update.NextSyncCommitteeRoot), 0); !ok || root != checkRoot {
		if ok && root == update.Header.StateRoot {
			// See https://github.com/ChainSafe/lodestar/issues/4574
			log.Warn("update.NextSyncCommitteeBranch rooted in update.Header.StateRoot  (Lodestar bug workaround applied)")
		} else {
			return errors.New("Invalid NextSyncCommittee merkle proof")
		}
	}
	return nil
}

// hasFinalizedHeader returns true if the update has a finalized header referred by the signed header
// and referring to the next sync committee.
// Note that in addition to this, a sufficient signer participation is also needed in order to fulfill
// the quasi-finality condition (see UpdateScore.isFinalized).
func (l *LightClientUpdate) hasFinalizedHeader() bool {
	return l.FinalizedHeader.BodyRoot != (common.Hash{})
}

// CalculateScore returns the UpdateScore describing the proof strength of the update
func (l *LightClientUpdate) CalculateScore() UpdateScore {
	l.score.signerCount = 0
	for _, v := range l.SyncCommitteeBits {
		l.score.signerCount += uint32(bits.OnesCount8(v))
	}
	l.score.subPeriodIndex = uint32(l.Header.Slot & 0x1fff)
	l.score.finalizedHeader = l.hasFinalizedHeader()
	return l.score
}

// trimZeroes removes zero bytes from the end of a byte slice
func trimZeroes(data []byte) []byte {
	l := len(data)
	for l > 0 && data[l-1] == 0 {
		l--
	}
	return data[:l]
}

// checkForkVersion returns true if the fork version of the update matches the forks defined in the chain config
func (l *LightClientUpdate) checkForkVersion(forks Forks) bool {
	return bytes.Equal(trimZeroes(l.ForkVersion), trimZeroes(forks.version(uint64(l.Header.Slot>>5))))
}

// headInfo contains the best signed header and the state of propagation belonging to a given block root
type headInfo struct {
	head         SignedHead
	hash         common.Hash
	signerCount  int
	receivedFrom map[sctServer]struct{}
	sentTo       map[sctClient]struct{}
	processed    bool
}

// headList is a list of best known heads for the few most recent slots
// Note: usually only the highest slot is interesting but in case of low signer participation or
// slow propagation/aggregation of signatures it might make sense to keep track of multiple heads
// as different clients might have different tradeoff preferences between delay and security.
type headList struct {
	list  []*headInfo // highest slot first
	limit int
}

// newHeadList creates a new headList
func newHeadList(limit int) headList {
	return headList{limit: limit}
}

// getHead returns the headInfo belonging to the given block root
func (h *headList) getHead(hash common.Hash) *headInfo {
	//return h.hashMap[hash]
	for _, headInfo := range h.list {
		if headInfo.hash == hash {
			return headInfo
		}
	}
	return nil
}

// updateHead adds or updates the given headInfo in the list
func (h *headList) updateHead(head *headInfo) {
	for i, hh := range h.list {
		if hh.head.Header.Slot <= head.head.Header.Slot {
			if hh.head.Header.Slot < head.head.Header.Slot {
				if len(h.list) < h.limit {
					h.list = append(h.list, nil)
				}
				copy(h.list[i+1:len(h.list)], h.list[i:len(h.list)-1])
			}
			h.list[i] = head
			return
		}
	}
	if len(h.list) < h.limit {
		h.list = append(h.list, head)
	}
}

// AddSignedHeads adds signed heads to the tracker if the syncing process has been finished;
// adds them to a deferred list otherwise that is processed when the syncing is finished.
func (s *SyncCommitteeTracker) AddSignedHeads(peer sctServer, heads []SignedHead) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if sp := s.connected[peer]; sp != nil && (sp.requesting || sp.queued) {
		sp.deferredHeads = append(sp.deferredHeads, heads...)
		return
	}
	s.addSignedHeads(peer, heads)
}

// addSignedHeads adds signed heads to the tracker after a successful verification
// (it is assumed that the local update chain has been synced with the given peer)
func (s *SyncCommitteeTracker) addSignedHeads(peer sctServer, heads []SignedHead) {
	var (
		broadcast   bool
		oldHeadHash common.Hash
	)
	if len(s.acceptedList.list) > 0 {
		oldHeadHash = s.acceptedList.list[0].hash
	}
	for _, head := range heads {
		signerCount := head.signerCount()
		if signerCount < s.signerThreshold {
			continue
		}
		//TODO if coming from an untrusted source and signerCount is lower than 2/3 then add to pending list first
		sigOk, age := s.verifySignature(head)
		if age < 0 {
			log.Warn("Future signed head received", "age", age)
		}
		if age > time.Minute*2 {
			log.Warn("Old signed head received", "age", age)
		}
		if !sigOk {
			peer.WrongReply("invalid header signature")
			continue
		}
		hash := head.Header.Hash()
		if h := s.acceptedList.getHead(hash); h != nil {
			h.receivedFrom[peer] = struct{}{}
			if signerCount > h.signerCount {
				h.head = head
				h.signerCount = signerCount
				h.sentTo = nil
				s.acceptedList.updateHead(h)
				if h.processed {
					s.processedList.updateHead(h)
					broadcast = true
				}
			}
		} else {
			var processed bool
			for _, p := range s.lastProcessed {
				if p == hash {
					processed = true
					break
				}
			}
			h := &headInfo{
				head:         head,
				hash:         hash,
				sentTo:       make(map[sctClient]struct{}),
				receivedFrom: map[sctServer]struct{}{peer: struct{}{}},
				processed:    processed,
			}
			s.acceptedList.updateHead(h)
			if h.processed {
				s.processedList.updateHead(h)
				broadcast = true
			}
		}
	}
	if broadcast {
		s.broadcastHeads()
	}
	if len(s.acceptedList.list) > 0 && oldHeadHash != s.acceptedList.list[0].hash {
		head := s.acceptedList.list[0].head.Header
		for _, subFn := range s.headSubs {
			subFn(head)
		}
	}
}

// SubscribeToNewHeads subscribes the given callback function to head beacon headers with a verified valid sync committee signature.
func (s *SyncCommitteeTracker) SubscribeToNewHeads(subFn func(Header)) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.headSubs = append(s.headSubs, subFn)
}

// CanBroadcastHead should be called when given head can be broadcast to other peers. Depending on the setup further data retrieval and/or
// processing might be needed for a new head before it is propagated further.
// Note that a signed head is broadcast when both addSignedHeads and CanBroadcastHead has been called for the given head (in any order).
func (s *SyncCommitteeTracker) CanBroadcastHead(hash common.Hash) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if headInfo := s.acceptedList.getHead(hash); headInfo != nil {
		headInfo.processed = true
		s.processedList.updateHead(headInfo)
		s.broadcastHeads()
	}
	s.lastProcessed[s.lastProcessedIndex] = hash
	s.lastProcessedIndex++
	if s.lastProcessedIndex == lastProcessedCount {
		s.lastProcessedIndex = 0
	}
}

// Activate allows broadcasting heads to the given peer
func (s *SyncCommitteeTracker) Activate(peer sctClient) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.broadcastTo[peer] = struct{}{}
	s.broadcastHeadsTo(peer, true)
}

// Deactivate stops broadcasting heads to the given peer
func (s *SyncCommitteeTracker) Deactivate(peer sctClient, remove bool) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.broadcastTo, peer)
	if remove {
		for _, headInfo := range s.processedList.list {
			delete(headInfo.sentTo, peer)
		}
	}
}

// broadcastHeads broadcasts new heads to all active peers unless they have been broadcast very recently,
// in which case it schedules a broadcast for a little bit later. The frequency of broadcasts is limited
// by the broadcastFrequencyLimit constant.
func (s *SyncCommitteeTracker) broadcastHeads() {
	now := s.clock.Now()
	sinceLast := time.Duration(now - s.lastBroadcast)
	if sinceLast < broadcastFrequencyLimit {
		if !s.broadcastScheduled {
			s.broadcastScheduled = true
			s.clock.AfterFunc(broadcastFrequencyLimit-sinceLast, func() {
				s.lock.Lock()
				s.broadcastHeadsNow()
				s.broadcastScheduled = false
				s.lock.Unlock()
			})
		}
		return
	}
	s.broadcastHeadsNow()
	s.lastBroadcast = now
}

// broadcastHeadsNow broadcasts new heads to all active peers.
func (s *SyncCommitteeTracker) broadcastHeadsNow() {
	for peer := range s.broadcastTo {
		s.broadcastHeadsTo(peer, false)
	}
}

// broadcastHeadsTo broadcasts new heads to the given peer. If sendEmpty is true then an empty
// broadcast message is sent even if there are no new signed heads to send.
func (s *SyncCommitteeTracker) broadcastHeadsTo(peer sctClient, sendEmpty bool) {
	heads := make([]SignedHead, 0, len(s.processedList.list))
	for _, headInfo := range s.processedList.list {
		if _, ok := headInfo.sentTo[peer]; !ok {
			heads = append(heads, headInfo.head)
			headInfo.sentTo[peer] = struct{}{}
		}
		if headInfo.signerCount > 341 {
			break
		}
	}
	if sendEmpty || len(heads) > 0 {
		peer.SendSignedHeads(heads)
	}
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
	for peer := range s.broadcastTo {
		if _, ok := s.advertisedTo[peer]; !ok {
			peer.SendUpdateInfo(info)
		}
	}
}

// advertiseCommitteesTo sends committee update chain advertisements to the given peer.
func (s *SyncCommitteeTracker) advertiseCommitteesTo(peer sctClient) {
	peer.SendUpdateInfo(s.getUpdateInfo())
	if s.advertisedTo == nil {
		s.advertisedTo = make(map[sctClient]struct{})
	}
	s.advertisedTo[peer] = struct{}{}
}

// checkForksAndConstraints checks whether the signed headers of the given committee update is
// on the right fork and the proven NextSyncCommitteeRoot matches the update chain constraints.
func (s *SyncCommitteeTracker) checkForksAndConstraints(update *LightClientUpdate) bool {
	if !s.genesisInit {
		log.Error("SyncCommitteeTracker not initialized")
		return false
	}
	if !update.checkForkVersion(s.forks) {
		return false
	}
	root, matchAll := s.constraints.CommitteeRoot(uint64(update.Header.Slot)>>13 + 1)
	return matchAll || root == update.NextSyncCommitteeRoot
}

// EnforceForksAndConstraints rolls back committee updates that do not match the tracker's forks
// and constraints and also starts new requests if possible (tracker mutex locked)
func (s *SyncCommitteeTracker) EnforceForksAndConstraints() {
	s.lock.Lock()
	s.enforceForksAndConstraints()
	s.lock.Unlock()
}

// enforceForksAndConstraints rolls back committee updates that do not match the tracker's forks
// and constraints and also starts new requests if possible (tracker mutex expected)
func (s *SyncCommitteeTracker) enforceForksAndConstraints() {
	if !s.genesisInit || !s.chainInit {
		return
	}
	s.committeeRootCache.Purge()
	for s.nextPeriod > s.firstPeriod {
		if update := s.GetBestUpdate(s.nextPeriod - 1); update == nil || s.checkForksAndConstraints(update) {
			if update == nil {
				log.Error("Sync committee update missing", "period", s.nextPeriod-1)
			}
			break
		}
		s.nextPeriod--
		s.deleteBestUpdate(s.nextPeriod)
	}
	if s.nextPeriod == s.firstPeriod {
		if root, matchAll := s.constraints.CommitteeRoot(s.firstPeriod); matchAll || s.getSyncCommitteeRoot(s.firstPeriod) != root || s.getSyncCommitteeLocked(s.firstPeriod) == nil {
			s.nextPeriod, s.firstPeriod, s.chainInit = 0, 0, false
		}
	}

	s.retrySyncAllPeers()
}

// Init initializes the tracker with the given GenesisData and starts the update syncing process.
// Note that Init may be called either at startup or later if it has to be fetched from the network based on a checkpoint hash.
func (s *SyncCommitteeTracker) Init(GenesisData GenesisData) {
	s.lock.Lock()
	s.forks.computeDomains(GenesisData.GenesisValidatorsRoot)
	s.genesisTime = GenesisData.GenesisTime
	s.genesisInit = true
	s.enforceForksAndConstraints()
	s.lock.Unlock()

	go s.syncLoop()
}

// ChainConfig contains built-in chain configuration presets for certain networks
type ChainConfig struct {
	GenesisData
	Forks      Forks
	Checkpoint common.Hash
}

// Fork describes a single beacon chain fork and also stores the calculated signature domain used after this fork.
type Fork struct {
	Epoch   uint64
	Name    string
	Version []byte
	domain  MerkleValue
}

// Forks is the list of all beacon chain forks in the chain configuration.
type Forks []Fork

// version returns the fork version at the given epoch.
func (bf Forks) version(epoch uint64) []byte {
	for i := len(bf) - 1; i >= 0; i-- {
		if epoch >= bf[i].Epoch {
			return bf[i].Version
		}
	}
	log.Error("Fork version unknown", "epoch", epoch)
	return nil
}

// domain returns the signature domain for the given epoch (assumes that domains have already been calculated).
func (bf Forks) domain(epoch uint64) MerkleValue {
	for i := len(bf) - 1; i >= 0; i-- {
		if epoch >= bf[i].Epoch {
			return bf[i].domain
		}
	}
	log.Error("Fork domain unknown", "epoch", epoch)
	return MerkleValue{}
}

// computeDomains calculates and stores signature domains for each fork in the list.
func (bf Forks) computeDomains(genesisValidatorsRoot common.Hash) {
	for i := range bf {
		bf[i].domain = computeDomain(bf[i].Version, genesisValidatorsRoot)
	}
}

// epoch returns the starting epoch of the fork identified by the given name.
func (bf Forks) epoch(name string) (uint64, bool) {
	for _, fork := range bf {
		if fork.Name == name {
			return fork.Epoch, true
		}
	}
	return 0, false
}

// signingRoot calculates the signing root of the given header.
func (bf Forks) signingRoot(header Header) common.Hash {
	var signingRoot common.Hash
	hasher := sha256.New()
	headerHash := header.Hash()
	hasher.Write(headerHash[:])
	domain := bf.domain(uint64(header.Slot) >> 5)
	hasher.Write(domain[:])
	hasher.Sum(signingRoot[:0])
	return signingRoot
}

func (f Forks) Len() int           { return len(f) }
func (f Forks) Swap(i, j int)      { f[i], f[j] = f[j], f[i] }
func (f Forks) Less(i, j int) bool { return f[i].Epoch < f[j].Epoch }

// fieldValue checks if the given fork parameter field is present in the given line
// and if it is then returns the field value and the name of the fork it belongs to.
func fieldValue(line, field string) (name, value string, ok bool) {
	if pos := strings.Index(line, field); pos >= 0 {
		cutFrom := strings.Index(line, "#") // cut in-line comments
		if cutFrom < 0 {
			cutFrom = len(line)
		}
		return line[:pos], strings.TrimSpace(line[pos+len(field) : cutFrom]), true
	}
	return "", "", false
}

// LoadForks parses the beacon chain configuration file (config.yaml) and extracts the list of forks
func LoadForks(fileName string) (Forks, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, fmt.Errorf("Error opening beacon chain config file: %v", err)
	}
	defer file.Close()

	forkVersions := make(map[string][]byte)
	forkEpochs := make(map[string]uint64)
	reader := bufio.NewReader(file)
	for {
		l, _, err := reader.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Error reading beacon chain config file: %v", err)
		}
		line := string(l)
		if name, value, ok := fieldValue(line, "_FORK_VERSION:"); ok {
			if v, err := hexutil.Decode(value); err == nil {
				forkVersions[name] = v
			} else {
				return nil, fmt.Errorf("Error decoding hex fork id \"%s\" in beacon chain config file: %v", value, err)
			}
		}
		if name, value, ok := fieldValue(line, "_FORK_EPOCH:"); ok {
			if v, err := strconv.ParseUint(value, 10, 64); err == nil {
				forkEpochs[name] = v
			} else {
				return nil, fmt.Errorf("Error parsing epoch number \"%s\" in beacon chain config file: %v", value, err)
			}
		}
	}

	var forks Forks
	forkEpochs["GENESIS"] = 0
	for name, epoch := range forkEpochs {
		if version, ok := forkVersions[name]; ok {
			delete(forkVersions, name)
			forks = append(forks, Fork{Epoch: epoch, Name: name, Version: version})
		} else {
			return nil, fmt.Errorf("Fork id missing for \"%s\" in beacon chain config file", name)
		}
	}
	for name := range forkVersions {
		return nil, fmt.Errorf("Epoch number missing for fork \"%s\" in beacon chain config file", name)
	}
	sort.Sort(forks)
	return forks, nil
}

// SctConstraints defines constraints on the synced update chain. These constraints include
// the GenesisData, a range of periods (first <= period < afterFixed) where committe roots
// are fixed and another "free" range (afterFixed <= period < afterLast) where committee roots
// are determined by the best known update chain.
// An implementation of SctConstraints should call initCallback to pass GenesisData whenever
// it is available (either during SetCallbacks or later). If the constraints are changed then
// it should call updateCallback.
//
// Note: this interface can be used either for light syncing mode (in which case only the
// checkpoint is fixed and any valid update chain can be synced starting from there) or full
// syncing light service mode (in which case a full beacon header chain is synced based on the
// externally driven consensus and the update chain is fully restricted based on that).
type SctConstraints interface {
	PeriodRange() (first, afterFixed, afterLast uint64)
	CommitteeRoot(period uint64) (root common.Hash, matchAll bool) // matchAll is true in the free range where any committee root matches the constraints
	SetCallbacks(initCallback func(GenesisData), updateCallback func())
}

// GenesisData is required for signature verification and is set by the SyncCommitteeTracker.Init function.
type GenesisData struct {
	GenesisTime           uint64
	GenesisValidatorsRoot common.Hash
}

// CheckpointData contains known committee roots based on a weak subjectivity checkpoint
type CheckpointData struct {
	Checkpoint     common.Hash
	Period         uint64
	CommitteeRoots []common.Hash
}

// LightClientInitData contains light sync initialization data based on a weak subjectivity checkpoint
type LightClientInitData struct {
	GenesisData
	CheckpointData
}

// sctInitBackend retrieves light sync initialization data based on a weak subjectivity checkpoint hash
type sctInitBackend interface {
	GetInitData(ctx context.Context, checkpoint common.Hash) (Header, LightClientInitData, error)
}

// WeakSubjectivityCheckpoint implements SctConstraints in a way that it fixes the committee belonging to the checkpoint and allows
// forward extending the committee chain indefinitely. If a parent constraint is specified then it is applied for committee periods
// older than the checkpoint period, also allowing backward syncing the committees.
// Note that light clients typically do not need to backward sync, this feature is intended for nodes serving other clients that might
// have an earlier checkpoint.
type WeakSubjectivityCheckpoint struct {
	lock sync.RWMutex

	parent                              SctConstraints // constraints applied to pre-checkpoint history (no old committees synced if nil)
	db                                  ethdb.KeyValueStore
	initData                            LightClientInitData
	initialized                         bool
	initTriggerCh, parentInitCh, stopCh chan struct{}
	initCallback                        func(GenesisData)
	updateCallback                      func()
}

// NewWeakSubjectivityCheckpoint creates a WeakSubjectivityCheckpoint that either initializes itself from the specified
// sctInitBackend based on the given checkpoint or from the database if the same checkpoint has been fetched before.
func NewWeakSubjectivityCheckpoint(db ethdb.KeyValueStore, backend sctInitBackend, checkpoint common.Hash, parent SctConstraints) *WeakSubjectivityCheckpoint {
	wsc := &WeakSubjectivityCheckpoint{
		parent:        parent,
		db:            db,
		initTriggerCh: make(chan struct{}, 1),
		stopCh:        make(chan struct{}),
	}
	if parent != nil {
		wsc.parentInitCh = make(chan struct{})
	}

	var haveInitData bool
	if enc, err := db.Get(initDataKey); err == nil {
		var initData LightClientInitData
		if err := rlp.DecodeBytes(enc, &initData); err == nil {
			if initData.Checkpoint == checkpoint || initData.Checkpoint == (common.Hash{}) {
				log.Info("Beacon chain initialized with stored checkpoint", "checkpoint", initData.Checkpoint)
				wsc.initData = initData
				haveInitData = true
			}
		} else {
			log.Error("Error decoding stored beacon checkpoint", "error", err)
		}
	}
	if !haveInitData && checkpoint == (common.Hash{}) {
		return nil
	}
	go func() {
		var initData LightClientInitData
		if !haveInitData {
		loop:
			for {
				select {
				case <-wsc.stopCh:
					return
				case <-wsc.initTriggerCh:
					ctx, _ := context.WithTimeout(context.Background(), time.Second*20)
					log.Info("Requesting beacon init data", "checkpoint", checkpoint)
					var (
						header Header
						err    error
					)
					if header, initData, err = backend.GetInitData(ctx, checkpoint); err == nil {
						log.Info("Successfully initialized beacon chain", "checkpoint", checkpoint, "slot", header.Slot)
						break loop
					} else {
						log.Warn("Failed to retrieve beacon init data", "error", err)
					}
				}
			}
		}
		if wsc.parentInitCh != nil {
			select {
			case <-wsc.stopCh:
				return
			case <-wsc.parentInitCh:
			}
		}
		wsc.lock.Lock()
		wsc.initData = initData
		wsc.initialized = true
		initCallback := wsc.initCallback
		wsc.initCallback = nil
		wsc.lock.Unlock()
		if initCallback != nil {
			initCallback(initData.GenesisData)
		}
	}()
	return wsc
}

// init initializes the checkpoint with the given init data
func (wsc *WeakSubjectivityCheckpoint) init(initData LightClientInitData) {
	wsc.lock.Lock()
	if enc, err := rlp.EncodeToBytes(&initData); err == nil {
		wsc.db.Put(initDataKey, enc)
	} else {
		log.Error("Error encoding initData", "error", err)
	}
	wsc.initData, wsc.initialized = initData, true
	updateCallback, initCallback := wsc.updateCallback, wsc.initCallback
	wsc.lock.Unlock()
	if initCallback != nil {
		initCallback(initData.GenesisData)
	}
	updateCallback()
}

// PeriodRange implements SctConstraints
func (wsc *WeakSubjectivityCheckpoint) PeriodRange() (first, afterFixed, afterLast uint64) {
	wsc.lock.RLock()
	defer wsc.lock.RUnlock()

	if !wsc.initialized {
		return
	}
	if wsc.parent != nil {
		first, afterFixed, afterLast = wsc.parent.PeriodRange()
	}
	if afterFixed < wsc.initData.Period {
		first = wsc.initData.Period
	}
	wscAfterLast := wsc.initData.Period + uint64(len(wsc.initData.CommitteeRoots))
	if afterFixed < wscAfterLast {
		afterFixed = wscAfterLast
	}
	afterLast = math.MaxUint64 // no constraints on valid committee updates after the checkpoint
	return
}

// CommitteeRoot implements SctConstraints
func (wsc *WeakSubjectivityCheckpoint) CommitteeRoot(period uint64) (root common.Hash, matchAll bool) {
	wsc.lock.RLock()
	defer wsc.lock.RUnlock()

	if !wsc.initialized {
		return common.Hash{}, false
	}
	switch {
	case period < wsc.initData.Period:
		if wsc.parent != nil {
			return wsc.parent.CommitteeRoot(period)
		}
		return common.Hash{}, false
	case period >= wsc.initData.Period && period < wsc.initData.Period+uint64(len(wsc.initData.CommitteeRoots)):
		return wsc.initData.CommitteeRoots[int(period-wsc.initData.Period)], false
	default:
		return common.Hash{}, true // match all, no constraints on valid committee updates after the checkpoint
	}
}

// SetCallbacks implements SctConstraints
func (wsc *WeakSubjectivityCheckpoint) SetCallbacks(initCallback func(GenesisData), updateCallback func()) {
	wsc.lock.Lock()
	if wsc.initialized {
		wsc.lock.Unlock()
		initCallback(wsc.initData.GenesisData)
	} else {
		wsc.initCallback = initCallback
		wsc.updateCallback = updateCallback
		wsc.lock.Unlock()
	}
	if wsc.parent != nil {
		wsc.parent.SetCallbacks(func(GenesisData) { close(wsc.parentInitCh) }, updateCallback)
	}
}

// TriggerFetch triggers fetching the init data from the backend
func (wsc *WeakSubjectivityCheckpoint) TriggerFetch() {
	select {
	case wsc.initTriggerCh <- struct{}{}:
	default:
	}
}

// Stop should be called after ODR backend shutdown to ensure that init request does not get stuck
func (wsc *WeakSubjectivityCheckpoint) Stop() {
	close(wsc.stopCh)
}
