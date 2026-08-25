package claudecode

import (
	"bytes"
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

// forkedMirrorFixture models an assistant node with two children. One child is
// a pair of rows Aplexica appended and stranded on a dead sibling branch when
// Claude Code appended its own child of the in-memory leaf.
//
// It returns the mirror path, the canonical plan that mirror is a subsequence
// of, and the uuid of the fork point.
type forkedMirrorFixture struct {
	adapter   *Adapter
	art       acf.Artifact
	dest      string
	sessionID string
	forkPoint string
	canonical []acf.TextTurn
}

func newForkedMirrorFixture(t *testing.T, home string, repair bool) forkedMirrorFixture {
	t.Helper()
	a := &Adapter{HomeDir: home, RepairForkedMirrors: repair}
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		// A codex rollout: localConversationSourcePath rejects it, so this
		// artifact can only ever reach the deterministic synthetic mirror.
		SourcePath: filepath.Join(home, ".codex", "sessions", "rollout-2026-07-30T12-00-00-abc.jsonl"),
		CreatedAt:  time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 7, 30, 12, 5, 0, 0, time.UTC),
	}
	first := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	dest, ok, err := a.MaterializeConversationSession(
		art, canonicalConversationHead(t, art.ArtifactID, first...), "codex")
	require.NoError(t, err)
	require.True(t, ok)

	raw, err := os.ReadFile(dest)
	require.NoError(t, err)
	// The assistant leaf of the first materialization is the node Claude Code
	// still holds when Aplexica extends the thread — the "64ba6050" analogue.
	forkPoint := lastClaudeConversationalUUID(raw)
	require.NotEmpty(t, forkPoint)

	// Aplexica extends the mirror normally: these two rows become the orphans.
	extended := append(append([]acf.TextTurn(nil), first...),
		acf.TextTurn{Role: "user", Text: "and of Poland?"},
		acf.TextTurn{Role: "assistant", Text: "Warsaw."},
	)
	art.UpdatedAt = art.UpdatedAt.Add(time.Minute)
	_, ok, err = a.MaterializeConversationSession(
		art, canonicalConversationHead(t, art.ArtifactID, extended...), "codex")
	require.NoError(t, err)
	require.True(t, ok, "the ordinary prefix append must land before the fork is introduced")

	sessionID := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	// Claude Code appends its own child of the SAME node, forking the graph and
	// stranding Aplexica's two rows.
	require.NoError(t, appendNativeClaudeUserAtParent(
		dest, sessionID, forkPoint, "and of Portugal?", true))

	before, err := os.ReadFile(dest)
	require.NoError(t, err)
	projection, err := parseClaudeVisibleLeaf(before)
	require.NoError(t, err)
	require.NotEqual(t, projection.nodeCount, len(projection.turns),
		"the fixture must strand conversational rows off the resume leaf chain")

	// Canonical, after Claude's own question and its answer were imported.
	canonical := append(append([]acf.TextTurn(nil), extended...),
		acf.TextTurn{Role: "user", Text: "and of Portugal?"},
		acf.TextTurn{Role: "assistant", Text: "Lisbon."},
	)
	art.UpdatedAt = art.UpdatedAt.Add(time.Minute)
	return forkedMirrorFixture{
		adapter: a, art: art, dest: dest,
		sessionID: sessionID, forkPoint: forkPoint, canonical: canonical,
	}
}

// appendClaudeAssistantRowAtParent appends one assistant row with an explicit
// content block list and model, so tests can reproduce the interleaved-thinking
// and Desktop-<synthetic> shapes Claude Code actually writes.
func appendClaudeAssistantRowAtParent(
	path, sessionID, parentUUID, model string, blocks []any, updateLeaf bool,
) error {
	uuid := deterministicUUID(sessionID+":assistant:"+model+":"+parentUUID, 0)
	row := map[string]any{
		"type":       "assistant",
		"uuid":       uuid,
		"parentUuid": parentOrNil(parentUUID),
		"sessionId":  sessionID,
		"message": map[string]any{
			"role": "assistant", "type": "message", "model": model, "content": blocks,
		},
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return err
	}
	appendix := append(encoded, '\n')
	if updateLeaf {
		lastPrompt, marshalErr := json.Marshal(map[string]any{
			"type": "last-prompt", "lastPrompt": "", "leafUuid": uuid, "sessionId": sessionID,
		})
		if marshalErr != nil {
			return marshalErr
		}
		appendix = append(appendix, lastPrompt...)
		appendix = append(appendix, '\n')
	}
	return appendClaudeTestRows(path, appendix)
}

func appendClaudeUserRowWithContent(path, sessionID, parentUUID string, content any) error {
	uuid := deterministicUUID(sessionID+":usercontent:"+parentUUID, 0)
	row := map[string]any{
		"type":       "user",
		"uuid":       uuid,
		"parentUuid": parentOrNil(parentUUID),
		"sessionId":  sessionID,
		"message":    map[string]any{"role": "user", "content": content},
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return appendClaudeTestRows(path, append(encoded, '\n'))
}

func fileIdentity(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info
}

func claudeSessionFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".jsonl" {
			out = append(out, entry.Name())
		}
	}
	return out
}

// B3: EncodeCanonical's streaming decoder stops at the first row it cannot
// decode and reports NO error, so a containment set built on it silently
// measures only the prefix before the damage. The strict proof must refuse.
func TestClaudeMirrorRowsContained_DeclinesUndecodableRow(t *testing.T) {
	plan := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
	}
	raw := []byte(`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
		`{"type":"user","uuid":"u1","parentUuid":"u0","message":{"role":"user",` + "\n")

	// Pin the reason this proof may not be built on EncodeCanonical.
	events, err := EncodeCanonical(raw)
	require.NoError(t, err, "EncodeCanonical reports success on a truncated stream")
	require.Len(t, acf.ExtractTextTurns(events), 1,
		"and silently yields only the rows before the damage")

	contained, _, reason := claudeMirrorRowsContained(raw, plan)
	require.False(t, contained)
	require.Equal(t, adapter.SessionDeclineGraphMalformed, reason)
}

// A trailing row without its newline is a writer mid-append, not a structural
// fault: it must decline as a race so the next pass converges.
func TestClaudeMirrorRowsContained_DeclinesTornTail(t *testing.T) {
	raw := []byte(`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
		`{"type":"user","uuid":"u1"`)
	contained, _, reason := claudeMirrorRowsContained(raw, []acf.TextTurn{{Role: "user", Text: "q1"}})
	require.False(t, contained)
	require.Equal(t, adapter.SessionDeclineRace, reason)
}

// B4: ExtractTextTurns DROPS rows whose normalized text is empty, so those rows
// contribute nothing to a turn-based containment set and it passes trivially —
// while a rebuild regenerates from the planned turns and destroys them. The
// row-level proof must see them and decline.
func TestClaudeMirrorRowsContained_DeclinesRowsThatNormalizeToEmpty(t *testing.T) {
	plan := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
	}
	head := `{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
		`{"type":"assistant","uuid":"a0","parentUuid":"u0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n"

	for _, tc := range []struct {
		name string
		row  string
	}{
		{
			name: "injected project memory",
			row:  `{"type":"user","uuid":"u1","parentUuid":"a0","message":{"role":"user","content":"# Project memory\nnever regenerate me"}}`,
		},
		{
			name: "local command context",
			row:  `{"type":"user","uuid":"u1","parentUuid":"a0","message":{"role":"user","content":"<command-name>/model</command-name>"}}`,
		},
		{
			name: "image only",
			row:  `{"type":"user","uuid":"u1","parentUuid":"a0","message":{"role":"user","content":[{"type":"image","source":{"data":"AAAA"}}]}}`,
		},
		{
			name: "scheduled task preamble only",
			row:  `{"type":"user","uuid":"u1","parentUuid":"a0","message":{"role":"user","content":"[IMPORTANT: You are running as a scheduled cron job. Say nothing more.]"}}`,
		},
		{
			name: "assistant tool call",
			row:  `{"type":"assistant","uuid":"a1","parentUuid":"a0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		},
		{
			name: "user tool result",
			row:  `{"type":"user","uuid":"u1","parentUuid":"a0","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(head + tc.row + "\n")

			// The turn-based proof this replaces cannot see the row at all.
			events, err := EncodeCanonical(raw)
			require.NoError(t, err)
			require.True(t, claudeTextTurnsSubsequence(acf.ExtractTextTurns(events), plan),
				"precondition: a turn-based containment proof passes trivially here")

			contained, _, reason := claudeMirrorRowsContained(raw, plan)
			require.False(t, contained, "a row the rebuild cannot reproduce must block the rebuild")
			require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason)
		})
	}
}

// The enumerated exceptions are the two shapes that provably carry no
// conversation on EITHER side: Claude Desktop's reserved <synthetic> reply,
// which canonical encoding skips outright, and the signature-only
// interleaved-thinking record Claude Code >=2.1.204 splits off the visible text
// record that follows it.
func TestClaudeMirrorRowsContained_AcceptsEnumeratedNonContentRows(t *testing.T) {
	plan := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
	}
	raw := []byte(
		`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"t0","parentUuid":"u0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":""}]}}` + "\n" +
			`{"type":"assistant","uuid":"a0","parentUuid":"t0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"assistant","uuid":"s0","parentUuid":"a0","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"indexed"}]}}` + "\n" +
			`{"type":"last-prompt","leafUuid":"a0","sessionId":"s"}` + "\n")

	contained, _, reason := claudeMirrorRowsContained(raw, plan)
	require.True(t, contained)
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)
}

// A NON-EMPTY thinking block is real reasoning the user can read — canonical.go
// preserves it as content for exactly that reason — and the rebuild emits text
// turns only. The enumerated exception covers the signature-only split record
// whose thinking text is "", and nothing wider: this mirror is co-owned, so
// "no synthetic mirror has ever carried thinking" is not a property the proof
// may rely on.
func TestClaudeMirrorRowsContained_DeclinesNonEmptyThinkingOnlyRow(t *testing.T) {
	plan := []acf.TextTurn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}}
	raw := []byte(
		`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"t0","parentUuid":"u0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"weighing it up"}]}}` + "\n" +
			`{"type":"assistant","uuid":"a0","parentUuid":"t0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n")
	contained, _, reason := claudeMirrorRowsContained(raw, plan)
	require.False(t, contained, "reasoning a user can read is content the rebuild cannot reproduce")
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason)
}

// The containment set is ordered: a row the canonical plan simply does not hold
// is an unimported continuation and must block the rebuild.
func TestClaudeMirrorRowsContained_DeclinesRowCanonicalNeverSaw(t *testing.T) {
	plan := []acf.TextTurn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}}
	raw := []byte(
		`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a0","parentUuid":"u0","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"user","uuid":"u1","parentUuid":"a0","message":{"role":"user","content":"typed here, never imported"}}` + "\n")
	contained, _, reason := claudeMirrorRowsContained(raw, plan)
	require.False(t, contained)
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason)
}

// Out-of-order rows are not a subsequence of the plan, so regenerating from the
// plan would reorder the user's transcript. Decline.
func TestClaudeMirrorRowsContained_DeclinesReorderedRows(t *testing.T) {
	plan := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2"},
	}
	raw := []byte(
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"q2"}}` + "\n" +
			`{"type":"user","uuid":"u0","parentUuid":"u1","message":{"role":"user","content":"q1"}}` + "\n")
	contained, _, _ := claudeMirrorRowsContained(raw, plan)
	require.False(t, contained)
}

// Flag OFF is the shipped behaviour, unchanged: the forked mirror declines
// forever and its bytes AND its inode are untouched.
func TestMaterializeConversationSession_ForkedMirrorRepairIsOffByDefault(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, false)
	require.False(t, (&Adapter{}).RepairForkedMirrors, "the repair must default to off")

	before, err := os.ReadFile(fx.dest)
	require.NoError(t, err)
	beforeInfo := fileIdentity(t, fx.dest)

	for i := 0; i < 3; i++ {
		_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
			fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
		require.NoError(t, err)
		require.False(t, ok)
		require.Equal(t, adapter.SessionDeclineForkedMirror, reason)
	}

	after, err := os.ReadFile(fx.dest)
	require.NoError(t, err)
	require.Equal(t, before, after, "the disabled path must not touch a single byte")
	require.True(t, os.SameFile(beforeInfo, fileIdentity(t, fx.dest)),
		"the disabled path must not replace the inode either")
	require.Len(t, claudeSessionFilesIn(t, filepath.Dir(fx.dest)), 1,
		"and must never publish a second session for the thread")
	require.NoDirExists(t, filepath.Join(home, ".aplexica", "quarantine"))
}

// Flag ON: the mirror whose every row is canonical is repaired losslessly, in
// place, on one inode, and the repair is terminal.
func TestMaterializeConversationSession_RepairsForkedSyntheticMirrorInPlace(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)
	beforeInfo := fileIdentity(t, fx.dest)
	beforeRaw, err := os.ReadFile(fx.dest)
	require.NoError(t, err)

	repaired, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok, "a forked mirror holding nothing canonical lacks must be repaired")
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	require.Equal(t, fx.dest, repaired, "the repair must stay on the one stable pathname")

	require.True(t, os.SameFile(beforeInfo, fileIdentity(t, fx.dest)),
		"the repair must preserve the inode a co-owning writer may hold")
	require.Len(t, claudeSessionFilesIn(t, filepath.Dir(fx.dest)), 1,
		"the repair must never create a second session for the thread")

	turns, err := ResumableTextTurns(mustReadFile(t, fx.dest))
	require.NoError(t, err)
	require.Equal(t, fx.canonical, turns,
		"the resume walk must now reconstruct the whole canonical thread")

	// The pre-repair bytes are preserved OUTSIDE ~/.claude, so a rewrite that
	// ever misbehaves leaves evidence and nothing new appears in /resume.
	preimages := preimageFiles(t, home)
	require.Len(t, preimages, 1)
	require.Equal(t, beforeRaw, mustReadFile(t, preimages[0]))

	// Terminal: the next pass matches and writes nothing at all.
	afterRaw := mustReadFile(t, fx.dest)
	afterInfo := fileIdentity(t, fx.dest)
	_, ok, reason, err = fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	require.Equal(t, afterRaw, mustReadFile(t, fx.dest), "a repaired mirror must be inert")
	require.True(t, os.SameFile(afterInfo, fileIdentity(t, fx.dest)))
	require.Len(t, preimageFiles(t, home), 1, "and must not keep taking pre-images")
}

// A forked fixture can carry an interleaved-thinking record. Prove the
// enumerated exception works through the materialization path, not only
// against the predicate in isolation.
func TestMaterializeConversationSession_RepairsForkedMirrorCarryingThinkingRecord(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)
	raw := mustReadFile(t, fx.dest)
	leaf := lastClaudeConversationalUUID(raw)
	require.NoError(t, appendClaudeAssistantRowAtParent(
		fx.dest, fx.sessionID, leaf, "claude-opus-4-8",
		[]any{map[string]any{"type": "thinking", "thinking": ""}}, true))

	_, ok, _, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.True(t, ok, "a signature-only thinking record must not block the repair")
	turns, err := ResumableTextTurns(mustReadFile(t, fx.dest))
	require.NoError(t, err)
	require.Equal(t, fx.canonical, turns)
}

// Flag ON but the fork stranded a row the plan cannot reproduce: the repair
// must refuse and leave every byte alone. This is the loss proof doing its job.
func TestMaterializeConversationSession_ForkedRepairRefusesUnprovableRow(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)
	raw := mustReadFile(t, fx.dest)
	leaf := lastClaudeConversationalUUID(raw)
	require.NoError(t, appendClaudeUserRowWithContent(
		fx.dest, fx.sessionID, leaf, "<command-name>/model</command-name>"))

	before := mustReadFile(t, fx.dest)
	beforeInfo := fileIdentity(t, fx.dest)
	_, ok, reason, err := fx.adapter.MaterializeConversationSessionReason(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineForkedMirror, reason)
	require.Equal(t, before, mustReadFile(t, fx.dest))
	require.True(t, os.SameFile(beforeInfo, fileIdentity(t, fx.dest)))
	require.Empty(t, preimageFiles(t, home), "a refused repair must not even take a pre-image")
}

// Scope limit, enforced rather than inferred: the SYNTHETIC rewrite is never
// authorized for the user's own transcript, whatever the flag says. The native
// rebuild is a separate resolver with a separate proof and its own pre-image;
// this asserts the synthetic one stays shut, which is the invariant
// mirrorRepairPolicy exists to state.
func TestMirrorRepairPolicy_NeverAuthorizesASyntheticRewriteOfANativeOrigin(t *testing.T) {
	home := t.TempDir()
	sessionID := "native-forked"
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", sessionID+".jsonl")
	existing := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	writeNativeClaudeTranscript(t, source, sessionID, home, existing)
	forkPoint := firstClaudeUserUUID(mustReadFile(t, source))
	require.NoError(t, appendNativeClaudeUserAtParent(source, sessionID, forkPoint, "and of Poland?", true))

	a := &Adapter{HomeDir: home, RepairForkedMirrors: true}
	art := nativeDeclineArtifact(t, source)
	head := canonicalConversationHead(t, art.ArtifactID,
		existing[0], existing[1], acf.TextTurn{Role: "user", Text: "and of Poland?"})
	plan, ok, err := a.conversationSessionPlan(art, head)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, plan.nativeOrigin)
	require.Equal(t, claudeMirrorRepairPolicy{}, a.mirrorRepairPolicy(plan, art.ArtifactID),
		"the synthetic rewrite must authorize nothing for a native-origin plan")
}

// Flag OFF: a forked NATIVE transcript is left byte-identical and reports the
// fork, exactly as it did before the native rebuild existed.
func TestMaterializeConversationSession_ForkedNativeUntouchedWhenRepairDisabled(t *testing.T) {
	home := t.TempDir()
	sessionID := "native-forked-off"
	source := filepath.Join(home, ".claude", "projects", "-Users-exampleuser-project", sessionID+".jsonl")
	existing := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	writeNativeClaudeTranscript(t, source, sessionID, home, existing)
	forkPoint := firstClaudeUserUUID(mustReadFile(t, source))
	require.NoError(t, appendNativeClaudeUserAtParent(source, sessionID, forkPoint, "and of Poland?", true))

	a := &Adapter{HomeDir: home}
	art := nativeDeclineArtifact(t, source)
	head := canonicalConversationHead(t, art.ArtifactID,
		existing[0], existing[1], acf.TextTurn{Role: "user", Text: "and of Poland?"})

	before := mustReadFile(t, source)
	beforeInfo := fileIdentity(t, source)
	_, ok, reason, err := a.MaterializeConversationSessionReason(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineForkedMirror, reason)
	require.Equal(t, before, mustReadFile(t, source))
	require.True(t, os.SameFile(beforeInfo, fileIdentity(t, source)))
	require.Len(t, claudeSessionFilesIn(t, filepath.Dir(source)), 1)
	require.Empty(t, preimageFiles(t, home))
}

// Read-before-clobber: a native append that lands between the snapshot and the
// commit must abort the repair with nothing lost and no second session.
func TestRepairForkedClaudeMirror_NativeAppendBeforeCommitAbortsLosslessly(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)

	snapshot, exists, changed, err := readClaudeSessionSnapshot(fx.dest)
	require.NoError(t, err)
	require.True(t, exists)
	require.False(t, changed)

	// Claude Code continues the session after we snapshotted it.
	require.NoError(t, appendNativeClaudeUserAtParent(
		fx.dest, fx.sessionID, lastClaudeConversationalUUID(snapshot.raw), "raced in", true))
	raced := mustReadFile(t, fx.dest)

	policy := claudeMirrorRepairPolicy{
		repairForkedMirror: true,
		syntheticDest:      fx.dest,
		preimageDir:        filepath.Join(home, ".aplexica", "quarantine", "claude-conversations", "test"),
	}
	rebuilt, err := rebuildDivergedClaudeMirror(
		fx.dest, snapshot,
		func(uuids []string, bridges []claudeBridgeRow) string {
			return transcodeToClaudeSessionWithUUIDs(
				fx.canonical, uuids, bridges, fx.sessionID, fx.art.ArtifactID, acf.MainBranch,
				home, "codex", "t", fx.art.UpdatedAt)
		},
		fx.canonical, fx.sessionID, fx.art.ArtifactID, acf.MainBranch, policy)
	require.NoError(t, err)
	require.False(t, rebuilt, "a stale snapshot must never authorize a rewrite")
	require.Equal(t, raced, mustReadFile(t, fx.dest), "the racing append must survive intact")
	require.Len(t, claudeSessionFilesIn(t, filepath.Dir(fx.dest)), 1)
	require.NoDirExists(t, policy.preimageDir)
}

// The commit primitive keeps the inode and refuses a stale snapshot on its own,
// so it is safe even if a caller ever reaches it without the outer re-read.
func TestRewriteClaudeSessionIfSnapshotCurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o644))

	snapshot, exists, changed, err := readClaudeSessionSnapshot(path)
	require.NoError(t, err)
	require.True(t, exists)
	require.False(t, changed)
	before := fileIdentity(t, path)

	written, raced, err := rewriteClaudeSessionIfSnapshotCurrent(path, snapshot, []byte("two\nthree\n"))
	require.NoError(t, err)
	require.True(t, written)
	require.False(t, raced)
	require.Equal(t, "two\nthree\n", string(mustReadFile(t, path)))
	require.True(t, os.SameFile(before, fileIdentity(t, path)), "the rewrite must reuse the inode")

	// The same (now stale) snapshot must be refused without touching the file.
	written, raced, err = rewriteClaudeSessionIfSnapshotCurrent(path, snapshot, []byte("clobbered\n"))
	require.NoError(t, err)
	require.False(t, written)
	require.True(t, raced)
	require.Equal(t, "two\nthree\n", string(mustReadFile(t, path)))
}

// Repair-pass budget (design rule 3). The quarantine breaker trips at 3
// failures / 10 minutes and blocks ALL materialization. The repair makes at
// most one commit attempt per materialization pass, and a repaired mirror is
// inert on the next pass, so a device cannot generate repair traffic at all.
func TestForkedMirrorRepair_MakesAtMostOneCommitAttemptPerPass(t *testing.T) {
	home := t.TempDir()
	fx := newForkedMirrorFixture(t, home, true)

	// Ten consecutive passes over an UNREPAIRABLE fork: zero writes, so zero
	// failures can be charged anywhere. 0 < 3 per 10 minutes.
	require.NoError(t, appendClaudeUserRowWithContent(
		fx.dest, fx.sessionID, lastClaudeConversationalUUID(mustReadFile(t, fx.dest)),
		"<command-name>/model</command-name>"))
	before := mustReadFile(t, fx.dest)
	beforeInfo := fileIdentity(t, fx.dest)
	for i := 0; i < 10; i++ {
		_, ok, _, err := fx.adapter.MaterializeConversationSessionReason(
			fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex")
		require.NoError(t, err)
		require.False(t, ok)
	}
	require.Equal(t, before, mustReadFile(t, fx.dest))
	require.True(t, os.SameFile(beforeInfo, fileIdentity(t, fx.dest)))

	// And a REPAIRABLE fork converges in exactly one write: the mtime/size
	// witness changes once across ten passes.
	repairable := newForkedMirrorFixture(t, t.TempDir(), true)
	writes := 0
	last := mustReadFile(t, repairable.dest)
	for i := 0; i < 10; i++ {
		_, _, _, err := repairable.adapter.MaterializeConversationSessionReason(
			repairable.art,
			canonicalConversationHead(t, repairable.art.ArtifactID, repairable.canonical...), "codex")
		require.NoError(t, err)
		now := mustReadFile(t, repairable.dest)
		if !bytes.Equal(last, now) {
			writes++
			last = now
		}
	}
	require.Equal(t, 1, writes, "ten passes must produce exactly one commit")
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// preimageFiles lists every pre-repair copy under the quarantine root. They
// live outside ~/.claude by construction, so they can never enter /resume.
func preimageFiles(t *testing.T, home string) []string {
	t.Helper()
	root := filepath.Join(home, ".aplexica", "quarantine", "claude-conversations")
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	return out
}
