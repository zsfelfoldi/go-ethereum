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

package lotterypmt

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/lespay/payment"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// Identity is the unique string identity of lottery payment.
const Identity = "Lottery"

// RevealPeriod is the full life cycle length of lottery. The
// number is quite arbitrary here, it's around 6.4 hours. We
// can set a more reasonable number later.
const RevealPeriod = 5760

var errInvalidOpt = errors.New("invalid operation")

// Role is the role of user in payment route.
type Role int

const (
	Sender Role = iota
	Receiver
)

// Config defines all user-selectable options for both
// sender and receiver.
type Config struct {
	// Role is the role of the user in the payment channel, either the
	// payer or the payee.
	Role Role

	// todo(rjl493456442) extend config for higher flexibility
}

// DefaultSenderConfig is the default manager config for sender.
var DefaultSenderConfig = &Config{
	Role: Sender,
}

// DefaultReceiverConfig is the default manager config for receiver.
var DefaultReceiverConfig = &Config{
	Role: Receiver,
}

// Manager is the enter point of the lottery payment no matter for sender
// or receiver. It defines the function wrapper of the underlying payment
// methods and offers the payment scheme codec.
type Manager struct {
	config      *Config
	chainReader payment.ChainReader
	contract    common.Address
	local       common.Address
	db          ethdb.Database

	txSigner     *bind.TransactOpts                // Signer used to sign transaction
	chequeSigner func(data []byte) ([]byte, error) // Signer used to sign cheque

	sender   *lotterybook.ChequeDrawer // Nil if manager is opened by receiver
	receiver *lotterybook.ChequeDrawee // Nil if manager is opened by sender

	// Backends used to interact with the underlying contract
	cBackend bind.ContractBackend
	dBackend bind.DeployBackend
}

// NewManager returns the manager instance for lottery payment.
func NewManager(config *Config, chainReader payment.ChainReader, txSigner *bind.TransactOpts, chequeSigner func(digestHash []byte) ([]byte, error), local, contract common.Address, cBackend bind.ContractBackend, dBackend bind.DeployBackend, db ethdb.Database) (*Manager, error) {
	m := &Manager{
		config:       config,
		chainReader:  chainReader,
		contract:     contract,
		local:        local,
		db:           db,
		txSigner:     txSigner,
		chequeSigner: chequeSigner,
		cBackend:     cBackend,
		dBackend:     dBackend,
	}
	if m.config.Role == Sender {
		sender, err := lotterybook.NewChequeDrawer(m.local, contract, txSigner, chequeSigner, chainReader, cBackend, dBackend, db)
		if err != nil {
			return nil, err
		}
		m.sender = sender
	} else {
		receiver, err := lotterybook.NewChequeDrawee(m.txSigner, m.local, contract, m.chainReader, m.cBackend, m.dBackend, m.db)
		if err != nil {
			return nil, err
		}
		m.receiver = receiver
	}
	return m, nil
}

func (m *Manager) deposit(receivers []common.Address, amounts []uint64, revealPeriod uint64) (common.Hash, error) {
	if m.config.Role != Sender {
		return common.Hash{}, errInvalidOpt
	}
	ctx, cancelFn := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancelFn()

	current := m.chainReader.CurrentHeader().Number.Uint64()
	id, err := m.sender.Deposit(ctx, receivers, amounts, current+revealPeriod)
	return id, err
}

// Deposit creates deposit for the given batch of receivers and corresponding
// deposit amount. If wait is true then a channel is returned, the channel will
// be closed only until the deposit is available for payment and emit a signal
// for it.
func (m *Manager) Deposit(receivers []common.Address, amounts []uint64, revealPeriod uint64, wait bool) (chan bool, error) {
	if revealPeriod == 0 {
		revealPeriod = RevealPeriod
	}
	id, err := m.deposit(receivers, amounts, revealPeriod)
	if err != nil {
		return nil, err
	}
	if !wait {
		return nil, nil
	}
	done := make(chan bool, 1)
	go func() {
		sink := make(chan []lotterybook.LotteryEvent, 64)
		sub := m.sender.SubscribeLotteryEvent(sink)
		defer sub.Unsubscribe()

		for {
			select {
			case events := <-sink:
				for _, event := range events {
					if event.Id == id && event.Status == lotterybook.LotteryActive {
						done <- true
						return
					}
				}
			case <-sub.Err():
				done <- false
				return
			}
		}
	}()
	return done, nil
}

// Pay initiates a payment to the designated payee with specified
// payemnt amount.
func (m *Manager) Pay(payee common.Address, amount uint64) ([]byte, error) {
	if m.config.Role != Sender {
		return nil, errInvalidOpt
	}
	cheque, err := m.sender.IssueCheque(payee, amount)
	if err != nil {
		return nil, err
	}
	proofOfPayment, err := rlp.EncodeToBytes(cheque)
	if err != nil {
		return nil, err
	}
	log.Debug("Generated payment", "amount", amount, "payee", payee)
	return proofOfPayment, nil
}

// Receive receives a payment from the payer and returns any error
// for payment processing and proving.
func (m *Manager) Receive(payer common.Address, proofOfPayment []byte) (uint64, error) {
	if m.config.Role != Receiver {
		return 0, errInvalidOpt
	}
	var cheque lotterybook.Cheque
	if err := rlp.DecodeBytes(proofOfPayment, &cheque); err != nil {
		return 0, err
	}
	amount, err := m.receiver.AddCheque(payer, &cheque)
	if err != nil {
		return 0, err
	}
	log.Debug("Resolved payment", "amount", amount, "payer", payer)
	return amount, nil
}

// Destory exits the payment and withdraws all expired lotteries
func (m *Manager) Destory() error {
	if m.config.Role != Sender {
		return errInvalidOpt
	}
	ctx, cancelFn := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancelFn()
	return m.sender.Destroy(ctx)
}

// LotteryPaymentSchema defines the schema of payment.
type LotteryPaymentSchema struct {
	Sender   common.Address
	Receiver common.Address
	Contract common.Address
}

// Identity implements payment.Schema, returns the identity of payment.
func (schema *LotteryPaymentSchema) Identity() string {
	return Identity
}

// Load implements payment.Schema, returns the specified field with given
// entry key.
func (schema *LotteryPaymentSchema) Load(key string) (interface{}, error) {
	typ := reflect.TypeOf(schema).Elem()
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Name == key {
			val := reflect.ValueOf(schema).Elem()
			return val.Field(i).Interface(), nil
		}
	}
	return nil, errors.New("not found")
}

// LocalSchema returns the payment schema of lottery payment.
func (m *Manager) LocalSchema() (payment.SchemaRLP, error) {
	var schema *LotteryPaymentSchema
	if m.config.Role == Sender {
		schema = &LotteryPaymentSchema{
			Sender:   m.local,
			Contract: m.contract,
		}
	} else {
		schema = &LotteryPaymentSchema{
			Receiver: m.local,
			Contract: m.contract,
		}
	}
	encoded, err := rlp.EncodeToBytes(schema)
	if err != nil {
		return payment.SchemaRLP{}, err
	}
	return payment.SchemaRLP{
		Key:   schema.Identity(),
		Value: encoded,
	}, nil
}

// ResolveSchema resolves the remote schema of lottery payment,
// ensure the schema is compatible with us.
func (m *Manager) ResolveSchema(blob []byte) (payment.Schema, error) {
	var schema LotteryPaymentSchema
	if err := rlp.DecodeBytes(blob, &schema); err != nil {
		return nil, err
	}
	if m.config.Role == Sender {
		if schema.Receiver == (common.Address{}) {
			return nil, errors.New("empty receiver address")
		}
		if schema.Contract != m.contract {
			return nil, errors.New("imcompatible contract")
		}
		return &schema, nil
	} else {
		if schema.Sender == (common.Address{}) {
			return nil, errors.New("empty sender address")
		}
		if schema.Contract != m.contract {
			return nil, errors.New("imcompatible contract")
		}
		return &schema, nil
	}
}
