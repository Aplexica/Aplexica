package syncd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/stretchr/testify/require"
)

// The sweep half of the origin-session repair trigger. T1 fires on IMPORT of
// a forked file, so a file forked before the trigger shipped heals only on its
// next native edit. The convergence sweep must find those on its own — bounded
// per sweep, newest first, never over a standing give-up record.

// inspectStubAdapter is a conversation-session target whose inspector verdict
// is scripted and counted, so the sweep's parse and enqueue caps can be
// asserted without seventeen real forked transcripts.
type inspectStubAdapter struct {
	fakeConvSource
	mu        sync.Mutex
	inspected int
	reusable  bool
	reason    adapter.SessionDeclineReason
	// forked overrides the scripted verdict per artifact: a listed ID always
	// answers "forked, not reusable", whatever reusable/reason say. It lets one
	// stub hold a mixed population — healthy candidates above a fork — which is
	// the starvation shape the budget-rotation tests pin.
	forked map[string]bool
}

func (f *inspectStubAdapter) MaterializeConversationSession(art acf.Artifact, _ acf.Event, _ string) (string, bool, error) {
	// A typed decline with a stable path: queued entries persist for the
	// assertions instead of draining to success.
	return filepath.Join("/tmp/inspect-stub", art.ArtifactID+".session"), false, nil
}

func (f *inspectStubAdapter) InspectConversationSessionSource(art acf.Artifact, _ acf.Event) (bool, bool, adapter.SessionDeclineReason, error) {
	f.mu.Lock()
	f.inspected++
	forked := f.forked[art.ArtifactID]
	f.mu.Unlock()
	if forked {
		return false, true, adapter.SessionDeclineForkedMirror, nil
	}
	return f.reusable, true, f.reason, nil
}

func (f *inspectStubAdapter) inspectedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspected
}

// seedOriginSessionCandidates writes n conversation artifacts whose SourcePath
// lives under root (the stub adapter's native root) with ascending recent
// update times, each with one payload-bearing create event so the sweep's head
// read succeeds. The session files themselves are created too — a real origin
// session exists on disk, and the sweep's verdict memo fingerprints it there.
// Returns ids ordered oldest-first.
func seedOriginSessionCandidates(t *testing.T, store *acf.Store, root, agent string, n int, base time.Time) []string {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		id := acf.NewID()
		source := filepath.Join(root, fmt.Sprintf("session-%03d.jsonl", i))
		require.NoError(t, os.WriteFile(source, []byte("{}\n"), 0o644))
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             acf.KindConversation,
			Scope:            acf.ScopeGlobal,
			Name:             fmt.Sprintf("conv-%03d", i),
			SourcePath:       source,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		}))
		payload, err := json.Marshal(acf.ConversationPayload{Format: "test.session", Content: "{}"})
		require.NoError(t, err)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID:    acf.NewID(),
			ArtifactID: id,
			Type:       acf.EventTypeCreate,
			Timestamp:  ts,
			Provenance: acf.Provenance{SourceAgent: agent},
			Payload:    payload,
		}))
		ids = append(ids, id)
	}
	return ids
}

func newInspectStubOrchestrator(t *testing.T, stub *inspectStubAdapter) (*Orchestrator, *acf.Store, string) {
	t.Helper()
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	root := filepath.Join(home, "stub-sessions")
	orch, err := NewOrchestrator(Config{
		Dir:      home,
		Adapters: []adapter.Adapter{stub},
		Store:    store,
		RootsByAdapter: map[string][]string{
			stub.Name(): {root},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	return orch, store, root
}

// Sweep pickup must heal a pre-existing forked file with no import event at
// all: the fork predates this daemon (its entry journal is empty), canonical
// already holds every turn, and only the periodic sweep ever looks.
func TestConvergenceSweep_PicksUpPreexistingForkedOriginSession(t *testing.T) {
	// Build the fork on a daemon WITHOUT the repair: T1 enqueues, the drain
	// declines, the transcript stays frozen. Then close it and erase the
	// journal — the state an older daemon (or a crash between import and
	// enqueue) leaves behind.
	h := forkedOriginFixture(t, false)
	require.True(t, h.orch.handleEvent(h.session))
	artifactID := h.artifactID(t)
	require.Eventually(t, func() bool {
		return len(h.canonicalTurns(t)) == len(forkFixtureAllTurns)
	}, 4*time.Second, 25*time.Millisecond)
	require.NoError(t, h.orch.Close())
	dropped, err := DropDeferredMaterializationJournal(h.store.Root, "", "")
	require.NoError(t, err)
	require.NotZero(t, dropped, "the fixture must have had a journaled entry to erase")

	// A fresh daemon with the repair authorized, over the same home. No import
	// event will ever arrive for the byte-stable forked file.
	cc := claudecode.New()
	cc.HomeDir = h.home
	cc.CanonicalConversations = true
	cc.RepairForkedMirrors = true
	cx := codex.New()
	cx.HomeDir = h.home
	orch, err := NewOrchestrator(Config{
		Dir:        h.home,
		Adapters:   []adapter.Adapter{cc, cx},
		Store:      h.store,
		Quarantine: DefaultQuarantineTracker(),
		RootsByAdapter: map[string][]string{
			"claude-code": {filepath.Join(h.home, ".claude", "projects")},
			"codex":       {filepath.Join(h.home, ".codex", "sessions")},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	if _, queued := func() (deferredMaterializationEntry, bool) {
		orch.deferredMaterializeMu.Lock()
		defer orch.deferredMaterializeMu.Unlock()
		queue := orch.deferredMaterialize["claude-code"]
		if queue == nil {
			return deferredMaterializationEntry{}, false
		}
		entry, ok := queue.entries[artifactID]
		return entry, ok
	}(); queued {
		t.Fatal("the fixture requires an empty queue: the sweep must be the only trigger")
	}

	// One sweep, driven through the real sweep entry point so the wiring is
	// what is under test, not just the helper.
	orch.mu.Lock()
	orch.convergence.everSwept = true
	orch.convergence.lastSweepAt = time.Now().UTC().Add(-time.Hour)
	orch.mu.Unlock()
	orch.convergenceSweepOnce(context.Background(), time.Now().UTC())

	require.Eventually(t, func() bool {
		return h.resumableTurnsEqual(forkFixtureAllTurns)
	}, 10*time.Second, 25*time.Millisecond,
		"the sweep must queue the forked origin session and the drain must repair it")
	require.Eventually(t, func() bool {
		return len(orch.DeferredMaterializations()) == 0
	}, 4*time.Second, 25*time.Millisecond,
		"the healed write must retire its entry")
}

// The parse bound: candidates beyond the per-sweep budget are not even
// inspected. All verdicts are reusable here, so the enqueue cap never breaks
// the loop first.
func TestConvergenceSweep_OriginSessionParseCap(t *testing.T) {
	stub := &inspectStubAdapter{fakeConvSource: fakeConvSource{name: "stub"}, reusable: true}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	seedOriginSessionCandidates(t, store, root, "stub", 20, time.Now().UTC().Add(-time.Hour))

	queued := orch.queueForkedOriginSessions(time.Now().UTC())
	require.Zero(t, queued)
	require.Equal(t, convergenceOriginSessionParsePerSweep, stub.inspectedCount(),
		"20 candidates must cost at most %d plan evaluations per sweep",
		convergenceOriginSessionParsePerSweep)
}

// The enqueue bound: however many candidates need repair, one sweep hands the
// drain at most two — the same budget as convergenceReadmitPerSweep, and
// nothing on this path can charge the quarantine breaker anyway (a
// conversation-session decline never reaches fanOut's Export arm).
func TestConvergenceSweep_OriginSessionEnqueueCap(t *testing.T) {
	stub := &inspectStubAdapter{
		fakeConvSource: fakeConvSource{name: "stub"},
		reason:         adapter.SessionDeclineForkedMirror,
	}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	ids := seedOriginSessionCandidates(t, store, root, "stub", 20, time.Now().UTC().Add(-time.Hour))

	queued := orch.queueForkedOriginSessions(time.Now().UTC())
	require.Equal(t, convergenceOriginSessionQueuePerSweep, queued)

	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["stub"]
	require.NotNil(t, queue)
	require.Len(t, queue.entries, convergenceOriginSessionQueuePerSweep)
	// Newest first: the sweep must spend its budget on the most recently
	// updated conversations.
	for _, id := range ids[len(ids)-convergenceOriginSessionQueuePerSweep:] {
		entry, ok := queue.entries[id]
		require.True(t, ok, "the newest candidates must be the ones queued")
		require.True(t, entry.includePrimary,
			"an origin-session write must carry includePrimary past same-source exclusion")
		require.Equal(t, "stub", entry.originAgent)
	}
	orch.deferredMaterializeMu.Unlock()
}

// Suppression: an already-queued origin repair and a standing give-up record
// are both skipped WITHOUT spending a plan evaluation; the give-up survives.
func TestConvergenceSweep_OriginSessionSkipsQueuedAndGivenUp(t *testing.T) {
	stub := &inspectStubAdapter{
		fakeConvSource: fakeConvSource{name: "stub"},
		reason:         adapter.SessionDeclineForkedMirror,
	}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	ids := seedOriginSessionCandidates(t, store, root, "stub", 3, time.Now().UTC().Add(-time.Hour))

	// ids[2] (newest): standing give-up record. ids[1]: origin repair queued.
	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["stub"]
	if queue == nil {
		queue = newDeferredMaterializationQueue()
		orch.deferredMaterialize["stub"] = queue
	}
	queue.abandoned = append(queue.abandoned, abandonedMaterialization{
		artifactID:  ids[2],
		originAgent: "stub",
		abandonedAt: time.Now().UTC().Add(-48 * time.Hour),
	})
	orch.deferredMaterializeMu.Unlock()
	orch.deferMaterialization("stub", ids[1], "stub", true, false, true)

	queued := orch.queueForkedOriginSessions(time.Now().UTC())
	require.Equal(t, 1, queued, "only the clean candidate may be queued")
	require.Equal(t, 1, stub.inspectedCount(),
		"suppressed candidates must not spend the parse budget")

	orch.deferredMaterializeMu.Lock()
	defer orch.deferredMaterializeMu.Unlock()
	entry, ok := queue.entries[ids[0]]
	require.True(t, ok)
	require.True(t, entry.includePrimary)
	preQueued, ok := queue.entries[ids[1]]
	require.True(t, ok)
	require.True(t, preQueued.includePrimary)
	require.Equal(t, "stub", preQueued.originAgent)
	_, escalated := queue.entries[ids[2]]
	require.False(t, escalated, "a given-up artifact must not be resurrected by the sweep")
	require.Len(t, queue.abandoned, 1, "the give-up record itself must survive the sweep")
}

func TestConvergenceSweep_OriginSessionWidensQueuedForeignWrite(t *testing.T) {
	stub := &inspectStubAdapter{
		fakeConvSource: fakeConvSource{name: "stub"},
		reason:         adapter.SessionDeclineForkedMirror,
	}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	ids := seedOriginSessionCandidates(t, store, root, "stub", 1, time.Now().UTC().Add(-time.Hour))

	// A foreign fan-out can still be pending when the origin file forks. That
	// request excludes the origin adapter and therefore cannot own the repair.
	escalationTestQueue(orch, "stub", ids[0])
	orch.deferredMaterializeMu.Lock()
	entry := orch.deferredMaterialize["stub"].entries[ids[0]]
	entry.originAgent = "codex"
	orch.deferredMaterialize["stub"].entries[ids[0]] = entry
	orch.deferredMaterializeMu.Unlock()

	queued := orch.queueForkedOriginSessions(time.Now().UTC())
	require.Equal(t, 1, queued)
	require.Equal(t, 1, stub.inspectedCount())

	orch.deferredMaterializeMu.Lock()
	defer orch.deferredMaterializeMu.Unlock()
	widened := orch.deferredMaterialize["stub"].entries[ids[0]]
	require.True(t, widened.includePrimary)
	require.False(t, widened.mirrorsOnly)
	require.Equal(t, "stub", widened.originAgent)
}

// Re-admission fidelity. An origin-session write is includePrimary by
// construction — that is the one flag that lets it past fan-out's same-source
// exclusion. A re-admitted give-up record that lost the flag would fan out to
// ZERO plans, read as success, and silently retire the needs_attention row
// with the fork untouched: a flag nothing but success may lower, lowered by a
// write that wrote nothing. The record must carry the flag and the sweep's
// readmit must restore it.
func TestConvergenceSweep_ReadmittedOriginSessionKeepsIncludePrimary(t *testing.T) {
	stub := &inspectStubAdapter{
		fakeConvSource: fakeConvSource{name: "stub"},
		reason:         adapter.SessionDeclineForkedMirror,
	}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	ids := seedOriginSessionCandidates(t, store, root, "stub", 1, time.Now().UTC().Add(-time.Hour))

	orch.deferredMaterializeMu.Lock()
	queue := orch.deferredMaterialize["stub"]
	if queue == nil {
		queue = newDeferredMaterializationQueue()
		orch.deferredMaterialize["stub"] = queue
	}
	queue.abandoned = append(queue.abandoned, abandonedMaterialization{
		artifactID:     ids[0],
		originAgent:    "stub",
		includePrimary: true,
		attempts:       3,
		abandonedAt:    time.Now().UTC().Add(-48 * time.Hour),
		declineReason:  adapter.SessionDeclineForkedMirror,
	})
	orch.deferredMaterializeMu.Unlock()

	readmitted := orch.readmitStuckMaterializations(time.Now().UTC())
	require.Equal(t, 1, readmitted)

	orch.deferredMaterializeMu.Lock()
	entry, ok := queue.entries[ids[0]]
	orch.deferredMaterializeMu.Unlock()
	require.True(t, ok, "the dwelled give-up record must be re-admitted")
	require.True(t, entry.includePrimary,
		"re-admission must preserve includePrimary or the write succeeds vacuously")

	// The re-admitted write really reaches the (declining) adapter and the
	// give-up record survives until a REAL success.
	require.Eventually(t, func() bool {
		orch.deferredMaterializeMu.Lock()
		defer orch.deferredMaterializeMu.Unlock()
		current, stillQueued := queue.entries[ids[0]]
		return stillQueued && current.attempts >= 1 && len(queue.abandoned) == 1
	}, 6*time.Second, 25*time.Millisecond,
		"the re-admitted write must be attempted for real, not retired vacuously")
}

// The journal must round-trip the flag too: a restart between escalation and
// re-admission would otherwise reproduce the vacuous-success path.
func TestDeferredMaterializationJournal_RoundTripsAbandonedIncludePrimary(t *testing.T) {
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())

	queue := newDeferredMaterializationQueue()
	queue.abandoned = append(queue.abandoned, abandonedMaterialization{
		artifactID:     "art-1",
		originAgent:    "stub",
		includePrimary: true,
		mirrorsOnly:    true,
		abandonedAt:    time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
	})
	require.NoError(t, writeDeferredMaterializationQueues(
		store.Root, map[string]*deferredMaterializationQueue{"stub": queue}))

	loaded, err := loadDeferredMaterializationQueues(store.Root)
	require.NoError(t, err)
	require.NotNil(t, loaded["stub"])
	require.Len(t, loaded["stub"].abandoned, 1)
	require.True(t, loaded["stub"].abandoned[0].includePrimary)
	require.True(t, loaded["stub"].abandoned[0].mirrorsOnly)
}

// A queued entry already owns the retry lifecycle. The import path retracts
// the drain's short circuit for this destination before the trigger runs
// (reopenDeferredMaterializationForDest), so re-deferring here would buy
// nothing — and cost a full inspection per native edit plus a forced real
// adapter attempt per paced slot, for the whole life of a flag-off fork. The
// post-import trigger must skip a queued artifact without spending the
// inspection and without disturbing the entry.
func TestOriginSessionRepair_QueuedEntrySkipsWithoutReinspection(t *testing.T) {
	stub := &inspectStubAdapter{
		fakeConvSource: fakeConvSource{name: "stub"},
		reason:         adapter.SessionDeclineForkedMirror,
	}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	ids := seedOriginSessionCandidates(t, store, root, "stub", 1, time.Now().UTC().Add(-time.Hour))
	art, found := orch.findArtifact(ids[0])
	require.True(t, found)

	orch.queueOriginSessionRepairs(stub, art.SourcePath, ids)
	require.Equal(t, 1, stub.inspectedCount())
	entry, ok := func() (deferredMaterializationEntry, bool) {
		orch.deferredMaterializeMu.Lock()
		defer orch.deferredMaterializeMu.Unlock()
		queue := orch.deferredMaterialize["stub"]
		if queue == nil {
			return deferredMaterializationEntry{}, false
		}
		e, ok := queue.entries[ids[0]]
		return e, ok
	}()
	require.True(t, ok, "the first import of the forked file must enqueue")
	require.True(t, entry.includePrimary)
	version := entry.version

	// The user keeps typing on the fork's branch: every further import of the
	// same file re-runs the trigger, and it must now cost nothing.
	orch.queueOriginSessionRepairs(stub, art.SourcePath, ids)
	require.Equal(t, 1, stub.inspectedCount(),
		"an already-queued artifact must not be re-inspected")
	after, ok := func() (deferredMaterializationEntry, bool) {
		orch.deferredMaterializeMu.Lock()
		defer orch.deferredMaterializeMu.Unlock()
		e, ok := orch.deferredMaterialize["stub"].entries[ids[0]]
		return e, ok
	}()
	require.True(t, ok)
	require.Equal(t, version, after.version,
		"an already-queued artifact must not be re-deferred")
}

// The starvation shape: sixteen healthy conversations all more recently
// updated than one pre-existing fork — the exact population T2 exists for on
// a busy device, since every `claude` invocation makes another healthy
// candidate. A fixed top-16 budget would re-inspect the identical healthy
// files every sweep and never reach the fork; the memoized verdicts must make
// the budget cover a moving window instead, and a later sweep must both reach
// the fork and stop paying for the candidates already judged.
func TestConvergenceSweep_OriginSessionBudgetRotatesPastHealthyCandidates(t *testing.T) {
	stub := &inspectStubAdapter{fakeConvSource: fakeConvSource{name: "stub"}, reusable: true}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	ids := seedOriginSessionCandidates(t, store, root, "stub",
		convergenceOriginSessionParsePerSweep+1, time.Now().UTC().Add(-time.Hour))
	forkID := ids[0] // the OLDEST candidate: every healthy one outranks it
	stub.forked = map[string]bool{forkID: true}

	require.Zero(t, orch.queueForkedOriginSessions(time.Now().UTC()),
		"the first sweep spends its whole budget on the newer healthy candidates")
	require.Equal(t, convergenceOriginSessionParsePerSweep, stub.inspectedCount())

	require.Equal(t, 1, orch.queueForkedOriginSessions(time.Now().UTC()),
		"the second sweep must reach and queue the starved fork")
	require.Equal(t, convergenceOriginSessionParsePerSweep+1, stub.inspectedCount(),
		"unchanged healthy candidates must not be re-inspected")
	orch.deferredMaterializeMu.Lock()
	entry, ok := orch.deferredMaterialize["stub"].entries[forkID]
	orch.deferredMaterializeMu.Unlock()
	require.True(t, ok, "the fork must be the write the second sweep queues")
	require.True(t, entry.includePrimary)

	// Once everything is judged — the fork queued, the rest memoized — a
	// further sweep over the unchanged device costs nothing at all.
	require.Zero(t, orch.queueForkedOriginSessions(time.Now().UTC()))
	require.Equal(t, convergenceOriginSessionParsePerSweep+1, stub.inspectedCount(),
		"a fully-judged unchanged device must cost zero inspections per sweep")
}

// The memo must vouch for exactly the (file bytes, canonical head) it judged:
// either moving retracts it and the candidate is re-inspected.
func TestConvergenceSweep_OriginSessionMemoInvalidatesOnFileOrHeadChange(t *testing.T) {
	stub := &inspectStubAdapter{fakeConvSource: fakeConvSource{name: "stub"}, reusable: true}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	ids := seedOriginSessionCandidates(t, store, root, "stub", 1, time.Now().UTC().Add(-time.Hour))
	art, found := orch.findArtifact(ids[0])
	require.True(t, found)

	require.Zero(t, orch.queueForkedOriginSessions(time.Now().UTC()))
	require.Equal(t, 1, stub.inspectedCount())

	// Unchanged file, unchanged head: the memoized verdict stands, free.
	require.Zero(t, orch.queueForkedOriginSessions(time.Now().UTC()))
	require.Equal(t, 1, stub.inspectedCount(),
		"an unchanged candidate must not charge the parse budget again")

	// The file moves: the memo no longer vouches for these bytes.
	require.NoError(t, os.WriteFile(art.SourcePath, []byte("{}\n{}\n"), 0o644))
	require.Zero(t, orch.queueForkedOriginSessions(time.Now().UTC()))
	require.Equal(t, 2, stub.inspectedCount(), "a changed file must be re-inspected")

	// The head moves with the file unchanged: same.
	head, hasHead, err := conversationHeadForBranch(store, ids[0], acf.MainBranch)
	require.NoError(t, err)
	require.True(t, hasHead)
	payload, err := json.Marshal(acf.ConversationPayload{Format: "test.session", Content: "{}"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: ids[0],
		Type:       acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{SourceAgent: "stub"},
		Payload:    payload,
		ParentHash: head.Hash,
	}))
	require.Zero(t, orch.queueForkedOriginSessions(time.Now().UTC()))
	require.Equal(t, 3, stub.inspectedCount(), "a moved canonical head must be re-inspected")
}

// The recency window: a conversation not updated within the window is not a
// candidate and spends nothing.
func TestConvergenceSweep_OriginSessionWindowExcludesStale(t *testing.T) {
	stub := &inspectStubAdapter{
		fakeConvSource: fakeConvSource{name: "stub"},
		reason:         adapter.SessionDeclineForkedMirror,
	}
	orch, store, root := newInspectStubOrchestrator(t, stub)
	seedOriginSessionCandidates(t, store, root, "stub", 2,
		time.Now().UTC().Add(-convergenceOriginSessionWindow-24*time.Hour))

	queued := orch.queueForkedOriginSessions(time.Now().UTC())
	require.Zero(t, queued)
	require.Zero(t, stub.inspectedCount(),
		"a conversation outside the recency window must not even be inspected")
}
