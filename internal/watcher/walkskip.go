package watcher

// SkipWalkDir reports whether a directory with the given base name should be
// pruned when recursively walking a tree to watch or scan it.
//
// These are dependency caches and VCS-internal directories that never hold
// agent artifacts (memory / skill / tool / conversation files). Descending into
// them is pure wasted work: a single project's node_modules can be tens of
// thousands of files, each costing a watch registration or a stat + dispatch
// that no adapter will ever claim. Pruning them keeps recursive watching and
// the startup backfill scan cheap.
//
// The set is intentionally small and conservative — only directories that are
// unambiguously machine-managed. It does NOT include user dot-directories that
// might legitimately carry agent config (e.g. an agent's own .kilo/rules is
// handled by that adapter's native-root watching, not by project recursion).
func SkipWalkDir(name string) bool {
	switch name {
	case "node_modules", ".git", ".hg", ".svn", ".jj":
		return true
	}
	return false
}
