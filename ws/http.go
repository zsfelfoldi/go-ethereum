package ws

import (
	"fmt"
	"net/http"

	"net/url"
	"os"

	"code.google.com/p/go.net/websocket"
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

// passes the security token from the path, returns an empty string when no token could be parsed
// path must have the form of: /geth/<token>
func tokenFromRequest(url *url.URL) string {
	token := url.Query().Get(accessTokenName)
	if len(token) == accessTokenLength {
		return token
	}

	return ""
}

// Verify if the client is allowed to consume the API
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

type WsHandler func(*websocket.Conn)

// Sets the custom handshake handler which will check if the client is allowed to consume the API
func (h WsHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s := websocket.Server{Handler: websocket.Handler(h), Handshake: wshandshake}
	s.Handler.ServeHTTP(w, req)
}

// read message
func receive(ws *websocket.Conn) (*WSRequest, error) {
	fmt.Println("receive message :)")
	req := new(WSRequest)
	err := websocket.JSON.Receive(ws, req)
	if err == nil {
		return req, nil
	}

	glog.V(logger.Warn).Infof("Unable to parse WS request %v\n", err)
	return nil, WSInvalidRequest
}

// send message
func send(ws *websocket.Conn, response *interface{}, err error) error {
	var res interface{}
	if err == nil {
		res = &WSSuccessResponse{}
	} else {
		res = &WSErrorResponse{}
	}

	return websocket.JSON.Send(ws, res)
}

// Main handler which will parse the incoming request and calls the appropriate request handler
func newWSHandler(eth *xeth.XEth) websocket.Handler {
	api := NewApi(eth)
	return func(ws *websocket.Conn) {
		req, err := receive(ws)

		if err != nil {
			ws.Close()
			return
		}

		var reply interface{}
		err = api.Execute(req, &reply)
		err = send(ws, &reply, err)
		if err != nil {
			ws.Close()
			return
		}
	}
}

// Start websocket service
func Start(eth *xeth.XEth, cfg Config) error {
	s := websocket.Server{Handler: newWSHandler(eth), Handshake: wshandshake}
	http.Handle(endpointPath, s)
	go http.ListenAndServe(fmt.Sprintf("%s:%d", cfg.ListenAddress, cfg.ListenPort), nil)
	return nil
}

// Stop websocket service
func Stop(eth *xeth.XEth) error {
	return nil
}
