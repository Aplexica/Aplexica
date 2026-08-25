package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/require"
)

// withTempHome points os.UserHomeDir() at a fresh temp dir so
// buildRulesEngine resolves ~/.aplexica/rules.toml inside the sandbox.
// Returns the temp home so callers can write a rules file under it.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// os.UserHomeDir consults $HOME on unix and %USERPROFILE% on Windows.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// Safe-by-default: with no user rules.toml, buildRulesEngine produces an
// engine with ZERO rules and Evaluate routes the artifact nowhere
// (AllowedAgents empty) — the daemon fans out to no agent until the
// user adds a rule. (Reverses BRD-05 §6 #1.)
func TestBuildRulesEngine_NoUserFile_EmptyAndRoutesNowhere(t *testing.T) {
	withTempHome(t)

	eng, err := buildRulesEngine()
	require.NoError(t, err)
	require.NotNil(t, eng)
	require.Empty(t, eng.Rules(), "no user file → zero rules (no shipped defaults)")

	dec := eng.Evaluate(syncrules.Artifact{
		ArtifactID: "x",
		Kind:       "memory",
		Type:       "memory",
	}, syncrules.EvaluateOpts{
		InstalledAgents: []string{"claude-code", "codex", "hermes", "openclaw", "kilo"},
	})
	require.Empty(t, dec.AllowedAgents, "no rule → fan out to nobody")
	require.Empty(t, dec.MatchedRules, "no rule should match")
}

// A user rules.toml is honored verbatim — and ONLY the user rules are
// in the engine (no shipped defaults merged underneath).
func TestBuildRulesEngine_UserFileOnly(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".aplexica")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rules.toml"), []byte(`
[[sync.rules]]
name = "send-memory-to-codex"
match.kind = "any"
match.type = ["memory"]
route.agents = ["codex"]
mode = "live"
`), 0o644))

	eng, err := buildRulesEngine()
	require.NoError(t, err)
	rules := eng.Rules()
	require.Len(t, rules, 1, "only the user rule — no shipped defaults")
	require.Equal(t, "send-memory-to-codex", rules[0].Name)

	dec := eng.Evaluate(syncrules.Artifact{
		ArtifactID: "x",
		Kind:       "memory",
		Type:       "memory",
	}, syncrules.EvaluateOpts{
		InstalledAgents: []string{"claude-code", "codex"},
	})
	require.ElementsMatch(t, []string{"codex"}, dec.AllowedAgents)
}

// A broken (unparseable) user file falls back to an EMPTY engine (never
// the old shipped defaults) and surfaces the parse error so the daemon
// can log a warning. A broken file must not silently re-enable fan-out.
func TestBuildRulesEngine_ParseError_FallsBackToEmpty(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".aplexica")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rules.toml"),
		[]byte("this is = not valid toml [[["), 0o644))

	eng, err := buildRulesEngine()
	require.Error(t, err, "parse error must be returned for logging")
	require.NotNil(t, eng, "engine must be non-nil even on parse error")
	require.Empty(t, eng.Rules(), "fallback engine is empty (deny-all), not defaults")

	dec := eng.Evaluate(syncrules.Artifact{Kind: "memory", Type: "memory"},
		syncrules.EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}})
	require.Empty(t, dec.AllowedAgents, "broken file → fan out to nobody")
}

// TestRulesWebAccessor_AddHotReloadsLiveEngine proves the hot-reload
// contract: an Add through the web rules accessor rebuilds the
// orchestrator's live engine so a SUBSEQUENT Evaluate (the same call a
// fan-out cycle makes) reflects the new rule — without a daemon restart.
func TestRulesWebAccessor_AddHotReloadsLiveEngine(t *testing.T) {
	home := withTempHome(t)
	rulesPath := filepath.Join(home, ".aplexica", "rules.toml")

	// Build a minimal orchestrator: a watched temp dir, an acf store, and
	// two real adapters so the engine has a known universe to route over.
	watched := t.TempDir()
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	adapters := []adapter.Adapter{claudecode.New(), codex.New()}

	// Start with the safe-by-default empty engine (no user rules file yet).
	startEng, err := buildRulesEngineFromPath(rulesPath)
	require.NoError(t, err)

	orch, err := syncd.NewOrchestrator(syncd.Config{
		Dir:         watched,
		Adapters:    adapters,
		Store:       store,
		RulesEngine: startEng,
	})
	require.NoError(t, err)
	defer orch.Close()

	deps := &webAPIDeps{orch: orch, rulesPath: rulesPath}
	acc := &rulesWebAccessor{deps: deps}

	opts := syncrules.EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}}
	art := syncrules.Artifact{ArtifactID: "x", Kind: "memory", Type: "memory"}

	// Before: empty engine → nobody.
	before := orch.RulesEngine().Evaluate(art, opts)
	require.Empty(t, before.AllowedAgents, "no rules yet → fan out to nobody")

	// Add a rule routing memory → codex through the accessor.
	require.NoError(t, acc.Add(syncrules.Rule{
		Name:  "memory-to-codex",
		Match: syncrules.MatchSpec{Kind: syncrules.MatchKindAny, Type: []string{"memory"}},
		Route: syncrules.RouteSpec{Agents: []string{"codex"}},
	}))

	// After: the LIVE engine the orchestrator holds must reflect the Add.
	after := orch.RulesEngine().Evaluate(art, opts)
	require.ElementsMatch(t, []string{"codex"}, after.AllowedAgents,
		"Add via accessor must hot-reload the live engine")
	require.Contains(t, after.MatchedRules, "memory-to-codex")
}

// TestRulesWebAccessor_AddPresetName proves a rule using a classic
// preset name is addable through the REAL accessor. Presets ARE the
// BRD-05 default rules, offered opt-in and POSTed via Add; since
// safe-by-default removed the always-on defaults there is no
// shipped-default name collision to reject (regression guard for the
// "Add from preset" 400 bug). Adding the same preset twice is rejected
// as a duplicate.
func TestRulesWebAccessor_AddPresetName(t *testing.T) {
	home := withTempHome(t)
	rulesPath := filepath.Join(home, ".aplexica", "rules.toml")

	startEng, err := buildRulesEngineFromPath(rulesPath)
	require.NoError(t, err)
	orch, err := syncd.NewOrchestrator(syncd.Config{
		Dir:         t.TempDir(),
		Adapters:    []adapter.Adapter{claudecode.New(), codex.New()},
		Store:       &acf.Store{Root: filepath.Join(t.TempDir(), "store")},
		RulesEngine: startEng,
	})
	require.NoError(t, err)
	defer orch.Close()
	acc := &rulesWebAccessor{deps: &webAPIDeps{orch: orch, rulesPath: rulesPath}}

	defaults, err := syncrules.ParseDefault()
	require.NoError(t, err)
	require.NotEmpty(t, defaults.Sync.Rules)
	preset := defaults.Sync.Rules[0] // a classic preset name, e.g. "default-all-to-all"

	require.NoError(t, acc.Add(preset), "a preset-named rule must be addable")
	require.Error(t, acc.Add(preset), "adding the same preset twice → already exists")

	rules, err := acc.List()
	require.NoError(t, err)
	names := make([]string, 0, len(rules))
	for _, r := range rules {
		names = append(names, r.Name)
	}
	require.Contains(t, names, preset.Name, "the preset is now a live user rule")
}
