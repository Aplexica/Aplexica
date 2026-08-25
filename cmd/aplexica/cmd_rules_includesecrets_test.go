package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRules_TestSurfacesIncludeSecrets is the regression test for FR-05.6
// explainability: the `rules test` human output must surface the resolved
// route.includeSecrets so a user debugging a tool-secrets rule can see the
// value the evaluator computed (it is advisory/explainability-only; secret
// gating is enforced in the secrets layer, but the decision must be visible).
func TestRules_TestSurfacesIncludeSecrets(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	userRules := filepath.Join(tmp, "rules.toml")

	id := seedMemoryArtifactWithTwoSources(t, storeRoot, "claude-code", "codex")

	// A rule that resolves route.includeSecrets = true for every artifact.
	require.NoError(t, os.WriteFile(userRules, []byte(`
[[sync.rules]]
name = "secrets-on"
match.kind = "any"
route.includeSecrets = true
mode = "live"
`), 0o644))

	out, err := runRulesCmd(t, "test", id, "--rules-file", userRules, "--store", storeRoot)
	require.NoError(t, err, "test output:\n%s", out)
	require.Contains(t, out, "includeSecrets: true",
		"`rules test` must surface the resolved includeSecrets value; out:\n%s", out)
}
