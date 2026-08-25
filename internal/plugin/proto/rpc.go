package proto

import "encoding/json"

// Request is the JSON-RPC 2.0 request envelope used over the plugin
// transport. ID is RawMessage so callers can use any JSON-valid form
// (number, string, null) without an extra encode hop.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the JSON-RPC 2.0 response envelope. Exactly one of Result
// and Error should be populated; the proto package does not enforce
// this — callers check after unmarshal.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCErrorPayload `json:"error,omitempty"`
}

// RPCErrorPayload is the body of the "error" field in a Response.
type RPCErrorPayload struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}
