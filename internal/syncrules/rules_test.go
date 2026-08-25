package syncrules

import (
	"bytes"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

// boolPtr is a test helper for the *bool Rule.Enabled field.
func boolPtr(b bool) *bool { return &b }

func TestParse_DefaultRules(t *testing.T) {
	cfg, err := ParseDefault()
	require.NoError(t, err)
	require.Len(t, cfg.Sync.Rules, 5)
	names := map[string]bool{}
	for _, r := range cfg.Sync.Rules {
		names[r.Name] = true
	}
	for _, want := range []string{
		"default-all-to-all",
		"fork-respects-origin",
		"private-stays-local",
		"tool-secrets-default-local",
		"ephemeral-projects-stay-local",
	} {
		require.True(t, names[want], "expected rule %q in defaults", want)
	}
}

func TestValidate_RejectsContradictoryPattern(t *testing.T) {
	rules := []Rule{{
		Name:  "bad",
		Route: RouteSpec{Agents: []string{"claude-code", "!claude-code"}},
	}}
	err := Validate(rules)
	require.Error(t, err)
	require.Contains(t, err.Error(), "contradictory")
}

func TestValidate_RejectsDuplicateNames(t *testing.T) {
	rules := []Rule{
		{Name: "a"},
		{Name: "a"},
	}
	err := Validate(rules)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate rule name")
}

func TestEvaluate_AllToAllDefault(t *testing.T) {
	cfg, err := ParseDefault()
	require.NoError(t, err)
	eng, err := New(cfg.Sync.Rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{
		ArtifactID: "x",
		Kind:       "memory",
		Type:       "memory",
	}, EvaluateOpts{InstalledAgents: []string{"claude-code", "codex", "hermes", "openclaw", "kilo"}})
	require.ElementsMatch(t,
		[]string{"claude-code", "codex", "hermes", "openclaw", "kilo"},
		dec.AllowedAgents,
	)
}

// route.historicalSyncDepth must round-trip through TOML and surface per-agent
// in the resolved Decision (the per-rule "historical sync depth" feature).
func TestParse_HistoricalSyncDepth(t *testing.T) {
	cfg, err := Parse([]byte(`
[[sync.rules]]
name = "conv-depth"
match.type = ["conversation"]
route.agents = ["codex", "kilo"]
route.historicalSyncDepth = { codex = 5, kilo = -1 }
`))
	require.NoError(t, err)
	require.Len(t, cfg.Sync.Rules, 1)
	require.Equal(t, 5, cfg.Sync.Rules[0].Route.HistoricalSyncDepth["codex"])
	require.Equal(t, -1, cfg.Sync.Rules[0].Route.HistoricalSyncDepth["kilo"], "-1 = all history")
}

func TestEvaluate_HistoricalSyncDepth(t *testing.T) {
	eng, err := New([]Rule{{
		Name:  "conv-depth",
		Match: MatchSpec{Kind: MatchKindAny, Type: []string{"conversation"}},
		Route: RouteSpec{
			Agents:              []string{"codex", "kilo"},
			HistoricalSyncDepth: map[string]int{"codex": 5, "kilo": -1},
		},
	}})
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{Kind: "conversation", Type: "conversation"},
		EvaluateOpts{InstalledAgents: []string{"claude-code", "codex", "kilo"}})
	require.Equal(t, 5, dec.HistoricalSyncDepth["codex"])
	require.Equal(t, -1, dec.HistoricalSyncDepth["kilo"])
	_, set := dec.HistoricalSyncDepth["claude-code"]
	require.False(t, set, "an agent the rule didn't set has no depth (falls back to the global cap)")
}

func TestParse_ScheduledIntervalSeconds(t *testing.T) {
	cfg, err := Parse([]byte(`
[[sync.rules]]
name = "scheduled-memory"
mode = "scheduled"
scheduledIntervalSeconds = 600
match.type = ["memory"]
`))
	require.NoError(t, err)
	require.Len(t, cfg.Sync.Rules, 1)
	require.Equal(t, 600, cfg.Sync.Rules[0].ScheduledIntervalSeconds)
}

func TestParse_ScheduledIntervalSecondsRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	err := toml.NewEncoder(&buf).Encode(Config{Sync: SyncSection{Rules: []Rule{{
		Name:                     "scheduled-memory",
		Mode:                     ModeScheduled,
		ScheduledIntervalSeconds: 600,
		Match:                    MatchSpec{Type: []string{"memory"}},
	}}}})
	require.NoError(t, err)
	require.Contains(t, buf.String(), "scheduledIntervalSeconds = 600")

	cfg, err := Parse(buf.Bytes())
	require.NoError(t, err)
	require.Equal(t, 600, cfg.Sync.Rules[0].ScheduledIntervalSeconds)
}

func TestEvaluate_ScheduledIntervalSeconds(t *testing.T) {
	eng, err := New([]Rule{{
		Name:                     "scheduled-memory",
		Mode:                     ModeScheduled,
		ScheduledIntervalSeconds: 600,
		Match:                    MatchSpec{Type: []string{"memory"}},
	}})
	require.NoError(t, err)

	dec := eng.Evaluate(Artifact{Kind: "memory", Type: "memory"},
		EvaluateOpts{InstalledAgents: []string{"claude-code"}})
	require.Equal(t, ModeScheduled, dec.Mode)
	require.Equal(t, 600, dec.ScheduledIntervalSeconds)
}

func TestEvaluate_ScheduledIntervalDefaultsToFifteenMinutes(t *testing.T) {
	eng, err := New([]Rule{{
		Name:  "scheduled-default",
		Mode:  ModeScheduled,
		Match: MatchSpec{Type: []string{"memory"}},
	}})
	require.NoError(t, err)

	dec := eng.Evaluate(Artifact{Kind: "memory", Type: "memory"},
		EvaluateOpts{InstalledAgents: []string{"claude-code"}})
	require.Equal(t, ModeScheduled, dec.Mode)
	require.Equal(t, DefaultScheduledIntervalSeconds, dec.ScheduledIntervalSeconds)
}

func TestEvaluate_LiveModeClearsScheduledInterval(t *testing.T) {
	eng, err := New([]Rule{
		{
			Name:                     "scheduled-memory",
			Mode:                     ModeScheduled,
			ScheduledIntervalSeconds: 600,
			Match:                    MatchSpec{Type: []string{"memory"}},
		},
		{
			Name:  "live-memory",
			Mode:  ModeLive,
			Match: MatchSpec{Type: []string{"memory"}},
		},
	})
	require.NoError(t, err)

	dec := eng.Evaluate(Artifact{Kind: "memory", Type: "memory"},
		EvaluateOpts{InstalledAgents: []string{"claude-code"}})
	require.Equal(t, ModeLive, dec.Mode)
	require.Zero(t, dec.ScheduledIntervalSeconds)
}

func TestValidate_RejectsNegativeScheduledIntervalSeconds(t *testing.T) {
	err := Validate([]Rule{{
		Name:                     "bad",
		Mode:                     ModeScheduled,
		ScheduledIntervalSeconds: -1,
	}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "scheduledIntervalSeconds")
}

func TestEvaluate_ForkRespectsOrigin(t *testing.T) {
	cfg, err := ParseDefault()
	require.NoError(t, err)
	eng, err := New(cfg.Sync.Rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{
		ArtifactID:  "x",
		Kind:        "memory",
		Type:        "memory",
		Tags:        []string{"fork-of:abc123"},
		OriginAgent: "codex",
	}, EvaluateOpts{InstalledAgents: []string{"claude-code", "codex", "hermes"}})
	require.Contains(t, dec.AllowedAgents, "codex",
		"fork-respects-origin should allow codex")
	// default-all-to-all also matches, so the union may include other
	// agents. Both rules should be in MatchedRules.
	require.Contains(t, dec.MatchedRules, "fork-respects-origin")
	require.Contains(t, dec.MatchedRules, "default-all-to-all")
}

func TestEvaluate_PrivateStaysLocal(t *testing.T) {
	cfg, err := ParseDefault()
	require.NoError(t, err)
	eng, err := New(cfg.Sync.Rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{
		ArtifactID: "x",
		Kind:       "memory",
		Type:       "memory",
		Tags:       []string{"private"},
	}, EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}})
	require.False(t, dec.RemoteAllowed, "private tag → route.remote=exclude")
}

func TestEvaluate_NegativePatternWins(t *testing.T) {
	rules := []Rule{
		{Name: "allow-all", Match: MatchSpec{Kind: MatchKindAny, Type: []string{"memory"}}, Route: RouteSpec{Agents: []string{"*"}}},
		{Name: "deny-openclaw", Match: MatchSpec{Kind: MatchKindAny, Type: []string{"memory"}}, Route: RouteSpec{Agents: []string{"!openclaw"}}},
	}
	eng, err := New(rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{Kind: "memory", Type: "memory"},
		EvaluateOpts{InstalledAgents: []string{"claude-code", "codex", "openclaw"}})
	require.NotContains(t, dec.AllowedAgents, "openclaw")
	require.Contains(t, dec.AllowedAgents, "claude-code")
	require.Contains(t, dec.AllowedAgents, "codex")
}

func TestEvaluate_SkillModeStrictPrecedence(t *testing.T) {
	rules := []Rule{
		{Name: "lossy-default", Match: MatchSpec{Type: []string{"skill"}}, Route: RouteSpec{SkillMode: SkillModeLossy}},
		{Name: "experimental-strict", Match: MatchSpec{Tag: []string{"experimental"}, Type: []string{"skill"}}, Route: RouteSpec{SkillMode: SkillModeStrict}},
	}
	eng, err := New(rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{Type: "skill", Tags: []string{"experimental"}},
		EvaluateOpts{InstalledAgents: []string{"claude-code"}})
	require.Equal(t, SkillModeStrict, dec.SkillMode)
}

func TestParse_RouteDevicesRoundTrips(t *testing.T) {
	// route.devices is a cloud-stage predicate: it must survive a Parse
	// round-trip verbatim so the rules API can surface it unchanged, even
	// though the local engine ignores it.
	toml := `
[[sync.rules]]
name = "phone-only"
match.type = ["memory"]
route.agents = ["claude-code"]
route.devices = ["device-abc", "device-xyz"]
`
	cfg, err := Parse([]byte(toml))
	require.NoError(t, err)
	require.Len(t, cfg.Sync.Rules, 1)
	require.Equal(t,
		[]string{"device-abc", "device-xyz"},
		cfg.Sync.Rules[0].Route.Devices,
		"route.devices must round-trip through Parse unchanged",
	)
}

func TestEvaluate_RouteDevicesIgnoredLocally(t *testing.T) {
	// The local engine is a single-device install: route.devices is a
	// cloud-stage predicate the relay/plugin resolve. The local engine
	// must IGNORE it entirely — a rule scoped to some OTHER device id must
	// still route to local agents exactly as if route.devices were absent.
	scoped := []Rule{{
		Name:  "scoped",
		Match: MatchSpec{Type: []string{"memory"}},
		Route: RouteSpec{Agents: []string{"claude-code"}, Devices: []string{"some-other-device"}},
	}}
	unscoped := []Rule{{
		Name:  "scoped",
		Match: MatchSpec{Type: []string{"memory"}},
		Route: RouteSpec{Agents: []string{"claude-code"}},
	}}

	art := Artifact{Kind: "memory", Type: "memory"}
	opts := EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}}

	engScoped, err := New(scoped)
	require.NoError(t, err)
	engUnscoped, err := New(unscoped)
	require.NoError(t, err)

	decScoped := engScoped.Evaluate(art, opts)
	decUnscoped := engUnscoped.Evaluate(art, opts)

	require.Equal(t, []string{"claude-code"}, decScoped.AllowedAgents,
		"route.devices must not change local agent routing")
	require.ElementsMatch(t, decUnscoped.AllowedAgents, decScoped.AllowedAgents,
		"a device-scoped rule must route identically to an unscoped one locally")
	require.Equal(t, decUnscoped.MatchedRules, decScoped.MatchedRules)
}

func TestEvaluate_CachedResultMatchesUncached(t *testing.T) {
	// A second Evaluate with the same inputs must return a Decision equal
	// to the first (the cache must be transparent, not lossy).
	cfg, err := ParseDefault()
	require.NoError(t, err)
	eng, err := New(cfg.Sync.Rules)
	require.NoError(t, err)
	art := Artifact{
		ArtifactID:  "x",
		Kind:        "memory",
		Type:        "memory",
		Tags:        []string{"fork-of:abc123"},
		OriginAgent: "codex",
	}
	opts := EvaluateOpts{InstalledAgents: []string{"claude-code", "codex", "hermes"}}
	first := eng.Evaluate(art, opts)
	second := eng.Evaluate(art, opts)
	require.Equal(t, first, second, "cached Evaluate must equal the first")
}

func TestEvaluate_TagChangeYieldsDifferentDecision(t *testing.T) {
	// Tag-change invalidation is structural: a different tag set is a
	// different cache key, so the Decision must reflect the new tags.
	cfg, err := ParseDefault()
	require.NoError(t, err)
	eng, err := New(cfg.Sync.Rules)
	require.NoError(t, err)
	opts := EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}}
	clean := eng.Evaluate(Artifact{ArtifactID: "x", Kind: "memory", Type: "memory"}, opts)
	require.True(t, clean.RemoteAllowed)
	private := eng.Evaluate(Artifact{
		ArtifactID: "x", Kind: "memory", Type: "memory", Tags: []string{"private"},
	}, opts)
	require.False(t, private.RemoteAllowed,
		"adding the private tag must yield a fresh decision, not a stale cached one")
}

func TestEvaluate_IsCached(t *testing.T) {
	// The Engine must memoize evaluations: after a first Evaluate the
	// internal cache must hold exactly one entry, and a second identical
	// call must not add another.
	cfg, err := ParseDefault()
	require.NoError(t, err)
	eng, err := New(cfg.Sync.Rules)
	require.NoError(t, err)
	art := Artifact{ArtifactID: "x", Kind: "memory", Type: "memory"}
	opts := EvaluateOpts{InstalledAgents: []string{"claude-code", "codex", "hermes"}}
	eng.Evaluate(art, opts)
	require.Len(t, eng.cache, 1, "first Evaluate must populate the cache")
	eng.Evaluate(art, opts)
	require.Len(t, eng.cache, 1, "identical Evaluate must hit the cache, not grow it")
}

func TestEvaluate_CachedSliceMutationIsolation(t *testing.T) {
	// Callers must not be able to mutate cached state by writing into the
	// returned slices: a later identical Evaluate must be unaffected.
	cfg, err := ParseDefault()
	require.NoError(t, err)
	eng, err := New(cfg.Sync.Rules)
	require.NoError(t, err)
	art := Artifact{ArtifactID: "x", Kind: "memory", Type: "memory"}
	opts := EvaluateOpts{InstalledAgents: []string{"claude-code", "codex", "hermes"}}
	first := eng.Evaluate(art, opts)
	require.NotEmpty(t, first.AllowedAgents)
	for i := range first.AllowedAgents {
		first.AllowedAgents[i] = "MUTATED"
	}
	first.MatchedRules[0] = "MUTATED"
	second := eng.Evaluate(art, opts)
	require.NotContains(t, second.AllowedAgents, "MUTATED",
		"mutating a returned slice must not corrupt the cache")
	require.NotContains(t, second.MatchedRules, "MUTATED")
}

func TestValidate_RejectsMatchSize(t *testing.T) {
	// match.size is listed by BRD-05 §5.2 but is NOT evaluated by matches();
	// a rule carrying it would silently match every artifact regardless of
	// size — the opposite of the safe restrictive intent. Until size routing
	// is implemented end-to-end (it also needs orchestrator + cache-key
	// support outside this package), Validate must reject a non-empty
	// match.size loudly at config-load time rather than accept a no-op.
	rules := []Rule{{
		Name:  "size-guard",
		Match: MatchSpec{Size: "< 1MB"},
	}}
	err := Validate(rules)
	require.Error(t, err)
	require.Contains(t, err.Error(), "match.size")
}

func TestParse_RejectsMatchSize(t *testing.T) {
	// Same guard, exercised through the TOML front door.
	toml := `
[[sync.rules]]
name = "size-guard"
match.size = "< 1MB"
route.agents = ["claude-code"]
`
	_, err := Parse([]byte(toml))
	require.Error(t, err)
	require.Contains(t, err.Error(), "match.size")
}

func TestParse_RejectsUnknownKey(t *testing.T) {
	// A misspelled match key (match.tags plural — a natural typo for the
	// documented match.tag) is silently dropped by toml.Decode, leaving a
	// rule with no predicates that matches EVERY artifact. Parse must reject
	// unknown keys so a fat-fingered rule fails loudly instead of becoming a
	// silent all-artifacts fan-out (FR-05.6 determinism/explainability).
	toml := `
[[sync.rules]]
name = "typo"
match.tags = ["work"]
route.agents = ["claude-code"]
`
	_, err := Parse([]byte(toml))
	require.Error(t, err)
	require.Contains(t, err.Error(), "match.tags",
		"error should name the unrecognized key")
}

func TestParse_AcceptsKnownKeys(t *testing.T) {
	// The unknown-key guard must not over-reject: a rule using only
	// documented keys (including the cloud-stage route.devices) parses clean.
	toml := `
[[sync.rules]]
name = "ok"
match.kind = "any"
match.tag = ["work"]
match.type = ["memory"]
route.agents = ["claude-code"]
route.devices = ["device-abc"]
`
	cfg, err := Parse([]byte(toml))
	require.NoError(t, err)
	require.Len(t, cfg.Sync.Rules, 1)
}

func TestParse_DefaultRulesHaveNoUnknownKeys(t *testing.T) {
	// Guard against the shipped defaults tripping the unknown-key check.
	_, err := Parse([]byte(defaultRulesTOML))
	require.NoError(t, err)
}

func TestEvaluate_AssignTags(t *testing.T) {
	rules := []Rule{
		{Name: "auto-tag-work",
			Match:  MatchSpec{AgentSource: []string{"claude-code"}, Type: []string{"memory"}},
			Assign: AssignSpec{Tags: []string{"work"}},
		},
	}
	eng, err := New(rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{Type: "memory", OriginAgent: "claude-code"},
		EvaluateOpts{InstalledAgents: []string{"claude-code"}})
	require.Contains(t, dec.AssignedTags, "work")
}

func TestEvaluate_DisabledRuleDoesNotContribute(t *testing.T) {
	// BRD-05 §6.5: a rule with Enabled=false is inactive — it contributes
	// NO allow/deny/tags/mode and never appears in MatchedRules, exactly as
	// if it were absent. Here a disabled assign-tag rule that WOULD match the
	// artifact must add nothing.
	rules := []Rule{
		{Name: "fanout", Match: MatchSpec{Type: []string{"memory"}}, Route: RouteSpec{Agents: []string{"claude-code"}}},
		{Name: "auto-tag-work", Match: MatchSpec{Type: []string{"memory"}}, Assign: AssignSpec{Tags: []string{"work"}}, Enabled: boolPtr(false)},
	}
	eng, err := New(rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{Type: "memory"},
		EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}})
	require.NotContains(t, dec.AssignedTags, "work", "disabled rule must not assign tags")
	require.NotContains(t, dec.MatchedRules, "auto-tag-work", "disabled rule must not appear in MatchedRules")
	require.Equal(t, []string{"claude-code"}, dec.AllowedAgents, "only the enabled rule routes")
}

func TestEvaluate_DisabledDenyRuleIsInert(t *testing.T) {
	// A disabled negative-pattern rule must NOT subtract from the allow set:
	// disabling the deny re-allows the agent it would have excluded.
	base := MatchSpec{Kind: MatchKindAny, Type: []string{"memory"}}
	rules := []Rule{
		{Name: "allow-all", Match: base, Route: RouteSpec{Agents: []string{"*"}}},
		{Name: "deny-openclaw", Match: base, Route: RouteSpec{Agents: []string{"!openclaw"}}, Enabled: boolPtr(false)},
	}
	eng, err := New(rules)
	require.NoError(t, err)
	dec := eng.Evaluate(Artifact{Kind: "memory", Type: "memory"},
		EvaluateOpts{InstalledAgents: []string{"claude-code", "openclaw"}})
	require.Contains(t, dec.AllowedAgents, "openclaw", "a disabled deny rule must not exclude openclaw")
	require.NotContains(t, dec.DeniedAgents, "openclaw")
}

func TestEvaluate_EnabledTrueAndNilBehaveIdentically(t *testing.T) {
	// Enabled=nil (field omitted, the back-compat default) and Enabled=true
	// must both be active and produce the same decision as each other.
	mk := func(en *bool) *Engine {
		eng, err := New([]Rule{
			{Name: "fanout", Match: MatchSpec{Type: []string{"memory"}}, Route: RouteSpec{Agents: []string{"claude-code"}}, Enabled: en},
		})
		require.NoError(t, err)
		return eng
	}
	art := Artifact{Type: "memory"}
	opts := EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}}

	nilDec := mk(nil).Evaluate(art, opts)
	trueDec := mk(boolPtr(true)).Evaluate(art, opts)

	require.Equal(t, []string{"claude-code"}, nilDec.AllowedAgents, "nil Enabled must be active")
	require.Equal(t, nilDec.AllowedAgents, trueDec.AllowedAgents, "Enabled=true must behave like nil")
	require.Equal(t, nilDec.MatchedRules, trueDec.MatchedRules)
}

func TestParse_EnabledRoundTrips(t *testing.T) {
	// enabled=false must survive a Parse round-trip so a portal-disabled
	// rule reloads disabled; a rule omitting the key must parse to nil
	// (the enabled default), never to a non-nil false.
	tomlSrc := `
[[sync.rules]]
name = "disabled-rule"
match.type = ["memory"]
route.agents = ["claude-code"]
enabled = false

[[sync.rules]]
name = "default-rule"
match.type = ["memory"]
route.agents = ["claude-code"]
`
	cfg, err := Parse([]byte(tomlSrc))
	require.NoError(t, err)
	require.Len(t, cfg.Sync.Rules, 2)
	require.NotNil(t, cfg.Sync.Rules[0].Enabled, "enabled=false must decode to a non-nil pointer")
	require.False(t, *cfg.Sync.Rules[0].Enabled, "enabled=false must round-trip as false")
	require.Nil(t, cfg.Sync.Rules[1].Enabled, "an omitted enabled key must decode to nil (enabled)")
}

func TestEncode_EnabledRoundTripsThroughWriteUserRules(t *testing.T) {
	// Mirror the daemon's writeUserRules serialisation (toml.NewEncoder →
	// Encode) and re-Parse it: enabled=false must survive the full
	// encode→decode cycle so disabling a rule persists across daemon
	// restarts. A nil Enabled must NOT emit the key (omitted = enabled).
	in := Config{Sync: SyncSection{Rules: []Rule{
		{Name: "off", Match: MatchSpec{Type: []string{"memory"}}, Route: RouteSpec{Agents: []string{"claude-code"}}, Enabled: boolPtr(false)},
		{Name: "on", Match: MatchSpec{Type: []string{"memory"}}, Route: RouteSpec{Agents: []string{"claude-code"}}},
	}}}
	var buf bytes.Buffer
	require.NoError(t, toml.NewEncoder(&buf).Encode(in))

	out, err := Parse(buf.Bytes())
	require.NoError(t, err)
	require.Len(t, out.Sync.Rules, 2)
	require.NotNil(t, out.Sync.Rules[0].Enabled)
	require.False(t, *out.Sync.Rules[0].Enabled, "enabled=false must survive encode→decode")
	require.Nil(t, out.Sync.Rules[1].Enabled, "nil Enabled must not be emitted as a key")
}

func TestEvaluate_BranchNameRegexMatches(t *testing.T) {
	// BRD-05 §5.2: match.branchName is a REGEX over the conversation's head
	// branch. A rule scoped to feature branches must match a "feature/x" head
	// and NOT match "main".
	rules := []Rule{{
		Name:  "feature-branches-to-codex",
		Match: MatchSpec{BranchName: "^feature/.*$"},
		Route: RouteSpec{Agents: []string{"codex"}},
	}}
	eng, err := New(rules)
	require.NoError(t, err)

	decMatch := eng.Evaluate(
		Artifact{Kind: "conversation", Type: "conversation", BranchName: "feature/x"},
		EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}},
	)
	require.Contains(t, decMatch.MatchedRules, "feature-branches-to-codex")
	require.Contains(t, decMatch.AllowedAgents, "codex")

	decNoMatch := eng.Evaluate(
		Artifact{Kind: "conversation", Type: "conversation", BranchName: "main"},
		EvaluateOpts{InstalledAgents: []string{"claude-code", "codex"}},
	)
	require.NotContains(t, decNoMatch.MatchedRules, "feature-branches-to-codex")
	require.Empty(t, decNoMatch.AllowedAgents,
		"main branch must not match a ^feature/.*$ rule (deny-by-default)")
}

func TestEvaluate_BranchNamePlainNameMatchesExactly(t *testing.T) {
	// A pattern with no metacharacters is still a valid regex; as a regex it
	// matches by substring unless anchored, so "main" matches a "main" head
	// and (because the pattern is unanchored) any branch CONTAINING "main".
	// The point this test pins: a plain-name pattern matches the exact branch
	// it names and does NOT match an unrelated branch.
	rules := []Rule{{
		Name:  "main-only",
		Match: MatchSpec{BranchName: "main"},
		Route: RouteSpec{Agents: []string{"codex"}},
	}}
	eng, err := New(rules)
	require.NoError(t, err)

	decMain := eng.Evaluate(
		Artifact{Kind: "conversation", Type: "conversation", BranchName: "main"},
		EvaluateOpts{InstalledAgents: []string{"codex"}},
	)
	require.Contains(t, decMain.MatchedRules, "main-only")

	decOther := eng.Evaluate(
		Artifact{Kind: "conversation", Type: "conversation", BranchName: "feature-x"},
		EvaluateOpts{InstalledAgents: []string{"codex"}},
	)
	require.NotContains(t, decOther.MatchedRules, "main-only")
}

func TestValidate_RejectsInvalidBranchNameRegex(t *testing.T) {
	// An un-compilable regex (unbalanced paren) must be rejected at load time
	// rather than silently never-matching, mirroring the match.size guard.
	rules := []Rule{{
		Name:  "bad-branch",
		Match: MatchSpec{BranchName: "feature/("},
	}}
	err := Validate(rules)
	require.Error(t, err)
	require.Contains(t, err.Error(), "branchName")
}

func TestNew_RejectsInvalidBranchNameRegex(t *testing.T) {
	// New runs the same compile-and-validate guard as Validate.
	rules := []Rule{{
		Name:  "bad-branch",
		Match: MatchSpec{BranchName: "["},
	}}
	_, err := New(rules)
	require.Error(t, err)
	require.Contains(t, err.Error(), "branchName")
}

func TestParse_RejectsInvalidBranchNameRegex(t *testing.T) {
	// Same guard exercised through the TOML front door.
	toml := `
[[sync.rules]]
name = "bad-branch"
match.branchName = "feature/("
route.agents = ["codex"]
`
	_, err := Parse([]byte(toml))
	require.Error(t, err)
	require.Contains(t, err.Error(), "branchName")
}
