package watcher

import "testing"

func TestSkipWalkDir(t *testing.T) {
	skip := []string{"node_modules", ".git", ".hg", ".svn", ".jj"}
	for _, n := range skip {
		if !SkipWalkDir(n) {
			t.Errorf("SkipWalkDir(%q) = false, want true (should be pruned)", n)
		}
	}
	// Agent config dirs and ordinary names must NOT be pruned — an agent's own
	// .kilo/.codex hold rules/config that adapters read via native-root watching.
	keep := []string{"src", ".kilo", ".kilocode", ".codex", ".claude", "AGENTS.md", "", "node_modules2", "git", "config"}
	for _, n := range keep {
		if SkipWalkDir(n) {
			t.Errorf("SkipWalkDir(%q) = true, want false (should be walked)", n)
		}
	}
}
