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

package payment

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rlp"
)

// PaymentRoute is the way the light client pays to the les server.
// PaymentRoute can be implemented in many different ways, such as
// off-chain payment, on-chain payment. All available payments must
// implement the following functions.
//
// Note paymentRoute is only a one-way payment channel.
type PaymentRoute interface {
	// Pay initiates a payment to the designated payee with specified
	// payemnt amount.
	Pay(amount uint64) error

	// Receive receives a payment from the payer and returns any error
	// for payment processing and proving.
	Receive(proofOfPayment []byte) error

	// Close exits the payment and opens the reqeust to withdraw all funds.
	Close() error

	// Info returns the information union of route(sender, receiver, contract)
	Info() (common.Address, common.Address, common.Address)
}

// PaymentSender represents the sender in the opened payment route.
type PaymentSender interface {
	// SendPayment sends the given cheque to the peer via network.
	SendPayment([]byte, string) error
}

// PaymentReceiver represents the receiver in the opened payment route.
type PaymentReceiver interface {
	// ReceivePayment notifies upper-level system we have received
	// the payment from the peer with specified amount.
	ReceivePayment(uint64) error
}

// CurrentHeader retrieves the current header from the local chain.
type ChainReader interface {
	// CurrentHeader retrieves the current header from the local chain.
	CurrentHeader() *types.Header

	// GetHeaderByNumber retrieves a block header from the database by number.
	GetHeaderByNumber(number uint64) *types.Header

	// SubscribeChainHeadEvent registers a subscription of ChainHeadEvent.
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
}

// PaymentPacket represents a network packet for payment.
type PaymentPacket struct {
	Identity       string
	ProofOfPayment rlp.RawValue
}
