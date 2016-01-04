package main

import (
	"encoding/json"
	"sync"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/node"
	rpc "github.com/ethereum/go-ethereum/rpc/v2"
	"fmt"
)

type buf struct {
	mu        sync.Mutex
	readBuf   []byte
	requests  chan []byte
	responses chan []byte
}

// will read the next request in json format
func (b *buf) Read(p []byte) (int, error) {
	// last read didn't read entire request, return remaining bytes
	if len(b.readBuf) > 0 {
		n := copy(p, b.readBuf)
		if n < len(b.readBuf) {
			b.readBuf = b.readBuf[:n]
		} else {
			b.readBuf = b.readBuf[:0]
		}
		return n, nil
	}

	// read next request
	req := <-b.requests
	n := copy(p, req)
	if n < len(req) {
		// buf too small, store remaining chunk for next read
		b.readBuf = req[n:]
	}

	return n, nil
}

func (b *buf) Write(p []byte) (n int, err error) {
	b.responses <- p
	return len(p), nil
}

func (b *buf) Close() error {
	close(b.requests)
	close(b.responses)

	return nil
}

type inProcClient struct {
	server *rpc.Server
	buf    *buf
}

func NewInProcClient(stack *node.Node) (*inProcClient, error) {
	server := rpc.NewServer()

	offered := stack.APIs()
	for _, api := range offered {
		server.RegisterName(api.Namespace, api.Service)
	}

	web3 := utils.NewPublicWeb3API(stack)
	server.RegisterName("web3", web3)

	var ethereum *eth.Ethereum
	if err := stack.Service(&ethereum); err == nil {
		net := utils.NewPublicNetAPI(stack.Server(), ethereum.NetVersion())
		server.RegisterName("net", net)
	} else {
		glog.V(logger.Warn).Infof("%v\n", err)
	}

	buf := &buf{
		requests: make(chan []byte),
		responses: make(chan []byte),
	}
	client := &inProcClient{
		server: server,
		buf: buf,
	}

	go func() {
		server.ServeCodec(rpc.NewJSONCodec(client.buf))
	}()

	return client, nil
}

func (c *inProcClient) Close() {
	// not implemented for console
}

func (c *inProcClient) Send(req interface{}) error {
	d, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.buf.requests <- d
	return nil
}

func (c *inProcClient) Recv() (interface{}, error) {
	data := <-c.buf.responses
	var response map[string]interface{}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}

	return response, nil
}

func (c *inProcClient) SupportedModules() (map[string]string, error) {
	req := map[string]interface{}{
		"id": 1,
		"method": "rpc_modules",
	}

	d, _ := json.Marshal(req)

	c.buf.requests <- d
	r := <-c.buf.responses

	var response map[string]interface{}
	if err := json.Unmarshal(r, &response); err != nil {
		return nil, err
	}
	
	if payload, ok := response["result"]; ok {
		mods := make(map[string]string)
		if modules, ok := payload.(map[string]interface{}); ok {
			for m, v := range modules {
				mods[m] = fmt.Sprintf("%s", v)
			}
			return mods, nil
		}
	}

	return nil, fmt.Errorf("unable to retrieve modules")
}
