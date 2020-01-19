// Copyright 2020 The go-ethereum Authors
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

package lotterybook

import (
	"context"
	"encoding/binary"
	"errors"
	"math/big"
	"math/rand"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook/merkletree"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
)

// ChequeDrawer represents the payment drawer in a off-chain payment channel.
// Usually in LES protocol the drawer refers to a light client.
//
// ChequeDrawer is self-contained and stateful, it will only offer the most
// basic function: Issue, Deposit and some relevant query APIs.
//
// Internally it relies on lottery manager for lottery life cycle management.
type ChequeDrawer struct {
	address  common.Address
	cdb      *chequeDB
	book     *LotteryBook
	chain    Blockchain
	lmgr     *lotteryManager
	cBackend bind.ContractBackend
	dBackend bind.DeployBackend
	rand     *rand.Rand

	txSigner     *bind.TransactOpts                // Used for production environment, transaction signer
	keySigner    func(data []byte) ([]byte, error) // Used for testing, cheque signer
	chequeSigner func(data []byte) ([]byte, error) // Used for production environment, cheque signer
}

// NewChequeDrawer creates a payment drawer and deploys the contract if necessary.
func NewChequeDrawer(address, contractAddr common.Address, txSigner *bind.TransactOpts, chequeSigner func(data []byte) ([]byte, error), chain Blockchain, cBackend bind.ContractBackend, dBackend bind.DeployBackend, db ethdb.Database) (*ChequeDrawer, error) {
	if contractAddr == (common.Address{}) {
		return nil, errors.New("empty contract address")
	}
	book, err := newLotteryBook(contractAddr, cBackend)
	if err != nil {
		return nil, err
	}
	cdb := newChequeDB(db)
	drawer := &ChequeDrawer{
		address:      address,
		cdb:          cdb,
		book:         book,
		txSigner:     txSigner,
		chequeSigner: chequeSigner,
		chain:        chain,
		lmgr:         newLotteryManager(address, chain, book.contract, cdb),
		cBackend:     cBackend,
		dBackend:     dBackend,
		rand:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	return drawer, nil
}

// ContractAddr returns the address of deployed accountbook contract.
func (drawer *ChequeDrawer) ContractAddr() common.Address {
	return drawer.book.address
}

// Close exits all background threads and closes all event subscribers.
func (drawer *ChequeDrawer) Close() {
	drawer.lmgr.close()
}

// newProbabilityTree constructs a probability tree(merkle tree) based on the
// given list of receivers and corresponding amounts. The payment amount can
// used as the initial weight for each payer. Since the underlying merkle tree
// is binary tree, so finally all weights will be adjusted to 1/2^N form.
func (drawer *ChequeDrawer) newProbabilityTree(payers []common.Address, amounts []uint64) (*merkletree.MerkleTree, []*merkletree.Entry, uint64, error) {
	if len(payers) != len(amounts) {
		return nil, nil, 0, errors.New("inconsistent payment receivers and amounts")
	}
	var totalAmount uint64
	for _, amount := range amounts {
		if amount == 0 {
			return nil, nil, 0, errors.New("invalid payment amount")
		}
		totalAmount += amount
	}
	entries := make([]*merkletree.Entry, len(payers))
	for index, amount := range amounts {
		entries[index] = &merkletree.Entry{
			Value:  payers[index].Bytes(),
			Weight: amount,
		}
	}
	tree, err := merkletree.NewMerkleTree(entries)
	if err != nil {
		return nil, nil, 0, err
	}
	return tree, entries, totalAmount, nil
}

// submitLottery creates the lottery based on the specified batch of payers and
// corresponding payment amount. Return the newly created cheque list.
func (drawer *ChequeDrawer) submitLottery(context context.Context, payers []common.Address, amounts []uint64, revealNumber uint64, onchainFn func(amount uint64, id [32]byte, blockNumber uint64, salt uint64) (*types.Transaction, error)) (*Lottery, error) {
	// Construct merkle probability tree with given payer list and corresponding
	// payer weight. Return error if the given weight is invalid.
	tree, entries, amount, err := drawer.newProbabilityTree(payers, amounts)
	if err != nil {
		return nil, err
	}
	// New random lottery salt to ensure the id is unique.
	salt := drawer.rand.Uint64()
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, salt)

	// Generate and store the temporary lottery record before the transaction
	// submission.
	//
	// If any crash happen then we can still claim back the deposit inside.
	// If we record the lottery but we fail to send out the creation transaction,
	// we can wipe the record during recovery.
	lotteryId := crypto.Keccak256Hash(append(tree.Hash().Bytes(), buf...))
	lottery := &Lottery{
		Id:           lotteryId,
		RevealNumber: revealNumber,
		Amount:       amount,
		Receivers:    payers,
	}
	drawer.cdb.writeLottery(drawer.address, lotteryId, true, lottery) // tmp = true

	// Submit the new created lottery to contract by specified on-chain function.
	start := time.Now()
	tx, err := onchainFn(amount, lotteryId, revealNumber, salt)
	if err != nil {
		return nil, err
	}
	receipt, err := bind.WaitMined(context, drawer.dBackend, tx)
	if err != nil {
		return nil, err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return nil, ErrTransactionFailed
	}
	depositDurationTimer.UpdateSince(start)

	// Generate empty unused cheques based on the newly created lottery.
	for _, entry := range entries {
		witness, err := tree.Prove(entry)
		if err != nil {
			return nil, err
		}
		cheque, err := newCheque(witness, drawer.ContractAddr(), salt)
		if err != nil {
			return nil, err
		}
		if drawer.keySigner != nil {
			// If it's testing, use provided key signer.
			if err := cheque.signWithKey(drawer.keySigner); err != nil {
				return nil, err
			}
		} else {
			// Otherwise, use provided clef as the production-environment signer.
			if err := cheque.sign(drawer.chequeSigner); err != nil {
				return nil, err
			}
		}
		drawer.cdb.writeCheque(common.BytesToAddress(entry.Value), drawer.address, cheque, true)
	}
	drawer.cdb.writeLottery(drawer.address, lotteryId, false, lottery) // Store the lottery after cheques
	drawer.cdb.deleteLottery(drawer.address, lotteryId, true)          // Now we can sunset the tmp record
	return lottery, nil
}

// createLottery creates the lottery based on the specified batch of payers and
// corresponding payment amount, returns the id of craeted lottery.
func (drawer *ChequeDrawer) createLottery(context context.Context, payers []common.Address, amounts []uint64, revealNumber uint64) (common.Hash, error) {
	onchainFn := func(amount uint64, id [32]byte, blockNumber uint64, salt uint64) (*types.Transaction, error) {
		// Create an independent auth opt to submit the lottery
		opt := &bind.TransactOpts{
			From:   drawer.address,
			Signer: drawer.txSigner.Signer,
			Value:  big.NewInt(int64(amount)),
		}
		return drawer.book.contract.NewLottery(opt, id, blockNumber, salt)
	}
	lottery, err := drawer.submitLottery(context, payers, amounts, revealNumber, onchainFn)
	if err != nil {
		return common.Hash{}, err
	}
	if err := drawer.lmgr.trackLottery(lottery); err != nil {
		return common.Hash{}, err
	}
	createLotteryGauge.Inc(1)
	return lottery.Id, err
}

// resetLottery resets a existed stale lottery with new batch of payment receivers
// and corresponding amount. Add more funds into lottery ff the deposit of stale
// lottery is not enough to cover the new amount. Otherwise the deposit given by
// lottery for each receiver may be higher than the specified value.
func (drawer *ChequeDrawer) resetLottery(context context.Context, id common.Hash, payers []common.Address, amounts []uint64, revealNumber uint64) (common.Hash, error) {
	// Short circuit if the specified stale lottery doesn't exist.
	lottery := drawer.cdb.readLottery(drawer.address, id)
	if lottery == nil {
		return common.Hash{}, errors.New("the lottery specified is not-existent")
	}
	onchainFn := func(amount uint64, newid [32]byte, blockNumber uint64, salt uint64) (*types.Transaction, error) {
		// Create an independent auth opt to submit the lottery
		var netAmount uint64
		if lottery.Amount < amount {
			netAmount = amount - lottery.Amount
		}
		opt := &bind.TransactOpts{
			From:   drawer.address,
			Signer: drawer.txSigner.Signer,
			Value:  big.NewInt(int64(netAmount)),
		}
		// The on-chain transaction may fail dues to lots of reasons like
		// the lottery doesn't exist or lottery hasn't been expired yet.
		return drawer.book.contract.ResetLottery(opt, id, newid, blockNumber, salt)
	}
	newLottery, err := drawer.submitLottery(context, payers, amounts, revealNumber, onchainFn)
	if err != nil {
		return common.Hash{}, err
	}
	// Update manager's status
	if err := drawer.lmgr.trackLottery(newLottery); err != nil {
		return common.Hash{}, err
	}
	if err := drawer.lmgr.deleteExpired(id); err != nil {
		return common.Hash{}, err
	}
	// Wipe useless old lottery and associated cheques
	drawer.cdb.deleteLottery(drawer.address, id, false)
	_, addresses := drawer.cdb.listCheques(
		drawer.address,
		func(addr common.Address, lid common.Hash) bool { return lid == id },
	)
	for _, addr := range addresses {
		drawer.cdb.deleteCheque(drawer.address, addr, id, true)
	}
	reownLotteryGauge.Inc(1)
	return newLottery.Id, nil
}

// Deposit is a wrapper function of `createLottery` and `resetLottery`.
// The strategy of deposit here is very simple: if there are any expired lotteries
// can be reowned, use these lottery first; otherwise create new lottery for deposit.
func (drawer *ChequeDrawer) Deposit(context context.Context, payers []common.Address, amounts []uint64, revealNumber uint64) (common.Hash, error) {
	expired, err := drawer.lmgr.expiredLotteris()
	if err != nil {
		return common.Hash{}, err
	}
	for _, l := range expired {
		newId, err := drawer.resetLottery(context, l.Id, payers, amounts, revealNumber)
		if err != nil {
			continue
		}
		return newId, nil
	}
	return drawer.createLottery(context, payers, amounts, revealNumber)
}

// destroyLottery destroys a stale lottery, claims all deposit inside back to
// our own pocket.
func (drawer *ChequeDrawer) destroyLottery(context context.Context, id common.Hash) error {
	// Short circuit if the specified stale lottery doesn't exist.
	lottery := drawer.cdb.readLottery(drawer.address, id)
	if lottery == nil {
		return errors.New("the lottery specified is not-existent")
	}
	// The on-chain transaction may fail dues to lots of reasons like
	// the lottery doesn't exist or lottery hasn't been expired yet.
	start := time.Now()
	tx, err := drawer.book.contract.DestroyLottery(drawer.txSigner, id)
	if err != nil {
		return err
	}
	receipt, err := bind.WaitMined(context, drawer.dBackend, tx)
	if err != nil {
		return err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return ErrTransactionFailed
	}
	destroyDurationTimer.UpdateSince(start)
	// Update manager's status
	if err := drawer.lmgr.deleteExpired(id); err != nil {
		return err
	}
	// Wipe useless lottery and associated cheques
	drawer.cdb.deleteLottery(drawer.address, id, false)
	_, addresses := drawer.cdb.listCheques(
		drawer.address,
		func(addr common.Address, lid common.Hash) bool { return lid == id },
	)
	for _, addr := range addresses {
		drawer.cdb.deleteCheque(drawer.address, addr, id, true)
	}
	return nil
}

// Destroy is a wrapper function of destroyLottery. It destorys all
// expired lotteries and claim back all deposit inside.
func (drawer *ChequeDrawer) Destroy(context context.Context) error {
	expired, err := drawer.lmgr.expiredLotteris()
	if err != nil {
		return err
	}
	reownLotteryGauge.Inc(1)
	for _, l := range expired {
		drawer.destroyLottery(context, l.Id)
	}
	return nil
}

// IssueCheque creates a cheque for issuing specified amount for payee.
//
// Many active lotteris can be used to create cheque, we use the simplest
// strategy here for lottery selection: choose a lottery ticket with the
// most recent expiration date and the remaining amount can cover the amount
// paid this time.
func (drawer *ChequeDrawer) IssueCheque(payer common.Address, amount uint64) (*Cheque, error) {
	lotteris, err := drawer.lmgr.activeLotteris()
	if err != nil {
		return nil, err
	}
	sort.Sort(LotteryByRevealTime(lotteris))
	for _, lottery := range lotteris {
		cheque := drawer.cdb.readCheque(payer, drawer.address, lottery.Id, true)
		if cheque == nil {
			continue
		}
		if lottery.balance(payer, cheque) >= amount {
			return drawer.issueCheque(payer, lottery.Id, amount)
		}
	}
	return nil, ErrNotEnoughDeposit // No suitable lottery found for payment
}

// issueCheque creates a cheque for issuing specified amount for payee.
//
// The drawer must have a corresponding lottery as a deposit if it wants
// to issue cheque. This lottery contains several potential redeemers of
// this lottery. The probability that each redeemer can redeem is different,
// so the expected amount of money received by each redeemer is the redemption
// probability multiplied by the lottery amount.
//
// A lottery ticket can be divided into n cheques for payment. Therefore, the
// cheque is paid in a cumulative amount. There is a probability of redemption
// in every cheque issued. The probability of redemption of a later-issued cheque
// needs to be strictly greater than that of the first-issued cheque.
//
// Because of the possible data loss, we can issue some double-spend cheques,
// they will be rejected by drawee. Finally we will amend local broken db
// by the evidence provided by drawee.
func (drawer *ChequeDrawer) issueCheque(payer common.Address, lotteryId common.Hash, amount uint64) (*Cheque, error) {
	cheque := drawer.cdb.readCheque(payer, drawer.address, lotteryId, true)
	if cheque == nil {
		return nil, errors.New("no cheque found")
	}
	lottery := drawer.cdb.readLottery(drawer.address, cheque.LotteryId)
	if lottery == nil {
		return nil, errors.New("broken db, has cheque but no lottery found")
	}
	// Short circuit if lottery is already expired.
	current := drawer.chain.CurrentHeader().Number.Uint64()
	if lottery.RevealNumber <= current {
		return nil, errors.New("expired lottery")
	}
	// Calculate the total assigned deposit in lottery for the specified payer.
	assigned := lottery.Amount >> (len(cheque.Witness) - 1)

	// Calculate new signed probability range according to new cumulative paid amount
	//
	// Note in the following calculation, it may lose precision.
	// In theory amount/assigned won't be very small. So it's safer to calculate
	// percentage first.
	diff := uint64(float64(amount) / float64(assigned) * float64(cheque.UpperLimit-cheque.LowerLimit+1))
	if diff == 0 {
		return nil, errors.New("invalid payment amount")
	}
	if cheque.SignedRange == 0 {
		cheque.SignedRange = cheque.LowerLimit + diff - 1
	} else {
		cheque.SignedRange = cheque.SignedRange + diff
	}
	// Ensure we still have enough deposit to cover payment.
	if cheque.SignedRange > cheque.UpperLimit {
		return nil, ErrNotEnoughDeposit
	}
	// Make the signature for cheque.
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(cheque.SignedRange))
	cheque.RevealRange = buf
	if drawer.keySigner != nil {
		// If it's testing, use provided key signer.
		if err := cheque.signWithKey(drawer.keySigner); err != nil {
			return nil, err
		}
	} else {
		// Otherwise, use provided clef as the production-environment signer.
		if err := cheque.sign(drawer.chequeSigner); err != nil {
			return nil, err
		}
	}
	drawer.cdb.writeCheque(payer, drawer.address, cheque, true)
	return cheque, nil
}

// Allowance returns the allowance remaining in the specified lottery that can
// be used to make payments to all included receivers.
func (drawer *ChequeDrawer) Allowance(id common.Hash) map[common.Address]uint64 {
	// Filter all cheques associated with given lottery.
	cheques, addresses := drawer.cdb.listCheques(
		drawer.address,
		func(addr common.Address, lid common.Hash) bool { return lid == id },
	)
	// Short circuit if no cheque found.
	if cheques == nil {
		return nil
	}
	// Short circuit of no corresponding lottery found, it should never happen.
	lottery := drawer.cdb.readLottery(drawer.address, id)
	if lottery == nil {
		return nil
	}
	allowance := make(map[common.Address]uint64)
	for index, cheque := range cheques {
		allowance[addresses[index]] = lottery.balance(addresses[index], cheque)
	}
	return allowance
}

// EstimatedExpiry returns the estimated remaining time(block number) while
// cheques can still be written using this deposit.
func (drawer *ChequeDrawer) EstimatedExpiry(lotteryId common.Hash) uint64 {
	lottery := drawer.cdb.readLottery(drawer.address, lotteryId)
	if lottery == nil {
		return 0
	}
	current := drawer.chain.CurrentHeader()
	if current == nil {
		return 0
	}
	height := current.Number.Uint64()
	if lottery.RevealNumber > height {
		return lottery.RevealNumber - height
	}
	return 0 // The lottery is already expired.
}

// SubscribeLotteryEvent registers a subscription of LotteryEvent.
func (drawer *ChequeDrawer) SubscribeLotteryEvent(ch chan<- []LotteryEvent) event.Subscription {
	return drawer.lmgr.subscribeLotteryEvent(ch)
}
