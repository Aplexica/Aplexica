package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	storeDir := filepath.Join(tmp, "store")
	s := &acf.Store{Root: storeDir}
	require.NoError(t, s.Init())

	project := filepath.Join(tmp, "myproj")
	// v0.61.0: BRD-02 §4.13.5 downgrades non-VCS paths to ScopeGlobal,
	// so a test that wants ScopeProject must stage an actual VCS marker.
	require.NoError(t, os.MkdirAll(filepath.Join(project, ".git"), 0o755))
	writeFile(t, filepath.Join(project, "CLAUDE.md"), "# Project Memory\n\nDo X.\n")

	a := New()
	ids, err := a.Import(context.Background(), s, filepath.Join(project, "CLAUDE.md"))
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, got.Kind)
	require.Equal(t, acf.ScopeProject, got.Scope)
	require.Equal(t, "CLAUDE.md", got.Name)
	require.NotEmpty(t, got.HeadEventHash)

	events, err := s.ReadEvents(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	decodedPayload, err := acf.DecodeMemoryPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "# Project Memory\n\nDo X.\n", decodedPayload.Content)
	require.Equal(t, "markdown", decodedPayload.Format)
	require.Equal(t, "claude-code", events[0].Provenance.SourceAgent)
	require.NoError(t, acf.VerifyChain(events))
}

func TestImportMemory_GlobalScope(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	fake := filepath.Join(tmp, ".claude", "CLAUDE.md")
	writeFile(t, fake, "# Global\n")

	a := &Adapter{HomeDir: tmp} // override home for the test
	ids, err := a.Import(context.Background(), s, fake)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindMemory, ids[0])
	require.Equal(t, acf.ScopeGlobal, got.Scope)
}

func TestExportMemory_WritesBytesIdenticalToImport(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "in", "CLAUDE.md")
	original := "# Source\nLine 1\nLine 2\n"
	writeFile(t, src, original)

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	dest := filepath.Join(tmp, "out", "CLAUDE.md")
	require.NoError(t, a.Export(context.Background(), s, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, original, string(got),
		"round-trip MUST preserve bytes exactly; otherwise the hub-format fidelity contract is broken")
}

func TestExportMemory_ReplaysUpdateEvents(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "CLAUDE.md")
	writeFile(t, src, "v1\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	artifactID := ids[0]

	// Manually append an "update" event with new content.
	head, err := s.HeadHash(acf.KindMemory, artifactID)
	require.NoError(t, err)
	updatePayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v2\n"})
	require.NoError(t, err)
	updated := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       "update",
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: a.DeviceID, SourceAgent: a.Name(), AdapterVersion: a.Version()},
		Payload:    updatePayload,
		ParentHash: head,
	}
	require.NoError(t, s.AppendEvent(acf.KindMemory, updated))

	dest := filepath.Join(tmp, "out", "CLAUDE.md")
	require.NoError(t, a.Export(context.Background(), s, artifactID, dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "v2\n", string(got), "export must use the latest event payload, not the first")
}
