package syncd

import (
	"testing"

	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins the zero-rules regression so it can never recur silently.
//
// What happened: a device had no ~/.aplexica/rules.toml. buildRulesEngine
// treats a missing file as "zero rules, no error", producing a NON-nil
// deny-all engine. Every fan-out target was then dropped by a bare `continue`
// and fanOut returned nil. Import, MQTT publish, MQTT receive and canonical
// store writes all kept working, so `aplexica status` appeared healthy while
// all cross-agent sync was dead.
//
// The fix is not to fan out anyway — fail-closed is correct and preserved.
// The fix is that this state must be LOUD.

// TestZeroRulesEngineIsDetectedAsStructurallyDisabled is the core assertion:
// a non-nil engine holding zero rules must be reported as structurally
// disabled. A NIL engine is a different thing entirely (rules disabled =>
// fan out to everything) and must NOT be reported as disabled.
func TestZeroRulesEngineIsDetectedAsStructurallyDisabled(t *testing.T) {
	t.Run("zero rules is disabled", func(t *testing.T) {
		o := &Orchestrator{}
		eng, err := syncrules.New(nil)
		require.NoError(t, err)
		o.cfg.RulesEngine = eng
		require.NotNil(t, o.cfg.RulesEngine, "a missing rules file yields a non-nil empty engine, not nil")
		assert.True(t, o.SyncStructurallyDisabled(),
			"an empty engine denies every target — the operator must be told")
		assert.True(t, o.rulesEngineIsEmpty())
	})

	t.Run("nil engine is NOT disabled", func(t *testing.T) {
		o := &Orchestrator{}
		assert.False(t, o.SyncStructurallyDisabled(),
			"nil engine means rules are off entirely, which fans out to everything")
	})
}

// TestZeroRulesReportsNoRulesConfiguredNotRulesDenied is the diagnosis
// shortcut. "A rule excluded this agent" and "you have no rules at all" are
// completely different problems with different remedies, and reporting the
// former for the latter makes the problem needlessly difficult to diagnose.
func TestZeroRulesReportsNoRulesConfiguredNotRulesDenied(t *testing.T) {
	emptyEng, err := syncrules.New(nil)
	require.NoError(t, err)
	empty := &Orchestrator{}
	empty.cfg.RulesEngine = emptyEng
	assert.Equal(t, ReasonNoRulesConfigured, empty.conversationRuleReason(),
		"zero rules must say so explicitly")
	assert.Equal(t, ReasonNoRulesConfigured, empty.rulesSuppressionReason(map[string]struct{}{}))

	// With rules present but this target excluded, the reason is the ordinary
	// routing one.
	ruleEng, err := syncrules.New([]syncrules.Rule{{Name: "some-rule",
		Route: syncrules.RouteSpec{Agents: []string{"claude-code"}}}})
	require.NoError(t, err)
	withRules := &Orchestrator{}
	withRules.cfg.RulesEngine = ruleEng
	assert.Equal(t, ReasonRulesDenied, withRules.conversationRuleReason())
	assert.Equal(t, ReasonRulesDenied,
		withRules.rulesSuppressionReason(map[string]struct{}{"claude-code": {}}))
}

// TestNoRulesConfiguredRemedyIsActionable: the operator must be handed the
// exact command. A mechanism-only log line such as
// "rules engine rebuilt (0 rules, unchanged)" describes the mechanism
// and tells the reader nothing about what to do.
func TestNoRulesConfiguredRemedyIsActionable(t *testing.T) {
	assert.Equal(t, ClassPolicy, ReasonNoRulesConfigured.Class(),
		"an unconfigured device is a policy state, not a defect to auto-repair")
	assert.Contains(t, ReasonNoRulesConfigured.Explain(), "nothing is copied between agents",
		"state the consequence in the user's terms")
	assert.Contains(t, ReasonNoRulesConfigured.Remedy(), "aplexica rules add",
		"hand over the exact command")
}

// TestSuppressionsSurfaceIsNilSafe keeps the status path robust: the daemon
// must render status even before the orchestrator is fully constructed.
func TestSuppressionsSurfaceIsNilSafe(t *testing.T) {
	var o *Orchestrator
	assert.NotPanics(t, func() {
		assert.Nil(t, o.Suppressions())
		assert.False(t, o.SyncStructurallyDisabled())
	})
}
