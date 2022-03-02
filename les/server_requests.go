// Copyright 2021 The go-ethereum Authors
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
	"encoding/binary"
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// serverBackend defines the backend functions needed for serving LES requests
type serverBackend interface {
	ArchiveMode() bool
	AddTxsSync() bool
	BlockChain() *core.BlockChain
	BeaconChain() *beaconChain
	TxPool() *core.TxPool
	GetHelperTrie(typ uint, index uint64) *trie.Trie
}

// Decoder is implemented by the messages passed to the handler functions
type Decoder interface {
	Decode(val interface{}) error
}

// RequestType is a static struct that describes an LES request type and references
// its handler function.
type RequestType struct {
	Name                                                             string
	MaxCount                                                         uint64
	InPacketsMeter, InTrafficMeter, OutPacketsMeter, OutTrafficMeter metrics.Meter
	ServingTimeMeter                                                 metrics.Timer
	Handle                                                           func(msg Decoder) (serve serveRequestFn, reqID, amount uint64, err error)
}

// serveRequestFn is returned by the request handler functions after decoding the request.
// This function does the actual request serving using the supplied backend. waitOrStop is
// called between serving individual request items and may block if the serving process
// needs to be throttled. If it returns false then the process is terminated.
// The reply is not sent by this function yet. The flow control feedback value is supplied
// by the protocol handler when calling the send function of the returned reply struct.
type serveRequestFn func(backend serverBackend, peer *clientPeer, waitOrStop func() bool) *reply

// Les3 contains the request types supported by les/2 and les/3
var Les3 = map[uint64]RequestType{
	GetBlockHeadersMsg: {
		Name:             "block header request",
		MaxCount:         MaxHeaderFetch,
		InPacketsMeter:   miscInHeaderPacketsMeter,
		InTrafficMeter:   miscInHeaderTrafficMeter,
		OutPacketsMeter:  miscOutHeaderPacketsMeter,
		OutTrafficMeter:  miscOutHeaderTrafficMeter,
		ServingTimeMeter: miscServingTimeHeaderTimer,
		Handle:           handleGetBlockHeaders,
	},
	GetBlockBodiesMsg: {
		Name:             "block bodies request",
		MaxCount:         MaxBodyFetch,
		InPacketsMeter:   miscInBodyPacketsMeter,
		InTrafficMeter:   miscInBodyTrafficMeter,
		OutPacketsMeter:  miscOutBodyPacketsMeter,
		OutTrafficMeter:  miscOutBodyTrafficMeter,
		ServingTimeMeter: miscServingTimeBodyTimer,
		Handle:           handleGetBlockBodies,
	},
	GetCodeMsg: {
		Name:             "code request",
		MaxCount:         MaxCodeFetch,
		InPacketsMeter:   miscInCodePacketsMeter,
		InTrafficMeter:   miscInCodeTrafficMeter,
		OutPacketsMeter:  miscOutCodePacketsMeter,
		OutTrafficMeter:  miscOutCodeTrafficMeter,
		ServingTimeMeter: miscServingTimeCodeTimer,
		Handle:           handleGetCode,
	},
	GetReceiptsMsg: {
		Name:             "receipts request",
		MaxCount:         MaxReceiptFetch,
		InPacketsMeter:   miscInReceiptPacketsMeter,
		InTrafficMeter:   miscInReceiptTrafficMeter,
		OutPacketsMeter:  miscOutReceiptPacketsMeter,
		OutTrafficMeter:  miscOutReceiptTrafficMeter,
		ServingTimeMeter: miscServingTimeReceiptTimer,
		Handle:           handleGetReceipts,
	},
	GetProofsV2Msg: {
		Name:             "les/2 proofs request",
		MaxCount:         MaxProofsFetch,
		InPacketsMeter:   miscInTrieProofPacketsMeter,
		InTrafficMeter:   miscInTrieProofTrafficMeter,
		OutPacketsMeter:  miscOutTrieProofPacketsMeter,
		OutTrafficMeter:  miscOutTrieProofTrafficMeter,
		ServingTimeMeter: miscServingTimeTrieProofTimer,
		Handle:           handleGetProofs,
	},
	GetHelperTrieProofsMsg: {
		Name:             "helper trie proof request",
		MaxCount:         MaxHelperTrieProofsFetch,
		InPacketsMeter:   miscInHelperTriePacketsMeter,
		InTrafficMeter:   miscInHelperTrieTrafficMeter,
		OutPacketsMeter:  miscOutHelperTriePacketsMeter,
		OutTrafficMeter:  miscOutHelperTrieTrafficMeter,
		ServingTimeMeter: miscServingTimeHelperTrieTimer,
		Handle:           handleGetHelperTrieProofs,
	},
	SendTxV2Msg: {
		Name:             "new transactions",
		MaxCount:         MaxTxSend,
		InPacketsMeter:   miscInTxsPacketsMeter,
		InTrafficMeter:   miscInTxsTrafficMeter,
		OutPacketsMeter:  miscOutTxsPacketsMeter,
		OutTrafficMeter:  miscOutTxsTrafficMeter,
		ServingTimeMeter: miscServingTimeTxTimer,
		Handle:           handleSendTx,
	},
	GetTxStatusMsg: {
		Name:             "transaction status query request",
		MaxCount:         MaxTxStatus,
		InPacketsMeter:   miscInTxStatusPacketsMeter,
		InTrafficMeter:   miscInTxStatusTrafficMeter,
		OutPacketsMeter:  miscOutTxStatusPacketsMeter,
		OutTrafficMeter:  miscOutTxStatusTrafficMeter,
		ServingTimeMeter: miscServingTimeTxStatusTimer,
		Handle:           handleGetTxStatus,
	},
}

// Les5 contains the request types supported by les/5     //TODO
var Les5 = map[uint64]RequestType{
	GetCommitteeProofsMsg: {
		Name:             "sync committee proof request",
		MaxCount:         MaxCommitteeFetch,
		InPacketsMeter:   miscInCommitteeProofPacketsMeter,
		InTrafficMeter:   miscInCommitteeProofTrafficMeter,
		OutPacketsMeter:  miscOutCommitteeProofPacketsMeter,
		OutTrafficMeter:  miscOutCommitteeProofTrafficMeter,
		ServingTimeMeter: miscServingTimeCommitteeProofTimer,
		Handle:           handleGetCommitteeProofs,
	},
	GetBeaconSlotsMsg: {
		Name:             "les/5 beacon header request",
		MaxCount:         MaxHeaderFetch,
		InPacketsMeter:   miscInBeaconHeaderPacketsMeter,
		InTrafficMeter:   miscInBeaconHeaderTrafficMeter,
		OutPacketsMeter:  miscOutBeaconHeaderPacketsMeter,
		OutTrafficMeter:  miscOutBeaconHeaderTrafficMeter,
		ServingTimeMeter: miscServingTimeBeaconHeaderTimer,
		Handle:           handleGetBeaconSlots,
	},
	GetExecHeadersMsg: {
		Name:             "les/5 exec header request",
		MaxCount:         MaxHeaderFetch,
		InPacketsMeter:   miscInExecHeaderPacketsMeter,
		InTrafficMeter:   miscInExecHeaderTrafficMeter,
		OutPacketsMeter:  miscOutExecHeaderPacketsMeter,
		OutTrafficMeter:  miscOutExecHeaderTrafficMeter,
		ServingTimeMeter: miscServingTimeExecHeaderTimer,
		Handle:           handleGetExecHeaders,
	},
}

// handleGetBlockHeaders handles a block header request
func handleGetBlockHeaders(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetBlockHeadersPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		// Gather headers until the fetch or network limits is reached
		var (
			bc              = backend.BlockChain()
			hashMode        = r.Query.Origin.Hash != (common.Hash{})
			first           = true
			maxNonCanonical = uint64(100)
			bytes           common.StorageSize
			headers         []*types.Header
			unknown         bool
		)
		for !unknown && len(headers) < int(r.Query.Amount) && bytes < softResponseLimit {
			if !first && !waitOrStop() {
				return nil
			}
			// Retrieve the next header satisfying the r
			var origin *types.Header
			if hashMode {
				if first {
					origin = bc.GetHeaderByHash(r.Query.Origin.Hash)
					if origin != nil {
						r.Query.Origin.Number = origin.Number.Uint64()
					}
				} else {
					origin = bc.GetHeader(r.Query.Origin.Hash, r.Query.Origin.Number)
				}
			} else {
				origin = bc.GetHeaderByNumber(r.Query.Origin.Number)
			}
			if origin == nil {
				break
			}
			headers = append(headers, origin)
			bytes += estHeaderRlpSize

			// Advance to the next header of the r
			switch {
			case hashMode && r.Query.Reverse:
				// Hash based traversal towards the genesis block
				ancestor := r.Query.Skip + 1
				if ancestor == 0 {
					unknown = true
				} else {
					r.Query.Origin.Hash, r.Query.Origin.Number = bc.GetAncestor(r.Query.Origin.Hash, r.Query.Origin.Number, ancestor, &maxNonCanonical)
					unknown = r.Query.Origin.Hash == common.Hash{}
				}
			case hashMode && !r.Query.Reverse:
				// Hash based traversal towards the leaf block
				var (
					current = origin.Number.Uint64()
					next    = current + r.Query.Skip + 1
				)
				if next <= current {
					infos, _ := json.Marshal(p.Peer.Info())
					p.Log().Warn("GetBlockHeaders skip overflow attack", "current", current, "skip", r.Query.Skip, "next", next, "attacker", string(infos))
					unknown = true
				} else {
					if header := bc.GetHeaderByNumber(next); header != nil {
						nextHash := header.Hash()
						expOldHash, _ := bc.GetAncestor(nextHash, next, r.Query.Skip+1, &maxNonCanonical)
						if expOldHash == r.Query.Origin.Hash {
							r.Query.Origin.Hash, r.Query.Origin.Number = nextHash, next
						} else {
							unknown = true
						}
					} else {
						unknown = true
					}
				}
			case r.Query.Reverse:
				// Number based traversal towards the genesis block
				if r.Query.Origin.Number >= r.Query.Skip+1 {
					r.Query.Origin.Number -= r.Query.Skip + 1
				} else {
					unknown = true
				}

			case !r.Query.Reverse:
				// Number based traversal towards the leaf block
				r.Query.Origin.Number += r.Query.Skip + 1
			}
			first = false
		}
		return p.replyBlockHeaders(r.ReqID, headers)
	}, r.ReqID, r.Query.Amount, nil
}

// handleGetBlockBodies handles a block body request
func handleGetBlockBodies(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetBlockBodiesPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		var (
			bytes  int
			bodies []rlp.RawValue
		)
		bc := backend.BlockChain()
		for i, hash := range r.Hashes {
			if i != 0 && !waitOrStop() {
				return nil
			}
			if bytes >= softResponseLimit {
				break
			}
			body := bc.GetBodyRLP(hash)
			if body == nil {
				p.bumpInvalid()
				continue
			}
			bodies = append(bodies, body)
			bytes += len(body)
		}
		return p.replyBlockBodiesRLP(r.ReqID, bodies)
	}, r.ReqID, uint64(len(r.Hashes)), nil
}

// handleGetCode handles a contract code request
func handleGetCode(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetCodePacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		var (
			bytes int
			data  [][]byte
		)
		bc := backend.BlockChain()
		for i, request := range r.Reqs {
			if i != 0 && !waitOrStop() {
				return nil
			}
			// Look up the root hash belonging to the request
			header := bc.GetHeaderByHash(request.BHash)
			if header == nil {
				p.Log().Warn("Failed to retrieve associate header for code", "hash", request.BHash)
				p.bumpInvalid()
				continue
			}
			// Refuse to search stale state data in the database since looking for
			// a non-exist key is kind of expensive.
			local := bc.CurrentHeader().Number.Uint64()
			if !backend.ArchiveMode() && header.Number.Uint64()+core.TriesInMemory <= local {
				p.Log().Debug("Reject stale code request", "number", header.Number.Uint64(), "head", local)
				p.bumpInvalid()
				continue
			}
			triedb := bc.StateCache().TrieDB()

			account, err := getAccount(triedb, header.Root, common.BytesToHash(request.AccKey))
			if err != nil {
				p.Log().Warn("Failed to retrieve account for code", "block", header.Number, "hash", header.Hash(), "account", common.BytesToHash(request.AccKey), "err", err)
				p.bumpInvalid()
				continue
			}
			code, err := bc.StateCache().ContractCode(common.BytesToHash(request.AccKey), common.BytesToHash(account.CodeHash))
			if err != nil {
				p.Log().Warn("Failed to retrieve account code", "block", header.Number, "hash", header.Hash(), "account", common.BytesToHash(request.AccKey), "codehash", common.BytesToHash(account.CodeHash), "err", err)
				continue
			}
			// Accumulate the code and abort if enough data was retrieved
			data = append(data, code)
			if bytes += len(code); bytes >= softResponseLimit {
				break
			}
		}
		return p.replyCode(r.ReqID, data)
	}, r.ReqID, uint64(len(r.Reqs)), nil
}

// handleGetReceipts handles a block receipts request
func handleGetReceipts(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetReceiptsPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		var (
			bytes    int
			receipts []rlp.RawValue
		)
		bc := backend.BlockChain()
		for i, hash := range r.Hashes {
			if i != 0 && !waitOrStop() {
				return nil
			}
			if bytes >= softResponseLimit {
				break
			}
			// Retrieve the requested block's receipts, skipping if unknown to us
			results := bc.GetReceiptsByHash(hash)
			if results == nil {
				if header := bc.GetHeaderByHash(hash); header == nil || header.ReceiptHash != types.EmptyRootHash {
					p.bumpInvalid()
					continue
				}
			}
			// If known, encode and queue for response packet
			if encoded, err := rlp.EncodeToBytes(results); err != nil {
				log.Error("Failed to encode receipt", "err", err)
			} else {
				receipts = append(receipts, encoded)
				bytes += len(encoded)
			}
		}
		return p.replyReceiptsRLP(r.ReqID, receipts)
	}, r.ReqID, uint64(len(r.Hashes)), nil
}

// handleGetProofs handles a proof request
func handleGetProofs(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetProofsPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		var (
			lastBHash common.Hash
			root      common.Hash
			header    *types.Header
			err       error
		)
		bc := backend.BlockChain()
		nodes := light.NewNodeSet()

		for i, request := range r.Reqs {
			if i != 0 && !waitOrStop() {
				return nil
			}
			// Look up the root hash belonging to the request
			if request.BHash != lastBHash {
				root, lastBHash = common.Hash{}, request.BHash

				if header = bc.GetHeaderByHash(request.BHash); header == nil {
					p.Log().Warn("Failed to retrieve header for proof", "hash", request.BHash)
					p.bumpInvalid()
					continue
				}
				// Refuse to search stale state data in the database since looking for
				// a non-exist key is kind of expensive.
				local := bc.CurrentHeader().Number.Uint64()
				if !backend.ArchiveMode() && header.Number.Uint64()+core.TriesInMemory <= local {
					p.Log().Debug("Reject stale trie request", "number", header.Number.Uint64(), "head", local)
					p.bumpInvalid()
					continue
				}
				root = header.Root
			}
			// If a header lookup failed (non existent), ignore subsequent requests for the same header
			if root == (common.Hash{}) {
				p.bumpInvalid()
				continue
			}
			// Open the account or storage trie for the request
			statedb := bc.StateCache()

			var trie state.Trie
			switch len(request.AccKey) {
			case 0:
				// No account key specified, open an account trie
				trie, err = statedb.OpenTrie(root)
				if trie == nil || err != nil {
					p.Log().Warn("Failed to open storage trie for proof", "block", header.Number, "hash", header.Hash(), "root", root, "err", err)
					continue
				}
			default:
				// Account key specified, open a storage trie
				account, err := getAccount(statedb.TrieDB(), root, common.BytesToHash(request.AccKey))
				if err != nil {
					p.Log().Warn("Failed to retrieve account for proof", "block", header.Number, "hash", header.Hash(), "account", common.BytesToHash(request.AccKey), "err", err)
					p.bumpInvalid()
					continue
				}
				trie, err = statedb.OpenStorageTrie(common.BytesToHash(request.AccKey), account.Root)
				if trie == nil || err != nil {
					p.Log().Warn("Failed to open storage trie for proof", "block", header.Number, "hash", header.Hash(), "account", common.BytesToHash(request.AccKey), "root", account.Root, "err", err)
					continue
				}
			}
			// Prove the user's request from the account or stroage trie
			if err := trie.Prove(request.Key, request.FromLevel, nodes); err != nil {
				p.Log().Warn("Failed to prove state request", "block", header.Number, "hash", header.Hash(), "err", err)
				continue
			}
			if nodes.DataSize() >= softResponseLimit {
				break
			}
		}
		return p.replyProofsV2(r.ReqID, nodes.NodeList())
	}, r.ReqID, uint64(len(r.Reqs)), nil
}

// handleGetHelperTrieProofs handles a helper trie proof request
func handleGetHelperTrieProofs(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetHelperTrieProofsPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		var (
			lastIdx  uint64
			lastType uint
			auxTrie  *trie.Trie
			auxBytes int
			auxData  [][]byte
		)
		bc := backend.BlockChain()
		nodes := light.NewNodeSet()
		for i, request := range r.Reqs {
			if i != 0 && !waitOrStop() {
				return nil
			}
			if auxTrie == nil || request.Type != lastType || request.TrieIdx != lastIdx {
				lastType, lastIdx = request.Type, request.TrieIdx
				auxTrie = backend.GetHelperTrie(request.Type, request.TrieIdx)
			}
			if auxTrie == nil {
				return nil
			}
			// TODO(rjl493456442) short circuit if the proving is failed.
			// The original client side code has a dirty hack to retrieve
			// the headers with no valid proof. Keep the compatibility for
			// legacy les protocol and drop this hack when the les2/3 are
			// not supported.
			err := auxTrie.Prove(request.Key, request.FromLevel, nodes)
			if p.version >= lpv4 && err != nil {
				return nil
			}
			if request.Type == htCanonical && request.AuxReq == htAuxHeader && len(request.Key) == 8 {
				header := bc.GetHeaderByNumber(binary.BigEndian.Uint64(request.Key))
				data, err := rlp.EncodeToBytes(header)
				if err != nil {
					log.Error("Failed to encode header", "err", err)
					return nil
				}
				auxData = append(auxData, data)
				auxBytes += len(data)
			}
			if nodes.DataSize()+auxBytes >= softResponseLimit {
				break
			}
		}
		return p.replyHelperTrieProofs(r.ReqID, HelperTrieResps{Proofs: nodes.NodeList(), AuxData: auxData})
	}, r.ReqID, uint64(len(r.Reqs)), nil
}

// handleSendTx handles a transaction propagation request
func handleSendTx(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r SendTxPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	amount := uint64(len(r.Txs))
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		stats := make([]light.TxStatus, len(r.Txs))
		for i, tx := range r.Txs {
			if i != 0 && !waitOrStop() {
				return nil
			}
			hash := tx.Hash()
			stats[i] = txStatus(backend, hash)
			if stats[i].Status == core.TxStatusUnknown {
				addFn := backend.TxPool().AddRemotes
				// Add txs synchronously for testing purpose
				if backend.AddTxsSync() {
					addFn = backend.TxPool().AddRemotesSync
				}
				if errs := addFn([]*types.Transaction{tx}); errs[0] != nil {
					stats[i].Error = errs[0].Error()
					continue
				}
				stats[i] = txStatus(backend, hash)
			}
		}
		return p.replyTxStatus(r.ReqID, stats)
	}, r.ReqID, amount, nil
}

// handleGetTxStatus handles a transaction status query
func handleGetTxStatus(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetTxStatusPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		stats := make([]light.TxStatus, len(r.Hashes))
		for i, hash := range r.Hashes {
			if i != 0 && !waitOrStop() {
				return nil
			}
			stats[i] = txStatus(backend, hash)
		}
		return p.replyTxStatus(r.ReqID, stats)
	}, r.ReqID, uint64(len(r.Hashes)), nil
}

// txStatus returns the status of a specified transaction.
func txStatus(b serverBackend, hash common.Hash) light.TxStatus {
	var stat light.TxStatus
	// Looking the transaction in txpool first.
	stat.Status = b.TxPool().Status([]common.Hash{hash})[0]

	// If the transaction is unknown to the pool, try looking it up locally.
	if stat.Status == core.TxStatusUnknown {
		lookup := b.BlockChain().GetTransactionLookup(hash)
		if lookup != nil {
			stat.Status = core.TxStatusIncluded
			stat.Lookup = lookup
		}
	}
	return stat
}

func handleGetCommitteeProofs(msg Decoder) (serveRequestFn, uint64, uint64, error) { //TODO
	return nil, 0, 0, nil
}

func handleGetBeaconSlots(msg Decoder) (serveRequestFn, uint64, uint64, error) { //TODO
	var r GetBeaconSlotsPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		bc := backend.BeaconChain()
		headBlock := bc.getBlockDataByBlockRoot(r.BeaconHash)
		if headBlock == nil {
			return nil
		}
		headSlot := headBlock.Header.Slot
		lastSlot := r.LastSlot
		if lastSlot > headSlot {
			lastSlot = headSlot
		}
		var lastHead *beaconBlockData
		if r.LastBeaconHead != (common.Hash{}) {
			lastHead = bc.getBlockDataByBlockRoot(r.LastBeaconHead)
		}

		var (
			firstSlot       uint64
			firstParentRoot common.Hash
			blocks          []*beaconBlockData
			proofFormats    []byte
			headers         []beaconHeaderForTransmission
			proofValues     merkleValues
			ht              *historicTree
		)

		if lastSlot >= r.MaxSlots+bc.tailLongTerm {
			firstSlot = lastSlot - r.MaxSlots + 1
		} else {
			firstSlot = bc.tailLongTerm
		}

		if lastSlot != headSlot || r.MaxSlots > 1 || lastHead != nil {
			// use historic tree (BeaconHash needs to be close to the current local head)
			if ht = bc.getHistoricTree(r.BeaconHash); ht == nil {
				return nil
			}
		}

		extendFirst := true // apply range extension at the beginning in case of empty slots
		// find lastHead ancestor in canonical chain, adjust firstSlot if necessary
		for lastHead != nil && lastHead.Header.Slot+1 >= firstSlot {
			// historic tree should exist because lastHead != nil
			if ht.getStateRoot(lastHead.Header.Slot) == lastHead.stateRoot {
				firstSlot = lastHead.Header.Slot + 1
				extendFirst = false
				break
			}
			lastHead = bc.getParent(lastHead)
		}

		if firstSlot <= lastSlot {
			if r.ProofFormatMask != 0 {
				if ht != nil {
					// collect beacon blocks for specified range in blocks
					// apply range extensions if necessary
					firstBlock := bc.getBlockData(firstSlot, ht.getStateRoot(firstSlot), false)
					for firstBlock == nil && extendFirst && firstSlot > bc.tailLongTerm {
						firstSlot-- // extend range so that first returned slot is not empty
						firstBlock = bc.getBlockData(firstSlot, ht.getStateRoot(firstSlot), false)
					}
					var lastBlock *beaconBlockData
					for {
						if lastSlot == headSlot {
							lastBlock = headBlock
							break
						}
						// historic tree should exist because lastSlot != headSlot
						if lastBlock = bc.getBlockData(lastSlot, ht.getStateRoot(lastSlot), false); lastBlock != nil {
							break
						}
						lastSlot++ // extend range so that last returned slot is not empty
					}
					blocks = make([]*beaconBlockData, lastSlot+1-firstSlot)
					blocks[0] = firstBlock
					for slot := firstSlot + 1; slot < lastSlot; slot++ {
						blocks[int(slot-firstSlot)] = bc.getBlockData(slot, ht.getStateRoot(slot), false)
					}
					blocks[int(lastSlot-firstSlot)] = lastBlock
				} else {
					blocks = []*beaconBlockData{headBlock}
				}
				// fill proofFormats and headers
				proofFormats = make([]byte, len(blocks))
				headers = make([]beaconHeaderForTransmission, 0, len(blocks))
				for i, blockData := range blocks {
					if blockData != nil {
						proofFormats[i] = blockData.ProofFormat & r.ProofFormatMask
						if blockData.ProofFormat != 0 {
							headers = append(headers, beaconHeaderForTransmission{
								ProposerIndex: blockData.Header.ProposerIndex,
								BodyRoot:      blockData.Header.BodyRoot,
							})
						}
						if firstParentRoot == (common.Hash{}) {
							firstParentRoot = blockData.Header.ParentRoot
						}
					}
				}
			} else {
				// just return the state root proofs for the requested range, no range extension or LastBeaconHead check
				proofFormats = make([]byte, int(lastSlot+1-firstSlot))
			}

			// create multiproof
			if ht != nil && firstSlot < headSlot {
				if _, ok := traverseProof(ht.historicStateReader(), newMultiProofWriter(slotRangeFormat(headSlot, firstSlot, proofFormats), &proofValues, nil)); !ok {
					//TODO error log
					return nil
				}
			} else {
				if _, ok := traverseProof(headBlock.proof().reader(nil), newMultiProofWriter(stateProofFormats[proofFormats[0]], &proofValues, nil)); !ok {
					//TODO error log
					return nil
				}
			}
		}

		if ht != nil {
			// ht.release()  //TODO
		}
		/*  //TODO ???
			if bytes += len(code); bytes >= softResponseLimit {
			break
		}*/
		return p.replyBeaconSlots(r.ReqID, firstSlot, proofFormats, proofValues, firstParentRoot, headers)
	}, r.ReqID, r.MaxSlots, nil //TODO ???amount calculation
}

func handleGetExecHeaders(msg Decoder) (serveRequestFn, uint64, uint64, error) {
	var r GetExecHeadersPacket
	if err := msg.Decode(&r); err != nil {
		return nil, 0, 0, err
	}
	return func(backend serverBackend, p *clientPeer, waitOrStop func() bool) *reply {
		bc := backend.BeaconChain()
		ec := backend.BlockChain()
		var (
			refBlock     *beaconBlockData
			execHash     common.Hash
			reader       proofReader
			leafIndex    uint64
			ht           *historicTree
			historicSlot uint64
			proofValues  merkleValues
		)
		if r.ReqMode != ExecHashMode {
			if refBlock = bc.getBlockDataByBlockRoot(r.BeaconOrExecHash); refBlock == nil {
				return nil
			}
		}
		switch r.ReqMode {
		case ExecHashMode:
			execHash = r.BeaconOrExecHash
		case BeaconHashMode:
			reader = refBlock.proof().reader(nil)
			if e, ok := refBlock.getStateValue(bsiExecHead); ok {
				execHash = common.Hash(e)
			} else {
				return nil
			}
			leafIndex = bsiExecHead
		case HistoricMode:
			// use historic tree (BeaconHash needs to be close to the current local head)
			if ht = bc.getHistoricTree(r.BeaconOrExecHash); ht == nil {
				//TODO ??ht.release
				return nil
			}
			reader = ht.historicStateReader()
			block := bc.getBlockDataByExecNumber(ht, r.HistoricNumber)
			if block == nil {
				return nil
			}
			if e, ok := block.getStateValue(bsiExecHead); ok {
				execHash = common.Hash(e)
			} else {
				return nil
			}
			historicSlot = block.Header.Slot
			leafIndex = childIndex(slotProofIndex(ht.headBlock.Header.Slot, historicSlot), bsiExecHead)
		case FinalizedMode:
			var finalBlock *beaconBlockData
			if finalBlockRoot, ok := refBlock.getStateValue(bsiFinalBlock); ok {
				finalBlock = bc.getBlockDataByBlockRoot(common.Hash(finalBlockRoot))
			}
			if finalBlock == nil {
				return nil
			}
			if e, ok := finalBlock.getStateValue(bsiExecHead); ok {
				execHash = common.Hash(e)
			} else {
				return nil
			}
			reader = refBlock.proof().reader(func(index uint64) proofReader {
				if index == bsiFinalBlock {
					return finalBlock.Header.proof(finalBlock.stateRoot).reader(func(index uint64) proofReader {
						if index == bhiStateRoot {
							return finalBlock.proof().reader(nil)
						}
						return nil
					})
				}
				return nil
			})
			leafIndex = bsiFinalExecHash
		default:
			return nil
		}
		if reader != nil {
			if _, ok := traverseProof(reader, newMultiProofWriter(newIndexMapFormat().addLeaf(leafIndex, nil), &proofValues, nil)); !ok {
				//TODO error log
				return nil
			}
		}
		if ht != nil {
			// ht.release()  //TODO
		}
		headers := make([]*types.Header, int(r.MaxAmount))
		headerPtr := int(r.MaxAmount)
		if r.MaxAmount > 0 {
			lastRetrieved := ec.GetHeaderByHash(execHash)
			if lastRetrieved != nil {
				var lastHead *types.Header
				if r.LastExecHead != (common.Hash{}) {
					lastHead = ec.GetHeaderByHash(r.LastExecHead)
				}
				for lastRetrieved != nil && headerPtr > 0 {
					for lastHead != nil && lastHead.Number.Uint64() > lastRetrieved.Number.Uint64() {
						lastHead = ec.GetHeader(lastHead.ParentHash, lastHead.Number.Uint64()-1)
					}
					if lastHead != nil && lastHead.Hash() == lastRetrieved.Hash() {
						break
					}
					headerPtr--
					headers[headerPtr] = lastRetrieved
					lastRetrieved = ec.GetHeader(lastRetrieved.ParentHash, lastRetrieved.Number.Uint64()-1)
				}
			}
			headers = headers[headerPtr:]
		}

		/*  //TODO ???
			if bytes += len(code); bytes >= softResponseLimit {
			break
		}*/
		return p.replyExecHeaders(r.ReqID, historicSlot, proofValues, headers)
	}, r.ReqID, r.MaxAmount, nil //TODO ???amount calculation, check
}
