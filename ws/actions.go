package ws

import (
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/xeth"
)

func init() {
	// register WS methods handlers
	actions[Quit] = quit
	actions[MinerStart] = minerStart
	actions[MinerStop] = minerStop
}

// websocket API stateless handler type
type RequestHandler func(eth *xeth.XEth, req *WSRequest, res *interface{}) error

func quit(eth *xeth.XEth, req *WSRequest, res *interface{}) error {
	glog.V(logger.Error).Infoln("quit called :)")
	eth.StopBackend()
	return nil
}

func minerStart(eth *xeth.XEth, req *WSRequest, res *interface{}) error {
	if eth.SetMining(true) {
		return nil
	}
	return MinerNotStarted
}

func minerStop(eth *xeth.XEth, req *WSRequest, res *interface{}) error {
	if !eth.SetMining(false) {
		return nil
	}
	return MinerNotStopped
}
