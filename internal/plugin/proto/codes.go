package proto

import (
	"errors"
	"fmt"
)

// JSON-RPC 2.0 standard codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Aplexica plugin protocol codes. See design spec §4.8.
const (
	// CodeUnrecognizedNativeFile signals the plugin does not claim the
	// file; orchestrator falls through to the next adapter.
	CodeUnrecognizedNativeFile = -32001
	// CodeParseErrorPlugin: the file IS this plugin's but is malformed.
	// Renamed from CodeParseError so it does not collide with the
	// JSON-RPC standard code; the design's "-32002 parse_error" maps here.
	CodeParseErrorPlugin = -32002
	// CodeReservedChainInvalid (-32003) reserved; see design §4.8.

	// CodeFormatUnsupported: handles_format said yes but export cannot
	// handle this payload.
	CodeFormatUnsupported = -32004
	// CodeIOError: plugin could not read or write the requested path.
	CodeIOError = -32005
	// CodeSecretExtractionFailed: tool import could not extract a declared secret.
	CodeSecretExtractionFailed = -32006
	// CodeInternal: catch-all for plugin bugs.
	CodeInternal = -32099
)

// RPCError is the error type the host package emits and the proxy
// package consumes. The Code maps to one of the constants above.
type RPCError struct {
	Code    int
	Message string
}

// Error satisfies the error interface.
func (e *RPCError) Error() string {
	return fmt.Sprintf("plugin rpc error %d: %s", e.Code, e.Message)
}

// IsUnrecognized reports whether err is an RPCError with code
// CodeUnrecognizedNativeFile (-32001). The sync orchestrator uses this
// to recognize the fall-through case and try the next adapter.
func IsUnrecognized(err error) bool {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc.Code == CodeUnrecognizedNativeFile
	}
	return false
}
