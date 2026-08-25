package syncd

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSuppressionCatalogIsComplete pins the operator experience: every reason
// must carry a class and a consequence sentence, and every DEFECT and POLICY
// reason must carry an actionable remedy. Capability reasons legitimately have
// none — "this agent is not installed" is a fact, not a problem to solve.
//
// This is the test that stops the catalog from rotting into the thing it
// replaced: a reason with no explanation is exactly the "rules engine rebuilt
// (0 rules)" log line that cost an hour of forensics.
func TestSuppressionCatalogIsComplete(t *testing.T) {
	for reason, meta := range suppressionCatalog {
		t.Run(string(reason), func(t *testing.T) {
			require.NotZero(t, meta.Class, "must declare a class")
			require.NotEmpty(t, meta.Explain, "must explain the consequence")
			assert.NotContains(t, meta.Explain, "engine", "explain the consequence, not the mechanism")
			if meta.Class != ClassCapability {
				require.NotEmpty(t, meta.Remedy, "policy and defect reasons must be actionable")
			}
		})
	}
}

// TestSuppressionUnknownReasonIsDefect: an unregistered reason must be treated
// as a defect, never as policy. Policy rows are never retried and are shown as
// intentional, so misclassifying an unknown suppression as policy would hide a
// real bug — the precise failure mode this package exists to prevent.
func TestSuppressionUnknownReasonIsDefect(t *testing.T) {
	var mystery SuppressionReason = "something_new_nobody_registered"
	assert.Equal(t, ClassDefect, mystery.Class())
	assert.NotEmpty(t, mystery.Explain())
	assert.NotEmpty(t, mystery.Remedy())
}

// TestSuppressionLedgerIsBoundedByAgentTimesReason is the R5 guarantee: the
// ledger's size depends on the number of agents and reasons, NEVER on the
// number of artifacts. Without this the ledger could not run on the hot path.
func TestSuppressionLedgerIsBoundedByAgentTimesReason(t *testing.T) {
	l := newSuppressionLedger()
	now := time.Now()
	agents := []string{"claude-code", "codex", "kilo", "openclaw", "hermes"}
	reasons := []SuppressionReason{ReasonRulesDenied, ReasonPaused, ReasonExportFailed}

	for i := 0; i < 10_000; i++ {
		l.record(agents[i%len(agents)], reasons[i%len(reasons)], fmt.Sprintf("artifact-%d", i), now)
	}

	snap := l.Snapshot()
	assert.Len(t, snap, len(agents)*len(reasons), "cardinality is agents x reasons, independent of artifact count")
	for _, row := range snap {
		assert.LessOrEqual(t, len(row.Exemplars), suppressionExemplarsPerKey,
			"exemplars are capped so a hot loop cannot grow the ledger")
	}
}

// TestSuppressionLedgerCountsAndExemplars covers the aggregation contract.
func TestSuppressionLedgerCountsAndExemplars(t *testing.T) {
	l := newSuppressionLedger()
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	l.record("codex", ReasonRulesDenied, "art-1", start)
	l.record("codex", ReasonRulesDenied, "art-2", start.Add(time.Minute))
	l.record("codex", ReasonRulesDenied, "art-1", start.Add(2*time.Minute)) // dedup

	snap := l.Snapshot()
	require.Len(t, snap, 1)
	row := snap[0]
	assert.Equal(t, uint64(3), row.Count, "every occurrence counts")
	assert.ElementsMatch(t, []string{"art-1", "art-2"}, row.Exemplars, "exemplars dedup")
	assert.Equal(t, "policy", row.Class)
	assert.Equal(t, start.Format(time.RFC3339), row.FirstAt)
	assert.Equal(t, start.Add(2*time.Minute).Format(time.RFC3339), row.LastAt)
}

// TestSuppressionLedgerClearDefectsKeepsPolicy is the core UX invariant: a
// successful write retracts DEFECT rows (the fault is gone) but must NOT
// retract POLICY rows. A rule that deliberately excludes an agent is still in
// force after some other write succeeds; forgetting it would tell the operator
// their data is syncing somewhere it is not.
func TestSuppressionLedgerClearDefectsKeepsPolicy(t *testing.T) {
	l := newSuppressionLedger()
	now := time.Now()
	l.record("codex", ReasonExportFailed, "art-1", now)     // defect
	l.record("codex", ReasonRulesDenied, "art-2", now)      // policy
	l.record("codex", ReasonAdapterNotInstalled, "a3", now) // capability
	l.record("kilo", ReasonExportFailed, "art-4", now)      // other agent

	l.clearDefects("codex")

	got := map[string]bool{}
	for _, row := range l.Snapshot() {
		got[row.Agent+"/"+row.Reason] = true
	}
	assert.False(t, got["codex/export_failed"], "defect cleared on success")
	assert.True(t, got["codex/rules_denied"], "policy survives — it is still in force")
	assert.True(t, got["codex/adapter_not_installed"], "capability survives")
	assert.True(t, got["kilo/export_failed"], "other agents untouched")
}

// TestSuppressionLedgerNilSafe: call sites must not need a nil guard, or they
// will grow one inconsistently and a drop site will go unrecorded again.
func TestSuppressionLedgerNilSafe(t *testing.T) {
	var l *suppressionLedger
	assert.NotPanics(t, func() {
		l.record("codex", ReasonRulesDenied, "art-1", time.Now())
		l.clearDefects("codex")
		assert.Nil(t, l.Snapshot())
	})
}

// TestSuppressionLedgerIgnoresEmptyInputs keeps junk rows out of the operator
// surface.
func TestSuppressionLedgerIgnoresEmptyInputs(t *testing.T) {
	l := newSuppressionLedger()
	now := time.Now()
	l.record("", ReasonRulesDenied, "art-1", now)
	l.record("codex", "", "art-1", now)
	assert.Empty(t, l.Snapshot())
}

// TestSuppressionSnapshotIsDeterministic keeps status output and golden tests
// stable across map iteration order.
func TestSuppressionSnapshotIsDeterministic(t *testing.T) {
	l := newSuppressionLedger()
	now := time.Now()
	for _, agent := range []string{"openclaw", "codex", "kilo", "claude-code"} {
		l.record(agent, ReasonPaused, "art", now)
		l.record(agent, ReasonRulesDenied, "art", now)
	}
	first := l.Snapshot()
	for i := 0; i < 20; i++ {
		assert.Equal(t, first, l.Snapshot(), "snapshot order must be stable")
	}
	assert.Equal(t, "claude-code", first[0].Agent, "sorted by agent")
	assert.Equal(t, string(ReasonPaused), first[0].Reason, "then by reason")
}

// TestSuppressionClassString pins the wire strings used by status JSON.
func TestSuppressionClassString(t *testing.T) {
	assert.Equal(t, "policy", ClassPolicy.String())
	assert.Equal(t, "defect", ClassDefect.String())
	assert.Equal(t, "capability", ClassCapability.String())
	assert.Equal(t, "unknown", SuppressionClass(0).String())
}
