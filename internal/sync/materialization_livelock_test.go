package syncd

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// --- the overflow give-up must not be gated on an unrelated quota ---

// The whole-target reconciliation is FAIL-FAST: it returns on the first
// per-artifact error, so a single legitimately withheld artifact aborts it at
// the same position on every pass. Its give-up is the only thing that ends
// that, and gating the give-up on the escalation rate budget made it reachable
// only while an unrelated quota happened to be free — a quota the design's own
// steady state keeps spent for ~24h at a time.
func TestOverflowGiveUp_IsNotGatedOnTheEscalationBudget(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, _ := newDeclineOrchestrator(t, target)

	now := time.Now().UTC()
	// The device's whole allowance is already spent on other artifacts.
	spent := newDeferredMaterializationQueue()
	for i := range deferredEscalationsPerWindow {
		spent.abandoned = append(spent.abandoned, abandonedMaterialization{
			artifactID:  fmt.Sprintf("artifact-spent-%d", i),
			abandonedAt: now.Add(-time.Hour),
		})
	}

	overflowed := newDeferredMaterializationQueue()
	overflowed.overflow = true
	overflowed.overflowAttempts = deferredMaterializationMaxAttempts
	overflowed.overflowFirstDeferred = now.Add(-2 * deferredMaterializationMaxAge)

	orch.deferredMaterializeMu.Lock()
	orch.deferredMaterialize = map[string]*deferredMaterializationQueue{
		"codex":       spent,
		"claude-code": overflowed,
	}
	budget := escalationsInWindow(orch.deferredMaterialize, now)
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, deferredEscalationsPerWindow, budget, "precondition: the budget is full")

	orch.recordDeferredMaterializationFailure(
		"claude-code", overflowed, true, "", deferredMaterializationEntry{},
		ErrInboundNativeMaterialization)

	orch.deferredMaterializeMu.Lock()
	stillOverflowed := overflowed.overflow
	gaveUp := len(overflowed.abandoned)
	orch.deferredMaterializeMu.Unlock()
	require.False(t, stillOverflowed,
		"a terminal state must never be reachable only when an unrelated quota is free")
	require.Equal(t, 1, gaveUp,
		"and it must be recorded, not silently discarded")
}

// --- the held population must stay O(1), not O(N) ---

// Entries the rate budget turns back stay in the retry queue by design: they
// are counted, not dropped. That makes the RETAINED population the thing whose
// rate matters, and per-entry pacing alone makes it proportional to N.
//
// Design rule 3: the quarantine breaker is 3 failures / 10 minutes per adapter
// and blocks ALL materialization including live sync. 100 held entries at one
// retry each per hour is 16.7 passes per 10 minutes; a single Export failure per
// pass therefore trips the breaker, quarantine withholds everything for 10
// minutes, clears, and the burst repeats — the permanent quarantine cycle
// convergence.go was written to end.
func TestDeferredMaterializationEscalation_HeldRetriesArePacedAsAPopulation(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, _ := newDeclineOrchestrator(t, target)

	const heldCount = 100
	ids := make([]string, 0, heldCount)
	for i := range heldCount {
		ids = append(ids, fmt.Sprintf("artifact-%03d", i))
	}
	queue := escalationTestQueue(orch, "claude-code", ids...)

	now := time.Now().UTC()
	for _, id := range ids {
		escalationTestEntry(orch, queue, id, func(e *deferredMaterializationEntry) {
			e.attempts = deferredMaterializationMaxAttempts
			e.firstDeferred = now.Add(-2 * deferredMaterializationMaxAge)
			e.quietSince = now.Add(-2 * deferredMaterializationQuietFor)
		})
	}
	for _, id := range ids {
		orch.deferredMaterializeMu.Lock()
		entry := queue.entries[id]
		orch.deferredMaterializeMu.Unlock()
		orch.recordDeferredMaterializationFailure(
			"claude-code", queue, false, id, entry, ErrInboundNativeMaterialization)
	}

	orch.deferredMaterializeMu.Lock()
	due := make([]time.Time, 0, len(queue.entries))
	held := 0
	for _, entry := range queue.entries {
		if entry.escalationHeld {
			held++
			due = append(due, entry.nextAttempt)
		}
	}
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, heldCount-deferredEscalationsPerWindow, held,
		"everything over the cap is held, not dropped")

	// Worst case retries in ANY 10-minute window, measured from the schedule the
	// queue actually produced.
	worst := 0
	for _, anchor := range due {
		inWindow := 0
		for _, other := range due {
			if !other.Before(anchor) && other.Sub(anchor) < 10*time.Minute {
				inWindow++
			}
		}
		if inWindow > worst {
			worst = inWindow
		}
	}
	require.Less(t, worst, 3,
		"held retries must stay under the quarantine breaker's 3-per-10-minutes, "+
			"however many entries are held (measured worst case: %d)", worst)
}

// An unblock fires on every daemon start. Clearing the held schedule there would
// collapse the whole held backlog into one burst each time, which is the same
// O(N) failure rate by another route.
func TestResumeAfterUnblock_DoesNotCollapseTheHeldSchedule(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, _ := newDeclineOrchestrator(t, target)
	queue := escalationTestQueue(orch, "claude-code", "artifact-held", "artifact-plain")

	scheduled := time.Now().UTC().Add(3 * time.Hour)
	escalationTestEntry(orch, queue, "artifact-held", func(e *deferredMaterializationEntry) {
		e.escalationHeld = true
		e.nextAttempt = scheduled
	})
	escalationTestEntry(orch, queue, "artifact-plain", func(e *deferredMaterializationEntry) {
		e.nextAttempt = scheduled
	})

	orch.resumeDeferredMaterializationAfterUnblock("claude-code")

	orch.deferredMaterializeMu.Lock()
	heldEntry := queue.entries["artifact-held"]
	plainEntry := queue.entries["artifact-plain"]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, scheduled, heldEntry.nextAttempt,
		"a held entry keeps its population slot across an unblock")
	require.True(t, plainEntry.nextAttempt.IsZero(),
		"an ordinary entry still earns its immediate retry")
}

// A restart must not make the whole held backlog due at once either.
func TestLoadDeferredMaterializationQueues_RepacesHeldEntries(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	queue := newDeferredMaterializationQueue()
	now := time.Now().UTC()
	for i := range 10 {
		id := fmt.Sprintf("artifact-%02d", i)
		queue.generation++
		queue.ids = append(queue.ids, id)
		queue.entries[id] = deferredMaterializationEntry{
			version: queue.generation, firstDeferred: now.Add(-30 * time.Hour),
			quietSince: now.Add(-6 * time.Hour), escalationHeld: true,
		}
	}
	require.NoError(t, writeDeferredMaterializationQueues(
		store.Root, map[string]*deferredMaterializationQueue{"claude-code": queue}))

	reloaded, err := loadDeferredMaterializationQueues(store.Root)
	require.NoError(t, err)
	restored := reloaded["claude-code"]
	require.NotNil(t, restored)

	seen := map[time.Time]bool{}
	for _, entry := range restored.entries {
		require.True(t, entry.escalationHeld, "the hold decision must survive a restart")
		require.False(t, entry.nextAttempt.IsZero(),
			"or a restart would make every held entry due at once")
		require.False(t, seen[entry.nextAttempt], "held slots must be distinct")
		seen[entry.nextAttempt] = true
	}
}

// --- the quiescence clock must reach disk on a withheld pass ---

// The D2 population is withheld on EVERY pass, so a persist rule of
// "!withheld || abandoned" never fired for it and quietSince stayed at its
// on-disk zero value forever. Escalation then required 6h of persisted age PLUS
// 2h of uninterrupted uptime, so a daemon restarting more often than every 2h
// could never give up — restart frequency deciding whether the system can
// terminate is the exact defect the age+quiescence rule replaced.
func TestRecordDeferredMaterializationFailure_JournalsQuiescenceOnAWithheldPass(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "codex", "", "team-notes", filepath.Join(root, "AGENTS.md"))
	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1)
	artifactID := memories[0].ArtifactID

	blocker := NewAdapterBlocker(map[string]string{
		"claude-code": "native safety backup verification pending",
	})
	orch, err := NewOrchestrator(Config{
		Dir:            root,
		Store:          store,
		Adapters:       []adapter.Adapter{&fakeConvSource{name: "codex"}, &fakeConvSource{name: "claude-code"}},
		AdapterBlocker: blocker,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	queue := escalationTestQueue(orch, "claude-code", artifactID)
	escalationTestEntry(orch, queue, artifactID, func(e *deferredMaterializationEntry) {
		e.firstDeferred = time.Now().UTC().Add(-30 * time.Hour)
	})

	orch.deferredMaterializeMu.Lock()
	entry := queue.entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	gateErr := orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
	require.True(t, deferredMaterializationWithheld(gateErr),
		"precondition: this pass never reaches the adapter")
	orch.recordDeferredMaterializationFailure("claude-code", queue, false, artifactID, entry, gateErr)

	// Exactly what a restart does to this state.
	reloaded, err := loadDeferredMaterializationQueues(store.Root)
	require.NoError(t, err)
	require.Contains(t, reloaded, "claude-code")
	persisted := reloaded["claude-code"].entries[artifactID]
	require.False(t, persisted.quietSince.IsZero(),
		"the only clock this population will ever have must reach disk")
	require.Equal(t, ReasonAdapterBlockedSafety, persisted.withheldReason)
}

// destPath is memory-only, so after a restart the first pass has no destination
// to witness. Comparing a zero witness against the first real one read as "the
// destination changed" and restarted the quiescence clock — on a path that
// fires on every daemon start.
func TestObserveDeferredMaterializationInputs_RestartDoesNotResetTheClock(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	orch := &Orchestrator{cfg: Config{Store: store}}

	dest := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(dest, []byte("one\n"), 0o644))

	art := acf.Artifact{
		ArtifactID: "artifact-conv", Kind: acf.KindConversation, HeadEventHash: "sha256:one",
	}
	queue := escalationTestQueue(orch, "claude-code", art.ArtifactID)

	// The state a restart restores: quiescence clock and head hash from the
	// journal, no destination (it is never persisted).
	established := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	escalationTestEntry(orch, queue, art.ArtifactID, func(e *deferredMaterializationEntry) {
		e.quietSince = established
		e.lastHeadHash = "sha256:one"
	})

	// Pass A: no destination known yet, so nothing about it can have changed.
	require.False(t, orch.observeDeferredMaterializationInputs(
		"claude-code", art, established.Add(time.Hour)))
	orch.deferredMaterializeMu.Lock()
	entry := queue.entries[art.ArtifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, established, entry.quietSince,
		"a pass that learned nothing must not restart the clock")

	// The real attempt runs and the typed decline hands over the destination.
	classifyDeferredMaterializationCause(&entry, newConversationDeclineError(
		"claude-code", art.ArtifactID, acf.MainBranch, adapter.SessionDeclineForkedMirror, dest))
	orch.deferredMaterializeMu.Lock()
	queue.entries[art.ArtifactID] = entry
	orch.deferredMaterializeMu.Unlock()

	// Pass B: the destination is unchanged on disk, so the clock must hold.
	require.True(t, orch.observeDeferredMaterializationInputs(
		"claude-code", art, established.Add(2*time.Hour)))
	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[art.ArtifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, established, entry.quietSince,
		"learning a destination for the first time is not the destination changing")
}

// countingEventPublisher records how many times each live event was published.
type countingEventPublisher struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *countingEventPublisher) Publish(kind string, _ any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[kind]++
}

func (c *countingEventPublisher) count(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[kind]
}

// --- escalation must be terminal per (artifact, reason) ---

// The rate budget bounds the RATE of new needs_attention rows; nothing bounded
// the TOTAL. A re-admitted entry that re-declined for the same reason spent a
// fresh escalation, so a device with a permanently stuck population emitted
// three give-up events a day about the same artifacts forever, and almost none
// of them were actionable.
func TestDeferredMaterializationEscalation_SameCauseIsRaisedOnce(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, _ := newDeclineOrchestrator(t, target)

	sink := &countingEventPublisher{}
	orch.cfg.EventPublisher = sink
	events := func() int { return sink.count("conversation.materialize_gave_up") }

	now := time.Now().UTC()
	queue := escalationTestQueue(orch, "claude-code", "artifact-stuck")
	age := func(e *deferredMaterializationEntry) {
		e.attempts = deferredMaterializationMaxAttempts
		e.firstDeferred = now.Add(-2 * deferredMaterializationMaxAge)
		e.quietSince = now.Add(-2 * deferredMaterializationQuietFor)
		e.declineReason = adapter.SessionDeclineForkedMirror
	}
	escalationTestEntry(orch, queue, "artifact-stuck", age)

	orch.deferredMaterializeMu.Lock()
	entry := queue.entries["artifact-stuck"]
	orch.deferredMaterializeMu.Unlock()
	orch.recordDeferredMaterializationFailure(
		"claude-code", queue, false, "artifact-stuck", entry,
		newConversationDeclineError("claude-code", "artifact-stuck", acf.MainBranch,
			adapter.SessionDeclineForkedMirror, target.dest))

	orch.deferredMaterializeMu.Lock()
	firstRaise := queue.abandoned[0].abandonedAt
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, 1, events())

	// The sweep re-admits it a day later and it declines for the same reason.
	for round := range 5 {
		requeueEscalationTestEntry(orch, queue, "artifact-stuck", "codex")
		escalationTestEntry(orch, queue, "artifact-stuck", age)
		orch.deferredMaterializeMu.Lock()
		entry = queue.entries["artifact-stuck"]
		orch.deferredMaterializeMu.Unlock()
		orch.recordDeferredMaterializationFailure(
			"claude-code", queue, false, "artifact-stuck", entry,
			newConversationDeclineError("claude-code", "artifact-stuck", acf.MainBranch,
				adapter.SessionDeclineForkedMirror, target.dest))
		require.Equal(t, 1, events(),
			"round %d re-raised a give-up the operator has already been told about", round)
	}

	orch.deferredMaterializeMu.Lock()
	require.Len(t, queue.abandoned, 1)
	require.Equal(t, firstRaise, queue.abandoned[0].abandonedAt,
		"a standing record keeps its FIRST-raised time, so it ages out of the rate "+
			"budget instead of consuming the allowance a genuinely new cause needs")
	orch.deferredMaterializeMu.Unlock()
}

// A DIFFERENT cause on the same artifact is genuinely new information and must
// still be raised.
func TestDeferredMaterializationEscalation_ANewCauseIsRaisedAgain(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, _ := newDeclineOrchestrator(t, target)

	now := time.Now().UTC()
	queue := escalationTestQueue(orch, "claude-code", "artifact-stuck")
	raise := func(reason adapter.SessionDeclineReason) {
		escalationTestEntry(orch, queue, "artifact-stuck", func(e *deferredMaterializationEntry) {
			e.attempts = deferredMaterializationMaxAttempts
			e.firstDeferred = now.Add(-2 * deferredMaterializationMaxAge)
			e.quietSince = now.Add(-2 * deferredMaterializationQuietFor)
		})
		orch.deferredMaterializeMu.Lock()
		entry := queue.entries["artifact-stuck"]
		orch.deferredMaterializeMu.Unlock()
		orch.recordDeferredMaterializationFailure(
			"claude-code", queue, false, "artifact-stuck", entry,
			newConversationDeclineError("claude-code", "artifact-stuck", acf.MainBranch,
				reason, target.dest))
	}

	raise(adapter.SessionDeclineForkedMirror)
	orch.deferMaterialization("claude-code", "artifact-stuck", "codex", false, false, false)
	raise(adapter.SessionDeclineMirrorDiverged)

	orch.deferredMaterializeMu.Lock()
	defer orch.deferredMaterializeMu.Unlock()
	require.Len(t, queue.abandoned, 1)
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, queue.abandoned[0].declineReason,
		"a changed cause is news and must be raised with its own classification")
}

// --- a blocked adapter must not make the terminal state unreachable ---

// startNativeStartupSafety re-arms the block for every agent on every daemon
// start, and an agent whose safety snapshot never verifies stays blocked
// indefinitely. The drain used to refuse to run at all while that held — and the
// drain is the ONLY thing that folds an entry's quiescence clock and evaluates
// its escalation rule, so the gate branch that exists for exactly this reason
// was unreachable from the code path that would use it.
func TestDeferredMaterializationDrain_BlockedTargetStillReachesTheGate(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	seedMemoryWithOrigin(t, store, "codex", "", "team-notes", filepath.Join(root, "AGENTS.md"))
	memories, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Len(t, memories, 1)
	artifactID := memories[0].ArtifactID

	blocker := NewAdapterBlocker(map[string]string{
		"claude-code": "native safety backup verification pending",
	})
	orch, err := NewOrchestrator(Config{
		Dir:            root,
		Store:          store,
		Adapters:       []adapter.Adapter{&fakeConvSource{name: "codex"}, &fakeConvSource{name: "claude-code"}},
		AdapterBlocker: blocker,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	art, err := store.ReadArtifact(acf.KindMemory, artifactID)
	require.NoError(t, err)

	queue := escalationTestQueue(orch, "claude-code", artifactID)
	escalationTestEntry(orch, queue, artifactID, func(e *deferredMaterializationEntry) {
		e.firstDeferred = time.Now().UTC().Add(-30 * time.Hour)
		e.quietSince = time.Now().UTC().Add(-2 * deferredMaterializationQuietFor)
		// The clock only survives a pass that observes no change, and the first
		// observation of an unseeded entry always looks like one.
		e.lastHeadHash = art.HeadEventHash
	})

	// Drive the REAL drain entry point, not materializeDeferredArtifact: the
	// early return that made this unreachable lived in the drain.
	orch.scheduleDeferredMaterializationDrain("claude-code")
	require.Eventually(t, func() bool {
		orch.deferredMaterializeMu.Lock()
		defer orch.deferredMaterializeMu.Unlock()
		return len(queue.abandoned) == 1
	}, 5*time.Second, 20*time.Millisecond,
		"a write no gate ever lets through must still reach a terminal state")

	rows := orch.DeferredMaterializations()
	require.Len(t, rows, 1)
	require.Equal(t, "needs_attention", rows[0]["state"])
	require.Equal(t, ReasonAdapterBlockedSafety.Remedy(), rows[0]["remedy"])
}

// --- the suppression snapshot must not be a write ---

// SnapshotAt held the ledger's mutex across the liveness callback, and that
// mutex is on the fan-out hot path. A verifier that blocks — or, as the shipped
// one did, runs a native safety pass — therefore stalled ALL cross-agent sync
// for its duration. Recording a suppression from inside the callback deadlocks
// against the old shape and completes against the new one.
func TestSuppressionSnapshotAt_DoesNotHoldTheLedgerLockAcrossTheVerifier(t *testing.T) {
	ledger := newSuppressionLedger()
	now := time.Now().UTC()
	ledger.record("claude-code", ReasonAdapterBlockedSafety, "artifact-1", now)

	done := make(chan []SuppressionSnapshot, 1)
	go func() {
		done <- ledger.SnapshotAt(now, func(string, SuppressionReason) (bool, bool) {
			// The fan-out hot path, running concurrently with a status read.
			ledger.record("codex", ReasonRulesDenied, "artifact-2", now)
			return true, true
		})
	}()
	select {
	case snaps := <-done:
		require.NotEmpty(t, snaps)
	case <-time.After(5 * time.Second):
		t.Fatal("SnapshotAt held the fan-out mutex across the liveness verifier")
	}
}

// The shipped verifier itself must be side-effect free. runtimeAdapterAvailable
// fires the runtime activation hook, which on this daemon verifies and can
// rebuild native safety snapshots and BLOCK the adapter — from a status read.
func TestSuppressionConditionLive_AdapterNotInstalledDoesNotActivate(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	discoverable := &runtimeDiscoverableTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	var activations int
	var mu sync.Mutex
	orch, err := NewOrchestrator(Config{
		Dir:                     root,
		Store:                   store,
		Adapters:                []adapter.Adapter{discoverable},
		DynamicAdapterDiscovery: true,
		RuntimeAdapterActivated: func(string, adapter.Discovery) {
			mu.Lock()
			activations++
			mu.Unlock()
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	live, known := orch.suppressionConditionLive("claude-code", ReasonAdapterNotInstalled)
	require.True(t, known)
	require.False(t, live, "the agent is installed, so the row is no longer current")

	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, activations,
		"asking a question must never run the native startup-safety pass")
}

// runtimeDiscoverableTarget is installed and runtime-discoverable, so
// runtimeAdapterAvailable would fire the activation hook for it.
type runtimeDiscoverableTarget struct {
	fakeConvSource
}

func (r *runtimeDiscoverableTarget) Discover() (adapter.Discovery, error) {
	return adapter.Discovery{Installed: true, RuntimeToken: "v1"}, nil
}

func (r *runtimeDiscoverableTarget) CandidateDiscovery() adapter.Discovery {
	return adapter.Discovery{Installed: true, RuntimeToken: "v1"}
}

var _ adapter.RuntimeDiscoverable = (*runtimeDiscoverableTarget)(nil)
