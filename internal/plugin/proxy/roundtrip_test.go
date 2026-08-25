package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

// memHandler is a minimal memory-only handler: it reads CLAUDE.md /
// MEMORY.md / AGENTS.md, treats them as markdown, and writes them back
// verbatim on export. Mirrors what a real memory plugin would do.
type memHandler struct{}

func (m *memHandler) Initialize(_ context.Context, _ proto.InitializeParams) (proto.InitializeResult, error) {
	return proto.InitializeResult{
		PluginName: "memstub", PluginVersion: "0.0.1", ABIVersion: proto.ABIVersion,
		Kinds:   []acf.Kind{acf.KindMemory},
		Formats: map[acf.Kind][]string{acf.KindMemory: {"markdown"}},
	}, nil
}
func (m *memHandler) Import(_ context.Context, p proto.ImportParams) (proto.ImportResult, error) {
	base := filepath.Base(p.NativePath)
	if base != "CLAUDE.md" && base != "MEMORY.md" && base != "AGENTS.md" {
		return proto.ImportResult{}, &proto.RPCError{Code: proto.CodeUnrecognizedNativeFile, Message: "not a memory file"}
	}
	content, err := os.ReadFile(p.NativePath)
	if err != nil {
		return proto.ImportResult{}, &proto.RPCError{Code: proto.CodeIOError, Message: err.Error()}
	}
	pld, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: string(content)})
	return proto.ImportResult{
		Imports: []proto.ImportedItem{{
			Kind: acf.KindMemory, Scope: acf.ScopeProject,
			Name: base, SourcePath: p.NativePath,
			Payload: json.RawMessage(pld),
		}},
	}, nil
}
func (m *memHandler) Export(_ context.Context, p proto.ExportParams) (proto.ExportResult, error) {
	var mp acf.MemoryPayload
	if err := json.Unmarshal(p.Payload, &mp); err != nil {
		return proto.ExportResult{}, &proto.RPCError{Code: proto.CodeFormatUnsupported, Message: err.Error()}
	}
	if err := os.WriteFile(p.DestPath, []byte(mp.Content), 0o644); err != nil {
		return proto.ExportResult{}, &proto.RPCError{Code: proto.CodeIOError, Message: err.Error()}
	}
	return proto.ExportResult{Written: true}, nil
}
func (m *memHandler) NativePath(_ context.Context, p proto.NativePathParams) (proto.NativePathResult, error) {
	if p.Artifact.Kind != acf.KindMemory {
		return proto.NativePathResult{}, nil
	}
	return proto.NativePathResult{Path: filepath.Join(p.ContextDir, p.Artifact.Name), Supports: true}, nil
}
func (m *memHandler) HandlesFormat(_ context.Context, p proto.HandlesFormatParams) (proto.HandlesFormatResult, error) {
	return proto.HandlesFormatResult{Handles: p.Kind == acf.KindMemory && p.Format == "markdown"}, nil
}
func (m *memHandler) Capabilities(_ context.Context, _ proto.CapabilitiesParams) (proto.CapabilitiesResult, error) {
	return proto.CapabilitiesResult{
		Name: "memhandler",
		Artifacts: proto.ArtifactSupport{
			Memory: true, Skill: false, Tool: false, Conversation: false,
		},
	}, nil
}
func (m *memHandler) Shutdown(_ context.Context, _ proto.ShutdownParams) (proto.ShutdownResult, error) {
	return proto.ShutdownResult{}, nil
}

func TestEndToEndImportExport(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	src := filepath.Join(tmp, "CLAUDE.md")
	if err := os.WriteFile(src, []byte("memory contents v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	transport, closer := startPlugin(t, &memHandler{})
	defer closer()
	p, err := Open(context.Background(), transport, store, "host-1", "0.26.0")
	if err != nil {
		t.Fatal(err)
	}

	// Import.
	ids, err := p.Import(context.Background(), store, src)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids: %v", ids)
	}

	// Export to a different path; content should match.
	dest := filepath.Join(tmp, "exported.md")
	if err := p.Export(context.Background(), store, ids[0], dest); err != nil {
		t.Fatalf("export: %v", err)
	}
	out, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "memory contents v1\n" {
		t.Errorf("round-trip content mismatch: got %q", out)
	}

	// Re-import: should produce an UPDATE event under the same ID.
	if err := os.WriteFile(src, []byte("memory contents v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids2, err := p.Import(context.Background(), store, src)
	if err != nil {
		t.Fatal(err)
	}
	if ids2[0] != ids[0] {
		t.Errorf("re-import minted new ID: %s -> %s", ids[0], ids2[0])
	}
	events, _ := store.ReadEvents(acf.KindMemory, ids[0])
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[1].Type != acf.EventTypeUpdate {
		t.Errorf("second event type = %s, want update", events[1].Type)
	}
}
