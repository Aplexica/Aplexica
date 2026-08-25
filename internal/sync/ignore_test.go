package syncd

import "testing"

func TestIgnoredNativePath(t *testing.T) {
	cases := map[string]bool{
		// SQLite databases + sidecars Codex churns continuously — ignored.
		"/Users/testuser/.codex/logs_2.sqlite":     true,
		"/Users/testuser/.codex/logs_2.sqlite-wal": true,
		"/Users/testuser/.codex/logs_2.sqlite-shm": true,
		"/Users/testuser/.codex/memories_1.sqlite": true,
		"/Users/testuser/.codex/state_5.sqlite":    true,
		"/Users/testuser/.codex/x.sqlite-journal":  true,
		"/Users/testuser/.codex/y.db":              true,
		"/Users/testuser/.codex/y.db-wal":          true,
		// The textual memory surface MUST still flow through import.
		"/Users/testuser/.codex/memories/personal.md": false,
		"/Users/testuser/.codex/AGENTS.md":            false,
		"/Users/testuser/.claude/CLAUDE.md":           false,
		"/Users/testuser/.codex/sessions/abc.jsonl":   false,
		"/Users/testuser/.hermes/state.db":            true, // hermeswatch imports directly; generic scans skip DBs
	}
	for path, want := range cases {
		if got := ignoredNativePath(path); got != want {
			t.Errorf("ignoredNativePath(%q) = %v, want %v", path, got, want)
		}
	}
}
