package proto

import (
	"errors"
	"testing"
)

func TestErrorCodeValues(t *testing.T) {
	cases := map[int]int{
		CodeParseError:             -32700,
		CodeInvalidRequest:         -32600,
		CodeMethodNotFound:         -32601,
		CodeInvalidParams:          -32602,
		CodeInternalError:          -32603,
		CodeUnrecognizedNativeFile: -32001,
		CodeParseErrorPlugin:       -32002,
		CodeFormatUnsupported:      -32004,
		CodeIOError:                -32005,
		CodeSecretExtractionFailed: -32006,
		CodeInternal:               -32099,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	}
}

func TestRPCErrorIsError(t *testing.T) {
	e := &RPCError{Code: CodeUnrecognizedNativeFile, Message: "nope"}
	if e.Error() == "" {
		t.Error("Error() must not be empty")
	}
}

func TestIsUnrecognized(t *testing.T) {
	e := &RPCError{Code: CodeUnrecognizedNativeFile, Message: "x"}
	if !IsUnrecognized(e) {
		t.Error("IsUnrecognized should match -32001")
	}
	other := &RPCError{Code: CodeParseErrorPlugin, Message: "x"}
	if IsUnrecognized(other) {
		t.Error("IsUnrecognized must not match -32002")
	}
	if IsUnrecognized(errors.New("plain")) {
		t.Error("IsUnrecognized must not match plain error")
	}
}
