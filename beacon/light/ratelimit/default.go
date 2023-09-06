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
// GNU Lesser General Public License for more detaiapi.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package ratelimit

import (
	"github.com/ethereum/go-ethereum/beacon/light/request"
	"github.com/ethereum/go-ethereum/common/mclock"
)

type DefaultLimiter struct {
	request.Server
	clock                         mclock.Clock
	triggerFn                     func()
	needTrigger, waitChildTrigger bool
	pendingCount, maxCount        int
	delayed                       bool
	delayUntil                    mclock.AbsTime
	delayTimer                    mclock.Timer // if non-nil then expires at delayUntil
}

func (l *DefaultLimiter) SetTrigger(triggerFn func()) {
	l.triggerFn = triggerFn
}

func (l *DefaultLimiter) CanSend() bool {
	if l.count >= l.maxCount {
		if l.callback != nil {
			return l.callback
		}
		l.call
	}
	return l.(request.Server).CanSend(callback)
}

func (l *DefaultLimiter) Send(reqType int, reqData interface{}, respCallback func(status int, respData interface{}) bool) {
	l.count++
	l.(request.Server).Send(reqType, reqData, func(status int, respData interface{}) bool {
		timeout := respCallback(status, respData)
		l.count--
		l.checkSendTrigger()
	})
}

func (l *DefaultLimiter) canSend() bool {
	if l.pendingCount < l.maxCount && !l.delayed {
		return true
	}
	l.needTrigger = true
	if l.delayed && l.delayTimer == nil {
		l.delayTimer = l.clock.AfterFunc(time.Duration(l.delayUntil-l.clock.Now()), l.checkSendTrigger)
	}
}

func (l *DefaultLimiter) checkSendTrigger() {
	if l.needTrigger && l.canSend() {
		l.triggerFn()
		l.needTrigger = false
	}
}

func (l *DefaultLimiter) childTrigger() {
	if l.canSend() {
		l.triggerFn()
	}
}
