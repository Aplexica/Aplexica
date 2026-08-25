// SPDX-License-Identifier: AGPL-3.0-or-later
package adapterplugin_test

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/pkg/adapterplugin"
)

// smokeHandler is a trivial Handler built entirely from the public SDK
// surface. It embeds BaseCapabilitiesHandler (so Capabilities is provided)
// and implements the remaining six methods with the SDK type aliases. The
// point of this test is to prove the SDK compiles and round-trips, not to
// exercise translation logic — that lives in cmd/aplexica-plugin-example.
type smokeHandler struct {
	adapterplugin.BaseCapabilitiesHandler
}

func (smokeHandler) Initialize(_ context.Context, _ adapterplugin.InitializeParams) (adapterplugin.InitializeResult, error) {
	return adapterplugin.InitializeResult{
		PluginName:    "smoke",
		PluginVersion: "0.0.1",
		ABIVersion:    adapterplugin.ABIVersion,
		Kinds:         []acf.Kind{acf.KindMemory},
		Formats:       map[acf.Kind][]string{acf.KindMemory: {"markdown"}},
	}, nil
}

func (smokeHandler) Import(_ context.Context, _ adapterplugin.ImportParams) (adapterplugin.ImportResult, error) {
	// Returning the unrecognized code is the canonical "not my file"
	// fall-through; exercise the RPCError alias + a Code constant here.
	return adapterplugin.ImportResult{}, &adapterplugin.RPCError{
		Code:    adapterplugin.CodeUnrecognizedNativeFile,
		Message: "smoke handler imports nothing",
	}
}

func (smokeHandler) Export(_ context.Context, _ adapterplugin.ExportParams) (adapterplugin.ExportResult, error) {
	return adapterplugin.ExportResult{Written: true}, nil
}

func (smokeHandler) NativePath(_ context.Context, _ adapterplugin.NativePathParams) (adapterplugin.NativePathResult, error) {
	return adapterplugin.NativePathResult{Supports: false}, nil
}

func (smokeHandler) HandlesFormat(_ context.Context, _ adapterplugin.HandlesFormatParams) (adapterplugin.HandlesFormatResult, error) {
	return adapterplugin.HandlesFormatResult{Handles: false}, nil
}

func (smokeHandler) Shutdown(_ context.Context, _ adapterplugin.ShutdownParams) (adapterplugin.ShutdownResult, error) {
	return adapterplugin.ShutdownResult{}, nil
}

// compile-time assertion that smokeHandler satisfies the public Handler
// alias.
var _ adapterplugin.Handler = smokeHandler{}

// pipePair mirrors the host serve_test helper: full-duplex in-memory
// transport so the client goroutine talks to a Serve goroutine.
func pipePair() (clientR io.Reader, clientW io.Writer, serverR io.Reader, serverW io.Writer, closeFn func()) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	closeFn = func() {
		c2sW.Close()
		s2cW.Close()
	}
	return s2cR, c2sW, c2sR, s2cW, closeFn
}

func TestServeInitializeRoundTrip(t *testing.T) {
	clientR, clientW, serverR, serverW, closeFn := pipePair()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := adapterplugin.Serve(context.Background(), smokeHandler{}, serverR, serverW); err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	}()

	w := proto.NewFrameWriter(clientW)
	r := proto.NewFrameReader(clientR)

	initParams, _ := json.Marshal(adapterplugin.InitializeParams{
		ABIVersion:    adapterplugin.ABIVersion,
		DaemonVersion: "test",
		DeviceID:      "dev-1",
	})
	req := proto.Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: proto.MethodInitialize, Params: initParams}
	reqBytes, _ := json.Marshal(req)
	if err := w.Write(reqBytes); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	frame, err := r.Read()
	if err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	var resp proto.Response
	if err := json.Unmarshal(frame, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %+v", resp.Error)
	}

	var result adapterplugin.InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.PluginName != "smoke" {
		t.Errorf("plugin_name = %q, want smoke", result.PluginName)
	}
	if result.ABIVersion != adapterplugin.ABIVersion {
		t.Errorf("abi_version = %q, want %q", result.ABIVersion, adapterplugin.ABIVersion)
	}

	// Clean shutdown via EOF.
	closeFn()
	wg.Wait()
}

// TestErrorCodeConstantsMatchProto guards against the SDK re-exports
// drifting from the underlying protocol codes.
func TestErrorCodeConstantsMatchProto(t *testing.T) {
	cases := []struct {
		name string
		sdk  int
		want int
	}{
		{"UnrecognizedNativeFile", adapterplugin.CodeUnrecognizedNativeFile, proto.CodeUnrecognizedNativeFile},
		{"ParseErrorPlugin", adapterplugin.CodeParseErrorPlugin, proto.CodeParseErrorPlugin},
		{"FormatUnsupported", adapterplugin.CodeFormatUnsupported, proto.CodeFormatUnsupported},
		{"IOError", adapterplugin.CodeIOError, proto.CodeIOError},
		{"SecretExtractionFailed", adapterplugin.CodeSecretExtractionFailed, proto.CodeSecretExtractionFailed},
		{"Internal", adapterplugin.CodeInternal, proto.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sdk != tc.want {
				t.Errorf("SDK code = %d, proto code = %d", tc.sdk, tc.want)
			}
		})
	}
	if adapterplugin.ABIVersion != proto.ABIVersion {
		t.Errorf("ABIVersion = %q, want %q", adapterplugin.ABIVersion, proto.ABIVersion)
	}
}
