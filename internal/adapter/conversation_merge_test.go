package adapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func conversationTestSourcePath(store *acf.Store, id string) string {
	return filepath.Join(store.Root, "native", id)
}

func convTurn(role, text string) acf.ConversationEvent {
	return acf.ConversationEvent{Type: acf.EventTypeTurn, Role: role, Content: []acf.ContentBlock{{Type: "text", Text: text}}}
}

func seedConversation(t *testing.T, store *acf.Store, id string, events []acf.ConversationEvent) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "c.jsonl", SourcePath: conversationTestSourcePath(store, id), CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := EncodeCanonicalConversationPayload(events)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: payload,
	}))
}

func threadTurns(t *testing.T, store *acf.Store, id string) []acf.TextTurn {
	t.Helper()
	evs, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	p, ok, err := acf.MaterializedConversationPayload(evs)
	require.NoError(t, err)
	require.True(t, ok)
	return acf.ExtractTextTurns(p.Events)
}

func threadTurnCount(t *testing.T, store *acf.Store, id string) int {
	t.Helper()
	return len(threadTurns(t, store, id))
}

func branchTurnCount(t *testing.T, store *acf.Store, id, branch string) int {
	t.Helper()
	p, _, ok, err := store.ProjectConversationPayloadForBranch(id, branch, acf.BranchProjectionOpts{})
	require.NoError(t, err)
	require.True(t, ok)
	return len(acf.ExtractTextTurns(p.Events))
}

func appendForkForTest(t *testing.T, store *acf.Store, id, branch string, parent acf.Event) acf.Event {
	t.Helper()
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:          acf.NewID(),
		ArtifactID:       id,
		Type:             acf.EventTypeForkOuter,
		Branch:           branch,
		Timestamp:        time.Now().UTC(),
		ParentHash:       parent.Hash,
		ForkSourceBranch: acf.MainBranch,
		ForkFromEventID:  parent.EventID,
		ForkOriginAgent:  "codex",
	}))
	evs, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	return evs[len(evs)-1]
}

func TestMergeConversationByThread(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000001"
	seedConversation(t, store, id, []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello")})
	params := OpaqueParams{SourceAgent: "codex"}

	// Unknown thread → not handled (caller does native import).
	_, handled, err := MergeConversationByThread(context.Background(), store, params, "no-such-thread", []acf.ConversationEvent{convTurn("user", "x")}, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.False(t, handled)

	// Same turns → handled no-op: the LOOP BREAK (no new event).
	ids, handled, err := MergeConversationByThread(context.Background(), store, params, id,
		[]acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello")}, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
	require.Equal(t, 2, threadTurnCount(t, store, id))

	// A continuation → handled, returns the thread id, thread grows.
	cont := []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello"), convTurn("user", "and 2+2?")}
	ids, handled, err = MergeConversationByThread(context.Background(), store, params, id, cont, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	cached, cachedOK, err := store.ValidatedCachedMaterializedConversationPayload(id)
	require.NoError(t, err)
	require.True(t, cachedOK, "generated continuation merge must prime the validated fan-out cache")
	require.Equal(t, acf.ExtractTextTurns(cont), acf.ExtractTextTurns(cached.Events))
	require.Equal(t, 3, threadTurnCount(t, store, id))

	// Re-importing that same continuation is again a no-op (idempotent, loop-safe).
	ids, handled, err = MergeConversationByThread(context.Background(), store, params, id, cont, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
}

func TestMergeConversationByThreadRef_MaterializedFingerprintSkipsStoreReplay(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000099"
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	turns := []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello")}
	hash := ConversationTurnsHash(acf.ExtractTextTurns(turns))

	// There is deliberately no event log. Reaching branch projection would
	// fail; the stamped materialization fingerprint must finish as a no-op
	// immediately after confirming that the thread artifact exists.
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(),
		store,
		OpaqueParams{SourceAgent: "claude-code"},
		ThreadRef{ArtifactID: id, BranchID: acf.MainBranch, MaterializedTurnsHash: hash},
		turns,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
}

func TestMergeConversationByThreadRef_MainUsesNewestSelfContainedAnchor(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000098"
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, CreatedAt: now, UpdatedAt: now,
	}))
	// The oldest event is syntactically valid JSON and hash-chain-valid, but its
	// superseded payload cannot decode as a ConversationPayload. A whole-history
	// replay would fail here; the newer full-state update is a self-contained
	// anchor and is all a main-branch continuation needs.
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: []byte(`{"format":5}`),
	}))
	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	base := []acf.ConversationEvent{convTurn("user", "q1"), convTurn("assistant", "a1")}
	full, err := EncodeCanonicalConversationPayload(base)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), ParentHash: art.HeadEventHash, Payload: full,
	}))

	continued := append(append([]acf.ConversationEvent(nil), base...), convTurn("user", "q2"))
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "codex"},
		ThreadRef{ArtifactID: id, BranchID: acf.MainBranch}, continued,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	cached, cachedOK, err := store.ValidatedCachedMaterializedConversationPayload(id)
	require.NoError(t, err)
	require.True(t, cachedOK)
	require.Equal(t, acf.ExtractTextTurns(continued), acf.ExtractTextTurns(cached.Events))
}

func TestMergeConversationByThreadRef_AppendsContinuationToForkBranch(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000030"
	seedConversation(t, store, id, []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello")})
	params := OpaqueParams{SourceAgent: "codex"}

	evs, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	create := evs[0]
	mainHeadBefore, err := store.HeadHashByBranch(acf.KindConversation, id, acf.MainBranch)
	require.NoError(t, err)
	fork := appendForkForTest(t, store, id, "experiment", create)

	cont := []acf.ConversationEvent{
		convTurn("user", "hi"),
		convTurn("assistant", "hello"),
		convTurn("user", "try another path"),
	}
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(),
		store,
		params,
		ThreadRef{ArtifactID: id, BranchID: "experiment"},
		cont,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, 2, branchTurnCount(t, store, id, acf.MainBranch))
	require.Equal(t, 3, branchTurnCount(t, store, id, "experiment"))

	mainHeadAfter, err := store.HeadHashByBranch(acf.KindConversation, id, acf.MainBranch)
	require.NoError(t, err)
	require.Equal(t, mainHeadBefore, mainHeadAfter, "fork continuation must not move main")

	evs, err = store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	head := evs[len(evs)-1]
	require.Equal(t, acf.EventTypeUpdate, head.Type)
	require.Equal(t, "experiment", head.Branch)
	require.Equal(t, fork.Hash, head.ParentHash)
}

func TestMergeConversationByThreadRef_AntiRevertIsBranchScoped(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000031"
	seedConversation(t, store, id, []acf.ConversationEvent{convTurn("user", "a"), convTurn("assistant", "b")})
	params := OpaqueParams{SourceAgent: "codex"}

	evs, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	create := evs[0]
	mainDelta, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{convTurn("user", "c"), convTurn("assistant", "d")},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate, Branch: acf.MainBranch,
		Timestamp: time.Now().UTC(), ParentHash: create.Hash, Payload: mainDelta,
	}))
	appendForkForTest(t, store, id, "experiment", create)

	// This is a strict prefix of main [a,b,c,d], but it is a real continuation
	// of branch [a,b]. The branch-scoped anti-revert guard must allow it.
	branchCont := []acf.ConversationEvent{
		convTurn("user", "a"),
		convTurn("assistant", "b"),
		convTurn("user", "c"),
	}
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(),
		store,
		params,
		ThreadRef{ArtifactID: id, BranchID: "experiment"},
		branchCont,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, 4, branchTurnCount(t, store, id, acf.MainBranch))
	require.Equal(t, 3, branchTurnCount(t, store, id, "experiment"))
}

// Anti-revert: a STALE shorter copy (a strict prefix of the current thread) must
// NOT overwrite a newer continuation. This is the data-loss guard — e.g. an agent
// that never received a later turn is re-scanned on a restart and re-imports its
// old, shorter copy.
func TestMergeConversationByThread_StaleCopyDoesNotRevert(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000002"
	full := []acf.ConversationEvent{convTurn("user", "60+60"), convTurn("assistant", "120"), convTurn("user", "70+70")}
	seedConversation(t, store, id, full)
	params := OpaqueParams{SourceAgent: "kilo"}

	stale := []acf.ConversationEvent{convTurn("user", "60+60"), convTurn("assistant", "120")}
	ids, handled, err := MergeConversationByThread(context.Background(), store, params, id, stale, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled, "a stale prefix copy is handled (suppressed), not native-imported")
	require.Empty(t, ids, "no event appended → no fan-out")
	require.Equal(t, 3, threadTurnCount(t, store, id), "the thread must NOT be reverted to 2 turns")

	cont := append(append([]acf.ConversationEvent(nil), full...), convTurn("assistant", "140"))
	ids, handled, err = MergeConversationByThread(context.Background(), store, params, id, cont, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, 4, threadTurnCount(t, store, id))
}

func TestMergeConversationByThreadRef_StaleGeneratedContinuationPreservesBothSuffixes(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000008"
	base := []acf.ConversationEvent{
		convTurn("user", "q1"),
		convTurn("assistant", "a1"),
	}
	current := append(append([]acf.ConversationEvent(nil), base...),
		convTurn("user", "q2-from-codex"),
		convTurn("assistant", "a2-from-codex"),
	)
	seedConversation(t, store, id, current)

	// Claude resumed the older generated [q1,a1] snapshot and wrote its own
	// question+answer before refreshing. The stamped base proves which rows are
	// old; neither the canonical Codex suffix nor Claude's new suffix may vanish.
	incoming := append(append([]acf.ConversationEvent(nil), base...),
		convTurn("user", "q2-from-claude"),
		convTurn("assistant", "a2-from-claude"),
	)
	baseTurns := acf.ExtractTextTurns(base)
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
		ThreadRef{
			ArtifactID:            id,
			BranchID:              acf.MainBranch,
			MaterializedTurnsHash: ConversationTurnsHash(baseTurns),
			MaterializedTurnCount: len(baseTurns),
		},
		incoming,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	head, err := acf.DecodeConversationPayload(events[1])
	require.NoError(t, err)
	require.Equal(t, acf.ConversationDeltaFormatV1, head.Format)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q2-from-claude"},
		{Role: "assistant", Text: "a2-from-claude"},
	}, acf.ExtractTextTurns(head.Events), "only the post-snapshot suffix should be appended")

	payload, ok, err := acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2-from-codex"},
		{Role: "assistant", Text: "a2-from-codex"},
		{Role: "user", Text: "q2-from-claude"},
		{Role: "assistant", Text: "a2-from-claude"},
	}, acf.ExtractTextTurns(payload.Events))
}

func TestMergeConversationByThreadRef_RejectsStaleSuffixWhenBaseStampDoesNotMatch(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000009"
	current := []acf.ConversationEvent{
		convTurn("user", "q1"), convTurn("assistant", "a1"),
		convTurn("user", "q2"), convTurn("assistant", "a2"),
	}
	seedConversation(t, store, id, current)
	incoming := []acf.ConversationEvent{
		convTurn("user", "tampered q1"), convTurn("assistant", "a1"),
		convTurn("user", "new suffix"),
	}

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "codex"},
		ThreadRef{
			ArtifactID:            id,
			BranchID:              acf.MainBranch,
			MaterializedTurnsHash: ConversationTurnsHash(acf.ExtractTextTurns(incoming[:2])),
			MaterializedTurnCount: 2,
		}, incoming, EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
	require.Equal(t, len(current), threadTurnCount(t, store, id))
}

func TestMergeConversationByThreadRef_RepairsAuthenticatedSyntheticTerminalTurn(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000005"
	real := []acf.ConversationEvent{
		convTurn("user", "how big is the solar system?"),
		convTurn("assistant", "It depends on the boundary."),
		convTurn("user", "how many planets?"),
		convTurn("assistant", "Eight."),
	}
	withPlaceholder := append(append([]acf.ConversationEvent(nil), real...),
		convTurn("assistant", "No response requested."))
	seedConversation(t, store, id, withPlaceholder)

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(),
		store,
		OpaqueParams{SourceAgent: "codex"},
		ThreadRef{
			ArtifactID:                 id,
			BranchID:                   acf.MainBranch,
			GeneratedSnapshot:          true,
			SanitizedSyntheticTurn:     true,
			AuthenticatedGeneratedPath: true,
		},
		real,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, len(real), threadTurnCount(t, store, id))

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	head := events[len(events)-1]
	payload, err := acf.DecodeConversationPayload(head)
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, payload.Format,
		"a corrective shrink must carry the complete clean state")
	require.Equal(t, acf.ExtractTextTurns(real), acf.ExtractTextTurns(payload.Events))
}

func TestMergeConversationByThreadRef_RepairsAuthenticatedPortableProjection(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000010"
	portable := []acf.ConversationEvent{
		convTurn("user", "what is capital of France?"),
		convTurn("assistant", "Paris."),
		convTurn("user", "how many people live in Paris?"),
		convTurn("assistant", "About 2.1 million."),
	}
	polluted := []acf.ConversationEvent{
		portable[0], portable[1],
		{Type: acf.EventTypeSystemNote, Content: []acf.ContentBlock{{Type: "text", Text: "<permissions instructions>private harness"}}},
		portable[2],
		{Type: acf.EventTypeToolCall, CallID: "call-1", ToolName: "exec"},
		{Type: acf.EventTypeToolResult, CallID: "call-1", Content: []acf.ContentBlock{{Type: "text", Text: "private tool output"}}},
		portable[3],
	}
	require.True(t, portableConversationEvents(portable))
	require.False(t, portableConversationEvents(polluted))
	require.Equal(t, acf.ExtractTextTurns(portable), acf.ExtractTextTurns(polluted))
	seedConversation(t, store, id, polluted)

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "codex"},
		ThreadRef{
			ArtifactID:                  id,
			BranchID:                    acf.MainBranch,
			GeneratedSnapshot:           true,
			SanitizedPortableProjection: true,
			AuthenticatedGeneratedPath:  true,
		},
		portable,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	head, err := acf.DecodeConversationPayload(events[1])
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, head.Format,
		"portable repair must be a complete replacement, not a delta after polluted rows")
	require.Equal(t, portable, head.Events)
	require.True(t, portableConversationEvents(head.Events))
}

func TestMergeConversationByThreadRef_RepairsAuthenticatedGeneratedEchoExpansion(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-0000000000e1"
	clean := []acf.ConversationEvent{
		convTurn("user", "q1"),
		convTurn("assistant", "a1"),
		convTurn("user", "q2"),
		convTurn("assistant", "a2"),
	}
	legacyGenerated := []acf.ConversationEvent{
		convTurn("user", "q1"),
		convTurn("user", "q1"),
		convTurn("assistant", "a1"),
		convTurn("assistant", "a1"),
		convTurn("user", "q2"),
		convTurn("assistant", "a2"),
	}
	polluted := []acf.ConversationEvent{
		convTurn("user", "q1"),
		convTurn("user", "q1"),
		convTurn("assistant", "a1"),
		convTurn("assistant", "a1"),
		convTurn("user", "q2"),
		convTurn("user", "q1"),
		convTurn("assistant", "a1"),
		convTurn("user", "q2"),
		convTurn("assistant", "a2"),
	}
	seedConversation(t, store, id, polluted)
	base := []acf.TextTurn{{Role: "user", Text: "q1"}}

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "codex"},
		ThreadRef{
			ArtifactID:                  id,
			BranchID:                    acf.MainBranch,
			MaterializedTurnCount:       len(base),
			MaterializedTurnsHash:       ConversationTurnsHash(base),
			SanitizedPortableProjection: true,
			SanitizedLegacyTurns:        acf.ExtractTextTurns(legacyGenerated),
			AuthenticatedGeneratedPath:  true,
		},
		clean,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	head, err := acf.DecodeConversationPayload(events[1])
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, head.Format)
	require.Equal(t, clean, head.Events)
}

func TestMergeConversationByThreadRef_RepairsProvenLegacyCommentaryTurns(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000012"
	portable := []acf.ConversationEvent{
		convTurn("user", "what is capital of France?"),
		convTurn("assistant", "Paris."),
		convTurn("user", "how many people live in Paris?"),
		convTurn("assistant", "About 2.1 million."),
	}
	polluted := []acf.ConversationEvent{
		portable[0], portable[1],
		{Type: acf.EventTypeSystemNote, Content: []acf.ContentBlock{{Type: "text", Text: "private harness"}}},
		portable[2],
		convTurn("assistant", "Searching the web"),
		{Type: acf.EventTypeToolCall, CallID: "search-1", ToolName: "web_search"},
		{Type: acf.EventTypeToolResult, CallID: "search-1", Content: []acf.ContentBlock{{Type: "text", Text: "private result"}}},
		portable[3],
	}
	seedConversation(t, store, id, polluted)

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "codex"},
		ThreadRef{
			ArtifactID:                  id,
			BranchID:                    acf.MainBranch,
			GeneratedSnapshot:           true,
			SanitizedPortableProjection: true,
			SanitizedLegacyTurns:        acf.ExtractTextTurns(polluted),
			AuthenticatedGeneratedPath:  true,
		},
		portable,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	head, err := acf.DecodeConversationPayload(events[1])
	require.NoError(t, err)
	require.Equal(t, portable, head.Events)
}

func TestMergeConversationByThreadRef_PortableRepairDoesNotEraseUnprovenAssistantTurn(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000013"
	portable := []acf.ConversationEvent{
		convTurn("user", "q"), convTurn("assistant", "a"),
	}
	polluted := append(append([]acf.ConversationEvent(nil), portable...),
		convTurn("assistant", "a genuine remote continuation"))
	seedConversation(t, store, id, polluted)

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "codex"},
		ThreadRef{
			ArtifactID:                  id,
			BranchID:                    acf.MainBranch,
			GeneratedSnapshot:           true,
			SanitizedPortableProjection: true,
			SanitizedLegacyTurns:        acf.ExtractTextTurns(portable),
			AuthenticatedGeneratedPath:  true,
		},
		portable,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
	require.Equal(t, 3, threadTurnCount(t, store, id))
}

func TestMergeConversationByThreadRef_PortableRepairRequiresAdapterAuthentication(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000011"
	portable := []acf.ConversationEvent{convTurn("user", "q"), convTurn("assistant", "a")}
	polluted := append(append([]acf.ConversationEvent(nil), portable...), acf.ConversationEvent{
		Type: acf.EventTypeSystemNote, Content: []acf.ContentBlock{{Type: "text", Text: "native system note"}},
	})
	seedConversation(t, store, id, polluted)

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "unknown"},
		ThreadRef{ArtifactID: id, BranchID: acf.MainBranch}, portable,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 1, "an untrusted equal-visible projection must remain a loop-break")
}

func TestRepairCanonicalConversationProjection_CASPreservesInterveningRemoteSuffix(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-0000000000ca"
	legacy := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "system", Content: []acf.ContentBlock{{Type: "text", Text: "private harness"}}},
		convTurn("user", "question"),
		convTurn("assistant", "working"),
		convTurn("assistant", "final answer"),
	}
	clean := []acf.ConversationEvent{convTurn("user", "question"), convTurn("assistant", "final answer")}
	seedConversation(t, store, id, legacy)

	remote := append(append([]acf.ConversationEvent(nil), legacy...), convTurn("assistant", "remote suffix"))
	store.IngestGate = func() error {
		// AppendEvent consults the gate before taking its artifact lock. Inject a
		// remote update after repair proof but before the repair CAS.
		store.IngestGate = nil
		head, ok, err := store.LastEvent(acf.KindConversation, id)
		require.NoError(t, err)
		require.True(t, ok)
		payload, err := EncodeCanonicalConversationPayload(remote)
		require.NoError(t, err)
		return store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: time.Now().UTC(), ParentHash: head.Hash, Payload: payload,
			Provenance: acf.Provenance{DeviceID: "remote", SourceAgent: "claude-code"},
		})
	}

	ids, repaired, err := RepairCanonicalConversationProjection(
		context.Background(), store, OpaqueParams{DeviceID: "local", SourceAgent: "codex"},
		conversationTestSourcePath(store, id), legacy, clean,
	)
	require.True(t, repaired)
	require.ErrorIs(t, err, acf.ErrHeadMismatch)
	require.Empty(t, ids)
	current, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, remote, current.Events,
		"the failed repair must not semantically replace the intervening continuation")
}

func TestRepairCanonicalConversationProjection_PreservesAttachmentsInEventAndCache(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-0000000000cb"
	legacy := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "system", Content: []acf.ContentBlock{{Type: "text", Text: "private harness"}}},
		convTurn("user", "question"),
		convTurn("assistant", "working"),
		convTurn("assistant", "final answer"),
	}
	clean := []acf.ConversationEvent{legacy[1], legacy[3]}
	attachments := []acf.Attachment{{
		Kind: "image", MimeType: "image/png", ContentHash: "attachment-proof", Bytes: 42, Filename: "proof.png",
	}}
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "c.jsonl", SourcePath: conversationTestSourcePath(store, id), CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: legacy, Attachments: attachments,
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: payload,
	}))

	ids, repaired, err := RepairCanonicalConversationProjection(
		context.Background(), store, OpaqueParams{DeviceID: "local", SourceAgent: "codex"},
		conversationTestSourcePath(store, id), legacy, clean,
	)
	require.NoError(t, err)
	require.True(t, repaired)
	require.Equal(t, []string{id}, ids)

	head, ok, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotContains(t, head.EventTags, acf.LegacyAdjacentAssistantEchoRepairEventTag,
		"generic projection sanitation must not mint adjacent-echo deletion authority")
	committed, err := acf.DecodeConversationPayload(head)
	require.NoError(t, err)
	require.Equal(t, clean, committed.Events)
	require.Equal(t, attachments, committed.Attachments)
	cached, cachedOK, err := store.ValidatedCachedMaterializedConversationPayload(id)
	require.NoError(t, err)
	require.True(t, cachedOK)
	require.Equal(t, attachments, cached.Attachments)
}

func TestMergeConversationByThreadRef_RepairsLegacyRetimestampedPollution(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000007"
	t0 := time.Date(2026, 7, 16, 22, 38, 0, 0, time.UTC)
	turn := func(role, text string, ts time.Time) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	synthetic := t0.Add(10 * time.Second)
	polluted := []acf.ConversationEvent{
		turn("user", "q1", t0),
		turn("assistant", "a2", synthetic),
		turn("assistant", "a1", synthetic),
		turn("user", "q1", synthetic),
		turn("user", "q2", synthetic),
		turn("assistant", "a1", t0.Add(time.Second)),
		turn("user", "q2", t0.Add(2*time.Second)),
	}
	seedConversation(t, store, id, polluted)
	clean := []acf.ConversationEvent{
		turn("user", "q1", synthetic),
		turn("assistant", "a1", synthetic),
		turn("user", "q2", synthetic),
		turn("assistant", "a2", synthetic),
	}

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "codex"},
		ThreadRef{ArtifactID: id, BranchID: acf.MainBranch, GeneratedSnapshot: true},
		clean, EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, len(clean), threadTurnCount(t, store, id))

	payload, _, ok, err := store.ProjectConversationPayloadForBranch(id, acf.MainBranch, acf.BranchProjectionOpts{})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2"},
		{Role: "assistant", Text: "a2"},
	}, acf.ExtractTextTurns(payload.Events))
}

func TestMergeConversationByThreadRef_RepairsProvenGeneratedLegacyEdgeEchoes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string
		codex bool
	}{
		{name: "codex", agent: "codex", codex: true},
		{name: "claude", agent: "claude-code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &acf.Store{Root: t.TempDir()}
			require.NoError(t, store.Init())
			id := acf.NewID()
			synthetic := time.Date(2026, 7, 18, 20, 10, 54, 114000000, time.UTC)
			turn := func(role, text string, ts time.Time) acf.ConversationEvent {
				return acf.ConversationEvent{
					Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
					Content: []acf.ContentBlock{{Type: "text", Text: text}},
				}
			}
			clean := []acf.ConversationEvent{
				turn("user", "what is capital of Poland", synthetic),
				turn("assistant", "Warsaw.", synthetic),
				turn("user", "how many people live in warsaw?", synthetic),
				turn("assistant", "About 1.87 million.", synthetic.Add(time.Minute)),
			}
			polluted := append([]acf.ConversationEvent{
				turn("assistant", "Warsaw.", synthetic),
			}, clean...)
			polluted = append(polluted, turn("assistant", "About 1.87 million.", synthetic.Add(2*time.Minute)))
			seedConversation(t, store, id, polluted)

			turns := acf.ExtractTextTurns(clean)
			ref := ThreadRef{
				ArtifactID: id, BranchID: acf.MainBranch, GeneratedSnapshot: true,
				MaterializedTurnCount: len(turns), MaterializedTurnsHash: ConversationTurnsHash(turns),
			}
			if tc.codex {
				ref.SanitizedPortableProjection = true
				ref.SanitizedLegacyTurns = turns
				ref.AuthenticatedGeneratedPath = true
			}
			ids, handled, err := MergeConversationByThreadRef(
				context.Background(), store, OpaqueParams{SourceAgent: tc.agent}, ref,
				clean, EncodeCanonicalConversationPayload,
			)
			require.NoError(t, err)
			require.True(t, handled)
			require.Equal(t, []string{id}, ids)

			events, err := store.ReadEvents(acf.KindConversation, id)
			require.NoError(t, err)
			require.Len(t, events, 2)
			head, err := acf.DecodeConversationPayload(events[1])
			require.NoError(t, err)
			require.Equal(t, acf.ConversationFormatV1, head.Format,
				"a corrective shrink must carry the complete clean state")
			require.Equal(t, turns, acf.ExtractTextTurns(head.Events))
		})
	}
}

func TestMergeConversationByThreadRef_RepairsAuthenticatedAdjacentAssistantEcho(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	turn := func(role, text string, ts time.Time) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	clean := []acf.ConversationEvent{
		turn("user", "what is capital of France?", t0),
		turn("assistant", "Paris.", t0.Add(2*time.Second)),
		turn("user", "how many people live in Paris?", t0.Add(3*time.Second)),
		turn("assistant", "About 2.1 million.", t0.Add(4*time.Second)),
	}
	dirtyFull := []acf.ConversationEvent{
		clean[0],
		turn("assistant", "Paris.", t0.Add(time.Second)),
		clean[1], clean[2], clean[3],
	}
	dirty := append(append([]acf.ConversationEvent(nil), dirtyFull...),
		turn("assistant", "About 2.1 million.", t0.Add(5*time.Second)),
		turn("user", "how many people live in Paris?", t0.Add(6*time.Second)),
	)
	attachment := acf.Attachment{
		Kind: "image", MimeType: "image/png", ContentHash: "proof", Bytes: 42, Filename: "proof.png",
	}
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "c.jsonl", SourcePath: conversationTestSourcePath(store, id), CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: dirty, Attachments: []acf.Attachment{attachment},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: payload,
	}))

	turns := acf.ExtractTextTurns(clean)
	base := turns[:1]
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store,
		OpaqueParams{DeviceID: "local", SourceAgent: "codex", AdapterVersion: "0.9.3"},
		ThreadRef{
			ArtifactID: id, BranchID: acf.MainBranch,
			MaterializedTurnCount: len(base), MaterializedTurnsHash: ConversationTurnsHash(base),
			SanitizedPortableProjection: true, SanitizedLegacyTurns: turns,
			AuthenticatedGeneratedPath: true,
		},
		clean, EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Contains(t, events[1].EventTags, acf.LegacyAdjacentAssistantEchoRepairEventTag,
		"the authenticated correction must carry peer-verifiable repair provenance")
	head, err := acf.DecodeConversationPayload(events[1])
	require.NoError(t, err)
	require.Equal(t, clean, head.Events)
	require.Equal(t, []acf.Attachment{attachment}, head.Attachments)
}

func TestMergeConversationByThreadRef_ClaudeSnapshotCannotAuthorizeAdjacentAssistantCleanup(t *testing.T) {
	clean := []acf.ConversationEvent{
		convTurn("user", "q1"), convTurn("assistant", "a1"),
		convTurn("user", "q2"), convTurn("assistant", "a2"),
	}
	dirtyFull := []acf.ConversationEvent{
		clean[0], clean[1], convTurn("assistant", "a1"), clean[2], clean[3],
	}
	dirtyConflict := append(append([]acf.ConversationEvent(nil), dirtyFull...),
		convTurn("assistant", "a2"), convTurn("user", "q2"))

	for _, tc := range []struct {
		name    string
		current []acf.ConversationEvent
	}{
		{name: "dirty full five", current: dirtyFull},
		{name: "materialized full plus delta seven", current: dirtyConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &acf.Store{Root: t.TempDir()}
			require.NoError(t, store.Init())
			id := acf.NewID()
			seedConversation(t, store, id, tc.current)
			turns := acf.ExtractTextTurns(clean)

			ids, handled, err := MergeConversationByThreadRef(
				context.Background(), store, OpaqueParams{DeviceID: "local", SourceAgent: "claude-code"},
				ThreadRef{
					ArtifactID: id, BranchID: acf.MainBranch,
					GeneratedSnapshot: true, MaterializedTurnCount: len(turns),
					MaterializedTurnsHash: ConversationTurnsHash(turns),
				},
				clean, EncodeCanonicalConversationPayload,
			)
			require.NoError(t, err)
			require.True(t, handled)
			require.Empty(t, ids)

			events, readErr := store.ReadEvents(acf.KindConversation, id)
			require.NoError(t, readErr)
			require.Len(t, events, 1, "Claude metadata must not mint a content-removing correction")
			materialized, ok, materializeErr := store.MaterializedConversationPayloadFromStore(id)
			require.NoError(t, materializeErr)
			require.True(t, ok)
			require.Equal(t, tc.current, materialized.Events)
		})
	}
}

func TestMergeConversationByThreadRef_AdjacentRepairCASPreservesConcurrentContinuation(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	clean := []acf.ConversationEvent{
		convTurn("user", "q1"), convTurn("assistant", "a1"),
		convTurn("user", "q2"), convTurn("assistant", "a2"),
	}
	dirty := []acf.ConversationEvent{
		clean[0], clean[1], convTurn("assistant", "a1"), clean[2], clean[3],
		convTurn("assistant", "a2"), convTurn("user", "q2"),
	}
	seedConversation(t, store, id, dirty)
	continuation := []acf.ConversationEvent{
		convTurn("user", "genuine concurrent question"),
		convTurn("assistant", "genuine concurrent answer"),
	}
	injected := false
	encoder := func(events []acf.ConversationEvent) (json.RawMessage, error) {
		if !injected {
			injected = true
			art, err := store.ReadArtifact(acf.KindConversation, id)
			require.NoError(t, err)
			payload, err := acf.EncodePayload(acf.ConversationPayload{
				Format: acf.ConversationDeltaFormatV1, Events: continuation,
			})
			require.NoError(t, err)
			require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
				EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
				Branch: acf.MainBranch, Timestamp: time.Now().UTC(), ParentHash: art.HeadEventHash,
				Provenance: acf.Provenance{DeviceID: "peer", SourceAgent: "hermes"}, Payload: payload,
			}))
		}
		return EncodeCanonicalConversationPayload(events)
	}
	turns := acf.ExtractTextTurns(clean)
	base := turns[:1]
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store,
		OpaqueParams{DeviceID: "local", SourceAgent: "codex", AdapterVersion: "0.9.3"},
		ThreadRef{
			ArtifactID: id, BranchID: acf.MainBranch,
			MaterializedTurnCount: len(base), MaterializedTurnsHash: ConversationTurnsHash(base),
			SanitizedPortableProjection: true, SanitizedLegacyTurns: turns,
			AuthenticatedGeneratedPath: true,
		},
		clean, encoder,
	)
	require.True(t, handled)
	require.ErrorIs(t, err, acf.ErrHeadMismatch)
	require.Empty(t, ids)

	events, readErr := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, readErr)
	require.Len(t, events, 2, "the failed correction must not append after the concurrent continuation")
	require.NotContains(t, events[1].EventTags, acf.LegacyAdjacentAssistantEchoRepairEventTag)
	materialized, ok, materializeErr := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, materializeErr)
	require.True(t, ok)
	want := append(append([]acf.ConversationEvent(nil), dirty...), continuation...)
	require.Equal(t, want, materialized.Events,
		"the observed-head CAS must preserve the complete concurrent suffix")
}

func TestMergeConversationByThreadRef_PortableRepairCASPreservesConcurrentContinuation(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	portable := []acf.ConversationEvent{
		convTurn("user", "question"), convTurn("assistant", "final answer"),
	}
	polluted := []acf.ConversationEvent{
		portable[0],
		{Type: acf.EventTypeSystemNote, Content: []acf.ContentBlock{{Type: "text", Text: "private harness"}}},
		convTurn("assistant", "working"),
		{Type: acf.EventTypeToolCall, CallID: "call-1", ToolName: "exec"},
		portable[1],
	}
	seedConversation(t, store, id, polluted)
	continuation := []acf.ConversationEvent{
		convTurn("user", "genuine concurrent question"),
		convTurn("assistant", "genuine concurrent answer"),
	}
	injected := false
	encoder := func(events []acf.ConversationEvent) (json.RawMessage, error) {
		if !injected {
			injected = true
			art, err := store.ReadArtifact(acf.KindConversation, id)
			require.NoError(t, err)
			payload, err := acf.EncodePayload(acf.ConversationPayload{
				Format: acf.ConversationDeltaFormatV1, Events: continuation,
			})
			require.NoError(t, err)
			require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
				EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
				Branch: acf.MainBranch, Timestamp: time.Now().UTC(), ParentHash: art.HeadEventHash,
				Provenance: acf.Provenance{DeviceID: "peer", SourceAgent: "hermes"}, Payload: payload,
			}))
		}
		return EncodeCanonicalConversationPayload(events)
	}

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store,
		OpaqueParams{DeviceID: "local", SourceAgent: "codex", AdapterVersion: "0.9.3"},
		ThreadRef{
			ArtifactID: id, BranchID: acf.MainBranch,
			GeneratedSnapshot: true, SanitizedPortableProjection: true,
			SanitizedLegacyTurns:       acf.ExtractTextTurns(polluted),
			AuthenticatedGeneratedPath: true,
		},
		portable, encoder,
	)
	require.True(t, handled)
	require.ErrorIs(t, err, acf.ErrHeadMismatch)
	require.Empty(t, ids)

	events, readErr := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, readErr)
	require.Len(t, events, 2, "the failed cleanup must not append after the concurrent continuation")
	materialized, ok, materializeErr := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, materializeErr)
	require.True(t, ok)
	want := append(append([]acf.ConversationEvent(nil), polluted...), continuation...)
	require.Equal(t, want, materialized.Events,
		"the observed-head CAS must preserve polluted history until cleanup can be retried safely")
}

func TestMergeConversationByThreadRef_PreservesUnprovenAdjacentAssistantRepeat(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	clean := []acf.ConversationEvent{
		convTurn("user", "q1"), convTurn("assistant", "a1"),
		convTurn("user", "q2"), convTurn("assistant", "a2"),
	}
	dirty := []acf.ConversationEvent{clean[0], clean[1], convTurn("assistant", "a1"), clean[2], clean[3]}
	seedConversation(t, store, id, dirty)
	turns := acf.ExtractTextTurns(clean)

	before, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	for _, ref := range []ThreadRef{
		{ArtifactID: id, BranchID: acf.MainBranch},
		{
			ArtifactID: id, BranchID: acf.MainBranch,
			MaterializedTurnCount: 2, MaterializedTurnsHash: ConversationTurnsHash(turns[:2]),
			SanitizedPortableProjection: true,
			// Missing the legacy projection reconstructed from authenticated
			// source bytes: a text-only stamp cannot authorize deletion.
		},
		{
			ArtifactID: id, BranchID: acf.MainBranch,
			MaterializedTurnCount: 1, MaterializedTurnsHash: ConversationTurnsHash(turns[:1]),
			SanitizedPortableProjection: true, SanitizedLegacyTurns: turns,
			// Every marker value is internally consistent, but the caller did
			// not correlate this file with Codex's deterministic generated path.
		},
	} {
		ids, handled, err := MergeConversationByThreadRef(
			context.Background(), store, OpaqueParams{SourceAgent: "codex"}, ref,
			clean, EncodeCanonicalConversationPayload,
		)
		require.NoError(t, err)
		require.True(t, handled)
		require.Empty(t, ids)
	}
	after, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Equal(t, before, after, "an unproven structural match must not append or rewrite any event")
	require.Equal(t, acf.ExtractTextTurns(dirty), func() []acf.TextTurn {
		payload, ok, err := store.MaterializedConversationPayloadFromStore(id)
		require.NoError(t, err)
		require.True(t, ok)
		return acf.ExtractTextTurns(payload.Events)
	}())
}

func TestMergeConversationByThreadRef_LegacyEdgeProofPreservesUniqueEventPayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*acf.ConversationEvent)
		find   func(acf.ConversationEvent) bool
	}{
		{
			name: "image block",
			mutate: func(event *acf.ConversationEvent) {
				event.Content = append(event.Content, acf.ContentBlock{Type: "image", Data: "unique-image"})
			},
			find: func(event acf.ConversationEvent) bool {
				for _, block := range event.Content {
					if block.Type == "image" && block.Data == "unique-image" {
						return true
					}
				}
				return false
			},
		},
		{
			name: "native extras",
			mutate: func(event *acf.ConversationEvent) {
				event.NativeExtras = []byte(`{"unique":true}`)
			},
			find: func(event acf.ConversationEvent) bool {
				return string(event.NativeExtras) == `{"unique":true}`
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &acf.Store{Root: t.TempDir()}
			require.NoError(t, store.Init())
			id := acf.NewID()
			t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
			turn := func(role, text string, ts time.Time) acf.ConversationEvent {
				return acf.ConversationEvent{
					Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
					Content: []acf.ContentBlock{{Type: "text", Text: text}},
				}
			}
			clean := []acf.ConversationEvent{
				turn("user", "q1", t0), turn("assistant", "a1", t0),
				turn("user", "q2", t0), turn("assistant", "a2", t0.Add(time.Second)),
			}
			leading := clean[1]
			leading.Content = append([]acf.ContentBlock(nil), leading.Content...)
			tc.mutate(&leading)
			polluted := append([]acf.ConversationEvent{leading}, clean...)
			polluted = append(polluted, turn("assistant", "a2", t0.Add(2*time.Second)))
			seedConversation(t, store, id, polluted)

			turns := acf.ExtractTextTurns(clean)
			ids, handled, err := MergeConversationByThreadRef(
				context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
				ThreadRef{
					ArtifactID: id, BranchID: acf.MainBranch, GeneratedSnapshot: true,
					MaterializedTurnCount: len(turns), MaterializedTurnsHash: ConversationTurnsHash(turns),
				},
				clean, EncodeCanonicalConversationPayload,
			)
			require.NoError(t, err)
			require.True(t, handled)
			require.Empty(t, ids, "a text-only hash must not authorize deletion of unique event payload")

			payload, ok, err := store.MaterializedConversationPayloadFromStore(id)
			require.NoError(t, err)
			require.True(t, ok)
			require.Condition(t, func() bool {
				for _, event := range payload.Events {
					if tc.find(event) {
						return true
					}
				}
				return false
			}, "the unique non-text/native payload must remain canonical")
		})
	}
}

func TestMergeConversationByThreadRef_LegacyEdgeRepairPreservesAttachments(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	turn := func(role, text string, ts time.Time) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	clean := []acf.ConversationEvent{
		turn("user", "q1", t0), turn("assistant", "a1", t0),
		turn("user", "q2", t0), turn("assistant", "a2", t0.Add(time.Second)),
	}
	polluted := append([]acf.ConversationEvent{turn("assistant", "a1", t0)}, clean...)
	polluted = append(polluted, turn("assistant", "a2", t0.Add(2*time.Second)))
	attachment := acf.Attachment{
		Kind: "image", MimeType: "image/png", ContentHash: "attachment-hash", Bytes: 42, Filename: "proof.png",
	}
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "c.jsonl", CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: polluted, Attachments: []acf.Attachment{attachment},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: payload,
	}))

	turns := acf.ExtractTextTurns(clean)
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
		ThreadRef{
			ArtifactID: id, BranchID: acf.MainBranch, GeneratedSnapshot: true,
			MaterializedTurnCount: len(turns), MaterializedTurnsHash: ConversationTurnsHash(turns),
		},
		clean, EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	materialized, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, clean, materialized.Events)
	require.Equal(t, []acf.Attachment{attachment}, materialized.Attachments)
}

func TestMergeConversationByThreadRef_RejectsUnprovenLegacyEdgeEchoShrink(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	turn := func(role, text string, ts time.Time) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	clean := []acf.ConversationEvent{
		turn("user", "q1", t0), turn("assistant", "a1", t0),
		turn("user", "q2", t0), turn("assistant", "a2", t0.Add(time.Second)),
	}
	polluted := append([]acf.ConversationEvent{turn("assistant", "a1", t0)}, clean...)
	polluted = append(polluted, turn("assistant", "a2", t0.Add(2*time.Second)))
	seedConversation(t, store, id, polluted)
	turns := acf.ExtractTextTurns(clean)

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
		ThreadRef{
			ArtifactID: id, BranchID: acf.MainBranch,
			MaterializedTurnCount: len(turns), MaterializedTurnsHash: ConversationTurnsHash(turns),
			// GeneratedSnapshot deliberately false: a native/stale copy cannot
			// authorize content removal even when its count/hash fields are forged.
		},
		clean, EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
	require.Equal(t, len(polluted), threadTurnCount(t, store, id))
}

func TestMergeConversationByThreadRef_SanitizerCannotRemoveMultipleOrDivergentTurns(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000006"
	full := []acf.ConversationEvent{
		convTurn("user", "q1"), convTurn("assistant", "a1"),
		convTurn("user", "q2"), convTurn("assistant", "a2"),
	}
	seedConversation(t, store, id, full)

	for _, incoming := range [][]acf.ConversationEvent{
		full[:2],
		{convTurn("user", "q1"), convTurn("assistant", "changed"), convTurn("user", "q2")},
	} {
		ids, handled, err := MergeConversationByThreadRef(
			context.Background(), store, OpaqueParams{SourceAgent: "codex"},
			ThreadRef{
				ArtifactID: id, BranchID: acf.MainBranch,
				SanitizedSyntheticTurn: true, AuthenticatedGeneratedPath: true,
			},
			incoming, EncodeCanonicalConversationPayload,
		)
		require.NoError(t, err)
		require.True(t, handled)
		require.Empty(t, ids)
		require.Equal(t, len(full), threadTurnCount(t, store, id))
	}
}

func TestMergeConversationByThread_DivergentCopyDoesNotReplace(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000004"
	full := []acf.ConversationEvent{convTurn("user", "q1"), convTurn("assistant", "a1"), convTurn("user", "q2")}
	seedConversation(t, store, id, full)
	params := OpaqueParams{SourceAgent: "claude-code"}

	divergent := []acf.ConversationEvent{convTurn("user", "q1"), convTurn("assistant", "older a1"), convTurn("user", "q2")}
	ids, handled, err := MergeConversationByThread(context.Background(), store, params, id, divergent, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids)
	require.Equal(t, 3, threadTurnCount(t, store, id), "divergent same-length copy must not replace the thread")
}

func TestWouldRevertThread(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000003"
	seedConversation(t, store, id, []acf.ConversationEvent{convTurn("user", "a"), convTurn("assistant", "b"), convTurn("user", "c")})
	turns := func(e ...acf.ConversationEvent) []acf.TextTurn { return acf.ExtractTextTurns(e) }
	require.True(t, WouldRevertThread(store, id, turns(convTurn("user", "a"), convTurn("assistant", "b"))), "strict prefix → revert")
	require.False(t, WouldRevertThread(store, id, turns(convTurn("user", "a"), convTurn("assistant", "b"), convTurn("user", "c"))), "equal → not a revert")
	require.False(t, WouldRevertThread(store, id, turns(convTurn("user", "a"), convTurn("assistant", "b"), convTurn("user", "c"), convTurn("assistant", "d"))), "longer → not a revert")
	require.True(t, WouldRevertThread(store, id, turns(convTurn("user", "a"), convTurn("assistant", "changed"), convTurn("user", "c"))), "same length divergent → revert")
	require.True(t, WouldRevertThread(store, id, turns(convTurn("user", "a"), convTurn("assistant", "changed"), convTurn("user", "c"), convTurn("assistant", "d"))), "longer divergent → revert")
	require.False(t, WouldRevertThread(store, "no-such", turns(convTurn("user", "a"))), "unknown artifact → false (normal path runs)")
}

// TestMergeConversationByThread_SnapshotHead reproduces the snapshot-head
// regression: conversation snapshots (every 100 events or 24h) append an
// EventTypeSnapshot whose Payload is nil. When such a snapshot becomes the
// head event, decoding the literal last event errors, so an unchanged
// re-import must still resolve to the most recent CONTENT-bearing event and
// report handled=true (the loop break) — NOT fall back to a path-keyed native
// import that mints a duplicate artifact.
func TestMergeConversationByThread_SnapshotHead(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000010"
	turns := []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello")}
	seedConversation(t, store, id, turns)
	params := OpaqueParams{SourceAgent: "codex"}

	// Append a snapshot event so it becomes the head (mirrors
	// retention.CreateSnapshot: Type=snapshot, Payload=nil).
	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     time.Now().UTC(),
		Payload:       nil,
		ParentHash:    art.HeadEventHash,
		SnapshotState: "sha256:deadbeef",
	}))

	// Same turns, but the head is now a snapshot → must still be a handled
	// no-op loop break (decode the latest content-bearing event, not the
	// snapshot).
	ids, handled, err := MergeConversationByThread(context.Background(), store, params, id, turns, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled, "snapshot-head re-import must be handled (loop break), not fall back to native import")
	require.Empty(t, ids)

	// A genuine continuation past a snapshot head still appends and fans out.
	cont := append(append([]acf.ConversationEvent{}, turns...), convTurn("user", "and 2+2?"))
	ids, handled, err = MergeConversationByThread(context.Background(), store, params, id, cont, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, 3, threadTurnCount(t, store, id))
}

// TestMergeConversationByThread_PayloadBearingSnapshotHead covers the
// post-prune steady state with FR-02.32 snapshots: after an on-snapshot prune
// the active log can be a payload-BEARING snapshot ALONE (the create event was
// compacted away). The snapshot carries the materialized turns, so an unchanged
// re-import must resolve those turns from the snapshot and report the loop break
// (handled=true, no ids) — not fall back to a duplicate native import.
func TestMergeConversationByThread_PayloadBearingSnapshotHead(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000011"
	turns := []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello")}
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "c.jsonl", SourcePath: conversationTestSourcePath(store, id), CreatedAt: now, UpdatedAt: now,
	}))
	// The ONLY active event is a payload-bearing snapshot carrying the turns —
	// exactly what the active log looks like after CreateSnapshot + PruneArtifact
	// compacts the pre-snapshot create event away.
	snapPayload, err := EncodeCanonicalConversationPayload(turns)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeSnapshot, Timestamp: now,
		Payload: snapPayload, SnapshotState: "sha256:abc",
	}))
	params := OpaqueParams{SourceAgent: "codex"}

	// Unchanged re-import → handled no-op loop break, resolved from the snapshot.
	ids, handled, err := MergeConversationByThread(context.Background(), store, params, id, turns, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled, "payload-bearing snapshot head must resolve turns (loop break), not fall back to native import")
	require.Empty(t, ids)

	// A genuine continuation past a snapshot-only head still appends and fans out.
	cont := append(append([]acf.ConversationEvent{}, turns...), convTurn("user", "more?"))
	ids, handled, err = MergeConversationByThread(context.Background(), store, params, id, cont, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)
}

// TestMergeConversationByThread_StampsCausedBy proves the continuation-append
// path stamps Provenance.CausedBy from the context, mirroring ImportOpaqueContent
// and ImportSkillReconciled. The orchestrator wraps the import ctx with
// WithCausedBy(ctx, sourceHash) on fan-out so the CausedBy-based recursion guard
// can recognize echoes; without this stamp a continuation carries no causal hash.
func TestMergeConversationByThread_StampsCausedBy(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-000000000020"
	seedConversation(t, store, id, []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello")})
	params := OpaqueParams{SourceAgent: "codex"}

	const sourceHash = "sha256:cause0001"
	ctx := WithCausedBy(context.Background(), sourceHash)

	// A genuine continuation → appends an update event whose Provenance must
	// carry the ctx's CausedBy hash.
	cont := []acf.ConversationEvent{convTurn("user", "hi"), convTurn("assistant", "hello"), convTurn("user", "and 2+2?")}
	ids, handled, err := MergeConversationByThread(ctx, store, params, id, cont, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	evs, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	head := evs[len(evs)-1]
	require.Equal(t, acf.EventTypeUpdate, head.Type)
	require.Equal(t, sourceHash, head.Provenance.CausedBy,
		"continuation-append must stamp Provenance.CausedBy from ctx (mirrors ImportOpaqueContent/ImportSkillReconciled)")
}

// convTurnAt is the timestamped sibling of convTurn. The stale-continuation
// echo tests need real payload timestamps because that is what makes an echoed
// row identifiable as the very same logical event rather than a look-alike.
func convTurnAt(role, text string, ts time.Time) acf.ConversationEvent {
	ev := convTurn(role, text)
	ev.Timestamp = ts.UTC()
	return ev
}

// TestMergeConversationByThreadRef_ReimportedStaleFileDoesNotDuplicate models
// a generated Claude Code session with [U1 A1] stamped + [U2 A2] native after
// its continuation was already published and a peer's [U3 A3] landed in
// canonical. Re-importing the unchanged four-turn file must not append [U2 A2]
// a second time.
func TestMergeConversationByThreadRef_ReimportedStaleFileDoesNotDuplicate(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-00000000002a"

	base := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	u1 := convTurnAt("user", "Question one?", base)
	a1 := convTurnAt("assistant", "Answer one.", base.Add(2*time.Second))
	u2 := convTurnAt("user", "Question two?", base.Add(20*time.Second+200*time.Millisecond))
	a2 := convTurnAt("assistant", "Answer two.", base.Add(21*time.Second+300*time.Millisecond))
	u3 := convTurnAt("user", "Question three?", base.Add(30*time.Second))
	a3 := convTurnAt("assistant", "Answer three.", base.Add(31*time.Second))

	// Canonical already holds this device's own continuation plus the peer's.
	seedConversation(t, store, id, []acf.ConversationEvent{u1, a1, u2, a2, u3, a3})

	// The unchanged native file: two Aplexica-stamped rows, two native rows.
	incoming := []acf.ConversationEvent{u1, a1, u2, a2}
	baseTurns := acf.ExtractTextTurns(incoming[:2])

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
		ThreadRef{
			ArtifactID:            id,
			BranchID:              acf.MainBranch,
			MaterializedTurnsHash: ConversationTurnsHash(baseTurns),
			MaterializedTurnCount: len(baseTurns),
		},
		incoming,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled, "the thread is ours; it must never fall through to a path-keyed import")
	require.Empty(t, ids, "an entirely echoed native suffix is not a continuation and must append nothing")

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 1, "no event may be appended")
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "Question one?"},
		{Role: "assistant", Text: "Answer one."},
		{Role: "user", Text: "Question two?"},
		{Role: "assistant", Text: "Answer two."},
		{Role: "user", Text: "Question three?"},
		{Role: "assistant", Text: "Answer three."},
	}, threadTurns(t, store, id))
}

// TestMergeConversationByThreadRef_ReimportIsIdempotentAfterCanonicalReorder
// replays field event [10], where the canonical suffix after the stamped base
// no longer starts with the echo because a duplicate block had already been
// committed ahead of it. Positional matching alone cannot see that echo, so the
// full-body identity trim has to.
func TestMergeConversationByThreadRef_ReimportIsIdempotentAfterCanonicalReorder(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-00000000002b"

	base := time.Date(2026, 7, 27, 2, 40, 0, 0, time.UTC)
	u1 := convTurnAt("user", "q1", base)
	a1 := convTurnAt("assistant", "a1", base.Add(time.Second))
	u2 := convTurnAt("user", "q2", base.Add(60*time.Second))
	a2 := convTurnAt("assistant", "a2", base.Add(68*time.Second))
	u3 := convTurnAt("user", "q3", base.Add(284*time.Second))
	a3 := convTurnAt("assistant", "a3", base.Add(287*time.Second))
	u4 := convTurnAt("user", "q4", base.Add(736*time.Second))
	a4 := convTurnAt("assistant", "a4", base.Add(742*time.Second))

	// Canonical carries an earlier [u2 a2] duplicate block, so the turns right
	// after this file's 4-turn stamped base are [u2 a2], not [u3 a3].
	seedConversation(t, store, id, []acf.ConversationEvent{u1, a1, u2, a2, u3, a3, u2, a2, u4, a4})

	incoming := []acf.ConversationEvent{u1, a1, u2, a2, u3, a3}
	baseTurns := acf.ExtractTextTurns(incoming[:4])

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
		ThreadRef{
			ArtifactID:            id,
			BranchID:              acf.MainBranch,
			MaterializedTurnsHash: ConversationTurnsHash(baseTurns),
			MaterializedTurnCount: len(baseTurns),
		},
		incoming,
		EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids, "[u3 a3] is already canonical; re-importing must not replay it")

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 1)
}

// TestMergeConversationByThreadRef_StaleContinuationAppendsOnlyTheNewSuffix
// pins the case the echo trim must NOT swallow: the native file's post-base
// turns partly echo canonical and then diverge into a real continuation. Only
// the divergent remainder may be appended, exactly once, and re-importing the
// same file afterwards must be a no-op.
func TestMergeConversationByThreadRef_StaleContinuationAppendsOnlyTheNewSuffix(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-00000000002c"

	base := time.Date(2026, 7, 27, 3, 0, 0, 0, time.UTC)
	u1 := convTurnAt("user", "q1", base)
	a1 := convTurnAt("assistant", "a1", base.Add(time.Second))
	u2 := convTurnAt("user", "q2", base.Add(10*time.Second))
	a2 := convTurnAt("assistant", "a2", base.Add(12*time.Second))
	peerQ := convTurnAt("user", "q-from-peer", base.Add(30*time.Second))
	peerA := convTurnAt("assistant", "a-from-peer", base.Add(32*time.Second))
	newQ := convTurnAt("user", "q-typed-here", base.Add(40*time.Second))
	newA := convTurnAt("assistant", "a-typed-here", base.Add(42*time.Second))

	seedConversation(t, store, id, []acf.ConversationEvent{u1, a1, u2, a2, peerQ, peerA})

	// Stamp says 2, but the file also holds the already-published [u2 a2] echo
	// AND a genuine new exchange written after it.
	incoming := []acf.ConversationEvent{u1, a1, u2, a2, newQ, newA}
	baseTurns := acf.ExtractTextTurns(incoming[:2])
	ref := ThreadRef{
		ArtifactID:            id,
		BranchID:              acf.MainBranch,
		MaterializedTurnsHash: ConversationTurnsHash(baseTurns),
		MaterializedTurnCount: len(baseTurns),
	}

	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
		ref, incoming, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	want := []acf.TextTurn{
		{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2"}, {Role: "assistant", Text: "a2"},
		{Role: "user", Text: "q-from-peer"}, {Role: "assistant", Text: "a-from-peer"},
		{Role: "user", Text: "q-typed-here"}, {Role: "assistant", Text: "a-typed-here"},
	}
	require.Equal(t, want, threadTurns(t, store, id),
		"the echoed [q2 a2] must be dropped and only the genuine new exchange appended")

	// Re-importing the identical unchanged file must now be a clean no-op.
	ids, handled, err = MergeConversationByThreadRef(
		context.Background(), store, OpaqueParams{SourceAgent: "claude-code"},
		ref, incoming, EncodeCanonicalConversationPayload)
	require.NoError(t, err)
	require.True(t, handled)
	require.Empty(t, ids, "the merge must be idempotent under repeated re-import")
	require.Equal(t, want, threadTurns(t, store, id))
}

// TestWouldRevertThread_RejectsStaleCopyThatOnlyReplaysCurrentTurns pins the
// fix for a repair being silently undone.
//
// A stale native copy of a repaired thread is a strict SUPERSET of the repaired
// head, so the old prefix-only test called it a legitimate continuation and let
// it re-assert the duplicates the repair had just removed. That is exactly what
// a stale re-import could otherwise do after the repair committed.
func TestWouldRevertThread_RejectsStaleCopyThatOnlyReplaysCurrentTurns(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-00000000003a"

	// The repaired head.
	repaired := []acf.ConversationEvent{
		convTurn("user", "q1"), convTurn("assistant", "a1"),
		convTurn("user", "q2"), convTurn("assistant", "a2"),
	}
	seedConversation(t, store, id, repaired)
	repairedTurns := acf.ExtractTextTurns(repaired)

	t.Run("stale copy re-asserting a removed trailing block is a revert", func(t *testing.T) {
		stale := append(append([]acf.TextTurn(nil), repairedTurns...),
			acf.TextTurn{Role: "user", Text: "q1"}, acf.TextTurn{Role: "assistant", Text: "a1"})
		require.True(t, adapterTextTurnsPrefixOrEqualForTest(repairedTurns, stale),
			"precondition: it passes the prefix test, which is why it used to be accepted")
		require.True(t, WouldRevertThread(store, id, stale))
	})

	t.Run("a genuine continuation is still accepted", func(t *testing.T) {
		cont := append(append([]acf.TextTurn(nil), repairedTurns...),
			acf.TextTurn{Role: "user", Text: "q3"}, acf.TextTurn{Role: "assistant", Text: "a3"})
		require.False(t, WouldRevertThread(store, id, cont))
	})

	t.Run("a replay followed by genuinely new content is accepted", func(t *testing.T) {
		mixed := append(append([]acf.TextTurn(nil), repairedTurns...),
			acf.TextTurn{Role: "user", Text: "q1"}, acf.TextTurn{Role: "assistant", Text: "a1"},
			acf.TextTurn{Role: "user", Text: "q3"})
		require.False(t, WouldRevertThread(store, id, mixed),
			"suppression is deferral: one new turn releases it so the continuation is never lost")
	})

	t.Run("a shorter divergent copy is still a revert", func(t *testing.T) {
		require.True(t, WouldRevertThread(store, id, []acf.TextTurn{{Role: "user", Text: "tampered"}}))
	})

	t.Run("an equal copy is not a revert", func(t *testing.T) {
		require.False(t, WouldRevertThread(store, id, repairedTurns))
	})
}

func adapterTextTurnsPrefixOrEqualForTest(prefix, full []acf.TextTurn) bool {
	return textTurnsPrefixOrEqual(prefix, full)
}
