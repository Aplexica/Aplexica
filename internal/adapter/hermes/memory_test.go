package hermes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip_Memory_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"plain", "Hello world.\n"},
		{"empty", ""},
		{"unicode", "héllo — wörld ✓ 🎉\n"},
		{"multiline", "# Heading\n\nPara.\n\n- a\n- b\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "MEMORY.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.ImportMemory(context.Background(), s, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "MEMORY.md")
			require.NoError(t, a.ExportMemory(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got),
				"hermes memory round-trip MUST be byte-identical")
		})
	}
}

// TestExportMemory_ProjectScopeUpsertsCentralSection: project-scoped memory
// must land in hermes' CENTRAL memory file as a delimited section (hermes
// never reads project folders), leaving the global content intact — and the
// import side must strip it so the global artifact stays pristine.
func TestExportMemory_ProjectScopeUpsertsCentralSection(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "AGENTS.md",
		Project:          &project.ProjectInfo{ID: "local:abc:demo", Path: "/p/demo"},
	}
	require.NoError(t, store.WriteArtifact(art))
	payload, err := memoryEncode([]byte("- demo project rule\n"))
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: art.ArtifactID,
		Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(),
		Provenance: acf.Provenance{SourceAgent: "codex"},
		Payload:    payload,
	}))

	dest := filepath.Join(tmp, "memories", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	require.NoError(t, os.WriteFile(dest, []byte("# central\n- global fact\n"), 0o644))

	a := newTestAdapter(t)
	require.NoError(t, a.ExportMemory(context.Background(), store, art.ArtifactID, dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	s := string(got)
	require.Contains(t, s, "- global fact", "global content preserved")
	require.Contains(t, s, "## Project: demo — AGENTS.md")
	require.Contains(t, s, "- demo project rule")

	// Round-trip: importing the composed file must NOT absorb the section.
	enc, err := memoryEncode(got)
	require.NoError(t, err)
	require.NotContains(t, string(enc), "demo project rule")
	require.Contains(t, string(enc), "global fact")
}
