// Copyright 2016 The go-ethereum Authors
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

// Package les implements the Light Ethereum Subprotocol.
package les

import (
	"crypto/rand"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/light"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"golang.org/x/net/context"
)

func (pm *ProtocolManager) sendTestRequests(cnt int) {
	start := mclock.Now()
	header := pm.blockchain.GetHeaderByNumber(pm.blockchain.CurrentHeader().Number.Uint64() - 1)
	if header == nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(cnt)
	for i := 0; i < cnt; i++ {
		go func() {
			state := light.NewLightState(light.StateTrieID(header), pm.odr)
			var addr common.Address
			rand.Read(addr[:])
			state.GetBalance(context.Background(), addr)
			wg.Done()
		}()
	}
	wg.Wait()
	glog.V(logger.Info).Infof("executed %v requests in %v", cnt, time.Duration(mclock.Now()-start))
}

func (pm *ProtocolManager) serverStats() {
	statsTicker := time.NewTicker(time.Minute)
	for {
		select {
		case <-pm.quitSync:
			return
		case <-statsTicker.C:
			pm.sendTestRequests(1000)
			pm.serverPool.lock.Lock()
			cnt, newCnt := 0, 0
			for id, entry := range pm.serverPool.entries {
				if entry.known {
					cnt++
					fn := fmt.Sprintf("stats_%016x.txt", id[0:8])
					f, err := os.OpenFile(fn, os.O_APPEND|os.O_WRONLY, 0666)
					if err != nil {
						f, _ = os.OpenFile(fn, os.O_CREATE|os.O_WRONLY, 0666)
						newCnt++
					}
					disc := (mclock.Now() - entry.lastDiscovered) / 1000000000
					if disc > 1000 {
						disc = 1000
					}
					conn := 0
					if entry.state == psRegistered {
						conn = 1
					}
					fails := uint(0)
					if entry.lastConnected != nil {
						fails = entry.lastConnected.fails
					}

					text := fmt.Sprintf("%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\n", time.Now(), time.Now().UnixNano()/1000000000, disc, conn, fails, entry.connectStats.recentAvg(), entry.connectStats.weight, entry.delayStats.recentAvg()/1000000000, entry.delayStats.weight, entry.responseStats.recentAvg()/1000000, entry.responseStats.weight, entry.timeoutStats.recentAvg(), entry.timeoutStats.weight)
					f.WriteString(text)
					f.Close()
				}
			}
			glog.V(logger.Info).Infof("wrote stats for %v servers (%v new)", cnt, newCnt)
			pm.serverPool.lock.Unlock()
		}
	}
}
