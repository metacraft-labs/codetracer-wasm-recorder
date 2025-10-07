package stylus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

type RpcClient struct {
	url string
	id  int
}

func NewRpcClient(url string) *RpcClient {
	return &RpcClient{url: url, id: 0}
}

func (client *RpcClient) Request(method string, params any) (json.RawMessage, error) {
	data, err := json.Marshal(rpcRequest{
		Jsonrpc: "2.0",
		Method:  method,
		Params:  params,
		Id:      client.id,
	})
	if err != nil {
		return nil, err
	}
	client.id++

	resp, err := http.Post(client.url, "application/json", bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, err
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("JSON RPC returned error: %v", *rpcResp.Error)
	}

	return rpcResp.Result, nil
}

type rpcRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
	Id      int         `json:"id"`
}

type rpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	Id      int             `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
