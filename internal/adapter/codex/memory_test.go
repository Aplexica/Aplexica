package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestImportMemory_WritesArtifactAndEvent(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	project := filepath.Join(tmp, "myproj")
	// v0.61.0: BRD-02 §4.13.5 downgrades non-VCS paths to ScopeGlobal,
	// so a test that wants ScopeProject must stage an actual VCS marker.
	require.NoError(t, os.MkdirAll(filepath.Join(project, ".git"), 0o755))
	writeFile(t, filepath.Join(project, "AGENTS.md"), "# Project AGENTS\n\nUse X.\n")

	a := New()
	ids, err := a.ImportMemory(context.Background(), s, filepath.Join(project, "AGENTS.md"))
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, got.Kind)
	require.Equal(t, acf.ScopeProject, got.Scope)
	require.Equal(t, "AGENTS.md", got.Name)
	require.NotEmpty(t, got.HeadEventHash)

	events, err := s.ReadEvents(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, "codex", events[0].Provenance.SourceAgent)
	require.NoError(t, acf.VerifyChain(events))

	payload, err := acf.DecodeMemoryPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "markdown", payload.Format)
	require.Equal(t, "# Project AGENTS\n\nUse X.\n", payload.Content)
}

func TestImportMemory_GlobalScope(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	fake := filepath.Join(tmp, ".codex", "AGENTS.md")
	writeFile(t, fake, "# Global Codex\n")

	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}
	ids, err := a.ImportMemory(context.Background(), s, fake)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindMemory, ids[0])
	require.Equal(t, acf.ScopeGlobal, got.Scope)
}

func TestExportMemory_WritesBytesIdenticalToImport(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "in", "AGENTS.md")
	original := "# Agent Instructions\n\nDo X.\nDo Y.\n"
	writeFile(t, src, original)

	a := New()
	ids, err := a.ImportMemory(context.Background(), s, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "AGENTS.md")
	require.NoError(t, a.ExportMemory(context.Background(), s, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, original, string(got),
		"AGENTS.md round-trip MUST be byte-identical; this is what proves ACF generalizes across adapters")
}
