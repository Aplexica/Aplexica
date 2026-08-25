package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// The loop-safety invariant: a rollout we synthesize, re-imported through the
// existing canonical encoder, must yield exactly the turns we started with — so
// Aplexica's own re-materialization produces no "new" turns and never loops.
func TestCodexRollout_RoundTripStable(t *testing.T) {
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is the project name?"},
		{Role: "assistant", Text: "No project name is specified. The current workspace directory is `/Users/testuser`."},
		{Role: "user", Text: "what is 2+2?"},
		{Role: "assistant", Text: "4."},
	}
	rollout := transcodeToCodexRollout(turns, "tid-123", "tid-123", "experiment", "/Users/testuser", "claude-code",
		time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))

	require.Contains(t, rollout, `"aplexica_thread_id":"tid-123"`, "thread id must be stamped for the merge")
	require.Contains(t, rollout, `"aplexica_branch_id":"experiment"`, "branch id must be stamped for branch-aware merge")
	require.Contains(t, rollout, `"cli_version":"`+syntheticCodexCLIVersion+`"`)
	require.NotContains(t, rollout, `"cli_version":"0.135.0"`, "synthetic rollouts must not impersonate a stale Codex release")

	generatedPath := filepath.Join(t.TempDir(), "generated.jsonl")
	require.NoError(t, os.WriteFile(generatedPath, []byte(rollout), 0o600))
	marked, err := codexSessionHasAplexicaThreadMarker(generatedPath)
	require.NoError(t, err)
	require.True(t, marked)
	nativePath := filepath.Join(t.TempDir(), "native.jsonl")
	require.NoError(t, os.WriteFile(nativePath, []byte(`{"type":"session_meta","payload":{"id":"native"}}`+"\n"+strings.Repeat("x", 8<<20)), 0o600))
	marked, err = codexSessionHasAplexicaThreadMarker(nativePath)
	require.NoError(t, err)
	require.False(t, marked, "a huge native tail must not enter the whole-file merge probe")
	ref, ok := codexThreadRef([]byte(rollout))
	require.True(t, ok)
	require.Equal(t, "tid-123", ref.ArtifactID)
	require.Equal(t, "experiment", ref.BranchID)
	require.True(t, ref.GeneratedSnapshot)
	require.Equal(t, adapter.ConversationTurnsHash(turns), ref.MaterializedTurnsHash)
	require.Equal(t, len(turns), ref.MaterializedTurnCount)

	continued := rollout + `{"timestamp":"2026-06-01T12:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"continued"}}` + "\n"
	continuedRef, ok := codexThreadRef([]byte(continued))
	require.True(t, ok)
	require.False(t, continuedRef.GeneratedSnapshot, "a later Codex row is not an unchanged generated mirror")
	require.False(t, continuedRef.SanitizedSyntheticTurn)
	require.Equal(t, len(turns), continuedRef.MaterializedTurnCount,
		"native rows must not extend the stamped generated base")

	withPlaceholder := continued + `{"timestamp":"2026-06-01T12:02:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No response requested."}]}}` + "\n"
	placeholderRef, ok := codexThreadRef([]byte(withPlaceholder))
	require.True(t, ok)
	require.False(t, placeholderRef.GeneratedSnapshot)
	require.True(t, placeholderRef.SanitizedSyntheticTurn,
		"the merge must be authorized to repair only an exact placeholder found in a generated rollout")

	// Re-import via the SAME canonical encoder the live import uses.
	roundEvents, err := EncodeCanonical([]byte(rollout))
	require.NoError(t, err)
	got := acf.ExtractTextTurns(roundEvents)
	require.Equal(t, turns, got,
		"materialize → EncodeCanonical → ExtractTextTurns must reproduce the original turns (loop-safety)")
}

func TestCodexRollout_EmptyTurns(t *testing.T) {
	if got := transcodeToCodexRollout(nil, "x", "x", "main", "/Users/testuser", "claude-code", time.Now()); got != "" {
		t.Errorf("no turns should yield empty rollout, got %q", got)
	}
}

func TestAuthenticatedGeneratedConversationPath_BindsDeltaHeadPathAndSessionID(t *testing.T) {
	home := t.TempDir()
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	id := acf.NewID()
	created := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "thread.jsonl", CreatedAt: created, UpdatedAt: created,
	}
	require.NoError(t, store.WriteArtifact(art))
	baseTurns := []acf.TextTurn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}}
	baseEvents := []acf.ConversationEvent{textTurnEvent(baseTurns[0]), textTurnEvent(baseTurns[1])}
	basePayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: baseEvents,
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Branch: acf.MainBranch, Timestamp: created, Payload: basePayload,
	}))
	baseHead, ok, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, ok)
	deltaPayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{textTurnEvent(acf.TextTurn{Role: "user", Text: "q2"})},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Branch: acf.MainBranch, Timestamp: created.Add(time.Second),
		ParentHash: baseHead.Hash, Payload: deltaPayload,
	}))

	materialized, head, ok, err := store.MaterializedConversationHeadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.ConversationDeltaFormatV1, func() string {
		payload, decodeErr := acf.DecodeConversationPayload(head)
		require.NoError(t, decodeErr)
		return payload.Format
	}())
	head.MaterializedConversation = &materialized
	a := New()
	a.HomeDir = home
	plan, ok, err := a.conversationSessionPlan(art, head)
	require.NoError(t, err)
	require.True(t, ok)
	raw := []byte(transcodeToCodexRollout(
		baseTurns, plan.sessionID, id, plan.branchID, home, "claude-code", plan.sessionTime,
	))
	ref, ok := codexThreadRef(raw)
	require.True(t, ok)
	require.NoError(t, os.MkdirAll(filepath.Dir(plan.dest), 0o755))
	require.NoError(t, os.WriteFile(plan.dest, raw, 0o600))

	require.True(t, a.authenticatedGeneratedConversationPath(store, plan.dest, raw, ref))
	require.False(t, a.authenticatedGeneratedConversationPath(
		store, filepath.Join(filepath.Dir(plan.dest), "forged.jsonl"), raw, ref,
	))
	wrongSession := []byte(strings.ReplaceAll(string(raw), plan.sessionID, acf.NewID()))
	require.False(t, a.authenticatedGeneratedConversationPath(store, plan.dest, wrongSession, ref))
}

func TestMaterializeConversationSession_AppendsWithoutReplacingOpenRollout(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	makeHead := func(turns ...acf.TextTurn) acf.Event {
		events := make([]acf.ConversationEvent, 0, len(turns))
		for i, turn := range turns {
			events = append(events, acf.ConversationEvent{
				Type: acf.EventTypeTurn, Role: turn.Role, Timestamp: base.Add(time.Duration(i) * time.Second),
				Content: []acf.ContentBlock{{Type: "text", Text: turn.Text}},
			})
		}
		payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
		require.NoError(t, err)
		return acf.Event{ArtifactID: artifactID, Type: acf.EventTypeUpdate, Timestamp: base, Payload: payload}
	}

	a := &Adapter{HomeDir: home}
	firstTurns := []acf.TextTurn{{Role: "user", Text: "capital?"}, {Role: "assistant", Text: "Warsaw."}, {Role: "user", Text: "population?"}}
	path, ok, err := a.MaterializeConversationSession(art, makeHead(firstTurns...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	before, err := os.Stat(path)
	require.NoError(t, err)
	openWriter, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	defer openWriter.Close()

	completeTurns := append(append([]acf.TextTurn(nil), firstTurns...), acf.TextTurn{Role: "assistant", Text: "1.87 million."})
	_, ok, err = a.MaterializeConversationSession(art, makeHead(completeTurns...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "materialization must preserve the inode held open by Codex")

	_, err = openWriter.WriteString(codexConvLine("user", "and the metro area?"))
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)
	require.Equal(t, append(completeTurns, acf.TextTurn{Role: "user", Text: "and the metro area?"}), acf.ExtractTextTurns(events),
		"a later native append through the original descriptor must remain visible at the path")
}

func TestMaterializeConversationSession_AppendRaceKeepsSingleRolloutAndDefers(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	makeHead := func(turns ...acf.TextTurn) acf.Event {
		events := make([]acf.ConversationEvent, 0, len(turns))
		for i, turn := range turns {
			events = append(events, acf.ConversationEvent{
				Type: acf.EventTypeTurn, Role: turn.Role, Timestamp: base.Add(time.Duration(i) * time.Second),
				Content: []acf.ContentBlock{{Type: "text", Text: turn.Text}},
			})
		}
		payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
		require.NoError(t, err)
		return acf.Event{ArtifactID: artifactID, Type: acf.EventTypeUpdate, Timestamp: base, Payload: payload}
	}

	a := &Adapter{HomeDir: home}
	promptTurns := []acf.TextTurn{
		{Role: "user", Text: "capital?"},
		{Role: "assistant", Text: "Warsaw."},
		{Role: "user", Text: "population?"},
	}
	primary, ok, err := a.MaterializeConversationSession(art, makeHead(promptTurns...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	primaryInfo, err := os.Stat(primary)
	require.NoError(t, err)
	nativeWriter, err := os.OpenFile(primary, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	defer nativeWriter.Close()

	completeTurns := append(append([]acf.TextTurn(nil), promptTurns...),
		acf.TextTurn{Role: "assistant", Text: "About 1.87 million."})
	hookCalls := 0
	retriedPath, ok, _, err := a.materializeConversationSession(art, makeHead(completeTurns...), "claude-code", func(path string) error {
		hookCalls++
		require.Equal(t, primary, path)
		_, writeErr := nativeWriter.WriteString(codexConvLine("user", "and the metro area?"))
		return writeErr
	})
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 1, hookCalls)
	require.Equal(t, primary, retriedPath, "a race must retain the one deterministic rollout")

	afterInfo, err := os.Stat(primary)
	require.NoError(t, err)
	require.True(t, os.SameFile(primaryInfo, afterInfo), "the active primary rollout inode must remain intact")
	primaryRaw, err := os.ReadFile(primary)
	require.NoError(t, err)
	primaryEvents, err := EncodeCanonical(primaryRaw)
	require.NoError(t, err)
	require.Equal(t, append(promptTurns, acf.TextTurn{Role: "user", Text: "and the metro area?"}),
		acf.ExtractTextTurns(primaryEvents), "the raced native continuation must not be reordered behind the remote answer")

	sessionFiles, err := filepath.Glob(filepath.Join(filepath.Dir(primary), "*.jsonl"))
	require.NoError(t, err)
	require.Equal(t, []string{primary}, sessionFiles, "an append race must not create a second resume entry")

	retriedPath, ok, err = a.MaterializeConversationSession(art, makeHead(completeTurns...), "claude-code")
	require.NoError(t, err)
	require.False(t, ok, "delivery must wait until the watcher imports the raced native turn")
	require.Equal(t, primary, retriedPath)
}

func TestMaterializeConversationSession_NonPrefixKeepsSingleRolloutAndDefers(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	baseTime := time.Date(2026, 7, 18, 22, 10, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	makeHead := func(turns ...acf.TextTurn) acf.Event {
		events := make([]acf.ConversationEvent, 0, len(turns))
		for i, turn := range turns {
			events = append(events, acf.ConversationEvent{
				Type: acf.EventTypeTurn, Role: turn.Role, Timestamp: baseTime.Add(time.Duration(i) * time.Second),
				Content: []acf.ContentBlock{{Type: "text", Text: turn.Text}},
			})
		}
		payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
		require.NoError(t, err)
		return acf.Event{ArtifactID: artifactID, Type: acf.EventTypeUpdate, Timestamp: baseTime, Payload: payload}
	}

	a := &Adapter{HomeDir: home}
	baseTurns := []acf.TextTurn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}}
	primary, ok, err := a.MaterializeConversationSession(art, makeHead(baseTurns...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	openWriter, err := os.OpenFile(primary, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	defer openWriter.Close()
	_, err = openWriter.WriteString(codexConvLine("user", "q-from-stale-codex") + codexConvLine("assistant", "a-from-stale-codex"))
	require.NoError(t, err)
	primaryInfo, err := os.Stat(primary)
	require.NoError(t, err)

	canonical := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q-from-peer"},
		{Role: "assistant", Text: "a-from-peer"},
		{Role: "user", Text: "q-from-stale-codex"},
		{Role: "assistant", Text: "a-from-stale-codex"},
	}
	retriedPath, ok, err := a.MaterializeConversationSession(art, makeHead(canonical...), "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, primary, retriedPath)

	afterInfo, err := os.Stat(primary)
	require.NoError(t, err)
	require.True(t, os.SameFile(primaryInfo, afterInfo), "non-prefix recovery must preserve Codex's active inode")
	primaryRaw, err := os.ReadFile(primary)
	require.NoError(t, err)
	primaryEvents, err := EncodeCanonical(primaryRaw)
	require.NoError(t, err)
	require.Equal(t, append(baseTurns,
		acf.TextTurn{Role: "user", Text: "q-from-stale-codex"},
		acf.TextTurn{Role: "assistant", Text: "a-from-stale-codex"},
	), acf.ExtractTextTurns(primaryEvents))

	sessionFiles, err := filepath.Glob(filepath.Join(filepath.Dir(primary), "*.jsonl"))
	require.NoError(t, err)
	require.Equal(t, []string{primary}, sessionFiles, "a divergence must not create remote or recovery rollouts")
}

func TestMaterializeConversationSession_ReusesEchoedPrimaryAndQuarantinesLegacyDuplicates(t *testing.T) {
	home := t.TempDir()
	artifactID := acf.NewID()
	base := time.Date(2026, 7, 23, 12, 20, 19, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: base, UpdatedAt: base,
	}
	makeHead := func(turns ...acf.TextTurn) acf.Event {
		events := make([]acf.ConversationEvent, 0, len(turns))
		for i, turn := range turns {
			events = append(events, acf.ConversationEvent{
				Type: acf.EventTypeTurn, Role: turn.Role, Timestamp: base.Add(time.Duration(i) * time.Second),
				Content: []acf.ContentBlock{{Type: "text", Text: turn.Text}},
			})
		}
		payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
		require.NoError(t, err)
		return acf.Event{ArtifactID: artifactID, Type: acf.EventTypeUpdate, Timestamp: base, Payload: payload}
	}

	a := &Adapter{HomeDir: home}
	prompt := acf.TextTurn{Role: "user", Text: "what is the size of our galaxy?"}
	primary, ok, err := a.MaterializeConversationSession(art, makeHead(prompt), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)

	// Codex can replay the generated prompt before writing the real answer, and
	// older Aplexica releases could also leave another generated resume entry.
	f, err := os.OpenFile(primary, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	require.NoError(t, func() error {
		defer f.Close()
		_, writeErr := f.WriteString(
			codexConvLine("user", prompt.Text) +
				codexConvLine("assistant", "About 100,000 light-years across.") +
				codexConvLine("assistant", "About 100,000 light-years across."),
		)
		return writeErr
	}())

	legacySessionID := acf.NewID()
	legacyPath := filepath.Join(
		filepath.Dir(primary),
		"rollout-"+base.Format("2006-01-02T15-04-05")+"-"+legacySessionID+".jsonl",
	)
	legacy := transcodeToCodexRollout(
		[]acf.TextTurn{prompt},
		legacySessionID,
		artifactID,
		acf.MainBranch,
		home,
		"claude-code",
		base,
	)
	require.NoError(t, os.WriteFile(legacyPath, []byte(legacy), 0o600))

	complete := []acf.TextTurn{prompt, {Role: "assistant", Text: "About 100,000 light-years across."}}
	reused, ok, err := a.MaterializeConversationSession(art, makeHead(complete...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, primary, reused)

	sessionFiles, err := filepath.Glob(filepath.Join(filepath.Dir(primary), "*.jsonl"))
	require.NoError(t, err)
	require.Equal(t, []string{primary}, sessionFiles)
	quarantined, err := filepath.Glob(filepath.Join(home, ".aplexica", "quarantine", "codex-conversations", "*", "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	require.Equal(t, filepath.Base(legacyPath), filepath.Base(quarantined[0]))
}

// TestWriteCodexConversationSession_NativeOriginRejectsSubstringSuffixMismatch
// pins the native-origin identity check to a suffix match, not a substring
// match. A "-COPY" sibling of a native rollout contains the original session
// id as a plain substring of its filename but does not end with it, and must
// never be accepted as the originating rollout the artifact recorded.
func TestWriteCodexConversationSession_NativeOriginRejectsSubstringSuffixMismatch(t *testing.T) {
	dir := t.TempDir()
	const sessionID = "019e0000-0000-7000-8000-000000000101"
	// The session id is a substring of this filename (old strings.Contains
	// check would match) but the filename's actual trailing "-<id>" suffix is
	// "-COPY", not the session id itself.
	path := filepath.Join(dir, "rollout-2026-01-02T03-04-05-"+sessionID+"-COPY.jsonl")
	raw := `{"type":"session_meta","payload":{"id":"` + sessionID + `"}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))

	err := writeCodexConversationSession(
		path, "irrelevant\n", []acf.TextTurn{{Role: "user", Text: "hi"}},
		"canonical-session-id", "thread-id", acf.MainBranch, true, nil,
	)
	require.Error(t, err,
		"a '-COPY' sibling containing the session id only as a substring must not authenticate as the originating native rollout")
	require.Contains(t, err.Error(), "refusing to overwrite unrelated existing session")
}

func TestAppendCodexRolloutIfUnchanged_RejectsSameLengthReplacementInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o644))
	snapshot, snapshotInfo, err := readCodexSessionSnapshot(path)
	require.NoError(t, err)

	replacement := path + ".replacement"
	require.NoError(t, os.WriteFile(replacement, []byte("new\n"), 0o644))
	require.NoError(t, os.Rename(replacement, path))

	err = appendCodexRolloutIfUnchanged(path, snapshot, snapshotInfo, "answer\n")
	require.ErrorIs(t, err, errCodexSessionChanged)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new\n", string(raw), "a replacement inode must never receive an append based on the stale snapshot")
}

func TestCodexThreadRef_MissingBranchDefaultsMain(t *testing.T) {
	raw := []byte(`{"type":"session_meta","payload":{"aplexica_thread_id":"tid-123"}}`)
	ref, ok := codexThreadRef(raw)
	require.True(t, ok)
	require.Equal(t, "tid-123", ref.ArtifactID)
	require.Equal(t, acf.MainBranch, ref.BranchID)
}

func TestCodexThreadRef_LegacyGeneratedRolloutSanitizesPlaceholder(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-07-16T22:38:02Z","type":"session_meta","payload":{"cli_version":"0.135.0","aplexica_thread_id":"legacy-thread","aplexica_branch_id":"main"}}
{"timestamp":"2026-07-16T22:40:17Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No response requested."}]}}
`)
	ref, ok := codexThreadRef(raw)
	require.True(t, ok)
	require.False(t, ref.GeneratedSnapshot)
	require.True(t, ref.SanitizedSyntheticTurn,
		"legacy Aplexica rollouts used real CLI versions but still require exact placeholder repair")
}

func TestCodexThreadRef_LegacySameTimestampRolloutStillAuthenticatesPlaceholder(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-07-16T22:38:02Z","type":"session_meta","payload":{"cli_version":"0.135.0","aplexica_thread_id":"legacy-thread","aplexica_branch_id":"main"}}
{"timestamp":"2026-07-16T22:38:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"real answer"}]}}
{"timestamp":"2026-07-16T22:38:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No response requested."}]}}
`)
	ref, ok := codexThreadRef(raw)
	require.True(t, ok)
	require.True(t, ref.GeneratedSnapshot,
		"older materializers reused their generated timestamp for later rows")
	require.True(t, ref.SanitizedSyntheticTurn,
		"same-timestamp legacy rows must still authorize only the exact placeholder repair")
}
