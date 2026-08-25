package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// Proxy is the daemon-side handle to one plugin. Implements
// adapter.Adapter.
type Proxy struct {
	mu     sync.Mutex
	fr     *proto.FrameReader
	fw     *proto.FrameWriter
	closer io.Closer
	nextID atomic.Int64

	manifest proto.InitializeResult
	store    *acf.Store
	deviceID string
}

// Open performs the initialize handshake and returns a Proxy ready for
// adapter calls. transport carries JSON-RPC frames; the daemon retains
// ownership (no close on Proxy.Close unless transport is also an
// io.Closer).
//
// store is the canonical store the Proxy will write to on Import (and
// read from on Export). Nil is allowed for tests that only exercise
// non-store-touching methods.
func Open(ctx context.Context, transport io.ReadWriter, store *acf.Store, deviceID, daemonVersion string) (*Proxy, error) {
	p := &Proxy{
		fr:       proto.NewFrameReader(transport),
		fw:       proto.NewFrameWriter(transport),
		store:    store,
		deviceID: deviceID,
	}
	if c, ok := transport.(io.Closer); ok {
		p.closer = c
	}
	var result proto.InitializeResult
	if err := p.call(ctx, proto.MethodInitialize, proto.InitializeParams{
		ABIVersion: proto.ABIVersion, DaemonVersion: daemonVersion, DeviceID: deviceID,
	}, &result); err != nil {
		return nil, fmt.Errorf("plugin/proxy: initialize: %w", err)
	}
	if result.ABIVersion != proto.ABIVersion {
		return nil, fmt.Errorf("plugin/proxy: abi_version mismatch — plugin %q, daemon %q", result.ABIVersion, proto.ABIVersion)
	}
	p.manifest = result
	return p, nil
}

// Name returns the plugin's name from its manifest.
func (p *Proxy) Name() string { return p.manifest.PluginName }

// Version returns the plugin's semver from its manifest.
func (p *Proxy) Version() string { return p.manifest.PluginVersion }

// Discover implements adapter.Adapter (FR-03.3). A loaded adapter plugin is
// by definition present on the machine — the user installed and configured
// it — so it reports Installed=true. The plugin protocol does not yet expose
// a `discover` method for native global-root enumeration, so GlobalRoots is
// empty: the daemon does not auto-watch plugin native homes in V1 (a plugin
// declares its paths via NativePath at fan-out time instead). Adding a
// protocol-level discover RPC is a follow-up tracked against the plugin
// protocol spec.
func (p *Proxy) Discover() (adapter.Discovery, error) {
	return adapter.Discovery{
		Installed: true,
		Detail:    "adapter plugin loaded (" + p.manifest.PluginName + ")",
	}, nil
}

// call performs one JSON-RPC round-trip. params is encoded; result is
// decoded into out (any non-nil pointer). Concurrent calls are
// serialized via p.mu because RPC pipelining is not required and
// serializing simplifies the framing.
func (p *Proxy) call(ctx context.Context, method string, params any, out any) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	id := p.nextID.Add(1)
	idJSON, _ := json.Marshal(id)

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode params: %w", err)
	}
	req := proto.Request{JSONRPC: "2.0", ID: idJSON, Method: method, Params: paramsJSON}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	if err := p.fw.Write(reqBytes); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	// Read the response off the goroutine so the round-trip can be
	// abandoned when ctx is cancelled/times out — a hung plugin that
	// never replies must not block Import/Export forever. The channel is
	// buffered (cap 1) so the reader can always deposit its result and
	// exit even after this call has returned on the ctx path, never
	// leaking the goroutine.
	type readResult struct {
		frame []byte
		err   error
	}
	resCh := make(chan readResult, 1)
	go func() {
		frame, err := p.fr.Read()
		resCh <- readResult{frame: frame, err: err}
	}()

	select {
	case <-ctx.Done():
		// The reply (if it ever comes) would land mid-stream and desync
		// framing for the next call, so the transport is spent. Close it
		// to unblock the orphaned reader and surface a terminal error to
		// any concurrent callers, matching RemoteProxy's transport-closed
		// handling.
		if p.closer != nil {
			_ = p.closer.Close()
		}
		return ctx.Err()
	case res := <-resCh:
		if res.err != nil {
			return fmt.Errorf("read response: %w", res.err)
		}
		var resp proto.Response
		if err := json.Unmarshal(res.frame, &resp); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if resp.Error != nil {
			return &proto.RPCError{Code: resp.Error.Code, Message: resp.Error.Message}
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
		}
		return nil
	}
}

// Capabilities implements adapter.Adapter for plugin proxies. v0.94.0
// adds the Capabilities RPC to the plugin protocol; the proxy calls
// it and converts the result to adapter.Capabilities.
//
// Plugins built against pre-v0.94.0 ABIs may return MethodNotFound;
// in that case the proxy falls back to the conservative default
// shape (everything claimed, no tool kinds declared) to preserve
// backwards compatibility.
func (p *Proxy) Capabilities() adapter.Capabilities {
	var result proto.CapabilitiesResult
	if err := p.call(context.Background(), proto.MethodCapabilities, proto.CapabilitiesParams{}, &result); err != nil {
		// Pre-v0.94.0 plugin or transport error — conservative
		// fallback so the orchestrator's dispatch keeps working.
		return adapter.Capabilities{
			Name: p.Name(),
			Artifacts: adapter.ArtifactSupport{
				Memory: true, Skill: true, Tool: true, Conversation: true,
			},
			Tools:           nil,
			NativeBasenames: nil,
			NotesURL:        "",
		}
	}
	tools := make([]adapter.ToolKind, 0, len(result.Tools))
	for _, t := range result.Tools {
		tools = append(tools, adapter.ToolKind(t))
	}
	surfaces := make([]adapter.Surface, 0, len(result.Surfaces))
	for _, surface := range result.Surfaces {
		surfaces = append(surfaces, adapter.Surface(surface))
	}
	return adapter.Capabilities{
		Name:     result.Name,
		Surfaces: surfaces,
		Artifacts: adapter.ArtifactSupport{
			Memory:       result.Artifacts.Memory,
			Skill:        result.Artifacts.Skill,
			Tool:         result.Artifacts.Tool,
			Conversation: result.Artifacts.Conversation,
		},
		Tools:           tools,
		NativeBasenames: result.NativeBasenames,
		BasenameToKind:  result.BasenameToKind,
		NotesURL:        result.NotesURL,
	}
}

// HandlesFormat implements adapter.Adapter. Errors are swallowed —
// the orchestrator treats any negative result as "doesn't handle"
// per the existing in-binary behavior.
func (p *Proxy) HandlesFormat(kind acf.Kind, format string) bool {
	var result proto.HandlesFormatResult
	err := p.call(context.Background(), proto.MethodHandlesFormat, proto.HandlesFormatParams{Kind: kind, Format: format}, &result)
	if err != nil {
		return false
	}
	return result.Handles
}

// NativePath implements adapter.Adapter.
func (p *Proxy) NativePath(artifact acf.Artifact, contextDir string) (string, bool, error) {
	var result proto.NativePathResult
	if err := p.call(context.Background(), proto.MethodNativePath, proto.NativePathParams{
		Artifact: artifact, ContextDir: contextDir,
	}, &result); err != nil {
		return "", false, err
	}
	return result.Path, result.Supports, nil
}

// Import implements adapter.Adapter. It performs the import RPC,
// then runs identity reconciliation on each ImportedItem and writes
// the resulting artifact + event to store. Secrets are written to
// the daemon's secrets store. A Proxy with a nil secrets store ignores
// returned secrets.
func (p *Proxy) Import(ctx context.Context, store *acf.Store, nativePath string) ([]string, error) {
	if store == nil {
		store = p.store
	}
	var result proto.ImportResult
	err := p.call(ctx, proto.MethodImport, proto.ImportParams{
		NativePath: nativePath,
		ContextDir: filepath.Dir(nativePath),
		CausedBy:   adapter.CausedByFromContext(ctx),
	}, &result)
	if err != nil {
		return nil, err
	}
	rec := Reconciler{
		Store:          store,
		DeviceID:       p.deviceID,
		SourceAgent:    p.manifest.PluginName,
		AdapterVersion: p.manifest.PluginVersion,
	}
	ids := make([]string, 0, len(result.Imports))
	for _, item := range result.Imports {
		id, aerr := rec.Apply(ctx, item, adapter.CausedByFromContext(ctx))
		if aerr != nil {
			return ids, aerr
		}
		ids = append(ids, id)
	}
	// Secrets are ignored when no secrets store is wired.
	_ = result.Secrets
	return ids, nil
}

// Export implements adapter.Adapter. The daemon reads the event chain,
// verifies it, replays it to a current payload, then asks the plugin
// to render that payload at destPath.
func (p *Proxy) Export(ctx context.Context, store *acf.Store, artifactID, destPath string) error {
	if store == nil {
		store = p.store
	}
	// Try every kind — same approach as sync.Orchestrator.findArtifact.
	var (
		art   acf.Artifact
		found bool
	)
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		a, err := store.ReadArtifact(kind, artifactID)
		if err == nil {
			art = a
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("plugin/proxy: artifact %s not found in any kind", artifactID)
	}
	if art.Tombstoned {
		return adapter.ErrArtifactTombstoned
	}
	events, err := store.ReadEvents(art.Kind, artifactID)
	if err != nil {
		return fmt.Errorf("plugin/proxy: read events: %w", err)
	}
	if err := acf.VerifyChain(events); err != nil {
		return fmt.Errorf("plugin/proxy: verify chain: %w", err)
	}
	payload, err := ReplayCurrentPayload(events)
	if err != nil {
		return err
	}
	var result proto.ExportResult
	return p.call(ctx, proto.MethodExport, proto.ExportParams{
		Artifact: art,
		Payload:  payload,
		DestPath: destPath,
	}, &result)
}

// Compile-time check: Proxy satisfies adapter.Adapter. If the adapter
// interface ever changes, this fails to build — the canary that tells
// us the plugin protocol needs a corresponding update.
var _ adapter.Adapter = (*Proxy)(nil)
