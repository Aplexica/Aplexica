package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// appendClaudeSidechainRow appends a Task/sub-agent row: conversational in
// shape, on its own root, and unreachable from the main resume walk.
func appendClaudeSidechainRow(path, sessionID, text string) error {
	row := map[string]any{
		"type":        "user",
		"uuid":        deterministicUUID(sessionID+":sidechain:"+text, 0),
		"parentUuid":  nil,
		"isSidechain": true,
		"sessionId":   sessionID,
		"message":     map[string]any{"role": "user", "content": text},
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return appendClaudeTestRows(path, append(encoded, '\n'))
}

// --- the loss proof must prove the whole ROW, not its text projection ---

// A captioned screenshot is the ordinary way a user pastes an image, and it
// yields exactly ONE text turn — the caption. A turn-level comparison therefore
// matches it against a planned turn and authorizes a rewrite that emits a bare
// text row, deleting the image. The proof must see the block list.
func TestClaudeMirrorRowsContained_DeclinesImageBesideItsCaption(t *testing.T) {
	plan := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "look at this"},
	}
	raw := []byte(
		`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a0","parentUuid":"u0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"user","uuid":"u1","parentUuid":"a0","message":{"role":"user","content":[{"type":"text","text":"look at this"},{"type":"image","source":{"data":"PNGPAYLOAD"}}]}}` + "\n")

	// Precondition: the row DOES project to exactly one matching text turn, so a
	// turn-level proof passes here and the rewrite would destroy the image.
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)
	require.True(t, claudeTextTurnsSubsequence(acf.ExtractTextTurns(events), plan))

	contained, _, reason := claudeMirrorRowsContained(raw, plan)
	require.False(t, contained, "a matched row carrying an image is not reproducible")
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason)
}

// thinking + visible text in one assistant row is the dominant Claude Code
// shape with extended thinking on. It projects to one text turn, so the
// turn-level proof matches it and the rewrite deletes the reasoning.
func TestClaudeMirrorRowsContained_DeclinesThinkingBesideVisibleText(t *testing.T) {
	plan := []acf.TextTurn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}}
	raw := []byte(
		`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a0","parentUuid":"u0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"REASONING-THE-USER-CAN-READ"},{"type":"text","text":"a1"},{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}` + "\n")

	contained, _, reason := claudeMirrorRowsContained(raw, plan)
	require.False(t, contained)
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason)
}

// End-to-end through the real materialization path: the repair must refuse and
// the image payload must still be on disk afterwards.
func TestMaterializeConversationSession_ForkedRepairPreservesPastedImage(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)
	require.NoError(t, appendClaudeUserRowWithContent(
		fx.dest, fx.sessionID, lastClaudeConversationalUUID(mustReadFile(t, fx.dest)),
		[]any{
			map[string]any{"type": "text", "text": "look at this"},
			map[string]any{"type": "image", "source": map[string]any{"data": "PNGPAYLOAD"}},
		}))
	canonical := append(append([]acf.TextTurn(nil), fx.canonical...),
		acf.TextTurn{Role: "user", Text: "look at this"})

	before := mustReadFile(t, fx.dest)
	_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, canonical...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineForkedMirror, reason)
	require.Equal(t, before, mustReadFile(t, fx.dest))
	require.Contains(t, string(mustReadFile(t, fx.dest)), "PNGPAYLOAD",
		"the pasted image must survive a repair pass that could not reproduce it")
}

// The same, for extended-thinking reasoning the user can read.
func TestMaterializeConversationSession_ForkedRepairPreservesThinking(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)
	require.NoError(t, appendClaudeAssistantRowAtParent(
		fx.dest, fx.sessionID, lastClaudeConversationalUUID(mustReadFile(t, fx.dest)),
		"claude-opus-4-8",
		[]any{map[string]any{"type": "thinking", "thinking": "REASONING-THE-USER-CAN-READ"}}, false))

	_, ok, _, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Contains(t, string(mustReadFile(t, fx.dest)), "REASONING-THE-USER-CAN-READ",
		"a rebuild that emits text turns only must never drop real reasoning")
}

// --- sidechains are not forks ---

// A Task/sub-agent transcript lives on its own root, so the main resume walk
// cannot reach it. Counting it made `nodeCount != len(turns)` true for a
// perfectly healthy file, which mis-reported the file as a forked mirror AND —
// with the repair enabled — routed it into a whole-file rewrite that would
// flatten the sub-agent's prompts into the resumable thread.
func TestParseClaudeVisibleLeaf_SidechainRowIsNotAFork(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		SourcePath:       filepath.Join(home, ".codex", "sessions", "rollout-side.jsonl"),
	}
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	dest, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, art.ArtifactID, turns...), "codex")
	require.NoError(t, err)
	require.True(t, ok)

	sessionID := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	require.NoError(t, appendClaudeSidechainRow(dest, sessionID, "SUBAGENT-PROMPT"))

	projection, err := parseClaudeVisibleLeaf(mustReadFile(t, dest))
	require.NoError(t, err)
	require.Equal(t, len(projection.turns), projection.nodeCount,
		"a sub-agent transcript must not make a healthy mirror look forked")
	require.Equal(t, turns, projection.turns)
}

// With the repair enabled, a healthy mirror carrying a sidechain must still
// materialize cleanly rather than be rewritten.
func TestMaterializeConversationSession_SidechainDoesNotTriggerTheRepair(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home, RepairForkedMirrors: true}
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		SourcePath:       filepath.Join(home, ".codex", "sessions", "rollout-side2.jsonl"),
	}
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	head := canonicalConversationHead(t, art.ArtifactID, turns...)
	dest, ok, err := a.MaterializeConversationSession(art, head, "codex")
	require.NoError(t, err)
	require.True(t, ok)

	sessionID := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	require.NoError(t, appendClaudeSidechainRow(dest, sessionID, "SUBAGENT-PROMPT"))
	before := mustReadFile(t, dest)

	_, ok, reason, err := a.MaterializeConversationSessionReason(art, head, "codex")
	require.NoError(t, err)
	require.True(t, ok, "a healthy mirror is not a forked mirror")
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	require.Equal(t, before, mustReadFile(t, dest),
		"and nothing may be rewritten, so the sub-agent transcript stays where it is")
	require.Contains(t, string(mustReadFile(t, dest)), "SUBAGENT-PROMPT")
}

// Belt and braces: even if a fork DID coexist with a sidechain, containment
// must refuse rather than flatten the sub-agent's prompts into the main chain.
func TestClaudeMirrorRowsContained_DeclinesSidechainRow(t *testing.T) {
	plan := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "SUBAGENT-PROMPT"},
	}
	raw := []byte(
		`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a0","parentUuid":"u0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"user","uuid":"s0","parentUuid":null,"isSidechain":true,"message":{"role":"user","content":"SUBAGENT-PROMPT"}}` + "\n")
	contained, _, reason := claudeMirrorRowsContained(raw, plan)
	require.False(t, contained)
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason)
}

// --- an empty mirror must be recoverable, and must never read as a race ---

// A zero-length session file is a structural state, not a writer mid-append.
// Calling it a race told the operator "the destination was being written on
// every attempt" about a thread that had been destroyed, and routed it to the
// one retry class that never terminates.
func TestClassifyClaudeMirrorDecline_EmptyFileIsStructural(t *testing.T) {
	require.Equal(t, adapter.SessionDeclineGraphMalformed,
		classifyClaudeMirrorDecline(nil, []acf.TextTurn{{Role: "user", Text: "q"}}, "s", "t", acf.MainBranch))
	require.Equal(t, adapter.SessionDeclineRace,
		classifyClaudeMirrorDecline([]byte(`{"type":"user"`),
			[]acf.TextTurn{{Role: "user", Text: "q"}}, "s", "t", acf.MainBranch),
		"a torn TRAILING row is still a live writer")
}

// The post-crash state the truncate-then-write commit can leave behind: the
// pathname exists and holds zero bytes. Before this, every subsequent pass
// reported a race and nothing could ever recreate the thread.
func TestMaterializeConversationSession_RecreatesAnEmptyMirror(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, false)
	beforeInfo := fileIdentity(t, fx.dest)

	// Simulate a crash between Truncate(0) and the write.
	require.NoError(t, os.Truncate(fx.dest, 0))
	require.Empty(t, mustReadFile(t, fx.dest))

	_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok, "an empty mirror holds no unimported turn, so it must be recreated")
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	require.True(t, os.SameFile(beforeInfo, fileIdentity(t, fx.dest)),
		"and recreated on the same inode a co-owning writer may hold")
	require.Len(t, claudeSessionFilesIn(t, filepath.Dir(fx.dest)), 1,
		"never by publishing a second session for the thread")

	turns, err := ResumableTextTurns(mustReadFile(t, fx.dest))
	require.NoError(t, err)
	require.Equal(t, fx.canonical, turns)
}

// The in-process half of the same protection: if anything fails after the
// destructive truncate, the snapshot bytes go back on the same inode.
func TestRestoreClaudeSessionBytes_PutsTheSnapshotBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	original := []byte("one\ntwo\n")
	require.NoError(t, os.WriteFile(path, original, 0o644))
	before := fileIdentity(t, path)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(0))
	require.Empty(t, mustReadFile(t, path), "precondition: the destructive step happened")

	cause := os.ErrDeadlineExceeded
	require.ErrorIs(t, restoreClaudeSessionBytes(f, original, cause), cause,
		"the original cause must survive the rollback")
	require.NoError(t, f.Close())

	require.Equal(t, original, mustReadFile(t, path))
	require.True(t, os.SameFile(before, fileIdentity(t, path)))
}

// --- the rebuild must keep the identity a live writer holds in memory ---

func TestClaudeRebuildUUIDs_ReusesMatchedRowsWithoutCollision(t *testing.T) {
	// Index 2 keeps the uuid it carried at its OLD index 0, which is also the
	// natural default for index 0. Assigning both would duplicate a uuid and
	// make parseClaudeVisibleLeaf reject the whole file.
	collide := deterministicUUID("sess", 0)
	uuids := claudeRebuildUUIDs("sess", 4, claudeContainment{preserved: map[int]string{2: collide}})
	require.Equal(t, collide, uuids[2], "a matched row keeps its identity")

	seen := map[string]bool{}
	for i, u := range uuids {
		require.NotEmpty(t, u)
		require.False(t, seen[u], "uuid %q assigned twice (index %d)", u, i)
		seen[u] = true
	}
	require.Len(t, seen, 4)

	// With nothing preserved the assignment is byte-identical to the shipped one.
	require.Equal(t,
		[]string{
			deterministicUUID("sess", 0), deterministicUUID("sess", 1),
			deterministicUUID("sess", 2), deterministicUUID("sess", 3),
		},
		claudeRebuildUUIDs("sess", 4, claudeContainment{}))
}

// The missing half of the required race test. Claude Code appends a child of
// the leaf it holds IN MEMORY, so a rebuild that regenerated every uuid left
// its next append parented on a node the file no longer holds — and
// parseClaudeVisibleLeaf hard-errors on that, converting a repairable
// forked_mirror into a permanently unrepairable graph_malformed with no remedy.
func TestMaterializeConversationSession_RepairKeepsClaudeRowIdentity(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)

	// The uuid Claude Code wrote, and therefore still holds as its leaf.
	claudeUUID := lastClaudeConversationalUUID(mustReadFile(t, fx.dest))
	require.NotEmpty(t, claudeUUID)

	_, ok, _, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok)

	repaired := mustReadFile(t, fx.dest)
	require.Contains(t, string(repaired), claudeUUID,
		"the rebuild must reuse the uuid of every row it matched")

	// Claude Code now appends its next turn parented on that in-memory leaf.
	require.NoError(t, appendNativeClaudeUserAtParent(
		fx.dest, fx.sessionID, claudeUUID, "and of Spain?", true))

	_, parseErr := parseClaudeVisibleLeaf(mustReadFile(t, fx.dest))
	require.NoError(t, parseErr,
		"a stale in-memory leaf must still resolve, or the mirror becomes unrepairable")

	_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineForkedMirror, reason,
		"a new fork is repairable; graph_malformed would be terminal and remedy-less")
}
