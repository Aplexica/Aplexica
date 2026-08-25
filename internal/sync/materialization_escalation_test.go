package syncd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// escalationTestQueue installs a queue directly instead of going through
// deferMaterialization, which would also start a background drain and race the
// deterministic single-step calls these tests make.
func escalationTestQueue(o *Orchestrator, agent string, ids ...string) *deferredMaterializationQueue {
	queue := newDeferredMaterializationQueue()
	o.deferredMaterializeMu.Lock()
	if o.deferredMaterialize == nil {
		o.deferredMaterialize = map[string]*deferredMaterializationQueue{}
	}
	for _, id := range ids {
		queue.generation++
		queue.ids = append(queue.ids, id)
		queue.entries[id] = deferredMaterializationEntry{version: queue.generation, originAgent: "codex"}
	}
	o.deferredMaterialize[agent] = queue
	o.deferredMaterializeMu.Unlock()
	return queue
}

func escalationTestEntry(o *Orchestrator, queue *deferredMaterializationQueue, id string, mutate func(*deferredMaterializationEntry)) deferredMaterializationEntry {
	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	entry := queue.entries[id]
	mutate(&entry)
	queue.entries[id] = entry
	return entry
}

// requeueEscalationTestEntry mirrors deferMaterialization's fresh-entry path
// without starting a background drain. Tests that single-step escalation state
// must not race a real worker mutating the same queue.
func requeueEscalationTestEntry(o *Orchestrator, queue *deferredMaterializationQueue, id, origin string) {
	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	queue.generation++
	entry := deferredMaterializationEntry{
		version:       queue.generation,
		originAgent:   origin,
		firstDeferred: time.Now().UTC(),
	}
	queue.carryEscalationStampLocked(&entry, id)
	queue.ids = append(queue.ids, id)
	queue.entries[id] = entry
}

// --- Age + quiescence, including the attempts=0 case ---

// The pre-fix budget required charged attempts. A write turned back by a policy
// gate never reaches the adapter and therefore charges nothing, leaving such
// entries at attempts=0 with no terminal state available.
func TestDeferredMaterializationEscalates_AtZeroAttemptsOnceQuiet(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	entry := deferredMaterializationEntry{
		attempts:      0,
		firstDeferred: now.Add(-deferredMaterializationEscalateAge - time.Hour),
		quietSince:    now.Add(-deferredMaterializationQuietFor - time.Hour),
	}
	require.False(t, deferredMaterializationExhausted(entry.attempts, entry.firstDeferred, now),
		"the attempt budget is structurally unreachable for this population")
	require.True(t, deferredMaterializationEscalates(entry, now),
		"an old entry whose inputs have not moved must reach a terminal state")
}

// Quiescence is what keeps the age trigger honest: a conversation someone has
// open in a TUI all day keeps moving its session file, so it is never quiet and
// must never be false-terminaled on elapsed time.
func TestDeferredMaterializationEscalates_NotWhileTheDestinationKeepsMoving(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * time.Hour)

	require.False(t, deferredMaterializationEscalates(deferredMaterializationEntry{
		firstDeferred: old,
		quietSince:    now.Add(-time.Minute),
	}, now), "inputs that changed a minute ago are not quiet")

	require.False(t, deferredMaterializationEscalates(deferredMaterializationEntry{
		firstDeferred: old,
	}, now), "an entry whose inputs were never observed cannot be judged quiet")

	require.False(t, deferredMaterializationEscalates(deferredMaterializationEntry{
		firstDeferred: now.Add(-time.Minute),
		quietSince:    old,
	}, now), "a young entry does not escalate however still it is")
}

// The attempt budget stays a valid independent trigger — it is the one that
// fires for a target that really refuses the write over and over.
func TestDeferredMaterializationEscalates_AttemptBudgetStillFires(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	require.True(t, deferredMaterializationEscalates(deferredMaterializationEntry{
		attempts:      deferredMaterializationMaxAttempts,
		firstDeferred: now.Add(-2 * deferredMaterializationMaxAge),
	}, now), "a fully charged entry escalates without needing a quiescence observation")
}

// D2 in full: an entry that is withheld on every pass, never charges an
// attempt, and survives repeated daemon restarts must still reach a terminal
// state. The restarts are simulated by round-tripping the journal, which is
// exactly what a restart does to this state.
func TestDeferredMaterialization_ZeroAttemptEntryEscalatesAcrossRestarts(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	now := time.Now().UTC()
	queue := newDeferredMaterializationQueue()
	queue.generation = 1
	queue.ids = []string{"artifact-memory"}
	queue.entries["artifact-memory"] = deferredMaterializationEntry{
		version:       1,
		originAgent:   "codex",
		firstDeferred: now.Add(-30 * time.Hour),
		quietSince:    now.Add(-6 * time.Hour),
		lastHeadHash:  "sha256:unchanged",
	}
	byTarget := map[string]*deferredMaterializationQueue{"claude-code": queue}

	// Repeated daemon restarts. startNativeStartupSafety re-arms the block for
	// every agent on every start, so the test must preserve that cadence.
	for range 3 {
		require.NoError(t, writeDeferredMaterializationQueues(store.Root, byTarget))
		reloaded, err := loadDeferredMaterializationQueues(store.Root)
		require.NoError(t, err)
		byTarget = reloaded
		queue = byTarget["claude-code"]
		require.NotNil(t, queue)
		entry := queue.entries["artifact-memory"]
		require.Zero(t, entry.attempts, "a withheld pass never charges an attempt")
		require.False(t, entry.quietSince.IsZero(),
			"the quiescence clock must survive a restart or restarts alone defeat escalation")
	}

	orch := &Orchestrator{deferredMaterialize: byTarget}
	withheld := fmt.Errorf("%w (%w)", ErrInboundNativeMaterialization, errDeferredMaterializationWithheld)
	orch.recordDeferredMaterializationFailure(
		"claude-code", queue, false, "artifact-memory", queue.entries["artifact-memory"], withheld)

	rows := deferredMaterializationRows(byTarget, nil)
	require.Len(t, rows, 1)
	require.Equal(t, "needs_attention", rows[0]["state"],
		"an entry that can never charge an attempt must still reach a terminal state")
	require.Equal(t, 0, rows[0]["attempts"])
}

// Exercise the condition through the real drain entry point: a kind=memory
// artifact whose target is gated on every pass. It charges no
// attempt (so the old budget could never fire), accrues quiescence anyway, and
// escalates with the GATE's own explanation and remedy rather than a generic
// "materialization incomplete" response.
func TestMaterializeDeferredArtifact_GateWithheldMemoryEscalatesWithTheGateRemedy(t *testing.T) {
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

	entry := queue.entries[artifactID]
	gateErr := orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
	require.True(t, deferredMaterializationWithheld(gateErr))
	orch.recordDeferredMaterializationFailure("claude-code", queue, false, artifactID, entry, gateErr)

	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Zero(t, entry.attempts, "a gate turned this back before the adapter saw it")
	require.Equal(t, ReasonAdapterBlockedSafety, entry.withheldReason)
	require.False(t, entry.quietSince.IsZero(),
		"the only clock this population will ever have must be running")

	// Age it past both thresholds and let the next gated pass raise it.
	escalationTestEntry(orch, queue, artifactID, func(e *deferredMaterializationEntry) {
		e.firstDeferred = time.Now().UTC().Add(-2 * deferredMaterializationEscalateAge)
		e.quietSince = time.Now().UTC().Add(-2 * deferredMaterializationQuietFor)
	})
	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	gateErr = orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
	orch.recordDeferredMaterializationFailure("claude-code", queue, false, artifactID, entry, gateErr)

	rows := orch.DeferredMaterializations()
	require.Len(t, rows, 1)
	require.Equal(t, "needs_attention", rows[0]["state"])
	require.Equal(t, 0, rows[0]["attempts"],
		"escalation must not depend on a counter this population can never increment")
	require.Contains(t, rows[0]["explain"], ReasonAdapterBlockedSafety.Explain())
	require.Equal(t, ReasonAdapterBlockedSafety.Remedy(), rows[0]["remedy"])
}

// An attempts=0 entry can be a memory rather than a conversation, so the
// escalation path must not depend on anything conversation-specific.
func TestObserveDeferredMaterializationInputs_TracksNonConversationKinds(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	orch := &Orchestrator{cfg: Config{Store: store}}

	art := acf.Artifact{
		ArtifactID:    "artifact-memory",
		Kind:          acf.KindMemory,
		Scope:         acf.ScopeGlobal,
		HeadEventHash: "sha256:one",
	}
	queue := escalationTestQueue(orch, "claude-code", art.ArtifactID)

	first := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	require.False(t, orch.observeDeferredMaterializationInputs("claude-code", art, first))

	orch.deferredMaterializeMu.Lock()
	entry := queue.entries[art.ArtifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, first, entry.quietSince)
	require.Equal(t, "sha256:one", entry.lastHeadHash)

	second := first.Add(time.Hour)
	require.False(t, orch.observeDeferredMaterializationInputs("claude-code", art, second),
		"a memory artifact names no destination, so nothing proves a retry is pointless")
	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[art.ArtifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, first, entry.quietSince,
		"but an unchanged pass must not restart the quiescence clock — that is what lets it escalate")

	art.HeadEventHash = "sha256:two"
	third := second.Add(time.Hour)
	require.False(t, orch.observeDeferredMaterializationInputs("claude-code", art, third))
	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[art.ArtifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, third, entry.quietSince, "new canonical content restarts the quiescence clock")
}

// A destination the agent is still writing keeps resetting the clock, which is
// the whole protection an open TUI gets.
func TestObserveDeferredMaterializationInputs_MovingDestinationIsNotQuiet(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	orch := &Orchestrator{cfg: Config{Store: store}}

	dest := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(dest, []byte("one\n"), 0o644))

	art := acf.Artifact{
		ArtifactID:    "artifact-conv",
		Kind:          acf.KindConversation,
		HeadEventHash: "sha256:one",
	}
	queue := escalationTestQueue(orch, "claude-code", art.ArtifactID)
	escalationTestEntry(orch, queue, art.ArtifactID, func(e *deferredMaterializationEntry) {
		e.destPath = dest
		e.declineObserved = true
	})

	base := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	require.False(t, orch.observeDeferredMaterializationInputs("claude-code", art, base))
	require.True(t, orch.observeDeferredMaterializationInputs("claude-code", art, base.Add(time.Hour)))

	require.NoError(t, os.WriteFile(dest, []byte("one\ntwo\n"), 0o644))
	moved := base.Add(2 * time.Hour)
	require.False(t, orch.observeDeferredMaterializationInputs("claude-code", art, moved),
		"the agent appended to its own session: this entry is not quiet")

	orch.deferredMaterializeMu.Lock()
	entry := queue.entries[art.ArtifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, moved, entry.quietSince)
}

// --- The unchanged-inputs short circuit lives in the drain ---

// It must be withheld, not charged: a pass that never reached the adapter is
// not evidence that the adapter refuses the write.
func TestMaterializeDeferredArtifact_UnchangedInputsAreWithheldNotCharged(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)
	queue := escalationTestQueue(orch, "claude-code", artifactID)

	entry := escalationTestEntry(orch, queue, artifactID, func(*deferredMaterializationEntry) {})
	err := orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
	require.ErrorIs(t, err, ErrInboundNativeMaterialization)
	require.False(t, deferredMaterializationWithheld(err),
		"the first pass really reached the adapter and must be charged")
	require.Equal(t, 1, target.count())
	orch.recordDeferredMaterializationFailure("claude-code", queue, false, artifactID, entry, err)

	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, 1, entry.attempts)
	require.Equal(t, target.dest, entry.destPath,
		"the decline must hand the drain the destination it refused")

	err = orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
	require.ErrorIs(t, err, ErrInboundNativeMaterialization)
	require.True(t, deferredMaterializationWithheld(err),
		"re-running an attempt whose inputs did not move is withheld, not a refusal")
	require.Equal(t, 1, target.count(),
		"the short circuit must not call the adapter at all")
	orch.recordDeferredMaterializationFailure("claude-code", queue, false, artifactID, entry, err)

	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, 1, entry.attempts, "a withheld pass must not spend the retry budget")
	require.Equal(t, 1, entry.withheldAttempts)
}

// A transient adapter error names no destination, so nothing proves that
// re-running would fail again. Short-circuiting on "the canonical head did not
// move" alone would strand every recoverable failure — including the
// blocked-target-then-transient-failure path the safety-clear drain depends on.
func TestMaterializeDeferredArtifact_TransientAdapterErrorIsAlwaysRetried(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)
	queue := escalationTestQueue(orch, "claude-code", artifactID)

	entry := queue.entries[artifactID]
	for pass := range 3 {
		err := orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
		require.Error(t, err)
		// Model a plain failure rather than a typed decline: the queue must not
		// treat it as evidence about the observed inputs.
		orch.recordDeferredMaterializationFailure(
			"claude-code", queue, false, artifactID, entry, errors.New("native write failed"))
		orch.deferredMaterializeMu.Lock()
		entry = queue.entries[artifactID]
		orch.deferredMaterializeMu.Unlock()
		require.Equal(t, pass+1, target.count(),
			"every pass after an unclassified failure must really reach the adapter")
	}
}

// Once the agent touches the file again the short circuit must reopen, or a
// conversation that resumes would never be retried.
func TestMaterializeDeferredArtifact_ShortCircuitReopensWhenTheDestinationMoves(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)
	queue := escalationTestQueue(orch, "claude-code", artifactID)

	entry := queue.entries[artifactID]
	err := orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
	orch.recordDeferredMaterializationFailure("claude-code", queue, false, artifactID, entry, err)
	require.Equal(t, 1, target.count())

	require.NoError(t, os.WriteFile(target.dest, []byte("agent continued the thread\n"), 0o644))

	orch.deferredMaterializeMu.Lock()
	entry = queue.entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	err = orch.materializeDeferredArtifact(t.Context(), "claude-code", artifactID, entry)
	require.False(t, deferredMaterializationWithheld(err))
	require.Equal(t, 2, target.count(), "a moved destination must be re-attempted")
}

// --- Per-device escalation rate budget ---

// A substantial retained population can qualify. One `aplexica daemon reload`
// fans out the whole store, so without a cap the first sweep could
// raise hundreds of needs_attention rows inside half an hour.
func TestDeferredMaterializationEscalation_CapsNewAttentionRowsPerDay(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, _ := newDeclineOrchestrator(t, target)

	ids := make([]string, 0, 200)
	for i := range 200 {
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
	abandoned := len(queue.abandoned)
	pending := len(queue.entries)
	held := 0
	for _, entry := range queue.entries {
		if entry.escalationHeld {
			held++
		}
	}
	orch.deferredMaterializeMu.Unlock()

	require.Equal(t, deferredEscalationsPerWindow, abandoned,
		"at most three new needs_attention rows may be raised per rolling 24h")
	require.Equal(t, len(ids)-deferredEscalationsPerWindow, pending,
		"entries beyond the cap stay queued, they are not dropped")
	require.Equal(t, pending, held, "and every one of them is counted")

	// Rule 3: the quarantine breaker is 3 failures / 10 minutes and blocks all
	// materialization. Escalation calls no adapter, so its worst case is 0
	// adapter failures per 10 minutes regardless of how many entries qualify.
	require.Zero(t, target.count(),
		"escalation must not touch an adapter, or a mass sweep would trip the breaker")
}

// Honest reporting: the held entries are visible with their own state, never
// silently truncated.
func TestDeferredMaterializationRows_ReportSuppressedEscalations(t *testing.T) {
	queue := newDeferredMaterializationQueue()
	queue.generation = 1
	queue.ids = []string{"artifact-held"}
	queue.entries["artifact-held"] = deferredMaterializationEntry{
		version:        1,
		firstDeferred:  time.Now().UTC().Add(-30 * time.Hour),
		escalationHeld: true,
	}

	rows := deferredMaterializationRows(map[string]*deferredMaterializationQueue{"claude-code": queue}, nil)
	require.Len(t, rows, 1)
	require.Equal(t, "pending", rows[0]["state"])
	require.Equal(t, true, rows[0]["escalationDeferred"],
		"an entry the rate budget turned back must say so rather than look like an ordinary retry")
}

// The convergence sweep returns give-up records to the queue two per sweep on a
// 15-minute floor. It must not hand the artifact a clean escalation history on
// the way: without the carried stamp, a permanently stuck write would spend a
// fresh escalation every time the sweep touched it.
func TestEscalationsInWindow_SurvivesConvergenceReadmission(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, _ := newDeclineOrchestrator(t, target)

	now := time.Now().UTC()
	gaveUpAt := now.Add(-convergenceReadmitMinDwell - time.Hour)
	orch.deferredMaterializeMu.Lock()
	queue := newDeferredMaterializationQueue()
	queue.abandoned = []abandonedMaterialization{{
		artifactID: "artifact-stuck", originAgent: "codex",
		attempts: deferredMaterializationMaxAttempts, abandonedAt: gaveUpAt,
	}}
	orch.deferredMaterialize["claude-code"] = queue
	orch.deferredMaterializeMu.Unlock()

	require.Equal(t, 1, orch.readmitStuckMaterializations(now))

	orch.deferredMaterializeMu.Lock()
	standing := len(queue.abandoned)
	entry := queue.entries["artifact-stuck"]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, 1, standing, "re-admission is not a repair, so the flag stays raised")
	require.Equal(t, gaveUpAt, entry.lastEscalatedAt,
		"a re-admitted entry must carry forward that the device already spent an escalation on it")
	require.True(t, entry.alreadyEscalatedFor(entry.declineReason, entry.withheldReason),
		"so re-declining for the same cause returns it to the standing set free of charge")
}

// The record and the entry it was re-admitted onto describe ONE escalation.
// Records are retained across re-admission now, so counting both would charge
// the device twice for the same write and starve genuinely new causes.
func TestEscalationsInWindow_CountsARe_AdmittedWriteOnce(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	raisedAt := now.Add(-2 * time.Hour)
	queue := newDeferredMaterializationQueue()
	queue.abandoned = []abandonedMaterialization{{artifactID: "artifact-stuck", abandonedAt: raisedAt}}
	queue.generation = 1
	queue.ids = []string{"artifact-stuck"}
	queue.entries["artifact-stuck"] = deferredMaterializationEntry{
		version: 1, lastEscalatedAt: raisedAt,
	}

	require.Equal(t, 1, escalationsInWindow(
		map[string]*deferredMaterializationQueue{"claude-code": queue}, now))
}

// The budget is derived from the persisted give-up records, so restarting the
// daemon cannot hand the device a fresh allowance.
func TestEscalationsInWindow_CountsPersistedGiveUpsAcrossTargets(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	claude := newDeferredMaterializationQueue()
	claude.abandoned = []abandonedMaterialization{
		{artifactID: "a", abandonedAt: now.Add(-time.Hour)},
		{artifactID: "b", abandonedAt: now.Add(-deferredEscalationWindow - time.Hour)},
	}
	codex := newDeferredMaterializationQueue()
	codex.abandoned = []abandonedMaterialization{
		{artifactID: "c", abandonedAt: now.Add(-2 * time.Hour)},
	}

	require.Equal(t, 2, escalationsInWindow(map[string]*deferredMaterializationQueue{
		"claude-code": claude, "codex": codex,
	}, now), "the budget is per device, and only the rolling window counts")
}

// --- Remedies that name a command which can actually resolve the class ---

// Every remedy Aplexica prints must be a command that resolves THAT class.
// `aplexica repair materialization --agent <x>` only lists and drops, so it is
// not a valid repair remedy.
func TestEscalatedMaterializationRemedy_NeverPointsAtANoOp(t *testing.T) {
	reasons := []adapter.SessionDeclineReason{
		adapter.SessionDeclineUnspecified,
		adapter.SessionDeclineOptOut,
		adapter.SessionDeclineRace,
		adapter.SessionDeclineNativeAhead,
		adapter.SessionDeclineDiverged,
		adapter.SessionDeclineMirrorDiverged,
		adapter.SessionDeclineForkedMirror,
		adapter.SessionDeclineChainUnspanned,
		adapter.SessionDeclineGraphMalformed,
	}
	surfaces := []materializationSurface{
		{},
		{mirrorRepairSupported: true},
		{mirrorRepairSupported: true, mirrorRepairEnabled: true},
	}
	for _, surface := range surfaces {
		for _, reason := range reasons {
			remedy := escalatedMaterializationRemedy("claude-code", "019e0000", reason, "", surface)
			require.NotContains(t, remedy, "repair materialization --agent",
				"listing and dropping repairs nothing (%s)", reason)
			if remedy != "" {
				require.True(t,
					strings.HasPrefix(remedy, "aplexica repair conversation ") ||
						strings.HasPrefix(remedy, "aplexica repair materialization --drop ") ||
						strings.HasPrefix(remedy, `set "sync": {"repairForkedMirrors": true}`),
					"unknown remedy command %q for %s", remedy, reason)
			}
			require.NotEmpty(t, escalatedMaterializationExplain(reason, "", surface),
				"a class with no command must still say what is wrong (%s)", reason)
		}
	}

	// A NATIVE divergence is now repairable by the containment-proven rebuild,
	// which is allowed to run against a native-origin session. So with the
	// switch off the remedy is that one config key — the minimum-involvement
	// answer for the one class the machine can otherwise finish by itself — and
	// only once it is on and the rebuild has still refused does the read-only
	// canonical inspection become the remaining lever.
	require.Equal(t,
		`set "sync": {"repairForkedMirrors": true} in <state-dir>/config.json, `+
			"then run: aplexica daemon restart",
		escalatedMaterializationRemedy("claude-code", "019e0000", adapter.SessionDeclineDiverged, "",
			materializationSurface{mirrorRepairSupported: true}))
	require.Equal(t, "aplexica repair conversation 019e0000",
		escalatedMaterializationRemedy("claude-code", "019e0000", adapter.SessionDeclineDiverged, "",
			materializationSurface{mirrorRepairSupported: true, mirrorRepairEnabled: true}))
}

// A target that does not SHIP the rebuild must never be offered its switch.
// codex reports SessionDeclineDiverged for a native-origin rollout and cannot
// read sync.repairForkedMirrors at all, so naming it there is a loop the
// operator can follow to completion and end exactly where they started — having
// enabled destructive rewriting of their Claude transcripts on the strength of a
// codex problem.
func TestEscalatedMaterializationRemedy_NeverOffersARepairTheTargetDoesNotHave(t *testing.T) {
	for _, reason := range []adapter.SessionDeclineReason{
		adapter.SessionDeclineDiverged,
		adapter.SessionDeclineForkedMirror,
		adapter.SessionDeclineChainUnspanned,
	} {
		remedy := escalatedMaterializationRemedy("codex", "019e0000", reason, "", materializationSurface{})
		require.NotContains(t, remedy, "repairForkedMirrors",
			"a target with no rebuild must not be sent at its flag (%s)", reason)
		require.NotContains(t,
			escalatedMaterializationExplain(reason, "", materializationSurface{}),
			"switched off on this device",
			"and must not be told the repair exists and is off (%s)", reason)
	}
	// The read-only canonical inspection stays available for the one class it
	// was always the lever for.
	require.Equal(t, "aplexica repair conversation 019e0000",
		escalatedMaterializationRemedy(
			"codex", "019e0000", adapter.SessionDeclineDiverged, "", materializationSurface{}))
}

// chain_unspanned is repaired by the same router as forked_mirror
// (rebuildDivergedClaudeMirror and repairDivergedNativeSession both key on "the
// walk did not span the file"), so the two classes must offer the same remedy in
// the same states. Splitting the reason must not split the advice.
func TestEscalatedMaterializationRemedy_ChainUnspannedMatchesTheForkedMirror(t *testing.T) {
	for _, surface := range []materializationSurface{
		{mirrorRepairSupported: true},
		{mirrorRepairSupported: true, mirrorRepairEnabled: true},
	} {
		require.Equal(t,
			escalatedMaterializationRemedy(
				"claude-code", "019e0000", adapter.SessionDeclineForkedMirror, "", surface),
			escalatedMaterializationRemedy(
				"claude-code", "019e0000", adapter.SessionDeclineChainUnspanned, "", surface),
			"the repair router does not distinguish them, so the remedy must not either")
	}
	off := escalatedMaterializationRemedy("claude-code", "019e0000",
		adapter.SessionDeclineChainUnspanned, "", materializationSurface{mirrorRepairSupported: true})
	require.Contains(t, off, "repairForkedMirrors")
	require.NotContains(t,
		escalatedMaterializationExplain(adapter.SessionDeclineChainUnspanned, "",
			materializationSurface{mirrorRepairSupported: true}),
		"has not seen",
		"this build repairs the class; it must not be called an unknown shape")
}

// D4, second occurrence. `aplexica repair conversation` collapses duplicated
// turns in the CANONICAL head. That is the verified cause when the agent's own
// native session diverged — but the opposite relation is also possible: the
// MIRROR holds a turn canonical never saw, and the
// canonical head is clean. Offering the canonical repair there costs the
// operator the attempt to discover the command does not apply.
func TestEscalatedMaterializationRemedy_MirrorDivergenceIsNotACanonicalRepair(t *testing.T) {
	require.Empty(t,
		escalatedMaterializationRemedy(
			"claude-code", "019e0000", adapter.SessionDeclineMirrorDiverged, "", materializationSurface{}),
		"a clean canonical head has nothing to collapse")
	explain := escalatedMaterializationExplain(
		adapter.SessionDeclineMirrorDiverged, "", materializationSurface{})
	require.Contains(t, explain, "has not imported",
		"and the explanation must name what actually holds the write")
	require.NotEqual(t,
		escalatedMaterializationExplain(adapter.SessionDeclineDiverged, "", materializationSurface{}),
		explain, "the two divergence directions must not read identically")
}

// The one class with an automatic fix must not be reported as unfixable. This
// build SHIPS the forked-mirror rebuild; when it is switched off, the row has to
// say so, and when it is on the offer must disappear rather than loop.
func TestEscalatedMaterializationRemedy_ForkedMirrorNamesTheSwitchWhenItIsOff(t *testing.T) {
	off := escalatedMaterializationRemedy("claude-code", "019e0000",
		adapter.SessionDeclineForkedMirror, "", materializationSurface{mirrorRepairSupported: true})
	require.Contains(t, off, "repairForkedMirrors")
	require.Contains(t, off, "aplexica daemon restart")
	require.NotContains(t, off, "aplexica config set",
		"that command writes the layered config, not the daemon's <state-dir>/config.json")
	require.NotContains(t,
		escalatedMaterializationExplain(
			adapter.SessionDeclineForkedMirror, "", materializationSurface{mirrorRepairSupported: true}),
		"No shipped command repairs that",
		"this build ships the repair; it is switched off, which is a different sentence")

	on := materializationSurface{mirrorRepairSupported: true, mirrorRepairEnabled: true}
	require.Empty(t,
		escalatedMaterializationRemedy(
			"claude-code", "019e0000", adapter.SessionDeclineForkedMirror, "", on),
		"with the repair already enabled there is nothing left to switch on")
	require.Contains(t,
		escalatedMaterializationExplain(adapter.SessionDeclineForkedMirror, "", on),
		"cannot reproduce",
		"and the explanation must say why the enabled repair still refused")
}

// A write the drain never delivered because a gate was closed is explained by
// the gate, using the suppression catalog that already describes it correctly.
func TestEscalatedMaterializationRemedy_UsesTheGateReasonWhenTheTargetWasNeverReached(t *testing.T) {
	explain := escalatedMaterializationExplain(adapter.SessionDeclineUnspecified, ReasonPaused, materializationSurface{})
	require.Contains(t, explain, ReasonPaused.Explain())
	require.Equal(t, ReasonPaused.Remedy(),
		escalatedMaterializationRemedy(
			"claude-code", "019e0000", adapter.SessionDeclineUnspecified, ReasonPaused, materializationSurface{}))
}

// The needs_attention row itself must carry the class-specific pair.
func TestDeferredMaterializationRows_AttentionRowCarriesTheClassRemedy(t *testing.T) {
	queue := newDeferredMaterializationQueue()
	queue.abandoned = []abandonedMaterialization{{
		artifactID:    "019e0000",
		attempts:      deferredMaterializationMaxAttempts,
		abandonedAt:   time.Now().UTC(),
		declineReason: adapter.SessionDeclineDiverged,
	}}

	rows := deferredMaterializationRows(
		map[string]*deferredMaterializationQueue{"claude-code": queue},
		map[string]materializationSurface{"claude-code": {mirrorRepairSupported: true}})
	require.Len(t, rows, 1)
	// The target ships the rebuild and it is switched off, so the row names the
	// one config key that lets it heal this class.
	require.Equal(t,
		`set "sync": {"repairForkedMirrors": true} in <state-dir>/config.json, `+
			"then run: aplexica daemon restart",
		rows[0]["remedy"])
	require.Contains(t, rows[0]["explain"], "diverged")
}
