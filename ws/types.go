package ws

import "encoding/json"

type Config struct {
	ListenAddress string
	ListenPort    uint
	SecurityToken string
}

type WSRequest struct {
	Id        interface{}     `json:"id"`
	WsVersion string          `json:"jsonrpc"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
}

type WSSuccessResponse struct {
	Id        interface{} `json:"id"`
	WsVersion string      `json:"jsonrpc"`
	Method    string      `json:"method"`
	Result    interface{} `json:"result"`
}

type WSErrorResponse struct {
	Id        interface{}    `json:"id"`
	WsVersion string         `json:"jsonrpc"`
	Method    string         `json:"method"`
	Error     *WSErrorObject `json:"error"`
}

type WSErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type MinerStartRequest struct {
}

type MinerStartResponse struct {
}

type MinerHashrateResponse struct {
	Hashrate int64 `json:"hashrate"`
}
