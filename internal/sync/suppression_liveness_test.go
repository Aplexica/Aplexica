package syncd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// D3, verified live: `aplexica status` reported "Writes to this agent are
// blocked until its safety backup is verified" for three agents at 18:30Z from
// rows whose only observation was at 13:12:10Z — nine seconds before the daemon
// logged that the verification had completed with zero agents blocked. The row
// survived because clearDefects only fires on a SUCCESSFUL write to that agent,
// which may never happen, and nothing tested whether the condition was still
// true.
func TestSuppressionSnapshot_ResolvedConditionStopsRenderingAsCurrent(t *testing.T) {
	l := newSuppressionLedger()
	blockedAt := time.Date(2026, 7, 30, 13, 12, 10, 0, time.UTC)
	l.record("codex", ReasonAdapterBlockedSafety, "artifact-1", blockedAt)

	resolved := func(string, SuppressionReason) (bool, bool) { return false, true }
	rows := l.SnapshotAt(blockedAt.Add(5*time.Hour+18*time.Minute), resolved)

	require.Len(t, rows, 1, "the evidence is kept for forensics")
	require.True(t, rows[0].Stale, "a condition that has resolved is not a current problem")
}

// The mirror-image requirement: a condition that is genuinely still true must
// keep rendering however long ago it was last observed. On an idle device
// nothing re-records it, so elapsed time alone must never retire it.
func TestSuppressionSnapshot_PersistentConditionStillRenders(t *testing.T) {
	l := newSuppressionLedger()
	at := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	l.record("codex", ReasonAdapterBlockedSafety, "artifact-1", at)

	live := func(string, SuppressionReason) (bool, bool) { return true, true }
	rows := l.SnapshotAt(at.Add(48*time.Hour), live)

	require.Len(t, rows, 1)
	require.False(t, rows[0].Stale, "a condition that is still true is still a current problem")
}

// Defect reasons that record an EVENT rather than a condition (an export that
// failed, a destination that moved) have nothing to re-verify. Those go quiet
// relative to their own recurrence cadence.
func TestSuppressionSnapshot_UnverifiableRowGoesStaleOnItsOwnCadence(t *testing.T) {
	l := newSuppressionLedger()
	at := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	l.record("codex", ReasonExportFailed, "artifact-1", at)

	unknown := func(string, SuppressionReason) (bool, bool) { return false, false }
	require.False(t, l.SnapshotAt(at.Add(time.Minute), unknown)[0].Stale,
		"a failure a minute old is current")
	require.True(t, l.SnapshotAt(at.Add(suppressionStaleFloor+time.Minute), unknown)[0].Stale,
		"a one-off failure nobody has seen since is history, not a live fault")
}

// A row that keeps recurring is measured against its own interval, so a slow
// but real repetition is never mistaken for a resolved condition.
func TestSuppressionSnapshot_RecurringRowStaysCurrentAtItsOwnInterval(t *testing.T) {
	l := newSuppressionLedger()
	base := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		l.record("codex", ReasonExportFailed, "artifact-1", base.Add(time.Duration(i)*time.Hour))
	}
	last := base.Add(4 * time.Hour)

	unknown := func(string, SuppressionReason) (bool, bool) { return false, false }
	require.False(t, l.SnapshotAt(last.Add(2*time.Hour), unknown)[0].Stale,
		"an hourly fault seen two hours ago is still on cadence")
	require.True(t, l.SnapshotAt(last.Add(9*time.Hour), unknown)[0].Stale,
		"nine hours without a recurrence is well outside an hourly cadence")
}

// Policy rows count writes their owner asked not to be copied. They are
// historical facts, not conditions, so only the ones the orchestrator can
// re-verify are ever retired.
func TestSuppressionSnapshot_PolicyRowsAreNotAgedOut(t *testing.T) {
	l := newSuppressionLedger()
	at := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	l.record("codex", ReasonRulesDenied, "artifact-1", at)

	unknown := func(string, SuppressionReason) (bool, bool) { return false, false }
	require.False(t, l.SnapshotAt(at.Add(72*time.Hour), unknown)[0].Stale)
}

// End to end through the orchestrator: the block that produced D3 must stop
// rendering the moment the blocker clears, without waiting for a successful
// write that may never come.
func TestOrchestratorSuppressions_ClearedAdapterBlockIsNoLongerCurrent(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	blocker := NewAdapterBlocker(map[string]string{"codex": "native safety backup verification pending"})
	orch, err := NewOrchestrator(Config{
		Dir:            root,
		Store:          store,
		Adapters:       []adapter.Adapter{&fakeConvSource{name: "codex"}},
		AdapterBlocker: blocker,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	orch.suppressions.record("codex", ReasonAdapterBlockedSafety, "artifact-1", time.Now().UTC())
	rows := orch.SyncSuppressions()
	require.Len(t, rows, 1)
	require.NotEqual(t, true, rows[0]["stale"], "while the adapter really is blocked, say so")

	blocker.Clear("codex")
	rows = orch.SyncSuppressions()
	require.Len(t, rows, 1)
	require.Equal(t, true, rows[0]["stale"],
		"the block is gone and must not be reported as a current false alarm")
}
