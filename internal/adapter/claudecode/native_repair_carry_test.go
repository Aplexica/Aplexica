package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// --- the rebuild may not delete a uuid-bearing row ---

// A `system/local_command` row and a `queued_command` attachment are the two
// shapes the binding constraint names by name: their bodies are content the
// turn projection never sees, and their uuids are parents a live Claude Code
// can still be holding. A containment-passing transcript may have a
// conversational row parented at one of these rows.
func TestRepairDivergedNativeSession_CarriesUUIDBearingRowsThrough(t *testing.T) {
	fx := newCarriedRowNativeFixture(t, true)

	before := mustReadFile(t, fx.dest)
	require.Contains(t, string(before), "the output the user read")
	require.Contains(t, string(before), "the notes the user pasted")

	_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok, "the fixture is containment-provable and must be repaired")
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)

	after := mustReadFile(t, fx.dest)
	require.Contains(t, string(after), "the output the user read",
		"a local-command body the canonical conversation never held must survive")
	require.Contains(t, string(after), "the notes the user pasted",
		"an attachment body the canonical conversation never held must survive")

	// Every uuid the file held is still in the file. That is the invariant, not
	// a per-subtype allowlist.
	for _, uuid := range allClaudeRowUUIDs(t, before) {
		require.Contains(t, string(after), uuid, "uuid %q was deleted by the rebuild", uuid)
	}

	// And the result is still a single walkable chain holding exactly canonical.
	projection, err := parseClaudeVisibleLeaf(after)
	require.NoError(t, err)
	require.True(t, projection.spans())
	require.False(t, projection.forked)
	require.Equal(t, fx.canonical, projection.turns)
}

// The stranded-parent hazard, end to end. Claude Code appends a child of the
// leaf it holds IN MEMORY; on the reference transcript that leaf is a `system`
// row. If the rebuild dropped it, the very next append names a uuid the file no
// longer holds, parseClaudeVisibleLeaf raises missing_parent, and the user's own
// transcript becomes permanently unresumable — strictly worse than the
// divergence the repair was called for.
func TestRepairDivergedNativeSession_NextAppendAtACarriedBridgeStillParses(t *testing.T) {
	fx := newCarriedRowNativeFixture(t, true)
	bridge := fx.systemUUID

	_, ok, _, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, appendClaudeRowsAtParent(fx.dest, fx.sessionID, bridge,
		"and what about its moons?"))

	projection, err := parseClaudeVisibleLeaf(mustReadFile(t, fx.dest))
	require.NoError(t, err,
		"a live agent's append onto a pre-repair uuid must still resolve")
	require.NotEmpty(t, projection.turns)
}

// --- the loss proof must reject a row it would truncate ---

// acf.NormalizeTextTurn strips a scheduled-task preamble off a user row and
// keeps the rest, so the row matches a planned turn while carrying strictly
// more text than the turn holds. The rebuild emits the turn, so committing on
// the match alone deletes the preamble out of the user's own file.
func TestClaudeMirrorRowsContained_DeclinesALossyNormalization(t *testing.T) {
	preamble := "[IMPORTANT: You are running as a scheduled task. Do not ask questions.]\n\n"
	prompt := "summarize yesterday's commits"
	raw := claudeTranscript(
		claudeUserRow("u1", "", preamble+prompt),
		claudeAssistantRow("a1", "u1", "Here they are."),
		claudeLastPromptRow("a1", prompt),
	)
	// The row really does project onto the bare prompt, so the shipped
	// turn-match accepted it.
	events, encoded := claudeRowCanonicalEvents([]byte(claudeUserRow("u1", "", preamble+prompt)))
	require.True(t, encoded)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: prompt}}, acf.ExtractTextTurns(events))

	contained, _, reason := claudeMirrorRowsContained(raw, []acf.TextTurn{
		{Role: "user", Text: prompt},
		{Role: "assistant", Text: "Here they are."},
	})
	require.False(t, contained, "a row the rebuild would shorten must decline")
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason)
}

// The round-trip clause must not start refusing ordinary rows: a plain prompt
// with surrounding whitespace normalizes to itself trimmed and is still
// contained.
func TestClaudeMirrorRowsContained_AcceptsAPlainRowWithWhitespace(t *testing.T) {
	raw := claudeTranscript(
		claudeUserRow("u1", "", "  what is the capital of Canada?  "),
		claudeAssistantRow("a1", "u1", "Ottawa."),
		claudeLastPromptRow("a1", "what is the capital of Canada?"),
	)
	contained, _, _ := claudeMirrorRowsContained(raw, []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	})
	require.True(t, contained)
}

// --- the owner's scenario, in its REALISTIC ordering ---

// Claude Code writes a turn; Aplexica materializes the foreign turns into the
// same transcript (a plain prefix append, which always succeeds); the still-open
// Claude Code then appends the user's next prompt as a child of the leaf it
// holds IN MEMORY, forking the graph at that node; canonical absorbs that prompt
// on the next import.
//
// The planner compares turns in FILE ORDER, so it sees an EXACT native session
// and reports the plan writable — only the writer discovers the graph cannot be
// walked. Routing the repair on the planner's reason alone therefore left this
// population, which is the common one, permanently stuck at forked_mirror with
// a repair that existed and could never be reached.
func TestMaterializeConversationSession_RepairsTheOwnerScenarioFork(t *testing.T) {
	fx := newOwnerScenarioForkFixture(t, true)

	// Precondition: writable plan, unwalkable graph.
	head := canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...)
	plan, ok, err := fx.adapter.conversationSessionPlan(fx.art, head)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, plan.nativeSource)
	require.True(t, plan.nativeWritable, "the planner sees an exact native session")
	require.Equal(t, adapter.SessionDeclineUnspecified, plan.declineReason)

	pre, err := parseClaudeVisibleLeaf(mustReadFile(t, fx.dest))
	require.NoError(t, err)
	require.False(t, pre.spans())
	require.True(t, pre.forked)
	require.Len(t, pre.turns, 4, "the resume walk sees only one branch")

	_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(fx.art, head, "codex")
	require.NoError(t, err)
	require.True(t, ok, "the owner's scenario must converge")
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)

	// The acceptance criterion: Claude Code ends up with ALL turns from ALL
	// agents, including its own, through its own resume walk.
	turns, err := ResumableTextTurns(mustReadFile(t, fx.dest))
	require.NoError(t, err)
	require.Equal(t, fx.canonical, turns)

	// Terminal: the next pass finds it exact and writes nothing.
	after := mustReadFile(t, fx.dest)
	_, ok, _, err = fx.adapter.MaterializeConversationSessionReason(fx.art, head, "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, string(after), string(mustReadFile(t, fx.dest)))
}

// Flag OFF reproduces today's behaviour on that same shape, byte for byte.
func TestMaterializeConversationSession_OwnerScenarioForkUntouchedWhenDisabled(t *testing.T) {
	fx := newOwnerScenarioForkFixture(t, false)
	before := mustReadFile(t, fx.dest)

	_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineForkedMirror, reason)
	require.Equal(t, string(before), string(mustReadFile(t, fx.dest)))
	require.Empty(t, preimageFiles(t, fx.adapter.HomeDir))
}

// A rewind fork — the user backed a prompt out and asked something else — is
// the population the flattening trade is made for. Nothing may be lost: both
// branches' turns and every uuid survive, and the linearization is the file's
// OWN physical row order, which is what canonical (and therefore every other
// agent) already holds.
func TestRepairDivergedNativeSession_RewindForkLosesNothing(t *testing.T) {
	fx := newRewindForkNativeFixture(t, true)
	before := mustReadFile(t, fx.dest)

	_, ok, _, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok)

	after := mustReadFile(t, fx.dest)
	for _, uuid := range allClaudeRowUUIDs(t, before) {
		require.Contains(t, string(after), uuid)
	}
	turns, err := ResumableTextTurns(after)
	require.NoError(t, err)
	require.Equal(t, fx.canonical, turns,
		"the rebuilt chain is the file's own physical row order")

	// The abandoned branch's prompt is still readable, not silently dropped.
	require.Contains(t, string(after), "what is the closest solar system to ours?")
}

// --- fixtures ---

type carriedRowFixture struct {
	adapter    *Adapter
	art        acf.Artifact
	dest       string
	sessionID  string
	canonical  []acf.TextTurn
	systemUUID string
}

// newCarriedRowNativeFixture is the diverged native shape with the two carried
// row kinds the corpus actually holds: a system/local_command whose body the
// user read, and an attachment whose body the user pasted. Neither projects to a
// canonical turn, so neither can be regenerated from the plan.
func newCarriedRowNativeFixture(t *testing.T, repair bool) carriedRowFixture {
	t.Helper()
	home := t.TempDir()
	a := New()
	a.HomeDir = home
	a.RepairForkedMirrors = repair

	sessionID := "7b2f1e4c-88d3-4b3f-9a2c-3f1e0d1c0b02"
	dest := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), sessionID+".jsonl")
	base := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	systemUUID := deterministicUUID(sessionID+":carried:system", 2)
	rows := []string{
		mustJSONLine(t, map[string]any{"type": "mode", "mode": "default", "sessionId": sessionID}),
		nativeRow(t, sessionID, home, base, 0, "u0", "", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "What is the size of Neptune?"},
		}),
		nativeRow(t, sessionID, home, base, 1, "a0", "u0", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "About four times Earth's diameter."}},
			},
		}),
		nativeRow(t, sessionID, home, base, 2, systemUUID, "a0", map[string]any{
			"type": "system", "subtype": "local_command", "isMeta": true,
			"content": "<command-name>/login</command-name>\n" +
				"<local-command-stdout>the output the user read</local-command-stdout>",
		}),
		nativeRow(t, sessionID, home, base, 3, "att1", systemUUID, map[string]any{
			"type": "attachment", "userType": "external",
			"attachment": map[string]any{
				"type": "queued_command", "text": "the notes the user pasted",
			},
		}),
		nativeRow(t, sessionID, home, base, 4, "u1", "att1", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "What is the closest planet to Neptune?"},
		}),
		nativeRow(t, sessionID, home, base, 5, "a1", "u1", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "Uranus."}},
			},
		}),
		mustJSONLine(t, map[string]any{
			"type": "last-prompt", "lastPrompt": "", "leafUuid": "a1", "sessionId": sessionID,
		}),
	}
	writeClaudeFixtureFile(t, dest, rows)

	canonical := []acf.TextTurn{
		{Role: "user", Text: "What is the size of Neptune?"},
		{Role: "assistant", Text: "About four times Earth's diameter."},
		{Role: "user", Text: "What is the temperature on Neptune?"},
		{Role: "assistant", Text: "Around -214 C."},
		{Role: "user", Text: "What is the closest planet to Neptune?"},
		{Role: "assistant", Text: "Uranus."},
	}
	return carriedRowFixture{
		adapter: a, art: nativeFixtureArtifact(dest), dest: dest, sessionID: sessionID,
		canonical: canonical, systemUUID: systemUUID,
	}
}

// newOwnerScenarioForkFixture reproduces the fork Aplexica MANUFACTURES: the
// canonical suffix is appended at the file's tip, and the still-open Claude Code
// then parents its next prompt at the leaf it held before that append landed.
func newOwnerScenarioForkFixture(t *testing.T, repair bool) carriedRowFixture {
	t.Helper()
	home := t.TempDir()
	a := New()
	a.HomeDir = home
	a.RepairForkedMirrors = repair

	sessionID := "5c9a2d7e-11b4-4c8a-8f01-6d2b3a4c5d03"
	dest := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), sessionID+".jsonl")
	base := time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC)
	rows := []string{
		mustJSONLine(t, map[string]any{"type": "mode", "mode": "default", "sessionId": sessionID}),
		nativeRow(t, sessionID, home, base, 0, "u1", "", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "claude q1"},
		}),
		nativeRow(t, sessionID, home, base, 1, "a1", "u1", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "claude a1"}},
			},
		}),
		// Aplexica's materialization of the codex turns, parented at the tip.
		nativeRow(t, sessionID, home, base, 2, "u2", "a1", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "codex q2"},
		}),
		nativeRow(t, sessionID, home, base, 3, "a2", "u2", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "codex a2"}},
			},
		}),
		mustJSONLine(t, map[string]any{
			"type": "last-prompt", "lastPrompt": "codex q2", "leafUuid": "a2", "sessionId": sessionID,
		}),
		// Claude Code, still open, appends a child of the leaf it holds in
		// memory. THIS is the fork, and Aplexica's own append created it.
		nativeRow(t, sessionID, home, base, 4, "u3", "a1", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "claude q3"},
		}),
		mustJSONLine(t, map[string]any{
			"type": "last-prompt", "lastPrompt": "claude q3", "leafUuid": "u3", "sessionId": sessionID,
		}),
		nativeRow(t, sessionID, home, base, 5, "a3", "u3", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "claude a3"}},
			},
		}),
	}
	writeClaudeFixtureFile(t, dest, rows)

	// Canonical after the import of that physical order.
	canonical := []acf.TextTurn{
		{Role: "user", Text: "claude q1"},
		{Role: "assistant", Text: "claude a1"},
		{Role: "user", Text: "codex q2"},
		{Role: "assistant", Text: "codex a2"},
		{Role: "user", Text: "claude q3"},
		{Role: "assistant", Text: "claude a3"},
	}
	return carriedRowFixture{
		adapter: a, art: nativeFixtureArtifact(dest), dest: dest,
		sessionID: sessionID, canonical: canonical,
	}
}

// newRewindForkNativeFixture is a fork the USER created by backing a prompt out
// and asking something else. Both branches are already in canonical, because
// import reads the file in physical order.
func newRewindForkNativeFixture(t *testing.T, repair bool) carriedRowFixture {
	t.Helper()
	home := t.TempDir()
	a := New()
	a.HomeDir = home
	a.RepairForkedMirrors = repair

	sessionID := "9e1d4c8b-33a7-4d2e-9c05-7a3b1c2d4e04"
	dest := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), sessionID+".jsonl")
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	rows := []string{
		nativeRow(t, sessionID, home, base, 0, "u1", "", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "tell me about the galaxy"},
		}),
		nativeRow(t, sessionID, home, base, 1, "a1", "u1", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "It is large."}},
			},
		}),
		// The branch the user abandoned.
		nativeRow(t, sessionID, home, base, 2, "u2", "a1", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "how many solar systems does it have?"},
		}),
		nativeRow(t, sessionID, home, base, 3, "a2", "u2", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "Billions."}},
			},
		}),
		// The rewind: a second child of the same node.
		nativeRow(t, sessionID, home, base, 4, "u3", "a1", map[string]any{
			"type": "user", "userType": "external",
			"message": map[string]any{"role": "user", "content": "what is the closest solar system to ours?"},
		}),
		nativeRow(t, sessionID, home, base, 5, "a3", "u3", map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": "Alpha Centauri."}},
			},
		}),
		mustJSONLine(t, map[string]any{
			"type": "last-prompt", "lastPrompt": "", "leafUuid": "a3", "sessionId": sessionID,
		}),
	}
	writeClaudeFixtureFile(t, dest, rows)

	canonical := []acf.TextTurn{
		{Role: "user", Text: "tell me about the galaxy"},
		{Role: "assistant", Text: "It is large."},
		{Role: "user", Text: "how many solar systems does it have?"},
		{Role: "assistant", Text: "Billions."},
		{Role: "user", Text: "what is the closest solar system to ours?"},
		{Role: "assistant", Text: "Alpha Centauri."},
	}
	return carriedRowFixture{
		adapter: a, art: nativeFixtureArtifact(dest), dest: dest,
		sessionID: sessionID, canonical: canonical,
	}
}

func nativeFixtureArtifact(dest string) acf.Artifact {
	return acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		SourcePath:       dest,
		CreatedAt:        time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 7, 31, 10, 30, 0, 0, time.UTC),
	}
}

func nativeRow(
	t *testing.T, sessionID, cwd string, base time.Time, index int,
	uuid, parent string, fields map[string]any,
) string {
	t.Helper()
	row := map[string]any{
		"uuid": uuid, "parentUuid": parentOrNil(parent), "sessionId": sessionID,
		"timestamp":   base.Add(time.Duration(index) * time.Second).UTC().Format(time.RFC3339Nano),
		"cwd":         cwd,
		"isSidechain": false, "version": "2.1.0",
	}
	for k, v := range fields {
		row[k] = v
	}
	return mustJSONLine(t, row)
}

func writeClaudeFixtureFile(t *testing.T, path string, rows []string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), claudeSessionFileMode))
}

// allClaudeRowUUIDs lists every uuid the file holds, in file order.
func allClaudeRowUUIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			UUID string `json:"uuid"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		if row.UUID != "" {
			out = append(out, row.UUID)
		}
	}
	return out
}

// appendClaudeRowsAtParent is what a live Claude Code writes when the user
// submits a prompt: the prompt row parented at the leaf it holds in memory, and
// a last-prompt naming it.
func appendClaudeRowsAtParent(path, sessionID, parent, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, claudeSessionFileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	uuid := deterministicUUID(sessionID+":live-append", 0)
	rows := []map[string]any{
		{
			"type": "user", "userType": "external", "uuid": uuid,
			"parentUuid": parentOrNil(parent), "sessionId": sessionID,
			"isSidechain": false, "version": "2.1.0",
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"message":   map[string]any{"role": "user", "content": text},
		},
		{"type": "last-prompt", "lastPrompt": text, "leafUuid": uuid, "sessionId": sessionID},
	}
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// --- the repair pass must not re-pay its cost on every attempt ---

// The rebuild reads the whole file and canonically encodes every row before it
// can discover it will refuse, and some transcripts can never satisfy
// the loss proof. Three paths have no short circuit in front of them — a
// whole-store RefanOutAll, the first drain sweep after every daemon restart, and
// live fan-out — so a refusal that is not remembered is that cost paid again for
// every stuck artifact, in one burst.
func TestRepairDivergedNativeSession_RemembersARefusal(t *testing.T) {
	fx := newCarriedRowNativeFixture(t, true)
	// An image beside its caption: one text turn, more than text on the row, so
	// the loss proof refuses permanently.
	require.NoError(t, appendClaudeImageRow(fx.dest, fx.sessionID,
		lastClaudeConversationalUUID(mustReadFile(t, fx.dest)), "look at this"))

	plan, ok, err := fx.adapter.conversationSessionPlan(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...))
	require.NoError(t, err)
	require.True(t, ok)
	policy := fx.adapter.nativeRepairPolicy(plan, fx.art.ArtifactID)
	require.True(t, policy.repairDivergedNative)

	repaired, err := fx.adapter.repairDivergedNativeSession(
		fx.dest, fx.canonical, fx.sessionID, fx.adapter.HomeDir, fx.art.UpdatedAt, policy)
	require.NoError(t, err)
	require.False(t, repaired)
	require.Len(t, fx.adapter.nativeRepairRefusals, 1, "the refusal must be remembered")
	remembered := fx.adapter.nativeRepairRefusals[fx.dest]
	require.Equal(t, adapter.ConversationTurnsHash(fx.canonical), remembered.planHash)
	require.True(t, fx.adapter.nativeRepairRefused(fx.dest, remembered),
		"unchanged evidence must match the remembered refusal")

	// The unchanged plan must continue to answer from the remembered refusal.
	repaired, err = fx.adapter.repairDivergedNativeSession(
		fx.dest, fx.canonical, fx.sessionID, fx.adapter.HomeDir, fx.art.UpdatedAt, policy)
	require.NoError(t, err)
	require.False(t, repaired)

	// The memo is keyed on the PLAN too: a canonical head that has moved is new
	// evidence and must be re-proven rather than inheriting the old verdict.
	moved := append(append([]acf.TextTurn(nil), fx.canonical...),
		acf.TextTurn{Role: "user", Text: "and its moons?"})
	movedVerdict := remembered
	movedVerdict.planHash = adapter.ConversationTurnsHash(moved)
	require.False(t, fx.adapter.nativeRepairRefused(fx.dest, movedVerdict),
		"a changed plan must miss the remembered refusal")
	repaired, err = fx.adapter.repairDivergedNativeSession(
		fx.dest, moved, fx.sessionID, fx.adapter.HomeDir, fx.art.UpdatedAt, policy)
	require.NoError(t, err)
	require.False(t, repaired)
	require.Equal(t, movedVerdict, fx.adapter.nativeRepairRefusals[fx.dest],
		"the changed plan must be re-proven and remembered independently")
}

// A transcript past the size bound is refused from its stat, never read. The
// pass runs inside a held import slot; reading an oversized transcript to prove
// a rebuild it will then refuse is not a trade worth
// making at any proof strength.
func TestRepairDivergedNativeSession_RefusesAnOversizeTranscript(t *testing.T) {
	fx := newCarriedRowNativeFixture(t, true)
	plan, ok, err := fx.adapter.conversationSessionPlan(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...))
	require.NoError(t, err)
	require.True(t, ok)
	policy := fx.adapter.nativeRepairPolicy(plan, fx.art.ArtifactID)

	before := mustReadFile(t, fx.dest)
	original := nativeRepairMaxBytes
	nativeRepairMaxBytes = 8
	t.Cleanup(func() { nativeRepairMaxBytes = original })

	repaired, err := fx.adapter.repairDivergedNativeSession(
		fx.dest, fx.canonical, fx.sessionID, fx.adapter.HomeDir, fx.art.UpdatedAt, policy)
	require.NoError(t, err)
	require.False(t, repaired)
	require.Equal(t, string(before), string(mustReadFile(t, fx.dest)))
	require.Empty(t, fx.adapter.nativeRepairRefusals,
		"a size refusal is about the bound, not about the file's contents, "+
			"so it must not be cached as a proven refusal")
}

// appendClaudeImageRow appends a user row carrying an image beside its caption:
// exactly one text turn, and content the rebuild cannot reproduce.
func appendClaudeImageRow(path, sessionID, parent, caption string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, claudeSessionFileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	uuid := deterministicUUID(sessionID+":image", 0)
	encoded, err := json.Marshal(map[string]any{
		"type": "user", "userType": "external", "uuid": uuid,
		"parentUuid": parentOrNil(parent), "sessionId": sessionID,
		"isSidechain": false, "version": "2.1.0",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": caption},
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "aGVsbG8=",
			}},
		}},
	})
	if err != nil {
		return err
	}
	_, err = f.Write(append(encoded, '\n'))
	return err
}

// The repair consults the resume graph BEFORE it rewrites, not only in its
// post-write verification. Containment proves nothing is lost; the walk is what
// decides whether the file is one Aplexica may reason about at all. A graph that
// hard-faults — a duplicate uuid here — is guessing territory, and the rebuild
// must refuse it even though every row is provably reproducible.
func TestRepairDivergedNativeSession_RefusesAnUnwalkableGraph(t *testing.T) {
	fx := newCarriedRowNativeFixture(t, true)
	// Duplicate a conversational uuid, which is what parseClaudeVisibleLeaf
	// rejects outright and claudeMirrorRowsContained does not look at.
	raw := mustReadFile(t, fx.dest)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	require.NoError(t, os.WriteFile(fx.dest,
		[]byte(strings.Join(append(lines, lines[1]), "\n")+"\n"), claudeSessionFileMode))

	before := mustReadFile(t, fx.dest)
	_, walkErr := parseClaudeVisibleLeaf(before)
	require.Error(t, walkErr, "precondition: the graph must be unwalkable")

	plan, ok, err := fx.adapter.conversationSessionPlan(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...))
	require.NoError(t, err)
	require.True(t, ok)
	repaired, err := fx.adapter.repairDivergedNativeSession(
		fx.dest, fx.canonical, fx.sessionID, fx.adapter.HomeDir, fx.art.UpdatedAt,
		fx.adapter.nativeRepairPolicy(plan, fx.art.ArtifactID))
	require.NoError(t, err)
	require.False(t, repaired)
	require.Equal(t, string(before), string(mustReadFile(t, fx.dest)))
}
