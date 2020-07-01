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

package les

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/lotterybook"
	"github.com/ethereum/go-ethereum/les/lespay/payment/lotterypmt"
	"github.com/ethereum/go-ethereum/log"
)

// PaymentRobot is the testing tool for automatic payment cycle.
type PaymentRobot struct {
	sender   *lotterypmt.PaymentSender
	receiver common.Address
	close    chan struct{}
}

func NewPaymentRobot(manager *lotterypmt.PaymentSender, receiver common.Address, close chan struct{}) *PaymentRobot {
	return &PaymentRobot{
		sender:   manager,
		receiver: receiver,
		close:    close,
	}
}

func (robot *PaymentRobot) Run(sendFn func(proofOfPayment []byte, identity string) error) {
	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	var (
		err     error
		deposit chan bool
	)
	depositFn := func() {
		if deposit != nil {
			log.Error("Depositing, skip new operation")
			return
		}
		deposit, err = robot.sender.Deposit([]common.Address{robot.receiver}, []uint64{100}, 60, true)
		if err != nil {
			log.Error("Failed to deposit", "err", err)
		}
	}
	depositFn()

	for {
		select {
		case <-ticker.C:
			if deposit != nil {
				continue
			}
			proofOfPayments, err := robot.sender.Pay(robot.receiver, 3)
			if err != nil {
				if err == lotterybook.ErrNotEnoughDeposit {
					depositFn()
					continue
				}
				log.Error("Failed to pay", "error", err)
				continue
			}
			for _, proofOfPayment := range proofOfPayments {
				sendFn(proofOfPayment, lotterypmt.Identity)
			}

		case <-deposit:
			deposit = nil
			log.Info("Deposit finished")

		case <-robot.close:
			return
		}
	}
}
