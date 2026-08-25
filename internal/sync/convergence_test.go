package syncd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func convergenceTestOrchestrator(t *testing.T) *Orchestrator {
	t.Helper()
	store := &acf.Store{Root: t.TempDir()}
	o := &Orchestrator{}
	o.cfg.Store = store
	o.deferredMaterialize = map[string]*deferredMaterializationQueue{}
	return o
}

// TestConvergenceQuiescentDeviceWorkDecays is the R5 guarantee. A quiescent
// device does NOT stop sweeping entirely — it must still find drift the
// daemon never observed (an external edit, a restore, a partial disk failure),
// which is why the backoff has a ceiling rather than an off switch. What it
// must do is spend exponentially less: sweep, then wait twice as long, up to
// the cap. This test pins that decay.
func TestConvergenceQuiescentDeviceWorkDecays(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	o.markConvergenceBaseline(start)
	fp, _ := o.storeFingerprint()

	// Just after a sweep: nothing to do.
	worth, _, _ := o.convergenceWorthSweeping(start.Add(time.Minute))
	assert.False(t, worth, "no sweep inside the current interval")

	// Once the current interval elapses, one verification sweep runs...
	worth, _, _ = o.convergenceWorthSweeping(start.Add(convergenceSweepMinInterval + time.Minute))
	assert.True(t, worth, "a periodic verification sweep still happens")

	// ...and because it found nothing, the next one is twice as far away.
	o.noteConvergenceSweep(start.Add(convergenceSweepMinInterval), fp, true, false)
	o.noteConvergenceSweep(start.Add(convergenceSweepMinInterval), fp, true, false)
	next := start.Add(convergenceSweepMinInterval)

	worth, _, _ = o.convergenceWorthSweeping(next.Add(convergenceSweepMinInterval + time.Minute))
	assert.False(t, worth, "a clean device waits longer each time — work decays")
}

// TestConvergenceSweepsWhenWorkIsOwed: the case that matters for the 84 stuck
// writes. A target owing work must be swept even when the store is unchanged,
// because those entries exhausted their retry budget and nothing else will
// ever drive them.
func TestConvergenceSweepsWhenWorkIsOwed(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	o.markConvergenceBaseline(start)

	q := newDeferredMaterializationQueue()
	q.abandoned = append(q.abandoned, abandonedMaterialization{
		artifactID: "019f-stuck", attempts: 64, abandonedAt: start,
	})
	o.deferredMaterialize["claude-code"] = q

	worth, _, _ := o.convergenceWorthSweeping(start.Add(convergenceSweepMinInterval + time.Second))
	assert.True(t, worth, "a write that exhausted its budget must still be revisited")
}

// TestConvergenceRespectsMinimumInterval is the R2 guarantee at this layer:
// even with work permanently owed, sweeps cannot run more often than the
// floor. Without this a permanently broken target would become a hot loop —
// exactly the "retries indefinitely and consumes resources" failure mode.
func TestConvergenceRespectsMinimumInterval(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	o.markConvergenceBaseline(start)

	q := newDeferredMaterializationQueue()
	q.abandoned = append(q.abandoned, abandonedMaterialization{artifactID: "stuck", attempts: 64})
	o.deferredMaterialize["codex"] = q

	worth, _, _ := o.convergenceWorthSweeping(start.Add(convergenceSweepMinInterval - time.Second))
	assert.False(t, worth, "the floor bounds sweep frequency even with work permanently owed")
}

// TestConvergenceSweepsWhenStoreChanged: new canonical content is the ordinary
// trigger.
func TestConvergenceSweepsWhenStoreChanged(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	o.markConvergenceBaseline(start)

	o.mu.Lock()
	o.convergence.lastFingerprint = convergenceFingerprint{artifacts: 99, latest: start}
	o.mu.Unlock()

	worth, _, _ := o.convergenceWorthSweeping(start.Add(convergenceSweepMinInterval + time.Second))
	assert.True(t, worth, "a changed store must be reconciled")
}

// TestConvergenceBackoffDoublesWhenCleanAndResetsOnDrift pins the adaptive
// policy: a healthy device converges toward near-zero work, and the moment
// drift appears it tightens back to the floor.
func TestConvergenceBackoffDoublesWhenCleanAndResetsOnDrift(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	fp := convergenceFingerprint{artifacts: 3, latest: now}

	o.noteConvergenceSweep(now, fp, true, false)
	o.mu.Lock()
	first := o.convergence.nextInterval
	o.mu.Unlock()
	assert.Equal(t, convergenceSweepMinInterval, first)

	o.noteConvergenceSweep(now, fp, true, false)
	o.mu.Lock()
	second := o.convergence.nextInterval
	o.mu.Unlock()
	assert.Equal(t, convergenceSweepMinInterval*2, second, "a clean sweep backs off")

	o.noteConvergenceSweep(now, fp, true, true)
	o.mu.Lock()
	afterDrift := o.convergence.nextInterval
	o.mu.Unlock()
	assert.Equal(t, convergenceSweepMinInterval, afterDrift, "drift tightens the interval")
}

// TestConvergenceBackoffIsCapped keeps the ceiling honest: drift the daemon
// never observed must still be found within a bounded window.
func TestConvergenceBackoffIsCapped(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	fp := convergenceFingerprint{artifacts: 1, latest: now}
	for i := 0; i < 40; i++ {
		o.noteConvergenceSweep(now, fp, true, false)
	}
	o.mu.Lock()
	got := o.convergence.nextInterval
	o.mu.Unlock()
	assert.Equal(t, convergenceSweepMaxInterval, got, "backoff is capped, never unbounded")
}

// TestConvergenceFirstTickDefersToStartupReconciliation: the daemon already
// reconciles at boot. Sweeping again immediately would duplicate that work on
// every restart.
func TestConvergenceFirstTickDefersToStartupReconciliation(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	worth, _, _ := o.convergenceWorthSweeping(now)
	assert.False(t, worth, "startup reconciliation covers first boot")
}

// TestConvergenceFingerprintDetectsChange covers the cheap change detector
// itself against a real store.
func TestConvergenceFingerprintDetectsChange(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	before, ok := o.storeFingerprint()
	require.True(t, ok)
	assert.Equal(t, 0, before.artifacts)

	require.NoError(t, o.cfg.Store.WriteArtifact(acf.Artifact{
		ArtifactID: "019f-test", Kind: acf.KindMemory, Name: "n",
		UpdatedAt: time.Now().UTC(),
	}))

	after, ok := o.storeFingerprint()
	require.True(t, ok)
	assert.Equal(t, 1, after.artifacts)
	assert.False(t, before.equal(after), "a new artifact must change the fingerprint")
}

// TestConvergenceRegistersAsBackgroundWork pins the shutdown contract. A sweep
// writes native agent files, and Close promises no orchestrator goroutine is
// still touching watched roots once it returns. The first version of this loop
// ran as a bare goroutine, so Close could return while a sweep was mid-write.
func TestConvergenceRegistersAsBackgroundWork(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	o.bgDone = make(chan struct{})

	// A closing orchestrator must refuse the registration outright rather
	// than starting a sweep that Close cannot join.
	o.bgMu.Lock()
	o.bgClosing = true
	o.bgMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.RunConvergence(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunConvergence must return immediately when the orchestrator is closing")
	}
}

// TestConvergenceStopsOnBgDone: Close can happen while the caller's context is
// still live. Without selecting on bgDone the loop would keep walking the
// store every tick against a closed orchestrator.
func TestConvergenceStopsOnBgDone(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	o.bgDone = make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.RunConvergence(context.Background())
	}()
	// Let it register and start ticking, then signal shutdown WITHOUT
	// cancelling the context.
	time.Sleep(50 * time.Millisecond)
	close(o.bgDone)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunConvergence must stop on bgDone even with a live context")
	}
}

// TestConvergenceReadmitIsBoundedPerSweep is the regression test for the
// quarantine storm. The first shipped version drove a whole-store fan-out and
// could trip the quarantine breaker, blocking sync. A sweep must re-admit
// fewer writes than that breaker's threshold so a broken write is retried
// steadily instead of taking every adapter down with it.
func TestConvergenceReadmitIsBoundedPerSweep(t *testing.T) {
	assert.Less(t, convergenceReadmitPerSweep, 3,
		"a sweep must stay under the quarantine threshold of 3 failures per window")

	o := convergenceTestOrchestrator(t)
	q := newDeferredMaterializationQueue()
	for i := 0; i < 50; i++ {
		q.abandoned = append(q.abandoned, abandonedMaterialization{
			artifactID: fmt.Sprintf("019f-stuck-%02d", i), attempts: 64,
		})
	}
	o.deferredMaterialize["claude-code"] = q

	n := o.readmitStuckMaterializations(time.Now().UTC())
	assert.Equal(t, convergenceReadmitPerSweep, n,
		"a sweep re-admits a bounded batch, never the whole backlog")
}

// TestConvergenceReadmitSkipsQueuedAndWholeTarget: re-admitting a pair that is
// already mid-retry would reset its budget and could keep it retrying forever,
// which is the failure the owner explicitly forbade. Whole-target give-up
// records carry no artifact id and belong to the overflow path.
func TestConvergenceReadmitSkipsQueuedAndWholeTarget(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	q := newDeferredMaterializationQueue()
	q.abandoned = append(q.abandoned,
		abandonedMaterialization{artifactID: "", attempts: 64},        // whole-target
		abandonedMaterialization{artifactID: "already", attempts: 64}, // mid-retry
	)
	q.entries["already"] = deferredMaterializationEntry{}
	q.ids = append(q.ids, "already")
	o.deferredMaterialize["codex"] = q

	assert.Zero(t, o.readmitStuckMaterializations(time.Now().UTC()),
		"neither a whole-target record nor an already-queued pair is re-admitted")
}

// TestConvergenceReadmitKeepsTheGiveUpRecordRaised: re-admission means "we will
// try this again", NOT "this is fixed". The record must stay raised until the
// write actually succeeds.
//
// It used to be deleted here, and the arithmetic of that is decisive: this
// sweep can delete 2 records every 15 minutes (192/day) against a creation cap
// of 3 per rolling 24h — a 64x mismatch, so a needs_attention row survived
// roughly 45 of every 1440 minutes. Requirement (a) asks for a raised flag;
// raising one and having a sibling subsystem lower it with the condition
// unresolved is not raising it.
func TestConvergenceReadmitKeepsTheGiveUpRecordRaised(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	now := time.Now().UTC()
	gaveUpAt := now.Add(-convergenceReadmitMinDwell - time.Hour)
	q := newDeferredMaterializationQueue()
	q.abandoned = append(q.abandoned, abandonedMaterialization{
		artifactID: "019f-one", attempts: 64, abandonedAt: gaveUpAt,
		declineReason: adapter.SessionDeclineForkedMirror,
	})
	o.deferredMaterialize["kilo"] = q

	require.Equal(t, 1, o.readmitStuckMaterializations(now))

	o.deferredMaterializeMu.Lock()
	remaining := len(o.deferredMaterialize["kilo"].abandoned)
	entry, queued := o.deferredMaterialize["kilo"].entries["019f-one"]
	o.deferredMaterializeMu.Unlock()
	require.True(t, queued, "and it really is queued again")
	assert.Equal(t, 1, remaining, "the flag stays raised until the write succeeds")
	assert.Equal(t, gaveUpAt, entry.lastEscalatedAt,
		"the fresh entry must carry forward that this device already raised it")
	assert.Equal(t, adapter.SessionDeclineForkedMirror, entry.escalatedReason,
		"and for what, so re-declining for the same cause spends no new escalation")
}

// A record that was raised moments ago is not re-admitted: the sweep's job is
// to drive writes nothing else will, not to churn the ones the live fan-out
// already re-defers on every commit.
func TestConvergenceReadmitRequiresDwell(t *testing.T) {
	o := convergenceTestOrchestrator(t)
	now := time.Now().UTC()
	q := newDeferredMaterializationQueue()
	q.abandoned = append(q.abandoned, abandonedMaterialization{
		artifactID: "019f-fresh", attempts: 64, abandonedAt: now.Add(-time.Hour),
	})
	o.deferredMaterialize["kilo"] = q

	assert.Zero(t, o.readmitStuckMaterializations(now),
		"a give-up raised an hour ago has not stood long enough to be re-driven")
}
