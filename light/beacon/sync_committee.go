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
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	lru "github.com/hashicorp/golang-lru"
	"github.com/minio/sha256-simd"
	bls "github.com/protolambda/bls12-381-util"
)

var (
	initDataKey      = []byte("init") // RLP(lightClientInitData)
	bestUpdateKey    = []byte("bu-")  // bigEndian64(syncPeriod) -> RLP(LightClientUpdate)  (nextCommittee only referenced by root hash)
	syncCommitteeKey = []byte("sc-")  // bigEndian64(syncPeriod) + committee root hash -> serialized committee
)

const (
	maxUpdateInfoLength = 128 //TODO ??same
	MaxUpdateFetch      = 128
	MaxCommitteeFetch   = 8
)

func (s *SignedHead) calculateScore() uint64 {
	if len(s.BitMask) != 64 {
		return 0 // signature check will filter it out later but we calculate score before sig check
	}
	var signerCount uint64
	for _, v := range s.BitMask {
		signerCount += uint64(bits.OnesCount8(v))
	}
	if signerCount <= 170 {
		return 0
	}
	baseScore := uint64(s.Header.Slot) * 0x400
	if signerCount <= 256 {
		return baseScore + signerCount*24 - 0x1000 // 0..0x800; 0..2 slots offset
	}
	if signerCount <= 341 {
		return baseScore + signerCount*12 - 0x400 // 0x800..0xC00; 2..3 slots offset
	}
	return baseScore + signerCount*3 + 0x800 // 0..0xC00..0xE00; 3..3.5 slots offset
}

type SignedHead struct {
	BitMask   []byte
	Signature []byte
	Header    Header
}

type syncCommittee struct {
	keys      [512]*bls.Pubkey
	aggregate *bls.Pubkey
}

type sctBackend interface {
	GetInitData(ctx context.Context, checkpoint common.Hash) (*BlockData, []byte, []byte, error)
	GetBestCommitteeProofs(ctx context.Context, peerID enode.ID, req CommitteeRequest) (CommitteeReply, error)
}

type SyncCommitteeTracker struct {
	lock                                                          sync.RWMutex
	db                                                            ethdb.Database
	backend                                                       sctBackend
	clock                                                         mclock.Clock
	bestUpdateCache, serializedCommitteeCache, syncCommitteeCache *lru.Cache

	initData                  lightClientInitData
	initTriggerCh, initDoneCh chan struct{}

	firstPeriod            uint64 // first bestUpdate stored in db (can be <= initData.Period)
	nextPeriod             uint64 // first bestUpdate not stored in db
	genesisValidatorsRoot  common.Hash
	forks                  Forks
	updateInfo             *UpdateInfo
	peers                  map[sctPeer]*sctPeerInfo
	requestTokenCh, stopCh chan struct{}
}

func NewSyncCommitteeTracker(db ethdb.Database, backend sctBackend, forks Forks, clock mclock.Clock) *SyncCommitteeTracker {
	db = rawdb.NewTable(db, "sct-")
	s := &SyncCommitteeTracker{
		db:             db,
		backend:        backend,
		clock:          clock,
		forks:          forks,
		peers:          make(map[sctPeer]*sctPeerInfo),
		initTriggerCh:  make(chan struct{}),
		initDoneCh:     make(chan struct{}),
		requestTokenCh: make(chan struct{}),
		stopCh:         make(chan struct{}),
	}
	s.bestUpdateCache, _ = lru.New(1000)
	s.serializedCommitteeCache, _ = lru.New(100)
	s.syncCommitteeCache, _ = lru.New(100) //TODO biztos jo, ha period alapjan cache-elunk? Nincs baj, ha lesz jobb vagy reorgolunk?

	if enc, err := s.db.Get(initDataKey); err == nil {
		var initData lightClientInitData
		if err := rlp.DecodeBytes(enc, &initData); err == nil {
			s.initData = initData
			s.nextPeriod = initData.Period + 1
			s.forks.computeDomains(initData.GenesisValidatorsRoot)
		} else {
			log.Error("Error decoding initData", "error", err)
		}
	}

	var np [8]byte
	binary.BigEndian.PutUint64(np[:], s.nextPeriod)
	iter := s.db.NewIterator(bestUpdateKey, np[:])
	kl := len(bestUpdateKey)
	// iterate through them all for simplicity; at most a few hundred items
	for iter.Next() {
		period := binary.BigEndian.Uint64(iter.Key()[kl : kl+8])
		if s.nextPeriod == 0 {
			s.firstPeriod = period
		}
		if s.nextPeriod != period {
			break // continuity guaranteed
		}
		s.nextPeriod = period + 1
	}
	iter.Release()

	// roll back updates belonging to a different fork
	for s.nextPeriod > 0 {
		if update := s.GetBestUpdate(s.nextPeriod - 1); update == nil || update.checkForkVersion(forks) {
			break //TODO checkpoint ele ne rollbackeljunk, ???
		}
		s.nextPeriod--
		s.deleteBestUpdate(s.nextPeriod)
	}
	go func() {
		for {
			select {
			case <-s.requestTokenCh:
				select {
				case <-s.clock.After(time.Second):
				case <-s.stopCh:
					return
				}
			case <-s.stopCh:
				return
			}
		}
	}()
	return s
}

func (s *SyncCommitteeTracker) Stop() {
	close(s.stopCh)
}

func (s *SyncCommitteeTracker) InitCheckpoint(checkpoint common.Hash) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.nextPeriod != nil && s.initData.Checkpoint == checkpoint {
		close(s.initDoneCh)
		return
	}

	go func() {
		for {
			select {
			case <-s.stopCh:
				return
			case <-s.initTriggerCh:
			}
			ctx, _ := context.WithTimeout(context.Background, time.Second*20)
			if block, committee, nextCommittee, err := s.backend.GetInitData(ctx, checkpoint); err == nil {
				s.init(block, committee, nextCommittee)
				close(s.initDoneCh)
				return
			}
		}
	}()
}

func (s *SyncCommitteeTracker) getInitData() lightClientInitData {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.initData
}

func (s *SyncCommitteeTracker) init(initData *lightClientInitData, committee, nextCommittee []byte) {
	s.lock.Lock()
	defer s.lock.Unlock()

	enc, err := rlp.EncodeToBytes(&initData)
	if err != nil {
		log.Error("Error encoding initData", "error", err)
		return
	}

	if s.getSyncCommitteeRoot(initData.Period) != initData.CommitteeRoot || s.getSyncCommitteeRoot(initData.Period+1) != initData.NextCommitteeRoot {
		s.clearDb()
		s.nextPeriod = initData.Period + 1
		s.firstPeriod = s.nextPeriod
		s.storeSerializedSyncCommittee(initData.Period, initData.CommitteeRoot, committee)
		s.storeSerializedSyncCommittee(initData.Period+1, initData.NextCommitteeRoot, nextCommittee)
	}
	s.db.Put(initDataKey, enc)
	s.initData = *initData
	s.forks.computeDomains(initData.GenesisValidatorsRoot)
}

func (s *SyncCommitteeTracker) clearDb() {
	iter := s.db.NewIterator(nil, nil)
	for iter.Next() {
		s.db.Delete(iter.Key())
	}
	iter.Release()
	s.bestUpdateCache.Purge()
	s.serializedCommitteeCache.Purge()
	s.syncCommitteeCache.Purge()
	s.updateInfo = nil
}

func getBestUpdateKey(period uint64) []byte {
	kl := len(bestUpdateKey)
	key := make([]byte, kl+8)
	copy(key[:kl], bestUpdateKey)
	binary.BigEndian.PutUint64(key[kl:], period)
	return key
}

func (s *SyncCommitteeTracker) getNextPeriod() uint64 {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.nextPeriod
}

func (s *SyncCommitteeTracker) GetBestUpdate(period uint64) *LightClientUpdate {
	if v, ok := s.bestUpdateCache.Get(period); ok {
		update, _ := v.(*LightClientUpdate)
		return update
	}
	if updateEnc, err := s.db.Get(getBestUpdateKey(period)); err == nil {
		update := new(LightClientUpdate)
		if err := rlp.DecodeBytes(updateEnc, update); err == nil {
			update.calculateScore()
			s.bestUpdateCache.Add(period, update)
			return update
		} else {
			log.Error("Error decoding best update", "error", err)
		}
	}
	s.bestUpdateCache.Add(period, nil)
	return nil
}

func (s *SyncCommitteeTracker) storeBestUpdate(update *LightClientUpdate) {
	period := uint64(update.Header.Slot) >> 13
	updateEnc, err := rlp.EncodeToBytes(update)
	if err != nil {
		log.Error("Error encoding LightClientUpdate", "error", err)
		return
	}
	s.bestUpdateCache.Add(period, update)
	s.db.Put(getBestUpdateKey(period), updateEnc)
	s.updateInfo = nil
}

func (s *SyncCommitteeTracker) deleteBestUpdate(period uint64) {
	s.db.Delete(getBestUpdateKey(period))
	s.bestUpdateCache.Remove(period)
	s.syncCommitteeCache.Remove(period)
	s.updateInfo = nil
}

const (
	sciSuccess = iota
	sciNeedCommittee
	sciWrongUpdate
	sciUnexpectedError
)

func (s *SyncCommitteeTracker) verifyUpdate(update *LightClientUpdate, committee *syncCommittee) bool {
	var checkRoot common.Hash
	if update.hasFinality() {
		if root, ok := verifySingleProof(update.FinalityBranch, BsiFinalBlock, MerkleValue(update.FinalizedHeader.Hash()), 0); !ok || root != update.Header.StateRoot {
			fmt.Println("wrong finality proof", ok, root, update.FinalizedHeader.StateRoot)
			return false
		}
		checkRoot = update.FinalizedHeader.StateRoot
	} else {
		checkRoot = update.Header.StateRoot
	}
	if root, ok := verifySingleProof(update.NextSyncCommitteeBranch, BsiNextSyncCommittee, MerkleValue(update.NextSyncCommitteeRoot), 0); !ok || root != checkRoot {
		fmt.Println("wrong nsc proof", ok, root, checkRoot)
		return false
	}
	return s.verifySignature(SignedHead{Header: *(update.signedHeader()), Signature: update.SyncCommitteeSignature, BitMask: update.SyncCommitteeBits}, committee)
}

// returns true if nextCommittee is needed; call again
// verifies update before inserting
func (s *SyncCommitteeTracker) insertUpdate(update *LightClientUpdate, committee *syncCommittee, nextCommittee []byte) int {
	s.lock.Lock()
	defer s.lock.Unlock()

	fmt.Println("insertUpdate", update, nextCommittee != nil)
	header := update.signedHeader()
	period := uint64(header.Slot) >> 13
	if !s.verifyUpdate(update, committee) {
		fmt.Println("wrong update")
		return sciWrongUpdate
	}

	if s.nextPeriod == 0 || period > s.nextPeriod || period < s.firstPeriod {
		log.Error("Unexpected insertUpdate", "period", period, "firstPeriod", s.firstPeriod, "nextPeriod", s.nextPeriod)
		return sciUnexpectedError
	}
	update.calculateScore()
	var rollback bool
	if period < s.nextPeriod {
		// update should already exist
		oldUpdate := s.GetBestUpdate(period)
		if oldUpdate == nil {
			log.Error("Update expected to exist but missing from db")
			return sciUnexpectedError
		}
		rollback = update.NextSyncCommitteeRoot != oldUpdate.NextSyncCommitteeRoot
		if rollback && period <= s.initData.Period {
			// never accept a different fork before the local checkpoint
			return sciWrongUpdate
		}
		if !update.score.betterThan(oldUpdate.score) {
			// not better that existing one, nothing to do
			return sciSuccess
		}
	}

	if (period == s.nextPeriod || rollback) && s.GetSerializedSyncCommittee(period+1, update.NextSyncCommitteeRoot) == nil {
		// committee is not yet stored in db
		if nextCommittee == nil {
			fmt.Println("need committee")
			return sciNeedCommittee
		}
		if SerializedCommitteeRoot(nextCommittee) != update.NextSyncCommitteeRoot {
			fmt.Println("wrong committee root")
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
	return sciSuccess
}

func getSyncCommitteeKey(period uint64, committeeRoot common.Hash) []byte {
	kl := len(syncCommitteeKey)
	key := make([]byte, kl+8+32)
	copy(key[:kl], syncCommitteeKey)
	binary.BigEndian.PutUint64(key[kl:kl+8], period)
	copy(key[kl+8:], committeeRoot[:])
	return key
}

func (s *SyncCommitteeTracker) GetSerializedSyncCommittee(period uint64, committeeRoot common.Hash) []byte {
	key := getSyncCommitteeKey(period, committeeRoot)
	var committee []byte
	if v, ok := s.serializedCommitteeCache.Get(string(key)); ok {
		committee, _ = v.([]byte)
	} else {
		committee, _ = s.db.Get(key)
		s.serializedCommitteeCache.Add(string(key), committee)
	}
	if len(committee) == 513*48 {
		return committee
	} else {
		return nil
	}
}

func (s *SyncCommitteeTracker) storeSerializedSyncCommittee(period uint64, committeeRoot common.Hash, committee []byte) {
	key := getSyncCommitteeKey(period, committeeRoot)
	s.serializedCommitteeCache.Add(string(key), committee)
	s.db.Put(key, committee)
}

func (s *SyncCommitteeTracker) verifySignature(head SignedHead, committee *syncCommittee) bool {
	if len(head.Signature) != 96 || len(head.BitMask) != 64 {
		fmt.Println("wrong sig size")
		return false
	}
	var signature bls.Signature
	var sigBytes [96]byte
	copy(sigBytes[:], head.Signature)
	if err := signature.Deserialize(&sigBytes); err != nil {
		fmt.Println("sig deserialize error", err)
		return false
	}

	//committee := s.getSyncCommittee(uint64(head.Header.Slot) >> 13)
	if committee == nil {
		fmt.Println("sig check: committee not found", uint64(head.Header.Slot)>>13)
		return false
	}
	var signerKeys [512]*bls.Pubkey
	signerCount := 0
	for i, key := range committee.keys {
		if head.BitMask[i/8]&(byte(1)<<(i%8)) != 0 {
			signerKeys[signerCount] = key
			signerCount++
		}
	}

	var signingRoot common.Hash
	hasher := sha256.New()
	headerHash := head.Header.Hash()
	hasher.Write(headerHash[:])
	domain := s.forks.domain(uint64(head.Header.Slot) >> 5)
	fmt.Println("sig check domain", domain)
	hasher.Write(domain[:])
	hasher.Sum(signingRoot[:0])
	fmt.Println("signingRoot", signingRoot, "signerCount", signerCount)

	vvv := bls.FastAggregateVerify(signerKeys[:signerCount], signingRoot[:], &signature)
	fmt.Println("sig bls check", vvv)
	return vvv
}

func computeDomain(forkVersion []byte, genesisValidatorsRoot common.Hash) MerkleValue {
	var forkVersion32, forkDataRoot, domain MerkleValue
	hasher := sha256.New()
	copy(forkVersion32[:len(forkVersion)], forkVersion)
	hasher.Write(forkVersion32[:])
	hasher.Write(genesisValidatorsRoot[:])
	hasher.Sum(forkDataRoot[:0])
	domain[0] = 7
	copy(domain[4:], forkDataRoot[:28])
	fmt.Println("computeDomain", forkVersion, genesisValidatorsRoot, domain)
	return domain
}

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

func deserializeSyncCommittee(enc []byte) *syncCommittee {
	if len(enc) != 513*48 {
		log.Error("Wrong input size for deserializeSyncCommittee", "expected", 513*48, "got", len(enc))
		return nil
	}
	sc := new(syncCommittee)
	for i := 0; i <= 512; i++ {
		pk := new(bls.Pubkey)
		var sk [48]byte
		copy(sk[:], enc[i*48:(i+1)*48])
		if err := pk.Deserialize(&sk); err != nil {
			log.Error("bls.Pubkey.Deserialize failed", "error", err, "data", sk)
			return nil
		}
		if i < 512 {
			sc.keys[i] = pk
		} else {
			sc.aggregate = pk
		}
	}
	return sc
}

func (s *SyncCommitteeTracker) getSyncCommitteeRoot(period uint64) common.Hash {
	if s.nextPeriod == 0 {
		return common.Hash{}
	}
	if period == s.initData.Period {
		return s.initData.CommitteeRoot
	}
	if period == s.initData.Period+1 {
		return s.initData.NextCommitteeRoot
	}
	if period == 0 {
		return common.Hash{}
	}
	if update := s.GetBestUpdate(period - 1); update != nil {
		return update.NextSyncCommitteeRoot
	}
	return common.Hash{}
}

func (s *SyncCommitteeTracker) GetSyncCommitteeRoot(period uint64) common.Hash {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.getSyncCommitteeRoot(period)
}

func (s *SyncCommitteeTracker) getSyncCommitteeLocked(period uint64) *syncCommittee {
	//fmt.Println("sct.getSyncCommittee", period)
	if v, ok := s.syncCommitteeCache.Get(period); ok {
		//fmt.Println(" cached")
		sc, _ := v.(*syncCommittee)
		return sc
	}
	if root := s.getSyncCommitteeRoot(period); root != (common.Hash{}) {
		//fmt.Println(" root", root)
		if sc := s.GetSerializedSyncCommittee(period, root); sc != nil {
			c := deserializeSyncCommittee(sc)
			s.syncCommitteeCache.Add(period, c)
			return c
		} else {
			//fmt.Println(" no committee")
			log.Error("Missing serialized sync committee", "period", period, "root", root)
		}
	} else {
		//fmt.Println(" no root", s.initData.Period)
	}
	return nil
}

func (s *SyncCommitteeTracker) getSyncCommittee(period uint64) *syncCommittee {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return s.getSyncCommitteeLocked(period)
}

type UpdateScore struct {
	signerCount, subPeriodIndex uint32
	finalized                   bool
}

type UpdateScores []UpdateScore //TODO RLP encode to single []byte

func (u *UpdateScore) encode(data []byte) {
	v := u.signerCount + u.subPeriodIndex<<10
	if u.finalized {
		v += 0x800000
	}
	var enc [4]byte
	binary.LittleEndian.PutUint32(enc[:], v)
	copy(data, enc[:3])
}

func (u *UpdateScore) decode(data []byte) {
	var enc [4]byte
	copy(enc[:3], data)
	v := binary.LittleEndian.Uint32(enc[:])
	u.signerCount = v & 0x3ff
	u.subPeriodIndex = (v >> 10) & 0x1fff
	u.finalized = (v & 0x800000) != 0
}

type UpdateInfo struct {
	LastPeriod uint64
	Scores     []byte // encoded; 3 bytes per period		//TODO use decoded version in memory
	//NextCommitteeRoot common.Hash	//TODO not needed?
}

func (s *SyncCommitteeTracker) GetUpdateInfo() *UpdateInfo {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.updateInfo != nil {
		return s.updateInfo
	}
	l := s.nextPeriod - s.firstPeriod
	if l > maxUpdateInfoLength {
		l = maxUpdateInfoLength
	}
	firstPeriod := s.nextPeriod - l

	u := &UpdateInfo{
		LastPeriod: s.nextPeriod - 1,
		Scores:     make([]byte, int(l)*3),
		//NextCommitteeRoot: s.getSyncCommitteeRoot(s.nextPeriod),
	}

	for period := firstPeriod; period < s.nextPeriod; period++ {
		if update := s.GetBestUpdate(uint64(period)); update != nil {
			scoreIndex := int(period-firstPeriod) * 3
			update.score.encode(u.Scores[scoreIndex : scoreIndex+3])
		} else {
			log.Error("Update missing from database", "period", period)
		}
	}

	s.updateInfo = u
	return u
}

/*type CommitteeRequest struct {
	UpdatePeriods, CommitteePeriods []uint64
}

type CommitteeReply struct {
	Updates    []LightClientUpdate
	Committees [][]byte
}

type committeeReplyWithId struct {
	id    uint64
	reply CommitteeReply
}*/

type sctPeerInfo struct {
	remoteInfo UpdateInfo
	forkPeriod uint64
	peerClosed chan struct{}
	wrongReply func()
}

func (s *SyncCommitteeTracker) SyncWithPeer(peerID enode.ID, peerClosed chan struct{}, wrongReply func(), remoteInfo UpdateInfo) {
	sp := &sctPeerInfo{
		remoteInfo: remoteInfo,
		forkPeriod: remoteInfo.LastPeriod + 1,
		peerClosed: peerClosed,
		wrongReply: wrongReply,
	}

	s.lock.Lock()
	oldSp := s.peers[peerID]
	s.peers[peerID] = peerInfo
	s.lock.Unlock()

	if oldSp != nil {
		// if the s.peers entry is changed before the goroutine could delete it then
		// it will continue with the updated sctPeerInfo instead of quitting
		return
	}

	go func() {
		exit := func() bool {
			s.lock.Lock()
			oldSp := sp
			sp = s.peers[peerID]
			if sp == oldSp {
				delete(s.peers, peerID)
			}
			s.lock.Unlock()
			return sp == oldSp
		}

		select {
		case s.initTriggerCh <- struct{}{}:
			for {
				select {
				case <-s.initDoneCh:
					break
				case <-s.stopCh:
					return
				}
			}
		case <-s.initDoneCh:
		case <-s.stopCh:
			return
		}

		for {
			var req CommitteeRequest
			for {
				s.lock.RLock()
				sp = s.peers[peerID]
				s.lock.RUnlock()
				req = s.nextRequest(sp)
				if req.UpdatePeriods == nil && req.CommitteePeriods == nil {
					if exit() {
						return
					}
					// if exit() returned false then sp is updated so it's worth trying again
				} else {
					break
				}
			}

			// throttle committee requests globally by waiting for a token
			for {
				select {
				case s.requestTokenCh <- struct{}{}:
					break
				case <-sp.peerClosed:
					if exit() {
						return
					}
				}
			}

			ctx, _ := context.WithTimeout(context.Background, time.Second*20)
			if reply, err := s.backend.GetBestCommitteeProofs(ctx, peerID, req); err == nil {
				if !s.processReply(sp, req, reply) {
					sp.wrongReply()
				}
			} else {
				if exit() {
					return
				}
			}
		}
	}()
}

func (s *SyncCommitteeTracker) nextRequest(sp *sctPeerInfo) CommitteeRequest {
	localInfo := s.GetUpdateInfo()
	localFirstPeriod := localInfo.LastPeriod + 1 - uint64(len(localInfo.Scores)/3)
	remoteFirstPeriod := sp.remoteInfo.LastPeriod + 1 - uint64(len(sp.remoteInfo.Scores)/3)
	var (
		period  uint64
		request CommitteeRequest
	)
	if remoteFirstPeriod > localFirstPeriod {
		period = remoteFirstPeriod
	} else {
		period = localFirstPeriod
	}
	for period <= sp.remoteInfo.LastPeriod && len(request.UpdatePeriods) < MaxUpdateFetch && len(request.CommitteePeriods) < MaxCommitteeFetch {
		var reqUpdate, reqCommittee bool
		if period <= localInfo.LastPeriod && period <= sp.forkPeriod {
			var localScore, remoteScore UpdateScore
			localPtr := int(period-localFirstPeriod) * 3
			localScore.decode(localInfo.Scores[localPtr : localPtr+3])
			remotePtr := int(period-remoteFirstPeriod) * 3
			remoteScore.decode(sp.remoteInfo.Scores[remotePtr : remotePtr+3])
			reqUpdate = remoteScore.betterThan(localScore)
			if period == sp.forkPeriod {
				reqCommittee = reqUpdate
				if !reqUpdate {
					break
				}
			}
		} else {
			reqUpdate, reqCommitee = true, true
		}
		if reqUpdate {
			request.UpdatePeriods = append(request.UpdatePeriods, period)
		}
		if reqCommitee {
			request.CommitteePeriods = append(request.CommitteePeriods, period+1)
		}
		period++
	}
	return request
}

func (s *SyncCommitteeTracker) processReply(sp *sctPeerInfo, sentRequest CommitteeRequest, reply CommitteeReply) bool {
	s.lock.Lock()
	defer s.lock.Unlock()

	fmt.Println("Processing committee reply", reply)
	if len(reply.Updates) != len(sentRequest.UpdatePeriods) || len(reply.Committees) != len(sentRequest.CommitteePeriods) {
		return false
	}
	var committeeIndex int
	firstPeriod := sp.remoteInfo.LastPeriod + 1 - uint64(len(sp.remoteInfo.Scores)/3)
	for i, update := range reply.Updates {
		period := uint64(update.Header.Slot) >> 13
		if period != sp.sentRequest.UpdatePeriods[i] {
			return false
		}
		if period < s.nextPeriod { // a previous insertUpdate could have reduced nextPeriod since the request was created
			return true // exit but do not fail because it is not the remote side's fault; retry with new request
		}
		var nextCommittee []byte
		if committeeIndex < len(sp.sentRequest.CommitteePeriods) && sp.sentRequest.CommitteePeriods[committeeIndex] == period+1 {
			// pick the matching nextCommittee from the Committees reply if present
			nextCommittee = reply.Committees[committeeIndex]
			if len(nextCommittee) != 513*48 {
				return false
			}
			committeeIndex++
		}

		remoteInfoIndex := int(period - firstPeriod)
		var remoteInfoScore UpdateScore
		remoteInfoScore.decode(sp.remoteInfo.Scores[remoteInfoIndex*3 : remoteInfoIndex*3+3])
		update.calculateScore()
		if remoteInfoScore.betterThan(update.score) {
			return false // remote did not deliver an update with the promised score
		}
		committee := s.getSyncCommitteeLocked(period)
		if committee == nil {
			log.Error("Missing sync committee while processing committee proofs reply", "period", period)
			return false // though not the remote's fault, fail here to avoid infinite retries
		}
		switch s.insertUpdate(&update, committee, nextCommittee) {
		case sciSuccess:
			if sp.forkPeriod == period {
				// if local chain is successfully updated to the remote fork then remote is not on a different fork anymore
				sp.forkPeriod = sp.remoteInfo.LastPeriod + 1
			}
		case sciWrongUpdate:
			return false
		case sciNeedCommittee:
			// remember that remote is on a different and more valuable fork; do not fail but construct next request accordingly
			sp.forkPeriod = period
			return true
		case sciUnexpectedError:
			// local error, insertUpdate has already printed an error log
			return false // though not the remote's fault, fail here to avoid infinite retries
		}
	}
	return true
}

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

type lightClientInitData struct {
	Checkpoint                                              common.Hash
	Period, GenesisTime                                     uint64
	CommitteeRoot, NextCommitteeRoot, GenesisValidatorsRoot common.Hash
}

func (l *LightClientUpdate) hasFinality() bool {
	return l.FinalizedHeader.BodyRoot != (common.Hash{})
}

func (l *LightClientUpdate) signedHeader() *Header { //TODO
	/*if l.hasFinality() {
		return &l.FinalizedHeader
	}*/
	return &l.Header
}

func (l *LightClientUpdate) calculateScore() {
	l.score.signerCount = 0
	for _, v := range l.SyncCommitteeBits {
		l.score.signerCount += uint32(bits.OnesCount8(v))
	}
	l.score.subPeriodIndex = uint32(l.Header.Slot & 0x1fff)
	l.score.finalized = l.hasFinality()
}

func (u *UpdateScore) penalty() uint64 {
	if u.signerCount == 0 {
		return math.MaxUint64
	}
	a := uint64(u.signerCount)
	b := a * a
	c := b * b
	p := a * b * c // signerCount ^ 7 (between 0 and 2^63)
	if !u.finalized {
		p += 0x2000000000000000 / uint64(u.subPeriodIndex+1)
	}
	return p
}

func (u UpdateScore) betterThan(w UpdateScore) bool {
	return u.penalty() < w.penalty()
}

func trimZeroes(data []byte) []byte {
	l := len(data)
	for l > 0 && data[l-1] == 0 {
		l--
	}
	return data[:l]
}

func (l *LightClientUpdate) checkForkVersion(forks Forks) bool {
	return bytes.Equal(trimZeroes(l.ForkVersion), trimZeroes(forks.version(uint64(l.signedHeader().Slot>>5))))
}

type hpPeer interface {
	sendBeaconHead(SignedHead)
}

type peerMap map[hpPeer]struct{}

type headInfo struct {
	head       SignedHead
	hash       common.Hash
	score      uint64
	sentTo     peerMap
	lastUpdate mclock.AbsTime
}

type HeadPropagator struct {
	lock      sync.Mutex
	sct       *SyncCommitteeTracker
	clock     mclock.Clock
	maxCount  int         // max length of bestHead
	bestHeads []*headInfo // short list, using linear search/sort
	subs      []peerMap
}

func NewHeadPropagator(sct *SyncCommitteeTracker, clock mclock.Clock, maxCount int) *HeadPropagator {
	hp := &HeadPropagator{
		sct:      sct,
		clock:    clock,
		maxCount: maxCount,
		subs:     make([]peerMap, maxCount),
	}
	for i := range hp.subs {
		hp.subs[i] = make(peerMap)
	}
	return hp
}

func (hp *HeadPropagator) Add(head SignedHead) {
	hp.lock.Lock()
	defer hp.lock.Unlock()

	score := head.calculateScore()
	if len(hp.bestHeads) == hp.maxCount && score <= hp.bestHeads[hp.maxCount-1].score {
		return
	}
	//TODO future slot filter
	if committee := hp.sct.getSyncCommittee(uint64(head.Header.Slot) >> 13); committee == nil || !hp.sct.verifySignature(head, committee) {
		return
	}

	h := &headInfo{
		head:       head,
		hash:       head.Header.Hash(),
		score:      score,
		sentTo:     make(peerMap),
		lastUpdate: hp.clock.Now(),
	}

	index := -1
	for i, hh := range hp.bestHeads {
		if hh.hash == h.hash {
			index = i
			break
		}
	}
	if index >= 0 {
		//TODO update freq limiter
		hp.bestHeads[index] = h
	} else {
		index = len(hp.bestHeads)
		hp.bestHeads = append(hp.bestHeads, h)
	}

	for index > 0 && hp.bestHeads[index].score > hp.bestHeads[index-1].score {
		hp.bestHeads[index], hp.bestHeads[index-1] = hp.bestHeads[index-1], hp.bestHeads[index]
		index--
	}

	if len(hp.bestHeads) > hp.maxCount {
		hp.bestHeads = hp.bestHeads[:hp.maxCount]
	}

	for i, subs := range hp.subs {
		if i > index {
			break
		}
		for peer := range subs {
			if _, ok := h.sentTo[peer]; !ok {
				peer.sendBeaconHead(h.head)
				h.sentTo[peer] = struct{}{}
			}
		}
	}
}

func (hp *HeadPropagator) getHeadsForPeer(peer hpPeer, level int, sub bool) []SignedHead {
	hp.lock.Lock()
	defer hp.lock.Unlock()

	var heads []SignedHead
	for i, head := range hp.bestHeads {
		if i >= level {
			break
		}
		if _, ok := head.sentTo[peer]; !ok {
			heads = append(heads, head.head)
			head.sentTo[peer] = struct{}{}
		}
	}

	if sub {
		if level > hp.maxCount {
			level = hp.maxCount
		}
		for i, subs := range hp.subs {
			if i == level-1 {
				subs[peer] = struct{}{}
			} else {
				delete(subs, peer)
			}
		}
	}
	return heads
}
