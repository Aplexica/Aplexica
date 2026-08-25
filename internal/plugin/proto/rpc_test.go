package proto

import (
	"encoding/json"
	"testing"
)

func TestRequestMarshal(t *testing.T) {
	req := Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
		Params:  json.RawMessage(`{"abi_version":"1"}`),
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"abi_version":"1"}}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestResponseResultMarshal(t *testing.T) {
	r := Response{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Result:  json.RawMessage(`{"ok":true}`),
	}
	b, _ := json.Marshal(r)
	got := string(b)
	want := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestResponseErrorMarshal(t *testing.T) {
	r := Response{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Error: &RPCErrorPayload{
			Code:    -32001,
			Message: "unrecognized native file",
		},
	}
	b, _ := json.Marshal(r)
	got := string(b)
	want := `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"unrecognized native file"}}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestResponseEitherResultOrError(t *testing.T) {
	r := Response{JSONRPC: "2.0", ID: json.RawMessage(`1`)}
	if _, err := json.Marshal(r); err != nil {
		t.Errorf("marshal empty response: %v", err)
	}
}
