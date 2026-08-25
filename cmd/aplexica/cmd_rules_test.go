package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func runRulesCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Reset BEFORE Execute so flag-state leaks between sequential
	// calls (cobra retains parsed values on the global var).
	rulesPath = ""
	rulesStoreRoot = ""
	rulesJSON = false
	rulesApplyRetroactive = false
	rulesApplyRuleName = ""
	rulesApplyDryRun = false
	t.Cleanup(func() {
		rulesPath = ""
		rulesStoreRoot = ""
		rulesJSON = false
		rulesApplyRetroactive = false
		rulesApplyRuleName = ""
		rulesApplyDryRun = false
	})
	return runRoot(t, append([]string{"rules"}, args...)...)
}

// FR-05.11 / BRD-05 §4.2: a tag-assigning rule must not be able to inject
// reserved-namespace tags (aplexica:* / fork-of:* / device:* / conflict:*)
// — those are system-only and `rules add` is a user-facing tag-writing
// surface. Mirrors the cmd_tag enforcement.
func TestRules_AddRejectsReservedAssignTag(t *testing.T) {
	for _, reserved := range []string{"fork-of:HACK", "aplexica:internal", "device:xyz", "conflict:abc"} {
		tmp := t.TempDir()
		userRules := filepath.Join(tmp, "rules.toml")
		fragment := filepath.Join(tmp, "fragment.toml")
		require.NoError(t, os.WriteFile(fragment, []byte(`
[[sync.rules]]
name = "tagger"
match.kind = "any"
assign.tags = ["`+reserved+`"]
mode = "live"
`), 0o644))

		out, err := runRulesCmd(t, "add", fragment, "--rules-file", userRules)
		require.Error(t, err, "add of reserved assign.tag %q must fail; out:\n%s", reserved, out)

		// And nothing should have been persisted.
		if _, statErr := os.Stat(userRules); statErr == nil {
			data, _ := os.ReadFile(userRules)
			require.NotContains(t, string(data), reserved,
				"reserved tag %q must not be written to the user rules file", reserved)
		}
	}
}

// A non-reserved assign.tag is still accepted.
func TestRules_AddAllowsNormalAssignTag(t *testing.T) {
	tmp := t.TempDir()
	userRules := filepath.Join(tmp, "rules.toml")
	fragment := filepath.Join(tmp, "fragment.toml")
	require.NoError(t, os.WriteFile(fragment, []byte(`
[[sync.rules]]
name = "tagger"
match.kind = "any"
assign.tags = ["work"]
mode = "live"
`), 0o644))

	out, err := runRulesCmd(t, "add", fragment, "--rules-file", userRules)
	require.NoError(t, err, "add of a normal assign.tag must succeed; out:\n%s", out)
}

// Safe-by-default (BRD-05 §6, reversed #1): a fresh install ships no
// always-on rules, so `rules list` shows ONLY the user's rules — never
// the classic defaults (those are opt-in presets now).
func TestRules_ListSafeByDefault_NoShippedDefaults(t *testing.T) {
	tmp := t.TempDir()
	userRules := filepath.Join(tmp, "rules.toml")

	out, err := runRulesCmd(t, "list", "--rules-file", userRules)
	require.NoError(t, err, "list output:\n%s", out)
	// None of the classic defaults should appear on a fresh install.
	for _, name := range []string{
		"default-all-to-all",
		"fork-respects-origin",
		"private-stays-local",
		"tool-secrets-default-local",
		"ephemeral-projects-stay-local",
	} {
		require.NotContains(t, out, name,
			"safe-by-default: %q must not appear in `rules list`", name)
	}
}

// After the user adds a rule it shows up labelled "user" — and only
// that rule (no shipped defaults merged underneath).
func TestRules_ListShowsUserRulesOnly(t *testing.T) {
	tmp := t.TempDir()
	userRules := filepath.Join(tmp, "rules.toml")
	fragment := filepath.Join(tmp, "fragment.toml")
	require.NoError(t, os.WriteFile(fragment, []byte(`
[[sync.rules]]
name = "my-only-rule"
match.kind = "any"
match.type = ["memory"]
route.agents = ["claude-code"]
mode = "live"
`), 0o644))

	out, err := runRulesCmd(t, "add", fragment, "--rules-file", userRules)
	require.NoError(t, err, "add output:\n%s", out)

	out, err = runRulesCmd(t, "list", "--rules-file", userRules)
	require.NoError(t, err)
	require.Contains(t, out, "my-only-rule")
	require.Contains(t, out, "user")
	require.NotContains(t, out, "default-all-to-all")
}

func TestRules_AddAndRemove(t *testing.T) {
	tmp := t.TempDir()
	userRules := filepath.Join(tmp, "rules.toml")
	fragment := filepath.Join(tmp, "fragment.toml")
	require.NoError(t, os.WriteFile(fragment, []byte(`
[[sync.rules]]
name = "work-memories-to-work-agents"
match.kind = "any"
match.tag = ["work"]
match.type = ["memory"]
route.agents = ["claude-code", "codex"]
mode = "live"
`), 0o644))

	out, err := runRulesCmd(t, "add", fragment, "--rules-file", userRules)
	require.NoError(t, err, "add output:\n%s", out)
	require.Contains(t, out, "added 1 rule")

	out, err = runRulesCmd(t, "list", "--rules-file", userRules)
	require.NoError(t, err)
	require.Contains(t, out, "work-memories-to-work-agents")

	out, err = runRulesCmd(t, "remove", "work-memories-to-work-agents", "--rules-file", userRules)
	require.NoError(t, err)
	require.Contains(t, out, "removed")
}

// Safe-by-default: with no user rules, `rules test` matches nothing and
// the artifact routes nowhere.
func TestRules_TestShowsDecision_NoRulesRoutesNowhere(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	userRules := filepath.Join(tmp, "rules.toml")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")

	out, err := runRulesCmd(t, "test", id, "--rules-file", userRules, "--store", storeRoot)
	require.NoError(t, err, "test output:\n%s", out)
	require.Contains(t, out, "allowed agents")
	// No default-all-to-all anymore → nothing matched.
	require.NotContains(t, out, "default-all-to-all")
	require.Contains(t, out, "matched rules: \n")
}

// With a user rule in place, `rules test` reflects that user rule's
// decision (and only it — no shipped defaults).
func TestRules_TestShowsDecision_WithUserRule(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	userRules := filepath.Join(tmp, "rules.toml")
	require.NoError(t, os.WriteFile(userRules, []byte(`
[[sync.rules]]
name = "send-all-memory"
match.kind = "any"
match.type = ["memory"]
route.agents = ["claude-code", "codex"]
mode = "live"
`), 0o644))
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")

	out, err := runRulesCmd(t, "test", id, "--rules-file", userRules, "--store", storeRoot)
	require.NoError(t, err, "test output:\n%s", out)
	require.Contains(t, out, "send-all-memory")
	require.NotContains(t, out, "default-all-to-all")
}
