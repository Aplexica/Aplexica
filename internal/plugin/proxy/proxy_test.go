package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// rwPair returns a pair (a, b) of io.ReadWriters such that everything
// written to a is read from b and vice versa.
func rwPair() (a, b io.ReadWriter, closeFn func()) {
	aR, bW := io.Pipe()
	bR, aW := io.Pipe()
	closeFn = func() { aW.Close(); bW.Close() }
	return readWriter{aR, aW}, readWriter{bR, bW}, closeFn
}

type readWriter struct {
	io.Reader
	io.Writer
}

// fixtureHandler is a stub Handler used by proxy tests. Tests can swap
// its function fields to drive specific RPC behaviors.
type fixtureHandler struct {
	initResult proto.InitializeResult
	importFn   func(proto.ImportParams) (proto.ImportResult, error)
	exportFn   func(proto.ExportParams) (proto.ExportResult, error)
	capsResult *proto.CapabilitiesResult
}

func (f *fixtureHandler) Initialize(_ context.Context, _ proto.InitializeParams) (proto.InitializeResult, error) {
	return f.initResult, nil
}
func (f *fixtureHandler) Import(_ context.Context, p proto.ImportParams) (proto.ImportResult, error) {
	if f.importFn != nil {
		return f.importFn(p)
	}
	return proto.ImportResult{}, nil
}
func (f *fixtureHandler) Export(_ context.Context, p proto.ExportParams) (proto.ExportResult, error) {
	if f.exportFn != nil {
		return f.exportFn(p)
	}
	return proto.ExportResult{Written: true}, nil
}
func (f *fixtureHandler) NativePath(_ context.Context, _ proto.NativePathParams) (proto.NativePathResult, error) {
	return proto.NativePathResult{}, nil
}
func (f *fixtureHandler) HandlesFormat(_ context.Context, _ proto.HandlesFormatParams) (proto.HandlesFormatResult, error) {
	return proto.HandlesFormatResult{Handles: true}, nil
}
func (f *fixtureHandler) Capabilities(_ context.Context, _ proto.CapabilitiesParams) (proto.CapabilitiesResult, error) {
	if f.capsResult != nil {
		return *f.capsResult, nil
	}
	return proto.CapabilitiesResult{
		Name: f.initResult.PluginName,
		Artifacts: proto.ArtifactSupport{
			Memory: true, Skill: true, Tool: true, Conversation: true,
		},
	}, nil
}
func (f *fixtureHandler) Shutdown(_ context.Context, _ proto.ShutdownParams) (proto.ShutdownResult, error) {
	return proto.ShutdownResult{}, nil
}

// startPlugin spawns a host.Serve goroutine wired to the given handler
// and returns the client-side transport (what the proxy uses).
func startPlugin(t *testing.T, h host.Handler) (io.ReadWriter, func()) {
	t.Helper()
	clientRW, serverRW, closer := rwPair()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = host.Serve(context.Background(), h, serverRW, serverRW)
	}()
	return clientRW, func() {
		closer()
		wg.Wait()
	}
}

func TestOpenHandshake(t *testing.T) {
	h := &fixtureHandler{
		initResult: proto.InitializeResult{
			PluginName:    "stub",
			PluginVersion: "0.1.2",
			ABIVersion:    proto.ABIVersion,
			Kinds:         []acf.Kind{acf.KindMemory},
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()

	p, err := Open(context.Background(), transport, nil, "host-1", "0.26.0")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if p.Name() != "stub" {
		t.Errorf("Name = %q", p.Name())
	}
	if p.Version() != "0.1.2" {
		t.Errorf("Version = %q", p.Version())
	}
}

func TestOpenABIMismatch(t *testing.T) {
	h := &fixtureHandler{
		initResult: proto.InitializeResult{
			PluginName: "stub", PluginVersion: "0.1", ABIVersion: "2",
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()

	_, err := Open(context.Background(), transport, nil, "host-1", "0.26.0")
	if err == nil {
		t.Fatal("expected ABI mismatch error")
	}
}

type nativePathFixture struct {
	fixtureHandler
}

func (n *nativePathFixture) NativePath(_ context.Context, p proto.NativePathParams) (proto.NativePathResult, error) {
	return proto.NativePathResult{Path: p.ContextDir + "/" + p.Artifact.Name, Supports: true}, nil
}

func TestProxyHandlesFormat(t *testing.T) {
	h := &fixtureHandler{initResult: proto.InitializeResult{ABIVersion: proto.ABIVersion, PluginName: "stub"}}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, err := Open(context.Background(), transport, nil, "h", "0")
	if err != nil {
		t.Fatal(err)
	}
	if !p.HandlesFormat(acf.KindMemory, "markdown") {
		t.Error("expected handles=true")
	}
}

func TestProxyNativePath(t *testing.T) {
	h := &nativePathFixture{
		fixtureHandler: fixtureHandler{
			initResult: proto.InitializeResult{ABIVersion: proto.ABIVersion, PluginName: "stub"},
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, err := Open(context.Background(), transport, nil, "h", "0")
	if err != nil {
		t.Fatal(err)
	}
	path, supports, err := p.NativePath(acf.Artifact{Kind: acf.KindMemory, Name: "CLAUDE.md"}, "/abs")
	if err != nil {
		t.Fatal(err)
	}
	if !supports || path != "/abs/CLAUDE.md" {
		t.Errorf("got (%q, %v)", path, supports)
	}
}

func TestProxyImportMemory(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "CLAUDE.md")
	os.WriteFile(sourcePath, []byte("hello"), 0o644)

	payload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "hello"})

	h := &fixtureHandler{
		initResult: proto.InitializeResult{ABIVersion: proto.ABIVersion, PluginName: "claude-code", PluginVersion: "0.2.0"},
		importFn: func(p proto.ImportParams) (proto.ImportResult, error) {
			if p.NativePath != sourcePath {
				return proto.ImportResult{}, &proto.RPCError{Code: proto.CodeUnrecognizedNativeFile, Message: "wrong path"}
			}
			return proto.ImportResult{
				Imports: []proto.ImportedItem{{
					Kind: acf.KindMemory, Scope: acf.ScopeProject,
					Name: "CLAUDE.md", SourcePath: sourcePath,
					Payload: json.RawMessage(payload),
				}},
			}, nil
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, err := Open(context.Background(), transport, store, "host-1", "0.26.0")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := p.Import(context.Background(), store, sourcePath)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(ids) != 1 || ids[0] == "" {
		t.Fatalf("ids: %v", ids)
	}
	art, err := store.ReadArtifact(acf.KindMemory, ids[0])
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if art.SourcePath != sourcePath {
		t.Errorf("source_path = %q", art.SourcePath)
	}
	events, _ := store.ReadEvents(acf.KindMemory, ids[0])
	if len(events) != 1 || events[0].Provenance.SourceAgent != "claude-code" {
		t.Errorf("events: %+v", events)
	}
	if events[0].Provenance.AdapterVersion != "0.2.0" {
		t.Errorf("adapter_version = %q", events[0].Provenance.AdapterVersion)
	}
}

func TestProxyImportUnrecognized(t *testing.T) {
	store := newTempStore(t)
	h := &fixtureHandler{
		initResult: proto.InitializeResult{ABIVersion: proto.ABIVersion, PluginName: "stub"},
		importFn: func(_ proto.ImportParams) (proto.ImportResult, error) {
			return proto.ImportResult{}, &proto.RPCError{Code: proto.CodeUnrecognizedNativeFile, Message: "nope"}
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, err := Open(context.Background(), transport, store, "h", "0")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Import(context.Background(), store, "/no/such")
	if err == nil {
		t.Fatal("expected error")
	}
	if !proto.IsUnrecognized(err) {
		t.Errorf("expected IsUnrecognized, got %v", err)
	}
}

func TestProxyExportMemory(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "CLAUDE.md")
	os.WriteFile(sourcePath, []byte("hello"), 0o644)

	// Seed the store via reconciler so we have a real artifact.
	rec := Reconciler{Store: store, DeviceID: "h", SourceAgent: "claude-code", AdapterVersion: "0.2.0"}
	payload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "hello"})
	id, err := rec.Apply(context.Background(), proto.ImportedItem{
		Kind: acf.KindMemory, Scope: acf.ScopeProject,
		Name: "CLAUDE.md", SourcePath: sourcePath,
		Payload: json.RawMessage(payload),
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	var gotParams proto.ExportParams
	h := &fixtureHandler{
		initResult: proto.InitializeResult{ABIVersion: proto.ABIVersion, PluginName: "claude-code", PluginVersion: "0.2.0"},
		exportFn: func(p proto.ExportParams) (proto.ExportResult, error) {
			gotParams = p
			return proto.ExportResult{Written: true}, nil
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, err := Open(context.Background(), transport, store, "h", "0")
	if err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(tmp, "out.md")
	if err := p.Export(context.Background(), store, id, destPath); err != nil {
		t.Fatalf("export: %v", err)
	}
	if gotParams.DestPath != destPath {
		t.Errorf("dest_path = %q", gotParams.DestPath)
	}
	var pld acf.MemoryPayload
	if err := json.Unmarshal(gotParams.Payload, &pld); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if pld.Content != "hello" {
		t.Errorf("plugin received content %q, want 'hello'", pld.Content)
	}
}

func TestProxyImportPropagatesDeviceID(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "CLAUDE.md")
	os.WriteFile(sourcePath, []byte("hi"), 0o644)

	payload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "hi"})

	h := &fixtureHandler{
		initResult: proto.InitializeResult{ABIVersion: proto.ABIVersion, PluginName: "stub", PluginVersion: "0.1.0"},
		importFn: func(p proto.ImportParams) (proto.ImportResult, error) {
			return proto.ImportResult{
				Imports: []proto.ImportedItem{{
					Kind: acf.KindMemory, Scope: acf.ScopeProject,
					Name: "CLAUDE.md", SourcePath: sourcePath,
					Payload: json.RawMessage(payload),
				}},
			}, nil
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, err := Open(context.Background(), transport, store, "device-xyz", "0.26.0")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := p.Import(context.Background(), store, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	events, _ := store.ReadEvents(acf.KindMemory, ids[0])
	if events[0].Provenance.DeviceID != "device-xyz" {
		t.Errorf("DeviceID = %q, want device-xyz", events[0].Provenance.DeviceID)
	}
}

func TestProxyExportTombstonedReturnsSentinel(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "x.md")
	os.WriteFile(sourcePath, []byte("hi"), 0o644)

	rec := Reconciler{Store: store, DeviceID: "h", SourceAgent: "a", AdapterVersion: "0"}
	payload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "hi"})
	id, _ := rec.Apply(context.Background(), proto.ImportedItem{
		Kind: acf.KindMemory, Scope: acf.ScopeProject,
		Name: "x.md", SourcePath: sourcePath, Payload: json.RawMessage(payload),
	}, "")
	art, _ := store.ReadArtifact(acf.KindMemory, id)
	art.Tombstoned = true
	store.WriteArtifact(art)

	h := &fixtureHandler{initResult: proto.InitializeResult{ABIVersion: proto.ABIVersion, PluginName: "a"}}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, _ := Open(context.Background(), transport, store, "h", "0")
	err := p.Export(context.Background(), store, id, filepath.Join(tmp, "out.md"))
	if !errors.Is(err, adapter.ErrArtifactTombstoned) {
		t.Errorf("got %v, want ErrArtifactTombstoned", err)
	}
}

// TestProxy_Capabilities_RoundTripsFromPlugin verifies the v0.94.0
// proto.MethodCapabilities RPC: the proxy's Capabilities() method
// calls the plugin and converts the result to adapter.Capabilities.
func TestProxy_Capabilities_RoundTripsFromPlugin(t *testing.T) {
	store := newTempStore(t)
	h := &fixtureHandler{
		initResult: proto.InitializeResult{
			ABIVersion: proto.ABIVersion, PluginName: "test-plugin",
		},
		capsResult: &proto.CapabilitiesResult{
			Name:     "test-plugin",
			Surfaces: []string{"cli", "desktop"},
			Artifacts: proto.ArtifactSupport{
				Memory: true, Skill: true, Tool: false, Conversation: false,
			},
			Tools:           []string{"mcp-server", "hook"},
			NativeBasenames: []string{"FOO.md", "BAR.md"},
			BasenameToKind:  nil,
			NotesURL:        "docs/adapters/test-plugin.md",
		},
	}
	transport, closer := startPlugin(t, h)
	defer closer()
	p, _ := Open(context.Background(), transport, store, "test-plugin", "0")

	caps := p.Capabilities()
	if caps.Name != "test-plugin" {
		t.Errorf("Capabilities.Name = %q; want test-plugin", caps.Name)
	}
	if caps.Artifacts.Tool {
		t.Errorf("Capabilities.Artifacts.Tool = true; want false (plugin declined tool kind)")
	}
	if len(caps.Surfaces) != 2 || caps.Surfaces[0] != adapter.SurfaceCLI || caps.Surfaces[1] != adapter.SurfaceDesktop {
		t.Errorf("Capabilities.Surfaces = %v; want [cli desktop]", caps.Surfaces)
	}
	if len(caps.Tools) != 2 {
		t.Errorf("Capabilities.Tools = %v; want 2 entries", caps.Tools)
	}
	if len(caps.NativeBasenames) != 2 {
		t.Errorf("Capabilities.NativeBasenames = %v; want 2 entries", caps.NativeBasenames)
	}
	if caps.NotesURL != "docs/adapters/test-plugin.md" {
		t.Errorf("Capabilities.NotesURL = %q; want docs/adapters/test-plugin.md", caps.NotesURL)
	}
}

// deadTransport accepts writes (so the request is "sent") but never
// produces a response: Read blocks until the transport is closed. It
// models a hung plugin that never replies on the wire.
type deadTransport struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newDeadTransport() *deadTransport {
	return &deadTransport{closed: make(chan struct{})}
}

func (d *deadTransport) Write(p []byte) (int, error) { return len(p), nil }

func (d *deadTransport) Read(p []byte) (int, error) {
	<-d.closed
	return 0, io.EOF
}

func (d *deadTransport) Close() error {
	d.closeOnce.Do(func() { close(d.closed) })
	return nil
}

// TestProxyCallHonorsContextCancellation asserts that a call whose plugin
// never replies returns when the context's deadline passes, rather than
// blocking forever on the response read. Before the fix, Proxy.call ignored
// ctx and read synchronously, so Import/Export against a hung plugin hung
// indefinitely; this test deadlocks (and trips the test timeout) pre-fix.
func TestProxyCallHonorsContextCancellation(t *testing.T) {
	store := newTempStore(t)
	transport := newDeadTransport()
	defer transport.Close()

	// Build a Proxy directly so Open's initialize handshake (which would
	// itself hang on the dead transport) is bypassed; we only exercise call.
	p := &Proxy{
		fr:       proto.NewFrameReader(transport),
		fw:       proto.NewFrameWriter(transport),
		closer:   transport,
		store:    store,
		deviceID: "h",
		manifest: proto.InitializeResult{PluginName: "stub", PluginVersion: "0"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := p.Import(ctx, store, "/no/such/path")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Import returned %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Import did not return after ctx deadline — call ignored ctx and blocked forever")
	}
}
