package syncd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQuarantine_NotQuarantinedByDefault(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	require.False(t, q.IsQuarantined("codex", time.Now()))
}

func TestQuarantine_TripsAtThreshold(t *testing.T) {
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	q := NewQuarantineTracker(3, 10*time.Minute)

	require.False(t, q.RecordFailure("codex", now))
	require.False(t, q.RecordFailure("codex", now.Add(time.Minute)))
	just := q.RecordFailure("codex", now.Add(2*time.Minute))
	require.True(t, just, "third failure within window must trip quarantine")

	require.True(t, q.IsQuarantined("codex", now.Add(3*time.Minute)))
}

func TestQuarantine_DoesNotTripBelowThreshold(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	q.RecordFailure("codex", time.Now())
	q.RecordFailure("codex", time.Now())
	require.False(t, q.IsQuarantined("codex", time.Now()))
}

func TestQuarantine_PrunesOutsideWindow(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	base := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)

	// Two failures BEFORE the window; one INSIDE. Should not trip.
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base.Add(time.Minute))
	q.RecordFailure("codex", base.Add(11*time.Minute)) // first two are now out-of-window

	require.False(t, q.IsQuarantined("codex", base.Add(11*time.Minute)),
		"failures outside the sliding window must NOT count toward threshold")
}

func TestQuarantine_SelfHealsAfterWindow(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	base := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)

	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	require.True(t, q.IsQuarantined("codex", base))

	// 11 minutes later, the quarantine has self-cleared.
	require.False(t, q.IsQuarantined("codex", base.Add(11*time.Minute)))
}

func TestQuarantine_RecordSuccessDoesNotUnquarantine(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	base := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	require.True(t, q.IsQuarantined("codex", base))

	q.RecordSuccess("codex")
	require.True(t, q.IsQuarantined("codex", base),
		"RecordSuccess clears failure history but does NOT auto-unquarantine — explicit Clear or window expiry is required")
}

func TestQuarantine_Clear(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	base := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	require.True(t, q.IsQuarantined("codex", base))

	q.Clear("codex")
	require.False(t, q.IsQuarantined("codex", base))
}

func TestQuarantine_RecordFailure_OnlyReportsJustQuarantinedOnce(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	base := time.Now()
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	require.True(t, q.RecordFailure("codex", base), "third → just-quarantined")
	require.False(t, q.RecordFailure("codex", base), "fourth → already quarantined")
}

func TestQuarantine_Snapshot(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	base := time.Now()
	for _, n := range []string{"codex", "kilo"} {
		q.RecordFailure(n, base)
		q.RecordFailure(n, base)
		q.RecordFailure(n, base)
	}
	snap := q.Snapshot(base)
	require.ElementsMatch(t, []string{"codex", "kilo"}, snap)
}

func TestQuarantine_PerAdapterIndependence(t *testing.T) {
	q := NewQuarantineTracker(3, 10*time.Minute)
	base := time.Now()
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	q.RecordFailure("codex", base)
	require.True(t, q.IsQuarantined("codex", base))
	require.False(t, q.IsQuarantined("kilo", base))
}
