// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/host"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/proxy"
)

// rwPair returns a pair (a, b) of io.ReadWriters such that everything
// written to a is read from b and vice versa. Mirrors the helper in
// internal/plugin/proxy/proxy_test.go (unexported there, replicated here).
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

// startPlugin spawns a host.Serve goroutine wired to the real example
// Handler and returns the client-side transport (what the proxy uses) plus
// a cleanup func. This exercises the plugin in-process over an in-memory
// transport — the daemon-side proxy.Open then drives it exactly as the
// out-of-process manager would, minus the exec boundary.
func startPlugin(t *testing.T) (io.ReadWriter, func()) {
	t.Helper()
	clientRW, serverRW, closer := rwPair()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = host.Serve(context.Background(), &Handler{}, serverRW, serverRW)
	}()
	return clientRW, func() {
		closer()
		wg.Wait()
	}
}

// newTempStore builds a fresh canonical store rooted in a temp dir.
// Mirrors the helper in internal/plugin/proxy/reconcile_test.go.
func newTempStore(t *testing.T) *acf.Store {
	t.Helper()
	dir := t.TempDir()
	s := &acf.Store{Root: filepath.Join(dir, "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	return s
}

// TestRoundtripImportExport proves the example plugin end-to-end against
// the real daemon-side proxy + reconciler:
//
//  1. Import MEMORY.md -> exactly one artifact ID and a daemon-minted
//     create event (proving the plugin is a pure translator and the
//     daemon-side Reconciler persisted the result).
//  2. Export to a fresh path -> byte-identical content.
//  3. Re-import a changed file -> SAME artifact ID + an update event.
func TestRoundtripImportExport(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, nativeBasename)
	const v1 = "example memory v1\n"
	if err := os.WriteFile(src, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	transport, closer := startPlugin(t)
	defer closer()

	p, err := proxy.Open(context.Background(), transport, store, "device-1", "0.26.0")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if p.Name() != pluginName {
		t.Fatalf("Name = %q, want %q", p.Name(), pluginName)
	}
	if p.Version() != pluginVersion {
		t.Fatalf("Version = %q, want %q", p.Version(), pluginVersion)
	}

	// 1. Import — daemon-side reconcile must mint one artifact + create event.
	ids, err := p.Import(context.Background(), store, src)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(ids) != 1 || ids[0] == "" {
		t.Fatalf("import ids = %v, want exactly one non-empty id", ids)
	}

	art, err := store.ReadArtifact(acf.KindMemory, ids[0])
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if art.SourcePath != src {
		t.Errorf("artifact source_path = %q, want %q", art.SourcePath, src)
	}
	if art.Scope != acf.ScopeGlobal {
		t.Errorf("artifact scope = %q, want %q", art.Scope, acf.ScopeGlobal)
	}

	events, err := store.ReadEvents(acf.KindMemory, ids[0])
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event after import, got %d", len(events))
	}
	if events[0].Type != acf.EventTypeCreate {
		t.Errorf("first event type = %s, want create", events[0].Type)
	}
	if events[0].Provenance.SourceAgent != pluginName {
		t.Errorf("provenance source_agent = %q, want %q", events[0].Provenance.SourceAgent, pluginName)
	}

	// 2. Export to a fresh path; content must be byte-identical.
	dest := filepath.Join(tmp, "exported.md")
	if err := p.Export(context.Background(), store, ids[0], dest); err != nil {
		t.Fatalf("export: %v", err)
	}
	out, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != v1 {
		t.Errorf("exported content = %q, want %q", out, v1)
	}

	// 3. Re-import a changed file — same ID, an update event appended.
	const v2 = "example memory v2\n"
	if err := os.WriteFile(src, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	ids2, err := p.Import(context.Background(), store, src)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if len(ids2) != 1 || ids2[0] != ids[0] {
		t.Fatalf("re-import minted new ID: %v -> %v", ids, ids2)
	}
	events2, err := store.ReadEvents(acf.KindMemory, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 2 {
		t.Fatalf("want 2 events after re-import, got %d", len(events2))
	}
	if events2[1].Type != acf.EventTypeUpdate {
		t.Errorf("second event type = %s, want update", events2[1].Type)
	}
}

// TestImportUnrecognizedFallsThrough verifies a non-MEMORY.md file is
// reported as CodeUnrecognizedNativeFile so the sync orchestrator falls
// through to the next adapter rather than treating the file as ours.
func TestImportUnrecognizedFallsThrough(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	other := filepath.Join(tmp, "NOTES.txt")
	if err := os.WriteFile(other, []byte("not a memory file"), 0o644); err != nil {
		t.Fatal(err)
	}

	transport, closer := startPlugin(t)
	defer closer()
	p, err := proxy.Open(context.Background(), transport, store, "device-1", "0")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	_, err = p.Import(context.Background(), store, other)
	if err == nil {
		t.Fatal("expected error for unrecognized native file")
	}
	if !proto.IsUnrecognized(err) {
		t.Errorf("expected CodeUnrecognizedNativeFile, got %v", err)
	}
}

// TestInitializeAndCapabilities checks the declared identity and the
// memory-only capability surface the orchestrator routes on.
func TestInitializeAndCapabilities(t *testing.T) {
	transport, closer := startPlugin(t)
	defer closer()
	p, err := proxy.Open(context.Background(), transport, newTempStore(t), "device-1", "0")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	caps := p.Capabilities()
	if caps.Name != pluginName {
		t.Errorf("caps.Name = %q, want %q", caps.Name, pluginName)
	}
	if !caps.Artifacts.Memory {
		t.Error("caps.Artifacts.Memory = false, want true")
	}
	if caps.Artifacts.Skill || caps.Artifacts.Tool || caps.Artifacts.Conversation {
		t.Errorf("caps claims non-memory kinds: %+v", caps.Artifacts)
	}
	if len(caps.NativeBasenames) != 1 || caps.NativeBasenames[0] != nativeBasename {
		t.Errorf("caps.NativeBasenames = %v, want [%q]", caps.NativeBasenames, nativeBasename)
	}
	if got := caps.BasenameToKind[nativeBasename]; got != acf.KindMemory {
		t.Errorf("BasenameToKind[%q] = %q, want %q", nativeBasename, got, acf.KindMemory)
	}

	// HandlesFormat: only memory/markdown.
	if !p.HandlesFormat(acf.KindMemory, memoryFormat) {
		t.Error("HandlesFormat(memory, markdown) = false, want true")
	}
	if p.HandlesFormat(acf.KindSkill, memoryFormat) {
		t.Error("HandlesFormat(skill, markdown) = true, want false")
	}
	if p.HandlesFormat(acf.KindMemory, "json") {
		t.Error("HandlesFormat(memory, json) = true, want false")
	}

	// NativePath for a memory artifact.
	path, supports, err := p.NativePath(acf.Artifact{Kind: acf.KindMemory, Name: nativeBasename}, "/ctx")
	if err != nil {
		t.Fatal(err)
	}
	if !supports || path != filepath.Join("/ctx", nativeBasename) {
		t.Errorf("NativePath = (%q, %v)", path, supports)
	}
	// NativePath declines non-memory kinds.
	_, supports, err = p.NativePath(acf.Artifact{Kind: acf.KindSkill, Name: "SKILL.md"}, "/ctx")
	if err != nil {
		t.Fatal(err)
	}
	if supports {
		t.Error("NativePath(skill) supports = true, want false")
	}
}
