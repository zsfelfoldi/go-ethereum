// Copyright 2015 The go-ethereum Authors
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

package comms

import (
	"fmt"
	"math/rand"
	"net"
	"os"

	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/rpc/codec"
	"github.com/ethereum/go-ethereum/rpc/shared"
)

type Stopper interface {
	Stop()
}

type InitFunc func(conn net.Conn) (Stopper, shared.EthereumApi, error)

type IpcConfig struct {
	Endpoint string
}

type ipcClient struct {
	endpoint string
	c        net.Conn
	codec    codec.Codec
	coder    codec.ApiCoder
}

func (self *ipcClient) Close() {
	self.coder.Close()
}

func (self *ipcClient) Send(msg interface{}) error {
	var err error
	if err = self.coder.WriteResponse(msg); err != nil {
		if err = self.reconnect(); err == nil {
			err = self.coder.WriteResponse(msg)
		}
	}
	return err
}

func (self *ipcClient) Recv() (interface{}, error) {
	response, err := self.coder.ReadResponse()
	if err != nil {
		return nil, err
	}
	return response, nil
}

func (self *ipcClient) SupportedModules() (map[string]string, error) {
	req := map[string]interface{}{
		"id":     1,
		"method": "rpc_modules",
	}

	if err := self.coder.WriteResponse(req); err != nil {
		return nil, err
	}

	res, _ := self.coder.ReadResponse()
	if response, ok := res.(map[string]interface{}); ok {
		if payload, ok := response["result"]; ok {
			mods := make(map[string]string)
			if modules, ok := payload.(map[string]interface{}); ok {
				for m, v := range modules {
					mods[m] = fmt.Sprintf("%s", v)
				}
				return mods, nil
			}
		}
	}

	// old version uses modules instead of rpc_modules, this can be removed after full migration
	req["method"] = "modules"
	if err := self.coder.WriteResponse(req); err != nil {
		return nil, err
	}

	res, _ = self.coder.ReadResponse()
	if response, ok := res.(map[string]interface{}); ok {
		if payload, ok := response["result"]; ok {
			mods := make(map[string]string)
			if modules, ok := payload.(map[string]interface{}); ok {
				for m, v := range modules {
					mods[m] = fmt.Sprintf("%s", v)
				}
				return mods, nil
			}
		}
	}

	return nil, fmt.Errorf("Invalid response")
}

// Create a new IPC client, UNIX domain socket on posix, named pipe on Windows
func NewIpcClient(cfg IpcConfig, codec codec.Codec) (*ipcClient, error) {
	return newIpcClient(cfg, codec)
}

// Start IPC server
func StartIpc(cfg IpcConfig, codec codec.Codec, initializer InitFunc) error {
	l, err := ipcListen(cfg)
	if err != nil {
		return err
	}
	go ipcLoop(cfg, codec, initializer, l)
	return nil
}

// CreateListener creates an listener, on Unix platforms this is a unix socket, on Windows this is a named pipe
func CreateListener(cfg IpcConfig) (net.Listener, error) {
	return ipcListen(cfg)
}

func ipcLoop(cfg IpcConfig, codec codec.Codec, initializer InitFunc, l net.Listener) {
	glog.V(logger.Info).Infof("IPC service started (%s)\n", cfg.Endpoint)
	defer os.Remove(cfg.Endpoint)
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			glog.V(logger.Debug).Infof("accept: %v", err)
			return
		}
		id := newIpcConnId()
		go func() {
			defer conn.Close()
			glog.V(logger.Debug).Infof("new connection with id %06d started", id)
			stopper, api, err := initializer(conn)
			if err != nil {
				glog.V(logger.Error).Infof("Unable to initialize IPC connection: %v", err)
				return
			}
			defer stopper.Stop()
			handle(id, conn, api, codec)
		}()
	}
}

func newIpcConnId() int {
	return rand.Int() % 1000000
}
