package openclaw

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectDirs_ReadsSessionHeaders(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte(`{"type":"session","id":"old","cwd":"/Users/testuser/repo","timestamp":"2026-06-02T01:00:00Z"}`+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.jsonl"), []byte(`{"type":"session","id":"new","cwd":"/Users/testuser/repo","timestamp":"2026-06-02T05:00:00Z"}`+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.jsonl"), []byte(`{"type":"session","id":"other","cwd":"/Users/testuser/other","timestamp":"2026-06-02T02:00:00Z"}`+"\n"), 0o644))

	got, err := (&Adapter{HomeDir: home}).ProjectDirs()
	require.NoError(t, err)
	m := map[string]string{}
	for _, p := range got {
		m[p.Path] = p.LastActive.UTC().Format("15:04")
	}
	require.Equal(t, "05:00", m["/Users/testuser/repo"], "newest session wins per cwd")
	require.Equal(t, "02:00", m["/Users/testuser/other"])
	require.Len(t, got, 2)
}

// TestProjectDirs_DoesNotDescendIntoBackendTree pins the Discover() boundary:
// ProjectDirs must only enumerate transcripts directly inside
// agents/<id>/sessions/ — never the swappable backend's rollout tree under
// agents/<id>/agent/codex-home/sessions/ (which "must NOT import as openclaw
// conversations"), and never the adapter's own materialized imports (whose
// synthetic ~/.openclaw/workspace cwd would otherwise self-harvest as a user
// project).
func TestProjectDirs_DoesNotDescendIntoBackendTree(t *testing.T) {
	home := t.TempDir()

	// A legitimate user session directly under agents/<id>/sessions/.
	sessions := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "real.jsonl"),
		[]byte(`{"type":"session","id":"real","cwd":"/Users/testuser/repo","timestamp":"2026-06-02T05:00:00Z"}`+"\n"), 0o644))

	// A backend-internal rollout transcript with a top-level cwd. Discover()
	// refuses to advertise this tree because the backend is swappable; it must
	// not leak as a project dir.
	backend := filepath.Join(home, ".openclaw", "agents", "main", "agent", "codex-home", "sessions")
	require.NoError(t, os.MkdirAll(backend, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(backend, "rollout.jsonl"),
		[]byte(`{"type":"session","id":"rollout","cwd":"/Users/testuser/backend-leak","timestamp":"2026-06-02T06:00:00Z"}`+"\n"), 0o644))

	// The adapter's own materialized import, living in sessions/ but carrying
	// the _aplexica marker and the synthetic workspace cwd. It must be skipped.
	workspace := filepath.Join(home, ".openclaw", "workspace")
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "imported.jsonl"),
		[]byte(`{"type":"session","id":"imported","cwd":"`+workspace+`","_aplexica":"canonical-import","timestamp":"2026-06-02T07:00:00Z"}`+"\n"), 0o644))

	got, err := (&Adapter{HomeDir: home}).ProjectDirs()
	require.NoError(t, err)

	paths := map[string]bool{}
	for _, p := range got {
		paths[p.Path] = true
	}
	require.True(t, paths["/Users/testuser/repo"], "real user session must be harvested")
	require.False(t, paths["/Users/testuser/backend-leak"], "backend-internal rollout cwd must NOT be harvested")
	require.False(t, paths[workspace], "materialized-import synthetic workspace cwd must NOT be harvested")
	require.Len(t, got, 1)
}
