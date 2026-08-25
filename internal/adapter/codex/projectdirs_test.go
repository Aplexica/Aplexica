package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectDirs_ReadsSessionCwds(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "06", "02")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	meta := func(cwd, ts string) string {
		return `{"type":"session_meta","payload":{"id":"x","cwd":"` + cwd + `","timestamp":"` + ts + `"}}` + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-a.jsonl"), []byte(meta("/Users/testuser", "2026-06-02T01:00:00Z")), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-b.jsonl"), []byte(meta("/Users/testuser", "2026-06-02T05:00:00Z")), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-c.jsonl"), []byte(meta("/Users/testuser/repo", "2026-06-02T02:00:00Z")), 0o644))

	got, err := (&Adapter{HomeDir: home}).ProjectDirs()
	require.NoError(t, err)
	m := map[string]string{}
	for _, p := range got {
		m[p.Path] = p.LastActive.UTC().Format("15:04")
	}
	require.Equal(t, "05:00", m["/Users/testuser"], "newest session wins")
	require.Equal(t, "02:00", m["/Users/testuser/repo"])
	require.Len(t, got, 2)
}

func TestProjectDirs_NormalizesDesktopWorktreeCwdsToOrigin(t *testing.T) {
	home := t.TempDir()
	origin := filepath.Join(home, "src", "project")
	worktree := makeCodexLinkedWorktree(t, home, origin, filepath.Join("thread", "project"))
	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "13")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	meta := func(cwd, ts string) []byte {
		raw, err := json.Marshal(map[string]any{
			"type": "session_meta",
			"payload": map[string]string{
				"id":        "x",
				"cwd":       cwd,
				"timestamp": ts,
			},
		})
		require.NoError(t, err)
		return append(raw, '\n')
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-origin.jsonl"), meta(origin, "2026-07-13T01:00:00Z"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-desktop.jsonl"), meta(worktree, "2026-07-13T05:00:00Z"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-nested.jsonl"), meta(filepath.Join(worktree, "pkg"), "2026-07-13T02:00:00Z"), 0o644))

	got, err := (&Adapter{HomeDir: home}).ProjectDirs()
	require.NoError(t, err)
	presences := make(map[string]string)
	for _, presence := range got {
		presences[presence.Path] = presence.LastActive.UTC().Format("15:04")
	}
	require.Equal(t, "05:00", presences[origin], "origin and worktree sessions should dedupe at the newest activity")
	require.Equal(t, "02:00", presences[filepath.Join(origin, "pkg")])
	require.Len(t, got, 2)
}
