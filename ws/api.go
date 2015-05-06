package ws

import (
	"errors"

	"github.com/ethereum/go-ethereum/xeth"
)

const (
	ApiVersion = "0.1"

	// WS methods
	Quit       = "quit"
	MinerStart = "miner_start"
	MinerStop  = "miner_stop"
)

var (
	// mapping between WS methods and handlers
	actions = make(map[string]RequestHandler)

	// WS errors
	WSInvalidRequest   = errors.New("Unable to parse request")
	UnauthorizedClient = errors.New("Unauthorized client")
	UnsupportedWsCall  = errors.New("Unsupported call")
	MinerNotStarted    = errors.New("Unable to start miner")
	MinerNotStopped    = errors.New("Unable to stop miner")
)

// Websocket API
type WsApi struct {
	eth *xeth.XEth
}

// Create new websocket API
func NewApi(xeth *xeth.XEth) *WsApi {
	return &WsApi{eth: xeth}
}

// Execute websocket request
func (self *WsApi) Execute(req *WSRequest, res *interface{}) error {
	if action, found := actions[req.Method]; found {
		return action(self.eth, req, res)
	}

	return UnsupportedWsCall
}
