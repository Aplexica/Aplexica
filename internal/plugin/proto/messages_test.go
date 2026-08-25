package proto

import (
	"encoding/json"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
)

func mustRoundTrip(t *testing.T, in, out any) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

func TestInitializeRoundTrip(t *testing.T) {
	p := InitializeParams{ABIVersion: "1", DaemonVersion: "0.26.0", DeviceID: "host-1"}
	var got InitializeParams
	mustRoundTrip(t, p, &got)
	if got != p {
		t.Errorf("got %+v", got)
	}
}

func TestInitializeResultRoundTrip(t *testing.T) {
	r := InitializeResult{
		PluginName:    "claude-code",
		PluginVersion: "0.2.0",
		ABIVersion:    "1",
		Kinds:         []acf.Kind{acf.KindMemory},
		Formats:       map[acf.Kind][]string{acf.KindMemory: {"markdown"}},
	}
	var got InitializeResult
	mustRoundTrip(t, r, &got)
	if got.PluginName != r.PluginName || len(got.Kinds) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestImportRoundTrip(t *testing.T) {
	p := ImportParams{NativePath: "/abs/file.md", ContextDir: "/abs", CausedBy: "deadbeef"}
	var got ImportParams
	mustRoundTrip(t, p, &got)
	if got != p {
		t.Errorf("got %+v", got)
	}
}

func TestImportResultRoundTrip(t *testing.T) {
	r := ImportResult{
		Imports: []ImportedItem{{
			Kind:       acf.KindMemory,
			Scope:      acf.ScopeGlobal,
			Name:       "CLAUDE.md",
			SourcePath: "/abs/file.md",
			Payload:    json.RawMessage(`{"format":"markdown","content":"x"}`),
		}},
		Secrets: []NamedSecret{{Name: "API_KEY", Value: "abc"}},
	}
	var got ImportResult
	mustRoundTrip(t, r, &got)
	if len(got.Imports) != 1 || got.Imports[0].Name != "CLAUDE.md" {
		t.Errorf("imports round-trip mismatch: %+v", got)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Value != "abc" {
		t.Errorf("secrets round-trip mismatch: %+v", got)
	}
}

func TestExportRoundTrip(t *testing.T) {
	p := ExportParams{
		Artifact: acf.Artifact{ArtifactID: "01HX", Kind: acf.KindMemory, Name: "CLAUDE.md"},
		Payload:  json.RawMessage(`{"format":"markdown","content":"x"}`),
		DestPath: "/abs/out.md",
	}
	var got ExportParams
	mustRoundTrip(t, p, &got)
	if got.Artifact.ArtifactID != "01HX" || got.DestPath != "/abs/out.md" {
		t.Errorf("got %+v", got)
	}
}

func TestExportResultRoundTrip(t *testing.T) {
	r := ExportResult{Written: true}
	var got ExportResult
	mustRoundTrip(t, r, &got)
	if !got.Written {
		t.Errorf("got %+v", got)
	}
}

func TestNativePathRoundTrip(t *testing.T) {
	p := NativePathParams{Artifact: acf.Artifact{Kind: acf.KindMemory}, ContextDir: "/abs"}
	var got NativePathParams
	mustRoundTrip(t, p, &got)
	if got.ContextDir != "/abs" {
		t.Errorf("got %+v", got)
	}
}

func TestNativePathResultRoundTrip(t *testing.T) {
	r := NativePathResult{Path: "/abs/CLAUDE.md", Supports: true}
	var got NativePathResult
	mustRoundTrip(t, r, &got)
	if got.Path != "/abs/CLAUDE.md" || !got.Supports {
		t.Errorf("got %+v", got)
	}
}

func TestHandlesFormatRoundTrip(t *testing.T) {
	p := HandlesFormatParams{Kind: acf.KindMemory, Format: "markdown"}
	var got HandlesFormatParams
	mustRoundTrip(t, p, &got)
	if got != p {
		t.Errorf("got %+v", got)
	}
	r := HandlesFormatResult{Handles: true}
	var gotR HandlesFormatResult
	mustRoundTrip(t, r, &gotR)
	if !gotR.Handles {
		t.Errorf("got %+v", gotR)
	}
}

func TestShutdownRoundTrip(t *testing.T) {
	var p ShutdownParams
	var got ShutdownParams
	mustRoundTrip(t, p, &got)
	var r ShutdownResult
	var gotR ShutdownResult
	mustRoundTrip(t, r, &gotR)
}
