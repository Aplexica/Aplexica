package syncd

import (
	"context"
	"errors"
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

// plannedDeclineTarget reproduces a retry-loop shape: the adapter can predict
// its session path (so the orchestrator
// guard-marks it before the write) but then declines the write because the
// native file is open, ahead, or divergent. Nothing is ever written.
type plannedDeclineTarget struct {
	fakeConvSource
	dest string

	mu       sync.Mutex
	attempts int
	// writeOnDecline models an adapter that appends and only then fails its
	// post-write verification — a decline that DID touch the file.
	writeOnDecline bool
}

func (p *plannedDeclineTarget) ConversationSessionPath(acf.Artifact, acf.Event, string) (string, bool, error) {
	return p.dest, true, nil
}

func (p *plannedDeclineTarget) MaterializeConversationSession(acf.Artifact, acf.Event, string) (string, bool, error) {
	p.mu.Lock()
	p.attempts++
	write := p.writeOnDecline
	p.mu.Unlock()
	if write {
		if err := os.WriteFile(p.dest, []byte("partial native bytes\n"), 0o644); err != nil {
			return p.dest, false, err
		}
	}
	return p.dest, false, nil
}

func (p *plannedDeclineTarget) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

var _ adapter.ConversationSessionTarget = (*plannedDeclineTarget)(nil)
var _ adapter.ConversationSessionPathTarget = (*plannedDeclineTarget)(nil)

func newDeclineOrchestrator(t *testing.T, target *plannedDeclineTarget) (*Orchestrator, *acf.Store, string) {
	t.Helper()
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	seedConversations(t, store, "codex", 1)
	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, conversations, 1)

	target.dest = filepath.Join(root, "native", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(target.dest), 0o755))

	orch, err := NewOrchestrator(Config{
		Dir:      root,
		Adapters: []adapter.Adapter{&fakeConvSource{name: "codex"}, target},
		Store:    store,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	return orch, store, conversations[0].ArtifactID
}

// A declined materialization must not leave a recursion-guard mark behind. The
// deferral queue re-attempts the same artifact indefinitely, so a mark taken
// per attempt keeps the suppression window permanently armed over a live
// session path and swallows the very continuation the adapter is waiting for.
func TestWriteConversationSession_DeclineWithoutWriteWithdrawsGuardMark(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, store, artifactID := newDeclineOrchestrator(t, target)

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	head, ok, err := store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)

	err = orch.writeConversationSession(convSessionPlan{
		st: target, name: "claude-code", art: art, branch: acf.MainBranch, sourceAgent: "codex",
	}, head)
	require.ErrorIs(t, err, ErrInboundNativeMaterialization)
	require.Equal(t, 1, target.count())
	require.False(t, orch.guard.Suppressed(target.dest),
		"a declined pass wrote nothing, so its guard mark must not suppress a real agent edit")
}

// The mirror-image safety property: an adapter that writes and then declines
// (post-write verification failed) must keep its guard mark, or the daemon
// re-imports its own bytes as an agent-side edit.
func TestWriteConversationSession_DeclineAfterWritingKeepsGuardMark(t *testing.T) {
	target := &plannedDeclineTarget{
		fakeConvSource: fakeConvSource{name: "claude-code"},
		writeOnDecline: true,
	}
	orch, store, artifactID := newDeclineOrchestrator(t, target)

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	head, ok, err := store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)

	err = orch.writeConversationSession(convSessionPlan{
		st: target, name: "claude-code", art: art, branch: acf.MainBranch, sourceAgent: "codex",
	}, head)
	require.ErrorIs(t, err, ErrInboundNativeMaterialization)
	require.True(t, orch.guard.Suppressed(target.dest),
		"bytes the orchestrator wrote must stay guarded even when the adapter reports a decline")
}

func TestRecursionGuard_UnmarkOnlyWithdrawsItsOwnMark(t *testing.T) {
	g := NewRecursionGuard(time.Minute)

	stale := g.Mark("/a")
	current := g.Mark("/a")
	g.Unmark("/a", stale)
	require.True(t, g.Suppressed("/a"), "a superseded token must not withdraw the newer mark")

	g.Unmark("/a", current)
	require.False(t, g.Suppressed("/a"))

	// Withdrawing twice, or withdrawing a path that was never marked, is a
	// no-op rather than a panic.
	g.Unmark("/a", current)
	g.Unmark("/never-marked", 1)
	require.False(t, g.Suppressed("/a"))
}

func TestDeferredMaterializationBackoff_GrowsToCap(t *testing.T) {
	require.Equal(t, deferredMaterializationRetryMin, deferredMaterializationBackoff(0))
	require.Equal(t, 2*deferredMaterializationRetryMin, deferredMaterializationBackoff(1))
	require.Equal(t, 4*deferredMaterializationRetryMin, deferredMaterializationBackoff(2))
	require.Equal(t, deferredMaterializationRetryMax, deferredMaterializationBackoff(64))
	require.Equal(t, deferredMaterializationRetryMax,
		deferredMaterializationBackoff(deferredMaterializationMaxAttempts))
	// Monotonic and never above the cap at any point in the schedule.
	previous := time.Duration(0)
	for attempts := range deferredMaterializationMaxAttempts {
		delay := deferredMaterializationBackoff(attempts)
		require.GreaterOrEqual(t, delay, previous)
		require.LessOrEqual(t, delay, deferredMaterializationRetryMax)
		previous = delay
	}
}

func TestDeferredMaterializationExhausted_NeedsBothAttemptsAndAge(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * deferredMaterializationMaxAge)
	recent := now.Add(-time.Minute)

	require.False(t, deferredMaterializationExhausted(deferredMaterializationMaxAttempts-1, old, now),
		"a long-queued entry that has barely been retried is not evidence of futility")
	require.False(t, deferredMaterializationExhausted(deferredMaterializationMaxAttempts, recent, now),
		"a burst of failures inside the backoff ramp must not abandon a young entry")
	require.False(t, deferredMaterializationExhausted(deferredMaterializationMaxAttempts, time.Time{}, now),
		"an entry with no recorded queue time cannot have its age judged")
	require.True(t, deferredMaterializationExhausted(deferredMaterializationMaxAttempts, old, now))
}

// The retry loop must stop eventually. Seeding an entry one attempt short of
// its budget exercises the real give-up path without waiting a day for it.
func TestDeferredMaterialization_AbandonsEntryAfterBudget(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)

	orch.deferMaterialization("claude-code", artifactID, "codex", false, false, true)
	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["claude-code"]
	entry := queue.entries[artifactID]
	entry.attempts = deferredMaterializationMaxAttempts - 1
	entry.firstDeferred = time.Now().UTC().Add(-2 * deferredMaterializationMaxAge)
	queue.entries[artifactID] = entry
	orch.deferredMaterializeMu.Unlock()

	orch.recordDeferredMaterializationFailure(
		"claude-code", queue, false, artifactID, entry, ErrInboundNativeMaterialization)

	orch.deferredMaterializeMu.Lock()
	_, stillQueued := queue.entries[artifactID]
	abandoned := append([]abandonedMaterialization(nil), queue.abandoned...)
	ids := append([]string(nil), queue.ids...)
	orch.deferredMaterializeMu.Unlock()

	require.False(t, stillQueued, "an entry past its budget must stop being retried")
	require.NotContains(t, ids, artifactID)
	require.Len(t, abandoned, 1)
	require.Equal(t, artifactID, abandoned[0].artifactID)
	require.Equal(t, deferredMaterializationMaxAttempts, abandoned[0].attempts)
	require.Contains(t, abandoned[0].lastErr, "inbound native materialization incomplete")

	// The give-up is reported, not silent.
	rows := orch.DeferredMaterializations()
	require.Len(t, rows, 1)
	require.Equal(t, "needs_attention", rows[0]["state"],
		"a give-up is surfaced as actionable, not as a terminal 'abandoned'")
	require.Equal(t, artifactID, rows[0]["artifactId"])
	require.Equal(t, "claude-code", rows[0]["agent"])
	require.NotEmpty(t, rows[0]["abandonedAt"])
	require.NotEmpty(t, rows[0]["explain"], "the operator must be told what the state means")
	// This entry gave up on an unclassified refusal, and no shipped command
	// resolves that. Offering one anyway is the defect this assertion guards:
	// `repair materialization --agent <x>` only lists and drops.
	remedy, _ := rows[0]["remedy"].(string)
	require.NotContains(t, remedy, "repair materialization --agent",
		"a remedy that repairs nothing is worse than none")
}

// A failing entry must be paced by its own backoff. A flat retry interval would
// otherwise produce an unbounded stream of repetitive log entries.
func TestDeferredMaterialization_FailingEntryBacksOff(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)

	orch.deferMaterialization("claude-code", artifactID, "codex", false, false, true)
	require.Eventually(t, func() bool { return target.count() >= 1 },
		3*time.Second, 10*time.Millisecond)

	// Within this window an unbounded 250ms retry would land dozens of
	// attempts; the doubling schedule allows only a handful.
	time.Sleep(1500 * time.Millisecond)
	require.Less(t, target.count(), 10,
		"a permanently declining entry must back off rather than retry at a flat interval")

	orch.deferredMaterializeMu.Lock()
	entry := orch.deferredMaterialize["claude-code"].entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Positive(t, entry.attempts)
	require.False(t, entry.firstDeferred.IsZero())
	require.False(t, entry.nextAttempt.IsZero())
	require.NotEmpty(t, entry.lastErr)
}

// Re-deferral is what the live fan-out does on every failed cycle. If it reset
// the budget, the entries that fail most often would be exactly the ones that
// could never be abandoned.
func TestDeferMaterialization_ReDeferralPreservesRetryBudget(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)

	first := time.Now().UTC().Add(-time.Hour)
	orch.deferMaterialization("claude-code", artifactID, "codex", false, false, true)
	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["claude-code"]
	entry := queue.entries[artifactID]
	entry.attempts = 9
	entry.firstDeferred = first
	entry.nextAttempt = time.Now().UTC().Add(time.Hour)
	queue.entries[artifactID] = entry
	orch.deferredMaterializeMu.Unlock()

	orch.deferMaterialization("claude-code", artifactID, "codex", true, false, true)

	orch.deferredMaterializeMu.Lock()
	updated := queue.entries[artifactID]
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, 9, updated.attempts)
	require.Equal(t, first, updated.firstDeferred)
	require.True(t, updated.nextAttempt.After(time.Now().UTC()),
		"a re-deferral must not reset the pending backoff into an immediate retry")
	require.True(t, updated.includePrimary, "coalescing still widens the request")
}

// An unblocked adapter is positive evidence that the blocking condition is
// gone, so queued writes should run now instead of after a backoff earned
// while the adapter was unreachable.
func TestResumeDeferredMaterializationAfterUnblock_ClearsBackoff(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)

	orch.deferMaterialization("claude-code", artifactID, "codex", false, false, true)
	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["claude-code"]
	entry := queue.entries[artifactID]
	entry.attempts = 20
	entry.nextAttempt = time.Now().UTC().Add(time.Hour)
	queue.entries[artifactID] = entry
	queue.overflowNextAttempt = time.Now().UTC().Add(time.Hour)
	orch.deferredMaterializeMu.Unlock()

	orch.resumeDeferredMaterializationAfterUnblock("claude-code")

	orch.deferredMaterializeMu.Lock()
	updated := queue.entries[artifactID]
	overflowNext := queue.overflowNextAttempt
	orch.deferredMaterializeMu.Unlock()
	require.True(t, updated.nextAttempt.IsZero())
	require.True(t, overflowNext.IsZero())
	require.Equal(t, 20, updated.attempts, "clearing the delay must not clear the evidence")
}

func TestDeferredMaterializationJournal_RoundTripsRetryAccounting(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	first := time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC)
	gaveUp := time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC)
	queue := newDeferredMaterializationQueue()
	queue.generation = 1
	queue.ids = []string{"artifact-pending"}
	queue.entries["artifact-pending"] = deferredMaterializationEntry{
		version: 1, originAgent: "codex", attempts: 12, firstDeferred: first, lastErr: "still open",
	}
	queue.abandoned = []abandonedMaterialization{{
		artifactID: "artifact-stuck", originAgent: "codex", attempts: 64,
		firstDeferred: first, abandonedAt: gaveUp, lastErr: "duplicated canonical head",
	}}
	require.NoError(t, writeDeferredMaterializationQueues(store.Root,
		map[string]*deferredMaterializationQueue{"claude-code": queue}))

	loaded, err := loadDeferredMaterializationQueues(store.Root)
	require.NoError(t, err)
	restored := loaded["claude-code"]
	require.NotNil(t, restored)
	require.Equal(t, 12, restored.entries["artifact-pending"].attempts)
	require.True(t, restored.entries["artifact-pending"].firstDeferred.Equal(first))
	require.Equal(t, "still open", restored.entries["artifact-pending"].lastErr)
	require.True(t, restored.entries["artifact-pending"].nextAttempt.IsZero(),
		"a restart earns one immediate retry before the persisted backoff resumes")
	require.Len(t, restored.abandoned, 1)
	require.Equal(t, "artifact-stuck", restored.abandoned[0].artifactID)
	require.True(t, restored.abandoned[0].abandonedAt.Equal(gaveUp))

	rows, err := LoadDeferredMaterializationJournal(store.Root)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	dropped, err := DropDeferredMaterializationJournal(store.Root, "claude-code", "artifact-stuck")
	require.NoError(t, err)
	require.Equal(t, 1, dropped)
	rows, err = LoadDeferredMaterializationJournal(store.Root)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "artifact-pending", rows[0]["artifactId"])

	dropped, err = DropDeferredMaterializationJournal(store.Root, "", "")
	require.NoError(t, err)
	require.Equal(t, 1, dropped)
	rows, err = LoadDeferredMaterializationJournal(store.Root)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// A fresh deferral earns a full retry budget again, but it must NOT retire the
// give-up record.
//
// The live fan-out re-defers on every failed cycle, so on a device under
// continuous fan-out this path ran constantly for exactly the artifacts that
// keep failing. Dropping the record here therefore did two harmful things at
// once: it erased the operator-facing flag before anyone could act on it (one
// `aplexica daemon reload` wiped the lot), and the record is also the evidence
// the per-device escalation cap counts, so the daily allowance could be
// refilled many times a day.
func TestDeferMaterialization_ReQueueKeepsTheGiveUpRecordAndItsBudget(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)

	gaveUpAt := time.Now().UTC().Add(-time.Minute)
	orch.deferredMaterializeMu.Lock()
	queue := newDeferredMaterializationQueue()
	queue.abandoned = []abandonedMaterialization{{
		artifactID: artifactID, attempts: 64, abandonedAt: gaveUpAt,
		declineReason: adapter.SessionDeclineForkedMirror,
	}}
	orch.deferredMaterialize["claude-code"] = queue
	before := escalationsInWindow(orch.deferredMaterialize, time.Now().UTC())
	orch.deferredMaterializeMu.Unlock()
	require.Equal(t, 1, before)

	orch.deferMaterialization("claude-code", artifactID, "codex", false, false, true)

	orch.deferredMaterializeMu.Lock()
	abandoned := len(queue.abandoned)
	entry, queued := queue.entries[artifactID]
	after := escalationsInWindow(orch.deferredMaterialize, time.Now().UTC())
	orch.deferredMaterializeMu.Unlock()
	require.True(t, queued)
	require.Zero(t, entry.attempts, "the retry budget really is fresh")
	require.Equal(t, 1, abandoned, "but the flag stays raised until the write succeeds")
	require.Equal(t, gaveUpAt, entry.lastEscalatedAt,
		"and the entry carries forward that this device already raised it")
	require.Equal(t, 1, after,
		"ordinary sync traffic must not refund the device's escalation allowance")
}

func TestDropDeferredMaterializations_ScopesToAgentAndArtifact(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, store, artifactID := newDeclineOrchestrator(t, target)
	seedConversations(t, store, "codex", 1)
	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	other := ""
	for _, art := range conversations {
		if art.ArtifactID != artifactID {
			other = art.ArtifactID
		}
	}
	require.NotEmpty(t, other)

	// Install the queues directly. deferMaterialization also starts their
	// background drains, which can consume an entry while this test is asserting
	// the pure agent/artifact scoping behavior of DropDeferredMaterializations.
	escalationTestQueue(orch, "claude-code", artifactID, other)
	escalationTestQueue(orch, "codex", artifactID)

	dropped, err := orch.DropDeferredMaterializations("claude-code", artifactID)
	require.NoError(t, err)
	require.Equal(t, 1, dropped)

	remaining := map[string]bool{}
	for _, row := range orch.DeferredMaterializations() {
		agent, _ := row["agent"].(string)
		id, _ := row["artifactId"].(string)
		remaining[agent+"/"+id] = true
	}
	require.False(t, remaining["claude-code/"+artifactID])
	require.True(t, remaining["claude-code/"+other])
	require.True(t, remaining["codex/"+artifactID])
}

// --- regressions found by the pre-merge adversarial review ---

// The overflow give-up marker carries no artifact ID. Skipping empty-ID
// abandoned records on load discarded the only surviving evidence that a whole
// target had been given up on, defeating the status/repair surfaces across a
// restart.
func TestDeferredMaterializationJournal_KeepsWholeTargetGiveUpRecord(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	queue := newDeferredMaterializationQueue()
	queue.abandoned = []abandonedMaterialization{{
		attempts:      deferredMaterializationMaxAttempts,
		firstDeferred: time.Date(2026, 7, 25, 9, 0, 0, 0, time.UTC),
		abandonedAt:   time.Date(2026, 7, 26, 9, 0, 0, 0, time.UTC),
		lastErr:       "native materialization target \"codex\" is unavailable",
	}}
	require.NoError(t, writeDeferredMaterializationQueues(store.Root,
		map[string]*deferredMaterializationQueue{"codex": queue}))

	rows, err := LoadDeferredMaterializationJournal(store.Root)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a whole-target give-up must survive a daemon restart")
	require.Equal(t, "needs_attention", rows[0]["state"])
	require.Equal(t, "codex", rows[0]["agent"])
	require.Equal(t, "", rows[0]["artifactId"])

	dropped, err := DropDeferredMaterializationJournal(store.Root, "codex", "")
	require.NoError(t, err)
	require.Equal(t, 1, dropped, "an operator must be able to clear it once repaired")
}

// A pause, quarantine, unresolved conflict, or momentarily missing adapter is
// reversible and user-controlled. Charging those against the give-up budget
// would let a 24h pause permanently forfeit every queued write.
func TestDeferredMaterialization_WithheldAttemptsDoNotSpendBudget(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)

	orch.deferMaterialization("claude-code", artifactID, "codex", false, false, true)
	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["claude-code"]
	entry := queue.entries[artifactID]
	entry.attempts = deferredMaterializationMaxAttempts - 1
	entry.firstDeferred = time.Now().UTC().Add(-2 * deferredMaterializationMaxAge)
	queue.entries[artifactID] = entry
	orch.deferredMaterializeMu.Unlock()

	withheld := fmt.Errorf("%w (%w)", ErrInboundNativeMaterialization, errDeferredMaterializationWithheld)
	for range 40 {
		orch.recordDeferredMaterializationFailure("claude-code", queue, false, artifactID, entry, withheld)
	}

	orch.deferredMaterializeMu.Lock()
	current, stillQueued := queue.entries[artifactID]
	abandoned := len(queue.abandoned)
	orch.deferredMaterializeMu.Unlock()
	require.True(t, stillQueued, "a paused target must not forfeit its queued writes")
	require.Zero(t, abandoned)
	require.Equal(t, deferredMaterializationMaxAttempts-1, current.attempts,
		"a gate that turned the attempt back before the adapter saw it is not an attempt")
	require.Equal(t, 40, current.withheldAttempts)
	require.True(t, current.nextAttempt.After(time.Now().UTC()),
		"withheld attempts must still be paced so a pause does not spin")

	// One failure that actually reached the adapter still spends the budget.
	orch.recordDeferredMaterializationFailure(
		"claude-code", queue, false, artifactID, entry, ErrInboundNativeMaterialization)
	orch.deferredMaterializeMu.Lock()
	_, stillQueued = queue.entries[artifactID]
	abandoned = len(queue.abandoned)
	orch.deferredMaterializeMu.Unlock()
	require.False(t, stillQueued)
	require.Equal(t, 1, abandoned)
}

func TestDeferredMaterializationWithheld_ClassifiesGateAndShutdown(t *testing.T) {
	require.True(t, deferredMaterializationWithheld(
		fmt.Errorf("%w (%w)", ErrInboundNativeMaterialization, errDeferredMaterializationWithheld)))
	require.True(t, deferredMaterializationWithheld(context.Canceled))
	require.False(t, deferredMaterializationWithheld(ErrInboundNativeMaterialization),
		"a plain adapter decline is a real attempt and must be charged")
	require.False(t, deferredMaterializationWithheld(errors.New("disk full")))
}

// An unblock clears the pacing accrued while the adapter was unreachable, but
// must not clear evidence of attempts that really happened.
func TestResumeAfterUnblock_ClearsWithheldPacingOnly(t *testing.T) {
	target := &plannedDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, _, artifactID := newDeclineOrchestrator(t, target)

	orch.deferMaterialization("claude-code", artifactID, "codex", false, false, true)
	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["claude-code"]
	entry := queue.entries[artifactID]
	entry.attempts = 7
	entry.withheldAttempts = 30
	queue.entries[artifactID] = entry
	queue.overflowWithheldAttempts = 12
	orch.deferredMaterializeMu.Unlock()

	orch.resumeDeferredMaterializationAfterUnblock("claude-code")

	orch.deferredMaterializeMu.Lock()
	updated := queue.entries[artifactID]
	overflowWithheld := queue.overflowWithheldAttempts
	orch.deferredMaterializeMu.Unlock()
	require.Zero(t, updated.withheldAttempts)
	require.Zero(t, overflowWithheld)
	require.Equal(t, 7, updated.attempts)
}

// In overflow mode the per-artifact ids are neither drained nor persisted, so
// reporting them made the live view and the on-disk view disagree by up to
// deferredMaterializationLimit rows across a restart.
func TestDeferredMaterializationRows_OverflowHidesUntrackedEntries(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	queue := newDeferredMaterializationQueue()
	queue.overflow = true
	queue.overflowAttempts = 3
	for _, id := range []string{"a1", "a2", "a3"} {
		queue.generation++
		queue.ids = append(queue.ids, id)
		queue.entries[id] = deferredMaterializationEntry{version: queue.generation}
	}
	queue.abandoned = []abandonedMaterialization{{artifactID: "old", attempts: 64}}
	byTarget := map[string]*deferredMaterializationQueue{"claude-code": queue}

	live := deferredMaterializationRows(byTarget, nil)
	require.NoError(t, writeDeferredMaterializationQueues(store.Root, byTarget))
	journal, err := LoadDeferredMaterializationJournal(store.Root)
	require.NoError(t, err)

	require.Len(t, live, 2, "one overflow row plus the abandoned record")
	require.Equal(t, len(live), len(journal),
		"the live and on-disk views must not disagree across a restart")
	for _, row := range live {
		require.NotEqual(t, "pending", row["state"])
	}
}

// Rewriting the journal stamps the current version, which doubles as the
// one-shot watermark for the local-projection repair migration. An offline
// drop must not silently consume it.
func TestDropDeferredMaterializationJournal_RefusesPreMigrationJournal(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	journal := filepath.Join(store.Root, deferredMaterializationDirtyName)
	require.NoError(t, os.WriteFile(journal,
		[]byte(`{"version":1,"targets":["claude-code","codex"]}`), 0o600))

	require.True(t, deferredMaterializationProjectionMigrationNeeded(store.Root))
	_, err := DropDeferredMaterializationJournal(store.Root, "codex", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "start the daemon once")
	require.True(t, deferredMaterializationProjectionMigrationNeeded(store.Root),
		"a refused drop must leave the migration watermark intact")

	raw, err := os.ReadFile(journal)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"version":1`)
}

// A store with no journal at all also reports "migration needed", but there is
// nothing to protect there — the drop must still work as a no-op.
func TestDropDeferredMaterializationJournal_MissingJournalIsANoOp(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	dropped, err := DropDeferredMaterializationJournal(store.Root, "", "")
	require.NoError(t, err)
	require.Zero(t, dropped)
}
