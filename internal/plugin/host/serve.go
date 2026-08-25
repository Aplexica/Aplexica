package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// Serve reads JSON-RPC requests from r, dispatches each to handler,
// and writes responses to w. Returns nil on clean EOF or after handling
// a successful Shutdown; returns a non-nil error only for unrecoverable
// transport failures.
func Serve(ctx context.Context, handler Handler, r io.Reader, w io.Writer) error {
	fr := proto.NewFrameReader(r)
	fw := proto.NewFrameWriter(w)
	for {
		frame, err := fr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		resp := dispatch(ctx, handler, frame)
		b, mErr := json.Marshal(resp)
		if mErr != nil {
			return mErr
		}
		if err := fw.Write(b); err != nil {
			return err
		}
		if resp.shutdown {
			return nil
		}
	}
}

// dispatchedResponse wraps proto.Response with a shutdown signal used
// only inside this package — it tells Serve to return after writing.
type dispatchedResponse struct {
	proto.Response
	shutdown bool
}

func (d dispatchedResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Response)
}

// dispatch decodes one JSON-RPC request frame and routes it to the
// appropriate Handler method. Sets shutdown=true on the response when
// the request was a shutdown so Serve knows to terminate after writing.
func dispatch(ctx context.Context, handler Handler, frame []byte) dispatchedResponse {
	var req proto.Request
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResp(nil, proto.CodeParseError, "parse error: "+err.Error())
	}
	switch req.Method {
	case proto.MethodInitialize:
		return call(ctx, req, handler.Initialize)
	case proto.MethodImport:
		return call(ctx, req, handler.Import)
	case proto.MethodExport:
		return call(ctx, req, handler.Export)
	case proto.MethodNativePath:
		return call(ctx, req, handler.NativePath)
	case proto.MethodHandlesFormat:
		return call(ctx, req, handler.HandlesFormat)
	case proto.MethodCapabilities:
		return call(ctx, req, handler.Capabilities)
	case proto.MethodShutdown:
		r := call(ctx, req, handler.Shutdown)
		r.shutdown = true
		return r
	default:
		return errResp(req.ID, proto.CodeMethodNotFound, "method not found: "+req.Method)
	}
}

// call is the generic dispatch helper. P and R are the typed param and
// result for the method.
func call[P any, R any](ctx context.Context, req proto.Request, fn func(context.Context, P) (R, error)) dispatchedResponse {
	var params P
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return errResp(req.ID, proto.CodeInvalidParams, "invalid params: "+err.Error())
		}
	}
	result, err := fn(ctx, params)
	if err != nil {
		var rpcErr *proto.RPCError
		if errors.As(err, &rpcErr) {
			return errResp(req.ID, rpcErr.Code, rpcErr.Message)
		}
		return errResp(req.ID, proto.CodeInternal, err.Error())
	}
	b, mErr := json.Marshal(result)
	if mErr != nil {
		return errResp(req.ID, proto.CodeInternal, "marshal result: "+mErr.Error())
	}
	return dispatchedResponse{Response: proto.Response{JSONRPC: "2.0", ID: req.ID, Result: b}}
}

// errResp builds a dispatchedResponse carrying a JSON-RPC error with
// the given code and message, echoing the request ID.
func errResp(id json.RawMessage, code int, message string) dispatchedResponse {
	return dispatchedResponse{
		Response: proto.Response{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &proto.RPCErrorPayload{Code: code, Message: message},
		},
	}
}
