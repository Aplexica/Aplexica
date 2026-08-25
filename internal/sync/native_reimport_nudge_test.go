package syncd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/stretchr/testify/require"
)

// nativeDivergenceHarness drives the OWNER'S scenario through the real
// orchestrator funnel with the real claude-code and codex adapters: a
// conversation that starts in Claude Code, is continued in Codex, and is then
// continued again in Claude Code WITHOUT resuming.
//
// It uses handleEvent rather than the watcher so the ordering that produces the
// divergence is deterministic. Every file it writes is a real transcript in a
// real temp home; nothing is faked below the adapter boundary.
type nativeDivergenceHarness struct {
	orch      *Orchestrator
	store     *acf.Store
	claude    *claudecode.Adapter
	codex     *codex.Adapter
	tracker   *QuarantineTracker
	home      string
	sessionID string
	session   string
	nextRow   int
}

func newNativeDivergenceHarness(t *testing.T, repair bool) *nativeDivergenceHarness {
	t.Helper()
	home := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.HomeDir = home
	cc.CanonicalConversations = true
	cc.RepairForkedMirrors = repair
	cx := codex.New()
	cx.HomeDir = home

	tracker := DefaultQuarantineTracker()
	orch, err := NewOrchestrator(Config{
		Dir:        home,
		Adapters:   []adapter.Adapter{cc, cx},
		Store:      store,
		Quarantine: tracker,
		RootsByAdapter: map[string][]string{
			"claude-code": {filepath.Join(home, ".claude", "projects")},
			"codex":       {filepath.Join(home, ".codex", "sessions")},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	sessionID := "0f2c1a6d-4b5e-4a71-9d33-1c2b3a4d5e6f"
	session := filepath.Join(home, ".claude", "projects", claudeProjectDirFor(home), sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(session), 0o755))
	require.NoError(t, os.WriteFile(session, nil, 0o644))

	return &nativeDivergenceHarness{
		orch: orch, store: store, claude: cc, codex: cx, tracker: tracker,
		home: home, sessionID: sessionID, session: session,
	}
}

// claudeProjectDirFor mirrors the adapter's own project-dir encoding. It is
// duplicated rather than exported: the encoding is the adapter's private
// contract with Claude Code, and a test is not a reason to widen its surface.
func claudeProjectDirFor(cwd string) string {
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-", ".", "-", "_", "-", " ", "-").Replace(cwd)
}

// typeInClaudeCode appends one turn to the native transcript exactly as Claude
// Code does: a uuid-bearing row parented on the previous one, and a last-prompt
// row naming the new leaf.
func (h *nativeDivergenceHarness) typeInClaudeCode(t *testing.T, role, text string) {
	t.Helper()
	raw, err := os.ReadFile(h.session)
	require.NoError(t, err)
	parent := lastClaudeUUIDIn(raw)
	uuid := fmt.Sprintf("native-%s-%d", role, h.nextRow)
	ts := time.Date(2026, 7, 31, 10, 0, h.nextRow, 0, time.UTC).Format(time.RFC3339Nano)
	h.nextRow++

	row := map[string]any{
		"type": role, "uuid": uuid, "sessionId": h.sessionID,
		"timestamp": ts, "cwd": h.home, "isSidechain": false, "version": "2.1.0",
	}
	if parent == "" {
		row["parentUuid"] = nil
	} else {
		row["parentUuid"] = parent
	}
	if role == "assistant" {
		row["message"] = map[string]any{
			"role": "assistant", "type": "message", "model": "claude-opus-4-8",
			"content": []any{map[string]any{"type": "text", "text": text}},
		}
	} else {
		row["message"] = map[string]any{"role": "user", "content": text}
		row["userType"] = "external"
	}
	appendJSONLine(t, h.session, row)
	appendJSONLine(t, h.session, map[string]any{
		"type": "last-prompt", "lastPrompt": text, "leafUuid": uuid, "sessionId": h.sessionID,
	})
}

// typeInCodex appends one turn to the rollout Codex owns, in Codex's own row
// shape, so the import that follows is the real one.
func (h *nativeDivergenceHarness) typeInCodex(t *testing.T, rollout, role, text string) {
	t.Helper()
	block := "input_text"
	if role == "assistant" {
		block = "output_text"
	}
	// One shared row counter across both agents, so every turn is stamped
	// strictly later than the turn typed before it — which is what makes the
	// wall-clock ordering of the scenario real rather than an artifact of which
	// file happened to be written first.
	ts := time.Date(2026, 7, 31, 10, 0, h.nextRow, 0, time.UTC).Format(time.RFC3339Nano)
	h.nextRow++
	appendJSONLine(t, rollout, map[string]any{
		"timestamp": ts,
		"type":      "response_item",
		"payload": map[string]any{
			"type": "message", "role": role,
			"content": []any{map[string]any{"type": block, "text": text}},
		},
	})
}

func appendJSONLine(t *testing.T, path string, row map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	require.NoError(t, err)
	_, err = f.Write(append(encoded, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
}

func lastClaudeUUIDIn(raw []byte) string {
	last := ""
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			Type string `json:"type"`
			UUID string `json:"uuid"`
		}
		if json.Unmarshal([]byte(line), &row) == nil &&
			(row.Type == "user" || row.Type == "assistant") && row.UUID != "" {
			last = row.UUID
		}
	}
	return last
}

func (h *nativeDivergenceHarness) artifactID(t *testing.T) string {
	t.Helper()
	arts, err := h.store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, arts, 1, "the whole scenario is ONE conversation")
	return arts[0].ArtifactID
}

func (h *nativeDivergenceHarness) canonicalTurns(t *testing.T) []acf.TextTurn {
	t.Helper()
	events, err := h.store.ReadEvents(acf.KindConversation, h.artifactID(t))
	require.NoError(t, err)
	payload, ok, err := acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)
	return acf.ExtractTextTurns(payload.Events)
}

// codexRollout returns the rollout the claude-origin conversation fanned out
// to. It is what Codex would open to "resume that conversation".
func (h *nativeDivergenceHarness) codexRollout(t *testing.T) string {
	t.Helper()
	var found string
	require.Eventually(t, func() bool {
		matches, err := filepath.Glob(filepath.Join(h.home, ".codex", "sessions", "*", "*", "*", "*.jsonl"))
		if err != nil || len(matches) != 1 {
			return false
		}
		found = matches[0]
		return true
	}, 4*time.Second, 25*time.Millisecond, "the claude-origin conversation must reach codex")
	return found
}

// THE OWNER'S ACCEPTANCE CRITERION.
//
// Claude turn -> Codex turn -> Claude turn WITHOUT resuming, on real files
// through the real orchestrator. Before this work the Claude transcript froze
// at its own turns forever: the import refused the native file as a regression
// and never looked at it again, and every materialization pass reported
// `diverged`. Afterwards every turn from every agent must be in canonical AND
// back in the Claude session file.
func TestOwnerScenario_ClaudeThenCodexThenClaudeWithoutResume(t *testing.T) {
	h := newNativeDivergenceHarness(t, true)

	// 1. The conversation starts in Claude Code.
	h.typeInClaudeCode(t, "user", "What is the size of Neptune?")
	h.typeInClaudeCode(t, "assistant", "About four times Earth's diameter.")
	require.True(t, h.orch.handleEvent(h.session))
	rollout := h.codexRollout(t)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "What is the size of Neptune?"},
		{Role: "assistant", Text: "About four times Earth's diameter."},
	}, h.canonicalTurns(t))

	// 2. Codex resumes that conversation and adds a question.
	h.typeInCodex(t, rollout, "user", "What is the temperature on Neptune?")
	h.typeInCodex(t, rollout, "assistant", "Around -214 C.")

	// 3. WITHOUT resuming, Claude Code adds another question first. This is the
	// ordering that produces the divergence: by the time either import runs,
	// each side holds a turn the other lacks.
	h.typeInClaudeCode(t, "user", "What is the closest planet to Neptune?")
	h.typeInClaudeCode(t, "assistant", "Uranus.")

	require.True(t, h.orch.handleEvent(rollout))
	require.True(t, h.orch.handleEvent(h.session))

	artifactID := h.artifactID(t)
	require.Eventually(t, func() bool {
		return len(h.canonicalTurns(t)) == 6
	}, 4*time.Second, 25*time.Millisecond,
		"canonical must hold every turn from both agents, got %v", h.canonicalTurns(t))

	expected := []acf.TextTurn{
		{Role: "user", Text: "What is the size of Neptune?"},
		{Role: "assistant", Text: "About four times Earth's diameter."},
		{Role: "user", Text: "What is the temperature on Neptune?"},
		{Role: "assistant", Text: "Around -214 C."},
		{Role: "user", Text: "What is the closest planet to Neptune?"},
		{Role: "assistant", Text: "Uranus."},
	}
	require.Equal(t, expected, h.canonicalTurns(t))

	// And back into the Claude session file, which is the half that was frozen.
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(h.session)
		if err != nil {
			return false
		}
		turns, err := claudecode.ResumableTextTurns(raw)
		return err == nil && acf.TextTurnsEqual(turns, expected)
	}, 6*time.Second, 25*time.Millisecond,
		"the Claude session file must end up holding every turn from every agent; got %v",
		readClaudeTurns(t, h.session))

	// The repaired file is still the user's own transcript, not an
	// Aplexica-generated mirror: a thread stamp on a pristine native source is a
	// permanent contradiction the planner refuses on the next pass.
	raw, err := os.ReadFile(h.session)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "aplexicaThreadId")

	// Exactly one Claude session for the thread. Multiplying one conversation
	// into recovery sessions is the failure this whole surface exists to avoid.
	sessions, err := filepath.Glob(filepath.Join(
		h.home, ".claude", "projects", "*", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, h.session, sessions[0])

	require.False(t, h.tracker.IsQuarantined("claude-code", time.Now()),
		"healing the owner's scenario must never quarantine an adapter")
	require.NotEmpty(t, artifactID)
}

// Flag OFF must reproduce the frozen behaviour exactly: canonical still learns
// everything (the import absorb is unconditional), but the user's transcript is
// left untouched because the rebuild is the part that is gated.
func TestOwnerScenario_TranscriptIsUntouchedWhenRepairDisabled(t *testing.T) {
	h := newNativeDivergenceHarness(t, false)

	h.typeInClaudeCode(t, "user", "What is the size of Neptune?")
	h.typeInClaudeCode(t, "assistant", "About four times Earth's diameter.")
	require.True(t, h.orch.handleEvent(h.session))
	rollout := h.codexRollout(t)

	h.typeInCodex(t, rollout, "user", "What is the temperature on Neptune?")
	h.typeInCodex(t, rollout, "assistant", "Around -214 C.")
	h.typeInClaudeCode(t, "user", "What is the closest planet to Neptune?")
	h.typeInClaudeCode(t, "assistant", "Uranus.")

	require.True(t, h.orch.handleEvent(rollout))
	require.True(t, h.orch.handleEvent(h.session))
	require.Eventually(t, func() bool {
		return len(h.canonicalTurns(t)) == 6
	}, 4*time.Second, 25*time.Millisecond)

	before, err := os.ReadFile(h.session)
	require.NoError(t, err)
	// Give the deferral drain room to run its passes against the frozen file.
	time.Sleep(200 * time.Millisecond)
	after, err := os.ReadFile(h.session)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after),
		"with the rebuild switched off the user's transcript must not be rewritten")
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "What is the size of Neptune?"},
		{Role: "assistant", Text: "About four times Earth's diameter."},
		{Role: "user", Text: "What is the closest planet to Neptune?"},
		{Role: "assistant", Text: "Uranus."},
	}, readClaudeTurns(t, h.session))
}

// R5, the healing test: an entry ALREADY stuck — the destination byte-stable
// and already in the scan cache, canonical frozen, the drain's own short
// circuit armed — must converge with no user action and no CLI command.
//
// This is the deadlock the nudge exists for. Before it, the import half
// short-circuited on scanCache.unchanged and the materialize half
// short-circuited on observeDeferredMaterializationInputs, each waiting for the
// other, and the artifact stayed frozen indefinitely.
func TestDivergedNativeImportNudge_HealsAStuckEntryWithoutUserAction(t *testing.T) {
	h := newNativeDivergenceHarness(t, true)
	h.typeInClaudeCode(t, "user", "What is the size of Neptune?")
	h.typeInClaudeCode(t, "assistant", "About four times Earth's diameter.")
	require.True(t, h.orch.handleEvent(h.session))
	rollout := h.codexRollout(t)

	h.typeInCodex(t, rollout, "user", "What is the temperature on Neptune?")
	h.typeInCodex(t, rollout, "assistant", "Around -214 C.")
	h.typeInClaudeCode(t, "user", "What is the closest planet to Neptune?")
	h.typeInClaudeCode(t, "assistant", "Uranus.")

	// FREEZE the import half, which is what the shipped code did to itself: the
	// pass that refused this file as a regression still recorded BOTH its scan
	// fingerprint and its destination hash, so afterwards the file reads as
	// already consumed and as not-changed-under-us. From here nothing external
	// will ever re-read it.
	h.orch.scanCache.record(h.session)
	h.orch.recordDestHash(h.session)
	require.True(t, h.orch.scanCache.unchanged(h.session),
		"the fixture must reproduce the frozen import half")
	require.True(t, h.orch.handleEvent(h.session),
		"and a plain watcher event over it must now be a no-op")
	require.Len(t, h.canonicalTurns(t), 2,
		"canonical has NOT learned the Claude continuation: that is the deadlock")

	// Only the codex side is delivered. Its fan-out to claude-code declines
	// `diverged`, and from that decline alone the device must converge.
	require.True(t, h.orch.handleEvent(rollout))

	expected := []acf.TextTurn{
		{Role: "user", Text: "What is the size of Neptune?"},
		{Role: "assistant", Text: "About four times Earth's diameter."},
		{Role: "user", Text: "What is the temperature on Neptune?"},
		{Role: "assistant", Text: "Around -214 C."},
		{Role: "user", Text: "What is the closest planet to Neptune?"},
		{Role: "assistant", Text: "Uranus."},
	}
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(h.session)
		if err != nil {
			return false
		}
		turns, err := claudecode.ResumableTextTurns(raw)
		return err == nil && acf.TextTurnsEqual(turns, expected)
	}, 10*time.Second, 25*time.Millisecond,
		"the stuck transcript must converge with no user action; canonical=%v file=%v",
		h.canonicalTurns(t), readClaudeTurns(t, h.session))

	require.Equal(t, expected, h.canonicalTurns(t))
	require.Equal(t, expected, readClaudeTurns(t, h.session))
	require.False(t, h.tracker.IsQuarantined("claude-code", time.Now()))

	// The give-up record is retired by the SUCCESSFUL write, not by the retry.
	require.Eventually(t, func() bool {
		return len(h.orch.DeferredMaterializations()) == 0
	}, 4*time.Second, 25*time.Millisecond,
		"a healed write must leave no queued entry and no needs_attention row")
}

// Design rule 3, machine-checked: a repair pass must stay under the failure
// budget of the systems it drives.
//
// The quarantine breaker is 3 failures / 10 minutes PER ADAPTER and blocks ALL
// materialization once tripped, live sync included. The nudge is import-only,
// and QuarantineTracker.RecordFailure is fed from exactly one call site — the
// Export loop in fanOut — which an import-only pass returns before reaching. So
// however many times it runs it charges ZERO. It is also capped device-wide, so
// a storm of declines cannot turn into a storm of whole-transcript re-parses.
func TestDivergedNativeImportNudge_StaysUnderTheQuarantineBudget(t *testing.T) {
	h := newNativeDivergenceHarness(t, false)
	h.typeInClaudeCode(t, "user", "What is the size of Neptune?")
	h.typeInClaudeCode(t, "assistant", "About four times Earth's diameter.")
	require.True(t, h.orch.handleEvent(h.session))
	artifactID := h.artifactID(t)

	queue := newDeferredMaterializationQueue()
	// Far more declines than three times the breaker's threshold inside one
	// window, which is the shape that would trip it if this path fed it.
	const declines = 200
	for i := 0; i < declines; i++ {
		h.orch.deferredMaterializeMu.Lock()
		if len(queue.ids) == 0 {
			queue.ids = append(queue.ids, artifactID)
		}
		queue.entries[artifactID] = deferredMaterializationEntry{firstDeferred: time.Now().UTC()}
		h.orch.deferredMaterializeMu.Unlock()
		h.orch.recordDeferredMaterializationFailure(
			"claude-code", queue, false, artifactID,
			deferredMaterializationEntry{},
			newConversationDeclineError("claude-code", artifactID, acf.MainBranch,
				adapter.SessionDeclineDiverged, h.session),
		)
	}

	now := time.Now()
	require.False(t, h.tracker.IsQuarantined("claude-code", now),
		"an import-only repair pass must never be able to quarantine an adapter")
	require.Empty(t, h.tracker.Snapshot(now))

	h.orch.nativeReimportMu.Lock()
	nudges := len(h.orch.nativeReimportAt)
	h.orch.nativeReimportMu.Unlock()
	require.LessOrEqual(t, nudges, nativeReimportNudgesPerWindow,
		"%d declines must not produce more than %d re-reads in one window",
		declines, nativeReimportNudgesPerWindow)
	require.Equal(t, 1, nudges, "the first decline is evidence and must be acted on")
}

// The nudge fires only for a divergence. Every other decline either resolves
// itself or names a file canonical has already fully consumed, so re-reading it
// is unpaid work on the drain's hot path.
func TestDivergedNativeDest_IsDivergedOnly(t *testing.T) {
	for _, reason := range []adapter.SessionDeclineReason{
		adapter.SessionDeclineRace,
		adapter.SessionDeclineNativeAhead,
		adapter.SessionDeclineMirrorDiverged,
		adapter.SessionDeclineForkedMirror,
		adapter.SessionDeclineChainUnspanned,
		adapter.SessionDeclineGraphMalformed,
		adapter.SessionDeclineOptOut,
		adapter.SessionDeclineUnspecified,
	} {
		require.Empty(t, divergedNativeDest(newConversationDeclineError(
			"claude-code", "art", acf.MainBranch, reason, "/tmp/session.jsonl")),
			"reason %s must not trigger a re-read", reason)
	}
	require.Equal(t, "/tmp/session.jsonl", divergedNativeDest(newConversationDeclineError(
		"claude-code", "art", acf.MainBranch, adapter.SessionDeclineDiverged, "/tmp/session.jsonl")))
	require.Empty(t, divergedNativeDest(ErrInboundNativeMaterialization))
}

// The nudge must be available again once the agent has written to the file:
// the pair it remembers is (artifact, destination BYTES), not the artifact
// alone, so a genuine new continuation is never ignored as "already seen".
func TestDivergedNativeImportNudge_RearmsWhenTheDestinationMoves(t *testing.T) {
	h := newNativeDivergenceHarness(t, false)
	h.typeInClaudeCode(t, "user", "What is the size of Neptune?")
	require.True(t, h.orch.handleEvent(h.session))
	artifactID := h.artifactID(t)

	h.orch.nudgeDivergedNativeImport("claude-code", artifactID, h.session)
	h.orch.nudgeDivergedNativeImport("claude-code", artifactID, h.session)
	require.Equal(t, 1, h.nudgeCount(), "an unchanged file is never re-parsed twice")

	// Pretend the window rolled over, then move the file.
	h.orch.nativeReimportMu.Lock()
	h.orch.nativeReimportAt = nil
	h.orch.nativeReimportMu.Unlock()
	h.typeInClaudeCode(t, "assistant", "About four times Earth's diameter.")

	h.orch.nudgeDivergedNativeImport("claude-code", artifactID, h.session)
	require.Equal(t, 1, h.nudgeCount(), "a moved destination re-arms the nudge")
}

func (h *nativeDivergenceHarness) nudgeCount() int {
	h.orch.nativeReimportMu.Lock()
	defer h.orch.nativeReimportMu.Unlock()
	return len(h.orch.nativeReimportAt)
}
