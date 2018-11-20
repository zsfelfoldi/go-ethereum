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

package les

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	colorable "github.com/mattn/go-colorable"
)

const simTestBlockCount = 6705042

func init() {
	flag.Parse()
	// register the Delivery service which will run as a devp2p
	// protocol when using the exec adapter
	fmt.Println("register start")
	adapters.RegisterServices(services)
	fmt.Println("register end")

	log.PrintOrigins(true)
	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(*loglevel), log.StreamHandler(colorable.NewColorableStderr(), log.TerminalFormat(true))))
}

var (
	adapter  = flag.String("adapter", "exec", "type of simulation: sim|socket|exec|docker")
	loglevel = flag.Int("loglevel", 0, "verbosity of logs")
	nodes    = flag.Int("nodes", 0, "number of nodes")
)

var services = adapters.Services{
	"lesclient": newLesClientService,
	"lesserver": newLesServerService,
}

func NewAdapter(adapterType string, services adapters.Services) (adapter adapters.NodeAdapter, teardown func(), err error) {
	teardown = func() {}
	switch adapterType {
	case "sim":
		adapter = adapters.NewSimAdapter(services)
		//	case "socket":
		//		adapter = adapters.NewSocketAdapter(services)
	case "exec":
		baseDir, err0 := ioutil.TempDir("", "les-test")
		if err0 != nil {
			return nil, teardown, err0
		}
		teardown = func() { os.RemoveAll(baseDir) }
		adapter = adapters.NewExecAdapter(baseDir)
	/*case "docker":
	adapter, err = adapters.NewDockerAdapter()
	if err != nil {
		return nil, teardown, err
	}*/
	default:
		return nil, teardown, errors.New("adapter needs to be one of sim, socket, exec, docker")
	}
	return adapter, teardown, nil
}

func testSim(t *testing.T, serverCount int, clientCount int, test func(ctx context.Context, net *simulations.Network, servers []*simulations.Node, clients []*simulations.Node)) {
	fmt.Println("test start")
	net, teardown, err := NewNetwork()
	defer teardown()
	if err != nil {
		t.Fatalf("Failed to create network: %v", err)
	}
	fmt.Println("1")

	timeout := 300 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	servers := make([]*simulations.Node, serverCount)
	clients := make([]*simulations.Node, clientCount)

	clientconf := adapters.RandomNodeConfig()
	clientconf.Services = []string{"lesclient"}
	for i, _ := range clients {
		client, err := net.NewNodeWithConfig(clientconf)
		if err != nil {
			t.Fatalf("Failed to create client: %v", err)
		}
		clients[i] = client
	}

	serverconf := adapters.RandomNodeConfig()
	serverconf.Services = []string{"lesserver"}
	serverconf.DataDir = "/media/1TB/.ethereum"
	for i, _ := range servers {
		server, err := net.NewNodeWithConfig(serverconf)
		if err != nil {
			t.Fatalf("Failed to create server: %v", err)
		}
		servers[i] = server
	}

	fmt.Println("2")

	for _, client := range clients {
		if err := net.Start(client.ID()); err != nil {
			t.Fatalf("Failed to start client node: %v", err)
		}
	}
	for _, server := range servers {
		if err := net.Start(server.ID()); err != nil {
			t.Fatalf("Failed to start server node: %v", err)
		}
	}
	fmt.Println("3")

	test(ctx, net, servers, clients)
}

func testSimGenerateChain(node *simulations.Node, targetHead uint64) {

}

func TestSim(t *testing.T) {
	testSim(t, 1, 1, func(ctx context.Context, net *simulations.Network, servers []*simulations.Node, clients []*simulations.Node) {
		server := servers[0]
		client := clients[0]

		var headNum uint64
		serverClient, err := server.Client()
		if err != nil {
			t.Fatalf("Failed to obtain server.Client(): %v", err)
		}
		if err := serverClient.CallContext(ctx, &headNum, "test_generateChain", simTestBlockCount); err != nil {
			t.Fatalf("Failed to call test_generateChain: %v", err)
		}

		fmt.Println("3x")

		net.Connect(client.ID(), server.ID())

		sim := simulations.NewSimulation(net)

		action := func(ctx context.Context) error {
			return nil
		}

		check := func(ctx context.Context, id enode.ID) (bool, error) {
			fmt.Println(id, "*****")
			// check we haven't run out of time
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			default:
			}

			// get the node
			node := net.GetNode(id)
			if node == nil {
				return false, fmt.Errorf("unknown node: %s", id)
			}
			client, err := node.Client()
			if err != nil {
				return false, err
			}
			var s string
			if err := client.CallContext(ctx, &s, "eth_blockNumber"); err != nil {
				return false, err
			}

			fmt.Println(id, s)
			head, err := hexutil.DecodeUint64(s)
			if err != nil {
				return false, err
			}
			return head == simTestBlockCount, nil
		}

		trigger := make(chan enode.ID)
		go func() {
			for {
				select {
				case trigger <- client.ID():
				case <-ctx.Done():
					return
				}
				select {
				case trigger <- server.ID():
				case <-ctx.Done():
					return
				}
				time.Sleep(time.Millisecond * 100)
			}
			//		trigger <- client.ID()
			//		trigger <- server.ID()
		}()

		step := &simulations.Step{
			Action:  action,
			Trigger: trigger,
			Expect: &simulations.Expectation{
				Nodes: []enode.ID{server.ID(), client.ID()},
				Check: check,
			},
		}

		result := sim.Run(ctx, step)
		if result.Error != nil {
			t.Fatalf("Simulation failed: %s", result.Error)
		}
	})
}

func NewNetwork() (*simulations.Network, func(), error) {
	adapter, adapterTeardown, err := NewAdapter(*adapter, services)
	if err != nil {
		return nil, adapterTeardown, err
	}
	defaultService := "streamer"
	net := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		ID:             "0",
		DefaultService: defaultService,
	})
	teardown := func() {
		adapterTeardown()
		net.Shutdown()
	}

	return net, teardown, nil
}

func newLesClientService(ctx *adapters.ServiceContext) (node.Service, error) {
	config := eth.DefaultConfig
	config.SyncMode = downloader.LightSync
	config.Ethash.PowMode = ethash.ModeFake
	return New(ctx.NodeContext, &config)
}

func newLesServerService(ctx *adapters.ServiceContext) (node.Service, error) {
	fmt.Println("server init start")
	defer fmt.Println("server init end")
	config := eth.DefaultConfig
	config.SyncMode = downloader.FullSync
	config.LightServ = 50
	config.LightPeers = 20
	ethereum, err := eth.New(ctx.NodeContext, &config)
	if err != nil {
		return nil, err
	}

	server, err := NewLesServer(ethereum, &config)
	if err != nil {
		return nil, err
	}
	ethereum.AddLesServer(server)
	ethereum.AddExtraAPIs([]rpc.API{{
		Namespace: "test",
		Version:   "1.0",
		Service:   &SimTestAPI{ethereum},
	}})
	return ethereum, nil
}

type SimTestAPI struct {
	ethereum *eth.Ethereum
}

func (s *SimTestAPI) GenerateChain(headNum uint64) (uint64, error) {
	db := s.ethereum.ChainDb()
	chain := s.ethereum.BlockChain()
	lastBlock := chain.CurrentBlock()
	lastNum := lastBlock.NumberU64()
	if headNum > lastNum {
		blocks, _ := core.GenerateChain(params.TestChainConfig, lastBlock, ethash.NewFaker(), db, int(headNum-lastNum), nil)
		if i, err := chain.InsertChain(blocks); err != nil {
			return chain.CurrentBlock().NumberU64(), fmt.Errorf("error at inserting block #%d: %v", i, err)
		}
		return chain.CurrentBlock().NumberU64(), nil
	}
	return headNum, nil
}
