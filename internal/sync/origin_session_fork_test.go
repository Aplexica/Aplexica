package syncd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

// The origin-session fork: the one divergence shape v1.0.65 left with no
// repair trigger at all.
//
// The owner's live sequence: a conversation starts in Claude Code, Codex
// continues it and those turns ARE materialized back into the Claude session
// file, and then — WITHOUT resuming — the user types the next prompt into the
// still-open Claude TUI. Claude Code appends at its stale IN-MEMORY leaf, so
// the file's parentUuid graph forks: the codex branch and the user's new
// branch both hang from the same assistant row. Claude's own import commits
// the new turns fine (canonical converges), but fan-out same-source-excludes
// the origin adapter, so no materialize attempt is ever made toward the forked
// file — no attempt, no decline, no queue entry, no nudge, no repair. The
// transcript stays frozen at the fork's own branch forever unless an unrelated
// FOREIGN turn happens to arrive.
//
// These tests reproduce that shape faithfully — the fork's user turn is
// reached through a NON-conversational system bridge row, exactly as recorded
// live (uuid b1ead6c7 <- parent 37396f9c (system) <- ... <- aac60bcf
// (assistant)) — and pin the trigger that ends it.

// forkClaudeCodeAtStaleLeaf appends the live fork shape to the Claude session:
// a system bridge row parented on the STALE leaf Claude Code still holds in
// memory, the user's next prompt parented on that bridge, the answer parented
// on the prompt, and a last-prompt row naming the new leaf. A direct
// conversational fork would not exercise the bridge-resolution path, so the
// bridge row is load-bearing fixture shape, not decoration.
func (h *nativeDivergenceHarness) forkClaudeCodeAtStaleLeaf(t *testing.T, staleLeaf, userText, answerText string) {
	t.Helper()
	require.NotEmpty(t, staleLeaf, "the fork needs the stale in-memory leaf")

	nextRow := func(role string) (string, string) {
		uuid := fmt.Sprintf("native-%s-%d", role, h.nextRow)
		ts := time.Date(2026, 7, 31, 10, 0, h.nextRow, 0, time.UTC).Format(time.RFC3339Nano)
		h.nextRow++
		return uuid, ts
	}

	bridgeUUID, bridgeTS := nextRow("bridge")
	appendJSONLine(t, h.session, map[string]any{
		"type": "system", "subtype": "stop_hook_summary", "isMeta": true,
		"uuid": bridgeUUID, "parentUuid": staleLeaf, "sessionId": h.sessionID,
		"timestamp": bridgeTS, "cwd": h.home, "isSidechain": false, "version": "2.1.0",
	})
	userUUID, userTS := nextRow("user")
	appendJSONLine(t, h.session, map[string]any{
		"type": "user", "uuid": userUUID, "parentUuid": bridgeUUID, "sessionId": h.sessionID,
		"timestamp": userTS, "cwd": h.home, "isSidechain": false, "version": "2.1.0",
		"message": map[string]any{"role": "user", "content": userText}, "userType": "external",
	})
	answerUUID, answerTS := nextRow("assistant")
	appendJSONLine(t, h.session, map[string]any{
		"type": "assistant", "uuid": answerUUID, "parentUuid": userUUID, "sessionId": h.sessionID,
		"timestamp": answerTS, "cwd": h.home, "isSidechain": false, "version": "2.1.0",
		"message": map[string]any{
			"role": "assistant", "type": "message", "model": "claude-opus-4-8",
			"content": []any{map[string]any{"type": "text", "text": answerText}},
		},
	})
	appendJSONLine(t, h.session, map[string]any{
		"type": "last-prompt", "lastPrompt": userText, "leafUuid": answerUUID, "sessionId": h.sessionID,
	})
}

var forkFixtureAllTurns = []acf.TextTurn{
	{Role: "user", Text: "What is the capital of Canada?"},
	{Role: "assistant", Text: "Ottawa."},
	{Role: "user", Text: "How many provinces does Canada have?"},
	{Role: "assistant", Text: "Ten, plus three territories."},
	{Role: "user", Text: "How big is Canada?"},
	{Role: "assistant", Text: "About 9.98 million square kilometres."},
}

// forkedOriginFixture drives the harness to the owner's exact pre-trigger
// state: canonical holds the first four turns, the Claude session file holds
// ALL of them physically but its resume walk reaches only the fork's own
// branch, and no foreign event is ever going to arrive.
func forkedOriginFixture(t *testing.T, repair bool) *nativeDivergenceHarness {
	t.Helper()
	h := newNativeDivergenceHarness(t, repair)

	// 1. The conversation starts in Claude Code.
	h.typeInClaudeCode(t, "user", forkFixtureAllTurns[0].Text)
	h.typeInClaudeCode(t, "assistant", forkFixtureAllTurns[1].Text)
	require.True(t, h.orch.handleEvent(h.session))
	raw, err := os.ReadFile(h.session)
	require.NoError(t, err)
	// The leaf Claude Code will still hold in memory when the user types again.
	staleLeaf := lastClaudeUUIDIn(raw)
	rollout := h.codexRollout(t)

	// 2. Codex resumes and adds a pair, and — unlike the divergence harness's
	// original scenario — its materialization LANDS in the Claude session file
	// before the user types again. That is the precondition for a single-file
	// graph fork rather than a two-sided divergence.
	h.typeInCodex(t, rollout, "user", forkFixtureAllTurns[2].Text)
	h.typeInCodex(t, rollout, "assistant", forkFixtureAllTurns[3].Text)
	require.True(t, h.orch.handleEvent(rollout))
	require.Eventually(t, func() bool {
		return h.resumableTurnsEqual(forkFixtureAllTurns[:4])
	}, 6*time.Second, 25*time.Millisecond,
		"the codex turns must be materialized into the Claude session before the fork")

	// 3. WITHOUT resuming, the user types into the still-open Claude TUI.
	h.forkClaudeCodeAtStaleLeaf(t, staleLeaf,
		forkFixtureAllTurns[4].Text, forkFixtureAllTurns[5].Text)

	// Prove the fixture is the live shape: physically the file holds every
	// turn, but the resume walk cannot span it — ResumableTextTurns fails
	// closed on exactly that state, which is the frozen-transcript proof.
	raw, err = os.ReadFile(h.session)
	require.NoError(t, err)
	for _, turn := range forkFixtureAllTurns {
		require.Contains(t, string(raw), turn.Text,
			"every turn must be physically present in the forked file")
	}
	_, walkErr := claudecode.ResumableTextTurns(raw)
	require.Error(t, walkErr,
		"the forked file's resume walk must not span its conversational rows")
	return h
}

// resumableTurnsEqual reports whether the Claude session file currently spans
// AND resumes to exactly the given turns. Tolerates the walk's fail-closed
// error while the file is forked, so it is safe inside Eventually closures.
func (h *nativeDivergenceHarness) resumableTurnsEqual(expected []acf.TextTurn) bool {
	raw, err := os.ReadFile(h.session)
	if err != nil {
		return false
	}
	turns, err := claudecode.ResumableTextTurns(raw)
	return err == nil && acf.TextTurnsEqual(turns, expected)
}

func (h *nativeDivergenceHarness) claudeQueueGeneration() uint64 {
	h.orch.deferredMaterializeMu.Lock()
	defer h.orch.deferredMaterializeMu.Unlock()
	queue := h.orch.deferredMaterialize["claude-code"]
	if queue == nil {
		return 0
	}
	return queue.generation
}

func (h *nativeDivergenceHarness) claudeQueueEntry(artifactID string) (deferredMaterializationEntry, bool) {
	h.orch.deferredMaterializeMu.Lock()
	defer h.orch.deferredMaterializeMu.Unlock()
	queue := h.orch.deferredMaterialize["claude-code"]
	if queue == nil {
		return deferredMaterializationEntry{}, false
	}
	entry, ok := queue.entries[artifactID]
	return entry, ok
}

func (h *nativeDivergenceHarness) claudeGiveUpFor(artifactID string) bool {
	h.orch.deferredMaterializeMu.Lock()
	defer h.orch.deferredMaterializeMu.Unlock()
	queue := h.orch.deferredMaterialize["claude-code"]
	if queue == nil {
		return false
	}
	for _, record := range queue.abandoned {
		if record.artifactID == artifactID {
			return true
		}
	}
	return false
}

// THE DEFECT, PINNED: a same-source fork must heal with NO foreign event. The
// import of the forked file itself is the only stimulus; from it the trigger
// must enqueue an includePrimary write toward the origin adapter, the drain
// must decline it as forked, and — with the repair authorized — the rebuild
// must linearize the transcript to every turn from every agent.
func TestOriginSessionFork_HealsWithNoForeignEvent(t *testing.T) {
	h := forkedOriginFixture(t, true)

	require.True(t, h.orch.handleEvent(h.session))
	artifactID := h.artifactID(t)

	require.Eventually(t, func() bool {
		return len(h.canonicalTurns(t)) == len(forkFixtureAllTurns)
	}, 4*time.Second, 25*time.Millisecond,
		"canonical must absorb the fork's turns, got %v", h.canonicalTurns(t))
	require.Equal(t, forkFixtureAllTurns, h.canonicalTurns(t))

	// The heal: the resume walk must recover EVERY turn, not just the fork's
	// branch, with no further user action and no foreign event.
	require.Eventually(t, func() bool {
		return h.resumableTurnsEqual(forkFixtureAllTurns)
	}, 10*time.Second, 25*time.Millisecond,
		"the forked transcript must linearize to all turns; canonical=%v",
		h.canonicalTurns(t))

	// The repaired file is still the user's own transcript.
	raw, err := os.ReadFile(h.session)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "aplexicaThreadId")

	// Exactly one Claude session for the thread — never a recovery copy.
	sessions, err := filepath.Glob(filepath.Join(h.home, ".claude", "projects", "*", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, h.session, sessions[0])

	require.False(t, h.tracker.IsQuarantined("claude-code", time.Now()),
		"healing the fork must never quarantine an adapter")

	// The successful write retires the entry and leaves no needs_attention row.
	require.Eventually(t, func() bool {
		return len(h.orch.DeferredMaterializations()) == 0
	}, 4*time.Second, 25*time.Millisecond,
		"a healed fork must leave no queued entry and no give-up record")

	// LOOP TERMINATION: re-importing the repaired file (an exact match) must
	// not enqueue again, and the queue must stay empty across another sweep.
	generation := h.claudeQueueGeneration()
	require.True(t, h.orch.handleEvent(h.session))
	require.Equal(t, generation, h.claudeQueueGeneration(),
		"re-importing a repaired file must not enqueue")
	h.orch.mu.Lock()
	h.orch.convergence.everSwept = true
	h.orch.convergence.lastSweepAt = time.Now().UTC().Add(-time.Hour)
	h.orch.mu.Unlock()
	h.orch.convergenceSweepOnce(context.Background(), time.Now().UTC())
	require.Equal(t, generation, h.claudeQueueGeneration(),
		"a convergence sweep over a healed device must not re-enqueue")
	require.Empty(t, h.orch.DeferredMaterializations())
	require.Equal(t, forkFixtureAllTurns, h.canonicalTurns(t),
		"the no-oscillation proof: canonical must not grow again")
	require.Equal(t, forkFixtureAllTurns, readClaudeTurns(t, h.session))
	require.NotEmpty(t, artifactID)
}

// Flag OFF: the trigger still fires — the entry enqueues with includePrimary,
// the drain declines it as forked (the classification that routes escalation)
// — but the user's transcript is untouched, because the rebuild is the part
// that is gated.
func TestOriginSessionFork_FlagOffQueuesAndLeavesTranscriptUntouched(t *testing.T) {
	h := forkedOriginFixture(t, false)

	require.True(t, h.orch.handleEvent(h.session))
	artifactID := h.artifactID(t)

	require.Eventually(t, func() bool {
		return len(h.canonicalTurns(t)) == len(forkFixtureAllTurns)
	}, 4*time.Second, 25*time.Millisecond,
		"canonical must still absorb the fork's turns with the repair off")

	// The trigger must enqueue the origin adapter itself, includePrimary, so
	// the drain can bypass same-source exclusion.
	require.Eventually(t, func() bool {
		entry, ok := h.claudeQueueEntry(artifactID)
		return ok && entry.includePrimary && entry.originAgent == "claude-code"
	}, 4*time.Second, 25*time.Millisecond,
		"the fork must enqueue an includePrimary write toward the origin adapter")

	// The drain's attempt classifies the decline so escalation can name it.
	require.Eventually(t, func() bool {
		entry, ok := h.claudeQueueEntry(artifactID)
		return ok && entry.declineReason == adapter.SessionDeclineForkedMirror
	}, 6*time.Second, 25*time.Millisecond,
		"the queued write must decline as forked_mirror")

	before, err := os.ReadFile(h.session)
	require.NoError(t, err)
	// Give the drain room for further passes against the gated repair.
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(h.session)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after),
		"with the rebuild switched off the user's transcript must not be rewritten")
	_, walkErr := claudecode.ResumableTextTurns(after)
	require.Error(t, walkErr, "the walk must still be unable to span the untouched fork")
	require.False(t, h.tracker.IsQuarantined("claude-code", time.Now()),
		"a declined fork write must never charge the quarantine breaker")
}

// The explicit loop-termination proof, isolated: after the repair, an import
// of the exact-match file must not enqueue — however many times it re-runs.
func TestOriginSessionFork_RepairedReimportDoesNotReenqueue(t *testing.T) {
	h := forkedOriginFixture(t, true)

	require.True(t, h.orch.handleEvent(h.session))
	require.Eventually(t, func() bool {
		return h.resumableTurnsEqual(forkFixtureAllTurns)
	}, 10*time.Second, 25*time.Millisecond)
	require.Eventually(t, func() bool {
		return len(h.orch.DeferredMaterializations()) == 0
	}, 4*time.Second, 25*time.Millisecond)

	generation := h.claudeQueueGeneration()
	for i := 0; i < 3; i++ {
		// Force a full re-read each round: a byte-stable file would otherwise
		// short-circuit on the scan cache, which is not the path under test.
		h.orch.scanCache.invalidate(h.session)
		require.True(t, h.orch.handleEvent(h.session))
		require.Equal(t, generation, h.claudeQueueGeneration(),
			"round %d: an exact-match re-import must not enqueue", i+1)
	}
	require.Equal(t, forkFixtureAllTurns, h.canonicalTurns(t))
}

// A give-up (needs_attention) record must suppress the post-import trigger:
// re-admitting an escalated write is the convergence sweep's job, with its
// dwell rules, and the import hook must not resurrect it on every edit.
func TestOriginSessionFork_GiveUpRecordSuppressesPostImportTrigger(t *testing.T) {
	h := forkedOriginFixture(t, false)

	require.True(t, h.orch.handleEvent(h.session))
	artifactID := h.artifactID(t)
	require.Eventually(t, func() bool {
		entry, ok := h.claudeQueueEntry(artifactID)
		return ok && entry.declineReason == adapter.SessionDeclineForkedMirror
	}, 6*time.Second, 25*time.Millisecond)

	// Age the entry past the escalation rule (age + quiescence), then let the
	// drain's next failed pass raise it: entry removed, give-up record left.
	h.orch.deferredMaterializeMu.Lock()
	queue := h.orch.deferredMaterialize["claude-code"]
	entry := queue.entries[artifactID]
	entry.firstDeferred = time.Now().UTC().Add(-7 * time.Hour)
	entry.quietSince = time.Now().UTC().Add(-3 * time.Hour)
	entry.nextAttempt = time.Time{}
	queue.entries[artifactID] = entry
	h.orch.deferredMaterializeMu.Unlock()
	h.orch.scheduleDeferredMaterializationDrain("claude-code")
	require.Eventually(t, func() bool {
		if _, queued := h.claudeQueueEntry(artifactID); queued {
			return false
		}
		return h.claudeGiveUpFor(artifactID)
	}, 6*time.Second, 25*time.Millisecond,
		"the aged entry must escalate into a give-up record")

	// The user keeps typing on the fork's branch: the file changes, imports,
	// and the trigger must NOT re-enqueue over the standing record.
	h.typeInClaudeCode(t, "user", "Anything else?")
	require.True(t, h.orch.handleEvent(h.session))
	_, queued := h.claudeQueueEntry(artifactID)
	require.False(t, queued,
		"a standing give-up record must suppress the post-import trigger")
	require.True(t, h.claudeGiveUpFor(artifactID),
		"the give-up record itself must survive the import")
}
