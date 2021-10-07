package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	//	"net/url"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/minio/sha256-simd"
	bls "github.com/protolambda/bls12-381-util"
)

type LightClientUpdate struct {
	Header                  BeaconBlockHeader `json:"header"`
	NextSyncCommittee       SyncCommittee     `json:"next_sync_committee"`
	NextSyncCommitteeBranch []common.Hash     `json:"next_sync_committee_branch"`
	FinalityHeader          BeaconBlockHeader `json:"finality_header"`
	FinalityBranch          []common.Hash     `json:"finality_branch"`
	SyncCommitteeBits       hexutil.Bytes     `json:"sync_committee_bits"`
	SyncCommitteeSignature  hexutil.Bytes     `json:"sync_committee_signature"`
	ForkVersion             hexutil.Bytes     `json:"fork_version"`
}

type SyncCommittee struct {
	Pubkeys         []hexutil.Bytes `json:"pubkeys"`
	AggregatePubkey hexutil.Bytes   `json:"aggregate_pubkey"`
}

type BeaconBlockHeader struct {
	Slot          common.Decimal `json:"slot"`
	ProposerIndex common.Decimal `json:"proposer_index"`
	ParentRoot    common.Hash    `json:"parent_root"`
	StateRoot     common.Hash    `json:"state_root"`
	BodyRoot      common.Hash    `json:"body_root"`
}

func treeHash(data [][32]byte) common.Hash {
	hasher := sha256.New()
	chunks := len(data)
	for chunks > 1 {
		for i := 0; i < chunks/2; i++ {
			hasher.Write(data[i*2][:])
			hasher.Write(data[i*2+1][:])
			hasher.Sum(data[i][:0])
			hasher.Reset()
		}
		chunks /= 2
	}
	return common.Hash(data[0])
}

func (h *BeaconBlockHeader) Hash() common.Hash {
	var data [8][32]byte
	binary.LittleEndian.PutUint64(data[0][:8], uint64(h.Slot))
	binary.LittleEndian.PutUint64(data[1][:8], uint64(h.ProposerIndex))
	data[2] = [32]byte(h.ParentRoot)
	data[3] = [32]byte(h.StateRoot)
	data[4] = [32]byte(h.BodyRoot)
	return treeHash(data[:])
}

func pubKeyHash(pubKey hexutil.Bytes) common.Hash {
	var data [2][32]byte
	copy(data[0][:], pubKey[:32])
	copy(data[1][:16], pubKey[32:48])
	return treeHash(data[:])
}

func (sc *SyncCommittee) Hash() common.Hash {
	pk := make([][32]byte, len(sc.Pubkeys))
	for i, pubkey := range sc.Pubkeys {
		pk[i] = pubKeyHash(pubkey)
	}
	var data [2][32]byte
	data[0] = [32]byte(treeHash(pk[:]))
	data[1] = [32]byte(pubKeyHash(sc.AggregatePubkey))
	return treeHash(data[:])
}

func isValidMerkleBranch(leaf common.Hash, proof []common.Hash, depth int, index uint64, root common.Hash) bool {
	hasher := sha256.New()
	value := leaf
	for i := 0; i < depth; i++ {
		//		if index%2 == 0 {
		if index%2 == 1 {
			hasher.Write(proof[i][:])
			hasher.Write(value[:])
		} else {
			hasher.Write(value[:])
			hasher.Write(proof[i][:])
		}
		index /= 2
		hasher.Sum(value[:0])
		hasher.Reset()
	}
	return value == root
}

func computeSigningRoot(objectRoot common.Hash, domain [32]byte) common.Hash {
	var data [2][32]byte
	data[0] = [32]byte(objectRoot)
	data[1] = domain
	return treeHash(data[:])
}

func computeDomain(domainType int, forkVersion uint32, genesisValidatorRoot common.Hash) [32]byte {
	var data [2][32]byte
	binary.BigEndian.PutUint32(data[0][:4], forkVersion)
	data[1] = [32]byte(genesisValidatorRoot)
	forkDataRoot := treeHash(data[:])
	var domain [32]byte
	//	binary.BigEndian.PutUint32(domain[:], uint32(domainType))
	binary.BigEndian.PutUint32(domain[:4], uint32(domainType))
	copy(domain[4:], forkDataRoot[:28])
	return domain
}

func main() {
	//	resp, _ := http.Get("http://127.0.0.1:9596/eth/v1/lightclient/latest_update_finalized")
	resp, _ := http.Get("http://127.0.0.1:9596/eth/v1/lightclient/best_updates?from=0&to=1000")
	body, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	//fmt.Println("LatestUpdate raw", string(body))

	resp2, _ := http.Get("http://127.0.0.1:9596/eth/v1/lightclient/init_proof/44044")
	body2, _ := ioutil.ReadAll(resp2.Body)
	resp2.Body.Close()
	var genesisValidatorRoot common.Hash
	copy(genesisValidatorRoot[:], body2[0x1039:0x1059])
	fmt.Printf("genesisValidatorRoot: %x\n", genesisValidatorRoot)

	var updates struct {
		Data []LightClientUpdate `json:"data"`
	}
	json.Unmarshal(body, &updates)

	for i, update := range updates.Data {
		fmt.Println()
		fmt.Println("update #", i)
		fmt.Println(update.Header.Slot, update.Header.Hash())
		fmt.Println(update.FinalityHeader.Slot, update.FinalityHeader.Hash())

		fmt.Println("Finality branch check", isValidMerkleBranch(update.Header.Hash(), update.FinalityBranch, 6, 41, update.FinalityHeader.StateRoot))
		fmt.Println("Sync committee branch check", isValidMerkleBranch(update.NextSyncCommittee.Hash(), update.NextSyncCommitteeBranch, 5, 23, update.Header.StateRoot))

		if i > 0 {
			syncCommittee := updates.Data[i-1].NextSyncCommittee
			var signature bls.Signature
			var sigBytes [96]byte
			copy(sigBytes[:], update.SyncCommitteeSignature)
			if err := signature.Deserialize(&sigBytes); err != nil {
				fmt.Println(1, err)
			}
			var signerKeys []*bls.Pubkey
			for i, pubkey := range syncCommittee.Pubkeys {
				//if update.SyncCommitteeBits[i/8]&(byte(128)>>(i%8)) != 0 {
				if update.SyncCommitteeBits[i/8]&(byte(1)<<(i%8)) != 0 {
					var pkBytes [48]byte
					copy(pkBytes[:], pubkey)
					pk := new(bls.Pubkey)
					if err := pk.Deserialize(&pkBytes); err != nil {
						fmt.Println(2, err)
					}
					signerKeys = append(signerKeys, pk)
				}
			}

			signingRoot := computeSigningRoot(update.FinalityHeader.Hash(), computeDomain(0x07000000, 0x01001020, genesisValidatorRoot))
			fmt.Println("Sync committee signature check", len(signerKeys), bls.FastAggregateVerify(signerKeys, signingRoot[:], &signature))
		}
	}

	resp3, _ := http.Get("http://127.0.0.1:9596/eth/v1/lightclient/latest_update_nonfinalized")
	body3, _ := ioutil.ReadAll(resp3.Body)
	resp3.Body.Close()
	var update struct {
		Data LightClientUpdate `json:"data"`
	}
	json.Unmarshal(body3, &update)
	head := update.Data.Header.Hash()
	fmt.Printf("latest head: %x\n", head)

	//	v := url.Values{}
	//	v.Set("paths", "[]")
	resp4, _ := http.Post("http://127.0.0.1:9596/eth/v1/lightclient/proof/"+update.Data.Header.StateRoot.Hex(), "application/json", strings.NewReader("[[\"stateRoots\", 1000]]"))
	body4, _ := ioutil.ReadAll(resp4.Body)
	resp4.Body.Close()
	fmt.Println(body4)
	fmt.Println(string(body4))
}
