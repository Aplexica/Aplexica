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
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

// typeInClaudeCodeAt is typeInClaudeCode with the parent named explicitly, which
// is what a still-open Claude Code actually does: it appends a child of the leaf
// it holds IN MEMORY, not of whatever row is last in the file.
func (h *nativeDivergenceHarness) typeInClaudeCodeAt(t *testing.T, parent, role, text string) string {
	t.Helper()
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
	return uuid
}

// THE OWNER'S ACCEPTANCE CRITERION IN ITS REALISTIC ORDERING.
//
// The sibling test types the third Claude turn before any materialization
// lands, which produces a plain DIVERGENCE. In practice the materialization
// lands first — it is a prefix append onto the file's tip and always succeeds —
// and the still-open Claude Code then parents its next prompt at the leaf it
// held before that append, FORKING the graph at that node.
//
// The planner compares turns in file order, so it reports a perfectly writable
// native plan; only the writer discovers the graph cannot be walked. Routing the
// repair on the planner's reason alone left this ordering permanently stuck.
func TestOwnerScenario_MaterializationFirstThenClaudeAppendsAtItsOwnLeaf(t *testing.T) {
	h := newNativeDivergenceHarness(t, true)

	h.typeInClaudeCode(t, "user", "What is the size of Neptune?")
	h.typeInClaudeCode(t, "assistant", "About four times Earth's diameter.")
	require.True(t, h.orch.handleEvent(h.session))
	rollout := h.codexRollout(t)

	// Claude's own leaf, captured BEFORE Aplexica appends the codex turns.
	claudeLeaf := lastClaudeUUIDIn(mustReadSession(t, h.session))

	h.typeInCodex(t, rollout, "user", "What is the temperature on Neptune?")
	h.typeInCodex(t, rollout, "assistant", "Around -214 C.")
	require.True(t, h.orch.handleEvent(rollout))

	// Let the materialization of the codex turns land in the Claude transcript.
	require.Eventually(t, func() bool {
		turns, err := claudecode.ResumableTextTurns(mustReadSession(t, h.session))
		return err == nil && len(turns) == 4
	}, 6*time.Second, 25*time.Millisecond,
		"the codex turns must reach the Claude transcript first; got %v",
		readClaudeTurns(t, h.session))

	// NOW the still-open Claude Code types, parented at its stale in-memory leaf.
	// The graph forks at that node, and canonical absorbs the new turn on the
	// import that follows.
	h.typeInClaudeCodeAt(t, claudeLeaf, "user", "What is the closest planet to Neptune?")
	require.True(t, h.orch.handleEvent(h.session))
	require.Eventually(t, func() bool { return len(h.canonicalTurns(t)) == 5 },
		4*time.Second, 25*time.Millisecond, "canonical must absorb the forked continuation")

	// The transcript is now forked and Claude Code's own resume walk reaches
	// only its own branch — three of the five turns — which is exactly the
	// "frozen at its own turns" the owner reported.
	_, walkErr := claudecode.ResumableTextTurns(mustReadSession(t, h.session))
	require.Error(t, walkErr, "precondition: the graph forked")

	// The next codex turn fans back out to claude-code, and THAT pass is where
	// the writer discovers the fork on an otherwise perfectly writable plan.
	h.typeInCodex(t, rollout, "assistant", "Uranus.")
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
		return acf.TextTurnsEqual(h.canonicalTurns(t), expected)
	}, 4*time.Second, 25*time.Millisecond,
		"canonical must hold every turn; got %v", h.canonicalTurns(t))

	var settled []acf.TextTurn
	require.Eventually(t, func() bool {
		turns, err := claudecode.ResumableTextTurns(mustReadSession(t, h.session))
		if err != nil {
			return false
		}
		settled = turns
		return acf.TextTurnsEqual(turns, expected)
	}, 6*time.Second, 25*time.Millisecond,
		"Claude Code must end up with ALL turns from ALL agents; last walkable projection %v", settled)

	require.NotContains(t, string(mustReadSession(t, h.session)), "aplexicaThreadId")
	require.False(t, h.tracker.IsQuarantined("claude-code", time.Now()))
}

// The turns the nudge absorbs are a real canonical commit, so every OTHER agent
// is now behind on them. importOnly returns before fanOut — deliberately, so the
// pass can charge the quarantine breaker nothing — which means nothing else in
// the tree derives "codex is behind on this artifact". Handing the targets to
// the deferral queue is what closes that, and it is the difference between
// convergence and a canonical-only absorb nobody ever sees.
func TestNudgeAbsorb_FansTheAbsorbedTurnsOutToTheOtherAgent(t *testing.T) {
	h := newNativeDivergenceHarness(t, true)

	h.typeInClaudeCode(t, "user", "What is the size of Neptune?")
	h.typeInClaudeCode(t, "assistant", "About four times Earth's diameter.")
	require.True(t, h.orch.handleEvent(h.session))
	rollout := h.codexRollout(t)

	h.typeInCodex(t, rollout, "user", "What is the temperature on Neptune?")
	h.typeInCodex(t, rollout, "assistant", "Around -214 C.")
	h.typeInClaudeCode(t, "user", "What is the closest planet to Neptune?")
	h.typeInClaudeCode(t, "assistant", "Uranus.")

	require.True(t, h.orch.handleEvent(rollout))

	// The nudge's pass, driven directly. nudgeDivergedNativeImport is a thin
	// rate-limited wrapper around exactly this call, and driving the call is what
	// isolates the property under test: an import-only pass returns BEFORE
	// fanOut, so the turns it absorbs reach canonical and — without the deferral
	// hand-off — no other agent. (The wrapper cannot be driven end to end here:
	// its trigger is a drain attempt that reaches the adapter, and claude-code's
	// runtime is not installed in a temp home, so the drain's availability gate
	// withholds every attempt before any decline is produced.)
	h.orch.scanCache.invalidate(h.session)
	handled, _ := h.orch.handleEventWithDisposition(h.session, eventHandlingOptions{importOnly: true})
	require.True(t, handled)

	expected := []acf.TextTurn{
		{Role: "user", Text: "What is the size of Neptune?"},
		{Role: "assistant", Text: "About four times Earth's diameter."},
		{Role: "user", Text: "What is the temperature on Neptune?"},
		{Role: "assistant", Text: "Around -214 C."},
		{Role: "user", Text: "What is the closest planet to Neptune?"},
		{Role: "assistant", Text: "Uranus."},
	}
	require.Eventually(t, func() bool {
		return acf.TextTurnsEqual(h.canonicalTurns(t), expected)
	}, 4*time.Second, 25*time.Millisecond, "canonical must absorb the native tail")

	// THE ASSERTION THIS TEST EXISTS FOR: the absorbed turns reach the OTHER
	// agent, not just canonical.
	require.Eventually(t, func() bool {
		return acf.TextTurnsEqual(codexRolloutTurns(t, rollout), expected)
	}, 8*time.Second, 50*time.Millisecond,
		"codex must receive the turns the nudge absorbed; got %v", codexRolloutTurns(t, rollout))
}

// The device-wide nudge budget must PACE the work, never delete it. A refused
// nudge that also removed its own trigger healed at most one artifact per daemon
// lifetime: the drain's decline short circuit suppresses every later attempt for
// unchanged inputs, so no further diverged decline is ever produced.
func TestNudgeBudget_RefusalKeepsTheTriggerAlive(t *testing.T) {
	o := &Orchestrator{}
	o.deferredMaterialize = map[string]*deferredMaterializationQueue{}
	queue := newDeferredMaterializationQueue()
	queue.entries["artifact-b"] = deferredMaterializationEntry{declineObserved: true}
	queue.ids = []string{"artifact-b"}
	o.deferredMaterialize["claude-code"] = queue

	// Exhaust the window.
	now := time.Now()
	for i := 0; i < nativeReimportNudgesPerWindow; i++ {
		o.nativeReimportAt = append(o.nativeReimportAt, now)
	}

	dest := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(dest, []byte("{}\n"), 0o644))

	refused := o.nudgeDivergedNativeImport("claude-code", "artifact-b", dest)
	require.True(t, refused, "the budget must report that it turned the nudge back")
	require.True(t, o.deferredMaterialize["claude-code"].entries["artifact-b"].declineObserved,
		"the nudge itself does not retract the short circuit")

	o.reofferDivergedNativeNudge("claude-code", "artifact-b")
	require.False(t, o.deferredMaterialize["claude-code"].entries["artifact-b"].declineObserved,
		"a budget refusal must leave the entry able to reproduce its decline")

	// And the witness was not recorded, so the very same pair is offered again
	// once the window rolls.
	o.nativeReimportAt = nil
	require.False(t, o.nudgeDivergedNativeImport("claude-code", "artifact-b", dest),
		"a refused nudge must not have consumed its once-per-bytes budget")
}

// A nudge that actually ran records its witness, so the same unchanged bytes are
// never re-parsed for the same reason — the property the budget cap depends on
// for a backlog to cost N units in total rather than N per pass.
func TestNudgeBudget_SuccessfulNudgeIsNotRepeatedForUnchangedBytes(t *testing.T) {
	o := &Orchestrator{}
	dest := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(dest, []byte("{}\n"), 0o644))

	require.False(t, o.nudgeDivergedNativeImport("claude-code", "artifact-a", dest))
	require.Len(t, o.nativeReimportAt, 1)
	require.False(t, o.nudgeDivergedNativeImport("claude-code", "artifact-a", dest))
	require.Len(t, o.nativeReimportAt, 1, "unchanged bytes must not spend budget twice")
}

func mustReadSession(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// codexRolloutTurns projects the rollout Codex owns through the codex adapter's
// own canonical encoding, so the assertion measures what Codex would resume.
func codexRolloutTurns(t *testing.T, rollout string) []acf.TextTurn {
	t.Helper()
	raw, err := os.ReadFile(rollout)
	if err != nil {
		return nil
	}
	var out []acf.TextTurn
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &row) != nil || row.Payload.Type != "message" {
			continue
		}
		var parts []string
		for _, block := range row.Payload.Content {
			if block.Type == "input_text" || block.Type == "output_text" {
				parts = append(parts, block.Text)
			}
		}
		text, ok := acf.NormalizeTextTurn(row.Payload.Role, strings.Join(parts, "\n\n"))
		if !ok || (row.Payload.Role != "user" && row.Payload.Role != "assistant") {
			continue
		}
		out = append(out, acf.TextTurn{Role: row.Payload.Role, Text: text})
	}
	return out
}
