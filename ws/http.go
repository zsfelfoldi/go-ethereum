package ws

import (
	"fmt"
	"net/http"

	"net/url"
	"os"

	"sync"

	"io"

	"code.google.com/p/go.net/websocket"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/xeth"
)

const (
	endpointPath = "/geth"
	// client must specify security token during the handshake
	accessTokenName = "accessToken"
	// 128 bits hex encoded security token
	accessTokenLength = 32
)

var (
	// connected clients, used for broadcast
	clients   []*websocket.Conn
	clientsMu sync.Mutex
)

func receive(ws *websocket.Conn) (*WSRequest, error) {
	req := new(WSRequest)
	err := websocket.JSON.Receive(ws, req)
	if err == nil {
		return req, nil
	}

	if err == io.EOF { /// client hang up
		return nil, err
	}

	glog.V(logger.Warn).Infof("Unable to parse WS request %v\n", err)
	return nil, WSInvalidRequest
}

func send(ws *websocket.Conn, request *WSRequest, response *interface{}, err error) error {
	var res interface{}
	if err == nil {
		if request != nil {
			res = &WSSuccessResponse{Id: request.Id, WsVersion: ApiVersion, Method: request.Method, Result: response}
		} else { // broadcast
			res = &WSSuccessResponse{Id: 0, WsVersion: ApiVersion, Method: "event", Result: response}
		}
	} else {
		if request != nil {
			res = &WSErrorResponse{Id: request.Id, WsVersion: ApiVersion, Method: request.Method, Error: &WSErrorObject{Code: 0, Message: err.Error()}}
		} else { // broadcast
			res = &WSErrorResponse{Id: 0, WsVersion: ApiVersion, Method: "broadcast", Error: &WSErrorObject{Code: 0, Message: err.Error()}}
		}
	}

	return websocket.JSON.Send(ws, res)
}

// parses the security token from the path /geth?accessToken=<token>
// returns an empty string when no token could be parsed
func tokenFromRequest(url *url.URL) string {
	token := url.Query().Get(accessTokenName)
	if len(token) == accessTokenLength {
		return token
	}

	return ""
}

// Verify if the client is able/allowed to consume the API
// - check token secret, prevent unauthorized localhost clients
func wshandshake(cfg *websocket.Config, req *http.Request) (err error) {
	expectedSecurityToken := os.Getenv(accessTokenName) // set by mist when geth is started
	receivedSecurityToken := tokenFromRequest(req.URL)

	if expectedSecurityToken == receivedSecurityToken {
		return nil
	}

	glog.V(logger.Info).Infoln("Blocked unauthorized WS connection - client supplied invalid security token")
	return UnauthorizedClient
}

// Main handler which will parse the incoming request and calls the appropriate request handler
func newWSHandler(xeth *xeth.XEth) websocket.Handler {
	api := NewApi(xeth)
	return func(ws *websocket.Conn) {
		clientsMu.Lock()
		clients = append(clients, ws)
		clientsMu.Unlock()

		defer func() {
			clientsMu.Lock()
			clients = append(clients, ws)
			defer clientsMu.Unlock()
			for i, c := range clients {
				if ws == c {
					clients = append(clients[0:i], clients[i:]...)
				}
			}
		}()

		for {
			req, err := receive(ws)
			if err != nil {
				return
			}

			var reply interface{}
			err = api.Execute(req, &reply)
			err = send(ws, req, &reply, err)
			if err != nil {
				return
			}
		}
	}
}

func broadcast(msg *interface{}, err error) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for _, c := range clients {
		send(c, nil, msg, err)
	}
}

func eventLoop(eth *eth.Ethereum) {
	chainEvents := eth.EventMux().Subscribe(core.ChainEvent{})
	splitEvents := eth.EventMux().Subscribe(core.ChainSplitEvent{})
	headEvents := eth.EventMux().Subscribe(core.ChainHeadEvent{})

	for {
		select {
		case ev := <-chainEvents.Chan():
			broadcast(&ev, nil)
			break
		case ev := <-splitEvents.Chan():
			broadcast(&ev, nil)
			break
		case ev := <-headEvents.Chan():
			broadcast(&ev, nil)
			break
		}
	}
}

// Start websocket service
func Start(xeth *xeth.XEth, eth *eth.Ethereum, cfg Config) error {
	s := websocket.Server{Handler: newWSHandler(xeth), Handshake: wshandshake}
	http.Handle(endpointPath, s)
	go http.ListenAndServe(fmt.Sprintf("%s:%d", cfg.ListenAddress, cfg.ListenPort), nil)
	go eventLoop(eth)
	return nil
}

// Stop websocket service
func Stop(eth *xeth.XEth) error {
	return nil
}
