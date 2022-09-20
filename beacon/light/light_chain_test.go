// Copyright 2023 The go-ethereum Authors
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

package light

import (
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/light/types"
	"github.com/ethereum/go-ethereum/beacon/merkle"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

func TestLightChainSetHead(t *testing.T) {
	for _, reload := range []bool{false, true} {
		c := newLightChainTest(t)
		a1, a2 := c.makeChain(types.Header{}, 100, false, false)
		b1, b2 := c.makeChain(a2, 150, true, false)
		c1, c2 := c.makeChain(b2, 200, true, true)
		d1, d2 := c.makeChain(c2, 250, true, true)
		_, e2 := c.makeChain(d2, 300, true, false)
		f1, f2 := c.makeChain(c2, 270, true, true)
		c.checkCanonical(a1, false)
		c.checkCanonical(f2, false)
		c.checkTail(e2, b1)
		c.checkTail(f2, b1)
		c.checkRange(false, types.Header{}, types.Header{}, false, types.Header{}, types.Header{})
		c.chain.SetHead(f2)
		c.checkCanonical(a2, false)
		c.checkCanonical(d1, false)
		c.checkCanonical(e2, false)
		if reload {
			c.reloadChain()
		}
		c.checkRange(true, b1, f2, true, c1, f2)
		c.chain.SetHead(e2)
		c.checkCanonical(f1, false)
		c.checkCanonical(f2, false)
		c.checkRange(true, b1, e2, true, c1, d2)
		_, g2 := c.makeChain(b2, 220, true, false)
		if reload {
			c.reloadChain()
		}
		c.chain.SetHead(g2)
		c.checkCanonical(c1, false)
		c.checkCanonical(f2, false)
		c.checkRange(true, b1, g2, false, types.Header{}, types.Header{})
		_, h2 := c.makeChain(types.Header{}, 100, false, false)
		if reload {
			c.reloadChain()
		}
		i1, i2 := c.makeChain(h2, 150, true, false)
		j1, j2 := c.makeChain(i2, 200, true, true)
		c.chain.SetHead(i2)
		c.checkTail(j2, i1)
		c.checkCanonical(b1, false)
		c.checkCanonical(j1, false)
		c.checkRange(true, i1, i2, false, types.Header{}, types.Header{})
		if reload {
			c.reloadChain()
		}
		c.chain.SetHead(j2)
		c.checkRange(true, i1, j2, true, j1, j2)
		c.chain.SetHead(i2)
		c.checkCanonical(j1, false)
		if reload {
			c.reloadChain()
		}
		c.checkRange(true, i1, i2, false, types.Header{}, types.Header{})
	}
}

func TestLightChainExtendHeaderTail(t *testing.T) {
	for _, reload := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			c := newLightChainTest(t)
			a1, a2 := c.makeChain(types.Header{}, 50, false, false)
			b1, b2 := c.makeChain(a2, 100, true, false)
			c.chain.SetHead(b2)
			c.checkTail(b2, b1)
			c.checkRange(true, b1, b2, false, types.Header{}, types.Header{})
			if reload {
				c.reloadChain()
			}
			if reverse {
				for i := len(c.headers) - 1; i >= 0; i-- {
					c.chain.AddHeader(c.headers[i])
				}
			} else {
				for _, header := range c.headers {
					c.chain.AddHeader(header)
				}
			}
			if reload {
				c.reloadChain()
			}
			c.checkTail(b2, a1)
			c.checkRange(true, a1, b2, false, types.Header{}, types.Header{})
		}
	}
}

func TestLightChainExtendStateRange(t *testing.T) {
	for _, reload := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			c := newLightChainTest(t)
			a1, a2 := c.makeChain(types.Header{}, 50, true, false)
			b1, b2 := c.makeChain(a2, 100, true, true)
			_, c2 := c.makeChain(b2, 150, true, false)
			c.chain.SetHead(c2)
			c.checkRange(true, a1, c2, true, b1, b2)
			if reload {
				c.reloadChain()
			}
			if reverse {
				for i := len(c.stateProofs) - 1; i >= 0; i-- {
					sp := c.stateProofs[i]
					c.chain.AddStateProof(sp.header, sp.proof)
				}
			} else {
				for _, sp := range c.stateProofs {
					c.chain.AddStateProof(sp.header, sp.proof)
				}
			}
			if reload {
				c.reloadChain()
			}
			c.checkRange(true, a1, c2, true, a1, c2)
		}
	}
}

func TestLightChainHasGetHeaderState(t *testing.T) {
	for _, reload := range []bool{false, true} {
		c := newLightChainTest(t)
		_, a2 := c.makeChain(types.Header{}, 50, false, false)
		b1, b2 := c.makeChain(a2, 100, true, false)
		_, c2 := c.makeChain(b2, 150, true, true)
		c.emptyRatio = 100
		_, d2 := c.makeChain(b2, 110, true, true)
		c.chain.SetHead(c2)
		if reload {
			c.reloadChain()
		}
		c.checkHeaderByHash(a2.Hash(), false)
		c.checkHeaderBySlot(a2.Slot, ErrNotFound)
		c.checkStateProof(a2, false)
		c.checkHeaderByHash(b1.Hash(), true)
		c.checkHeaderBySlot(b1.Slot, nil)
		c.checkStateProof(b1, false)
		c.checkHeaderByHash(c2.Hash(), true)
		c.checkHeaderBySlot(c2.Slot, nil)
		c.checkStateProof(c2, true)
		c.checkHeaderByHash(d2.Hash(), true)
		c.checkStateProof(d2, true)
		c.chain.SetHead(d2)
		if reload {
			c.reloadChain()
		}
		c.checkHeaderBySlot(d2.Slot-1, ErrEmptySlot)
		c.checkHeaderBySlot(d2.Slot, nil)
		c.checkHeaderBySlot(d2.Slot+1, ErrNotFound)
	}
}

func TestLightChainPrune(t *testing.T) {
	for _, reload := range []bool{false, true} {
		c := newLightChainTest(t)
		a1, a2 := c.makeChain(types.Header{}, 100, true, true)
		b1, b2 := c.makeChain(types.Header{}, 100, true, true)
		c.chain.SetHead(a2)
		if reload {
			c.reloadChain()
		}
		c.checkRange(true, a1, a2, true, a1, a2)
		c.checkHeaderByHash(a1.Hash(), true)
		c.checkStateProof(a1, true)
		c.checkHeaderByHash(b1.Hash(), true)
		c.checkStateProof(b1, true)
		c.chain.Prune(0, true)
		c.checkHeaderByHash(a1.Hash(), true)
		c.checkStateProof(a1, true)
		c.checkHeaderByHash(b1.Hash(), true)
		c.checkStateProof(b1, true)
		c.chain.Prune(100, false)
		if reload {
			c.reloadChain()
		}
		c.checkHeaderByHash(a1.Hash(), true)
		c.checkStateProof(a1, true)
		c.checkHeaderByHash(b1.Hash(), false)
		c.checkStateProof(b1, false)
		c.chain.Prune(100, true)
		c.checkHeaderByHash(a1.Hash(), false)
		c.checkStateProof(a1, false)
		c.checkRange(true, a2, a2, true, a2, a2)
		c.chain.Prune(1000, false)
		if reload {
			c.reloadChain()
		}
		c.checkHeaderByHash(a2.Hash(), true)
		c.checkStateProof(a2, true)
		c.checkHeaderByHash(b2.Hash(), false)
		c.checkStateProof(b2, false)
		c.chain.Prune(1000, true)
		c.checkHeaderByHash(a2.Hash(), false)
		c.checkStateProof(a2, false)
		if reload {
			c.reloadChain()
		}
		c.checkRange(false, types.Header{}, types.Header{}, false, types.Header{}, types.Header{})
	}
}

type lightChainTest struct {
	t           *testing.T
	db          *memorydb.Database
	proofFormat merkle.CompactProofFormat
	chain       *LightChain
	headers     []types.Header // not added to the chain yet
	stateProofs []testProof    // not added to the chain yet
	emptyRatio  int            // percent of empty slots between section head and tail
}

type testProof struct {
	header types.Header
	proof  merkle.MultiProof
}

func newLightChainTest(t *testing.T) *lightChainTest {
	c := &lightChainTest{
		t:           t,
		db:          memorydb.New(),
		proofFormat: merkle.EncodeCompactProofFormat(merkle.NewIndexMapFormat().AddLeaf(42, nil).AddLeaf(67, nil)),
		emptyRatio:  20,
	}
	c.chain = NewLightChain(c.db)
	return c
}

func (c *lightChainTest) checkRange(chainInit bool, chainTail, chainHead types.Header, stateInit bool, stateTail, stateHead types.Header) {
	ch, ct, ci := c.chain.HeaderRange()
	if ci != chainInit || (ci && (ct != chainTail || ch != chainHead)) {
		c.t.Errorf("Incorrect header chain range (expected: %v %d %d, got: %v %d %d)", chainInit, chainTail.Slot, chainHead.Slot, ci, ct.Slot, ch.Slot)
	}
	if chainInit {
		c.checkCanonical(chainTail, true)
		c.checkCanonical(chainHead, true)
	}
	sh, st, si := c.chain.StateProofRange()
	if si != stateInit || (si && (st != stateTail || sh != stateHead)) {
		c.t.Errorf("Incorrect state proof range (expected: %v %d %d, got: %v %d %d)", stateInit, stateTail.Slot, stateHead.Slot, si, st.Slot, sh.Slot)
	}
	if stateInit {
		c.checkCanonical(stateTail, true)
		c.checkCanonical(stateHead, true)
	}
}

func (c *lightChainTest) checkCanonical(header types.Header, expected bool) {
	if canonical := c.chain.IsCanonical(header); canonical != expected {
		c.t.Errorf("Canonical status of header at slot %d is incorrect (expected: %v, got: %v)", header.Slot, expected, canonical)
	}
}

func (c *lightChainTest) checkTail(header, expTail types.Header) {
	for {
		if parent, err := c.chain.GetParent(header); err == nil {
			header = parent
		} else {
			break
		}
	}
	if header != expTail {
		c.t.Errorf("Incorrect chain tail found by repeated GetParent (expected slot: %d, got: %d)", expTail.Slot, header.Slot)
	}
}

func (c *lightChainTest) reloadChain() {
	c.chain = NewLightChain(c.db)
}

func (c *lightChainTest) makeChain(from types.Header, targetHeadSlot uint64, addHeaders, addStateProofs bool) (tail, head types.Header) {
	head = from
	valueCount := c.proofFormat.ValueCount()
	for head.Slot < targetHeadSlot {
		var (
			slot       uint64
			parentRoot common.Hash
		)
		if head != (types.Header{}) {
			slot = head.Slot + 1
			parentRoot = head.Hash()
		}
		for slot < targetHeadSlot && rand.Intn(100) < c.emptyRatio {
			slot++
		}
		stateProof := merkle.MultiProof{
			Format: c.proofFormat,
			Values: make(merkle.Values, valueCount),
		}
		for i, _ := range stateProof.Values {
			stateProof.Values[i] = merkle.Value(randomHash())
		}
		head = types.Header{
			Slot:          slot,
			ProposerIndex: uint64(rand.Intn(10000)),
			BodyRoot:      randomHash(),
			StateRoot:     stateProof.RootHash(),
			ParentRoot:    parentRoot,
		}
		if tail == (types.Header{}) {
			tail = head
		}
		if addHeaders {
			c.chain.AddHeader(head)
		} else {
			c.headers = append(c.headers, head)
		}
		if addStateProofs {
			if err := c.chain.AddStateProof(head, stateProof); err != nil {
				c.t.Fatalf("AddStateProof failed (error: %v)", err)
			}
		} else {
			c.stateProofs = append(c.stateProofs, testProof{head, stateProof})
		}
	}
	return
}

func (c *lightChainTest) checkHeaderByHash(blockRoot common.Hash, expFound bool) {
	if found := c.chain.HasHeader(blockRoot); found != expFound {
		c.t.Errorf("Incorrect result from HasHeader (expected %v, got %v)", expFound, found)
	}
	if header, err := c.chain.GetHeaderByHash(blockRoot); err == nil {
		if !expFound {
			c.t.Errorf("Unexpected header found by GetHeaderByHash")
		}
		if header.Hash() != blockRoot {
			c.t.Errorf("Header with incorrect hash returned by GetHeaderByHash")
		}
	} else {
		if err != ErrNotFound {
			c.t.Errorf("Unexpected error from GetHeaderByHash: %v", err)
		}
		if expFound {
			c.t.Errorf("Expected header not found by GetHeaderByHash")
		}
	}
}

func (c *lightChainTest) checkHeaderBySlot(slot uint64, expErr error) {
	header, err := c.chain.GetHeaderBySlot(slot)
	if err != expErr {
		c.t.Errorf("Incorrect error output from GetHeaderBySlot (expected %v, got %v)", expErr, err)
	}
	if err == nil {
		if header.Slot != slot {
			c.t.Errorf("Header with incorrect slot returned by GetHeaderBySlot")
		}
		if !c.chain.IsCanonical(header) {
			c.t.Errorf("Non-canonical header returned by GetHeaderBySlot")
		}
	}
}

func (c *lightChainTest) checkStateProof(header types.Header, expFound bool) {
	if found := c.chain.HasStateProof(header); found != expFound {
		c.t.Errorf("Incorrect result from HasStateProof (expected %v, got %v)", expFound, found)
	}
	if proof, err := c.chain.GetStateProof(header.Slot, header.StateRoot); err == nil {
		if !expFound {
			c.t.Errorf("Unexpected state proof found by GetStateProof")
		}
		if proof.RootHash() != header.StateRoot {
			c.t.Errorf("State proof with incorrect root hash returned by GetStateProof")
		}
	} else {
		if err != ErrNotFound {
			c.t.Errorf("Unexpected error from GetStateProof: %v", err)
		}
		if expFound {
			c.t.Errorf("Expected state proof not found by GetStateProof")
		}
	}
}

func randomHash() (hash common.Hash) {
	rand.Read(hash[:])
	return
}
