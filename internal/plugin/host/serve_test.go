package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

type stubHandler struct {
	initCalled     bool
	shutdownCalled bool
}

func (s *stubHandler) Initialize(_ context.Context, _ proto.InitializeParams) (proto.InitializeResult, error) {
	s.initCalled = true
	return proto.InitializeResult{
		PluginName:    "stub",
		PluginVersion: "0.0.1",
		ABIVersion:    proto.ABIVersion,
		Kinds:         []acf.Kind{acf.KindMemory},
		Formats:       map[acf.Kind][]string{acf.KindMemory: {"markdown"}},
	}, nil
}
func (s *stubHandler) Import(_ context.Context, _ proto.ImportParams) (proto.ImportResult, error) {
	return proto.ImportResult{}, nil
}
func (s *stubHandler) Export(_ context.Context, _ proto.ExportParams) (proto.ExportResult, error) {
	return proto.ExportResult{Written: true}, nil
}
func (s *stubHandler) NativePath(_ context.Context, _ proto.NativePathParams) (proto.NativePathResult, error) {
	return proto.NativePathResult{Supports: false}, nil
}
func (s *stubHandler) HandlesFormat(_ context.Context, _ proto.HandlesFormatParams) (proto.HandlesFormatResult, error) {
	return proto.HandlesFormatResult{Handles: true}, nil
}
func (s *stubHandler) Capabilities(_ context.Context, _ proto.CapabilitiesParams) (proto.CapabilitiesResult, error) {
	return proto.CapabilitiesResult{
		Name:      "stub",
		Artifacts: proto.ArtifactSupport{Memory: true, Skill: true, Tool: true, Conversation: true},
	}, nil
}
func (s *stubHandler) Shutdown(_ context.Context, _ proto.ShutdownParams) (proto.ShutdownResult, error) {
	s.shutdownCalled = true
	return proto.ShutdownResult{}, nil
}

// pipePair returns (clientR, clientW, serverR, serverW) wired so writes
// to clientW are read by serverR and writes to serverW are read by
// clientR — a full-duplex in-memory transport for two goroutines.
func pipePair() (io.Reader, io.Writer, io.Reader, io.Writer, func()) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	closer := func() {
		c2sW.Close()
		s2cW.Close()
	}
	return s2cR, c2sW, c2sR, s2cW, closer
}

func TestServeInitializeAndShutdown(t *testing.T) {
	clientR, clientW, serverR, serverW, closer := pipePair()
	h := &stubHandler{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = Serve(context.Background(), h, serverR, serverW)
	}()
	w := proto.NewFrameWriter(clientW)
	r := proto.NewFrameReader(clientR)

	initParams, _ := json.Marshal(proto.InitializeParams{ABIVersion: proto.ABIVersion, DaemonVersion: "0", DeviceID: "test"})
	req := proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: proto.MethodInitialize, Params: initParams}
	b, _ := json.Marshal(req)
	if err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	frame, err := r.Read()
	if err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	var resp proto.Response
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("got error: %+v", resp.Error)
	}
	if !h.initCalled {
		t.Error("handler.Initialize not called")
	}

	sdReq := proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: proto.MethodShutdown, Params: json.RawMessage(`{}`)}
	b, _ = json.Marshal(sdReq)
	if err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	frame, err = r.Read()
	if err != nil {
		t.Fatalf("read shutdown: %v", err)
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("got shutdown error: %+v", resp.Error)
	}
	closer()
	wg.Wait()
	if !h.shutdownCalled {
		t.Error("handler.Shutdown not called")
	}
}

func TestServeEOFExitsCleanly(t *testing.T) {
	_, _, serverR, serverW, closer := pipePair()
	h := &stubHandler{}
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), h, serverR, serverW) }()
	closer()
	if err := <-done; err != nil {
		t.Errorf("Serve returned %v on EOF, want nil", err)
	}
}

func TestServeUnknownMethod(t *testing.T) {
	clientR, clientW, serverR, serverW, closer := pipePair()
	defer closer()
	go Serve(context.Background(), &stubHandler{}, serverR, serverW)

	w := proto.NewFrameWriter(clientW)
	r := proto.NewFrameReader(clientR)
	req := proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "no_such_method"}
	b, _ := json.Marshal(req)
	w.Write(b)
	frame, _ := r.Read()
	var resp proto.Response
	json.Unmarshal(frame, &resp)
	if resp.Error == nil || resp.Error.Code != proto.CodeMethodNotFound {
		t.Errorf("got %+v, want method-not-found", resp.Error)
	}
}

func TestServeMalformedFrame(t *testing.T) {
	clientR, clientW, serverR, serverW, closer := pipePair()
	defer closer()
	go Serve(context.Background(), &stubHandler{}, serverR, serverW)

	w := proto.NewFrameWriter(clientW)
	r := proto.NewFrameReader(clientR)
	w.Write([]byte(`{not-json`))
	frame, _ := r.Read()
	var resp proto.Response
	json.Unmarshal(frame, &resp)
	if resp.Error == nil || resp.Error.Code != proto.CodeParseError {
		t.Errorf("got %+v, want parse-error", resp.Error)
	}
}

type erroringHandler struct{ stubHandler }

func (e *erroringHandler) Import(_ context.Context, _ proto.ImportParams) (proto.ImportResult, error) {
	return proto.ImportResult{}, &proto.RPCError{Code: proto.CodeUnrecognizedNativeFile, Message: "nope"}
}

func TestServeHandlerRPCError(t *testing.T) {
	clientR, clientW, serverR, serverW, closer := pipePair()
	defer closer()
	go Serve(context.Background(), &erroringHandler{}, serverR, serverW)

	w := proto.NewFrameWriter(clientW)
	r := proto.NewFrameReader(clientR)
	params, _ := json.Marshal(proto.ImportParams{NativePath: "/x"})
	req := proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: proto.MethodImport, Params: params}
	b, _ := json.Marshal(req)
	w.Write(b)
	frame, _ := r.Read()
	var resp proto.Response
	json.Unmarshal(frame, &resp)
	if resp.Error == nil || resp.Error.Code != proto.CodeUnrecognizedNativeFile {
		t.Errorf("got %+v, want -32001", resp.Error)
	}
}

type plainErrorHandler struct{ stubHandler }

func (p *plainErrorHandler) Import(_ context.Context, _ proto.ImportParams) (proto.ImportResult, error) {
	return proto.ImportResult{}, errors.New("kablooey")
}

func TestServeHandlerPlainErrorBecomesInternal(t *testing.T) {
	clientR, clientW, serverR, serverW, closer := pipePair()
	defer closer()
	go Serve(context.Background(), &plainErrorHandler{}, serverR, serverW)

	w := proto.NewFrameWriter(clientW)
	r := proto.NewFrameReader(clientR)
	params, _ := json.Marshal(proto.ImportParams{NativePath: "/x"})
	req := proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: proto.MethodImport, Params: params}
	b, _ := json.Marshal(req)
	w.Write(b)
	frame, _ := r.Read()
	var resp proto.Response
	json.Unmarshal(frame, &resp)
	if resp.Error == nil || resp.Error.Code != proto.CodeInternal {
		t.Errorf("got %+v, want -32099", resp.Error)
	}
}
