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
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/les/payment"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// Identity is the unique string identity of lottery payment.
const Identity = "Lottery"

var errInvalidOpt = errors.New("invalid operation")

// Role is the role of user in payment route.
type Role int

const (
	Sender Role = iota
	Receiver
)

type Route struct {
	role        Role
	chainReader payment.ChainReader
	local       common.Address
	contract    common.Address
	remote      common.Address
	sender      payment.PaymentSender     // Nil if route is opened by receiver
	receiver    payment.PaymentReceiver   // Nil if route is opened by sender
	drawer      *lotterybook.ChequeDrawer // Nil if route is opened by receiver
	drawee      *lotterybook.ChequeDrawee // Nil if route is opened by sender
}

func newRoute(role Role, chainReader payment.ChainReader, local, remote, contract common.Address, drawer *lotterybook.ChequeDrawer, drawee *lotterybook.ChequeDrawee, sender payment.PaymentSender, receiver payment.PaymentReceiver) *Route {
	route := &Route{
		role:        role,
		chainReader: chainReader,
		local:       local,
		remote:      remote,
		contract:    contract,
		sender:      sender,
		receiver:    receiver,
		drawer:      drawer,
		drawee:      drawee,
	}
	return route
}

// Pay initiates a payment to the designated payee with specified
// payemnt amount.
func (r *Route) Pay(amount uint64) error {
	if r.role != Sender {
		return errInvalidOpt
	}
	cheque, err := r.drawer.IssueCheque(r.remote, amount)
	if err != nil {
		return err
	}
	proofOfPayment, err := rlp.EncodeToBytes(cheque)
	if err != nil {
		return err
	}
	log.Debug("Sent payment", "amount", amount, "route", r.contract)
	return r.sender.SendPayment(proofOfPayment, Identity)
}

// Receive receives a payment from the payer and returns any error
// for payment processing and proving.
func (r *Route) Receive(proofOfPayment []byte) error {
	if r.role != Receiver {
		return errInvalidOpt
	}
	var cheque lotterybook.Cheque
	if err := rlp.DecodeBytes(proofOfPayment, &cheque); err != nil {
		return err
	}
	amount, err := r.drawee.AddCheque(r.remote, &cheque)
	if err != nil {
		return err
	}
	log.Debug("Received payment", "amount", amount, "route", r.contract)
	return r.receiver.ReceivePayment(amount)
}

// Close exits the payment and withdraw all expired lotteries
func (r *Route) Close() error {
	if r.role != Sender {
		return nil
	}
	ctx, cancelFn := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancelFn()
	return r.drawer.Destroy(ctx)
}

// Info returns the infomation union of payment route.
func (r *Route) Info() (common.Address, common.Address, common.Address) {
	if r.role == Sender {
		return r.local, r.remote, r.contract
	} else {
		return r.remote, r.local, r.contract
	}
}

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

// Manager is responsible for payment routes management.
type Manager struct {
	config      *Config
	chainReader payment.ChainReader
	contract    common.Address
	local       common.Address
	db          ethdb.Database

	txSigner     *bind.TransactOpts                // Signer used to sign transaction
	chequeSigner func(data []byte) ([]byte, error) // Signer used to sign cheque

	// routes are all established channels. For payment receiver,
	// the key of routes map is sender's address; otherwise the
	// key refers to receiver's address.
	routes   map[common.Address]*Route
	lock     sync.RWMutex              // The lock used to protect routes
	sender   *lotterybook.ChequeDrawer // Nil if manager is opened by receiver
	receiver *lotterybook.ChequeDrawee // Nil if manager is opened by sender

	// Backends used to interact with the underlying contract
	cBackend bind.ContractBackend
	dBackend bind.DeployBackend
}

// newRoute initializes a one-to-one payment channel for both sender and receiver.
func NewManager(config *Config, chainReader payment.ChainReader, txSigner *bind.TransactOpts, chequeSigner func(digestHash []byte) ([]byte, error), local, contract common.Address, cBackend bind.ContractBackend, dBackend bind.DeployBackend, db ethdb.Database) (*Manager, error) {
	m := &Manager{
		config:       config,
		chainReader:  chainReader,
		contract:     contract,
		local:        local,
		txSigner:     txSigner,
		chequeSigner: chequeSigner,
		db:           db,
		cBackend:     cBackend,
		dBackend:     dBackend,
		routes:       make(map[common.Address]*Route),
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

// OpenRoute establishes a new payment route for new customer or new service
// provider. If we are payment receiver, the addr refers to the lottery contract
// addr, otherwise the addr refers to receiver's address.
func (m *Manager) OpenRoute(schema payment.Schema, sender payment.PaymentSender, receiver payment.PaymentReceiver) (payment.PaymentRoute, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if m.config.Role == Receiver {
		elem, err := schema.Load("Sender")
		if err != nil {
			return nil, err
		}
		remote := elem.(common.Address)
		if _, exist := m.routes[remote]; exist {
			return nil, errors.New("duplicated payment route")
		}
		m.routes[remote] = newRoute(Receiver, m.chainReader, m.local, remote, m.contract, nil, m.receiver, nil, receiver)
		log.Debug("Opened route", "local", m.local, "remote", remote)
		return m.routes[remote], nil
	} else {
		elem, err := schema.Load("Receiver")
		if err != nil {
			return nil, err
		}
		remote := elem.(common.Address)
		if _, exist := m.routes[remote]; exist {
			return nil, errors.New("duplicated payment route")
		}
		// We are payment sender, establish a outgoing route with
		// specified counterparty address and peer.
		m.routes[remote] = newRoute(Sender, m.chainReader, m.local, remote, m.contract, m.sender, nil, sender, nil)
		log.Debug("Opened route", "local", m.local, "remote", remote)
		return m.routes[remote], nil
	}
}

// CloseRoute closes a route with given address. If we are payment receiver,
// the addr refers to the sender's address, otherwise, the address refers to
// receiver's address.
func (m *Manager) CloseRoute(addr common.Address) error {
	m.lock.Lock()
	defer m.lock.Unlock()
	delete(m.routes, addr)
	log.Debug("Closed route", "address", addr)
	return nil
}

func (m *Manager) deposit(receivers []common.Address, amounts []uint64) (common.Hash, error) {
	if m.config.Role != Sender {
		return common.Hash{}, errInvalidOpt
	}
	ctx, cancelFn := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancelFn()

	// TODO(rjl493456442) implement reveal range oracle for better number recommendation.
	current := m.chainReader.CurrentHeader().Number.Uint64()
	id, err := m.sender.Deposit(ctx, receivers, amounts, current+5760)
	return id, err
}

// Deposit creates deposit for the given batch of receivers and corresponding
// deposit amount.
func (m *Manager) Deposit(receivers []common.Address, amounts []uint64) error {
	_, err := m.deposit(receivers, amounts)
	return err
}

// DepositAndWait creates deposit for the given batch of receivers and corresponding
// deposit amount. Wait until the deposit is available for payment and emit a signal
// for it.
func (m *Manager) DepositAndWait(receivers []common.Address, amounts []uint64) (chan bool, error) {
	id, err := m.deposit(receivers, amounts)
	if err != nil {
		return nil, err
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

// Remotes returns the address of counterparty peer for all
// established payment routes.
func (m *Manager) Remotes() []common.Address {
	m.lock.RLock()
	defer m.lock.RUnlock()
	var addresses []common.Address
	for addr := range m.routes {
		addresses = append(addresses, addr)
	}
	return addresses
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
	if m.config.Role == Sender {
		if err := rlp.DecodeBytes(blob, &schema); err != nil {
			return nil, err
		}
		if schema.Receiver == (common.Address{}) {
			return nil, errors.New("invald schema")
		}
		if schema.Contract != m.contract {
			return nil, errors.New("invald schema")
		}
		return &schema, nil
	} else {
		if err := rlp.DecodeBytes(blob, &schema); err != nil {
			return nil, err
		}
		if schema.Sender == (common.Address{}) {
			return nil, errors.New("invald schema")
		}
		if schema.Contract != m.contract {
			return nil, errors.New("invald schema")
		}
		return &schema, nil
	}
}
