package syncd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

// TestLatestPayloadBearingEvent_* characterize the exact (head, ok, redacted)
// contract of the active-log walk so the delegation to acf.LatestPayloadEvent
// stays behavior-preserving. The redaction-as-barrier policy lives HERE (acf is
// policy-free), so these cases pin it down explicitly.

func TestLatestPayloadBearingEvent_ReturnsNewestMutatingEvent(t *testing.T) {
	for _, typ := range []acf.EventType{acf.EventTypeCreate, acf.EventTypeUpdate, acf.EventTypeResolution} {
		t.Run(string(typ), func(t *testing.T) {
			head, ok, redacted := latestPayloadBearingEvent([]acf.Event{
				{EventID: "older", Type: acf.EventTypeCreate, Payload: json.RawMessage(`{}`)},
				{EventID: "newer", Type: typ, Payload: json.RawMessage(`{}`)},
			})
			require.True(t, ok)
			require.False(t, redacted)
			require.Equal(t, "newer", head.EventID)
		})
	}
}

func TestLatestPayloadBearingEvent_PayloadBearingSnapshot(t *testing.T) {
	head, ok, redacted := latestPayloadBearingEvent([]acf.Event{
		{EventID: "snap", Type: acf.EventTypeSnapshot, Payload: json.RawMessage(`{"format":"x"}`)},
	})
	require.True(t, ok)
	require.False(t, redacted)
	require.Equal(t, "snap", head.EventID)
}

func TestConversationHead_MaterializesDeltaHead(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "delta conversation",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	base, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "hello"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Payload:    base,
	}))
	parent, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	delta, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "next"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "ok"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(time.Minute),
		ParentHash: parent,
		Payload:    delta,
	}))
	active, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)

	selected, ok, err := conversationHead(store, id, []acf.Event{active[len(active)-1]})
	require.NoError(t, err)
	require.True(t, ok)
	p, err := acf.DecodeConversationPayload(selected)
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, p.Format)
	require.Len(t, p.Events, 4)
	require.Equal(t, "hello", p.Events[0].Content[0].Text)
	require.Equal(t, "next", p.Events[2].Content[0].Text)
}

func TestLatestPayloadBearingEvent_PayloadlessSnapshotSkipped(t *testing.T) {
	head, ok, redacted := latestPayloadBearingEvent([]acf.Event{
		{EventID: "create", Type: acf.EventTypeCreate, Payload: json.RawMessage(`{}`)},
		{EventID: "snap", Type: acf.EventTypeSnapshot, Payload: json.RawMessage("null")},
	})
	require.True(t, ok)
	require.False(t, redacted)
	require.Equal(t, "create", head.EventID, "payload-less snapshot is skipped; the create below wins")
}

func TestLatestPayloadBearingEvent_RedactionIsAuthoritativeBarrier(t *testing.T) {
	// Newest mutating event is a redaction → redacted, and the pre-redaction
	// create must NOT be returned (the caller must not fall back to compacted).
	_, ok, redacted := latestPayloadBearingEvent([]acf.Event{
		{EventID: "create", Type: acf.EventTypeCreate, Payload: json.RawMessage(`{}`)},
		{EventID: "redaction", Type: acf.EventTypeRedaction},
	})
	require.False(t, ok)
	require.True(t, redacted, "a redaction newer than every payload is authoritative")
}

func TestLatestPayloadBearingEvent_PayloadNewerThanRedactionWins(t *testing.T) {
	// A payload event NEWER than the redaction is the live head — the redaction
	// only shadows what came before it.
	head, ok, redacted := latestPayloadBearingEvent([]acf.Event{
		{EventID: "create", Type: acf.EventTypeCreate, Payload: json.RawMessage(`{}`)},
		{EventID: "redaction", Type: acf.EventTypeRedaction},
		{EventID: "update", Type: acf.EventTypeUpdate, Payload: json.RawMessage(`{}`)},
	})
	require.True(t, ok)
	require.False(t, redacted)
	require.Equal(t, "update", head.EventID)
}

func TestLatestPayloadBearingEvent_NoPayloadNoRedaction(t *testing.T) {
	_, ok, redacted := latestPayloadBearingEvent(nil)
	require.False(t, ok)
	require.False(t, redacted)

	_, ok, redacted = latestPayloadBearingEvent([]acf.Event{
		{Type: acf.EventTypeForkOuter},
		{Type: acf.EventTypeSnapshot, Payload: json.RawMessage("null")},
	})
	require.False(t, ok, "no payload-bearing event")
	require.False(t, redacted, "no redaction → caller may retry against the compacted layer")
}

// TestConversationHead_LegacyPayloadlessSnapshotRecoversFromCompacted is the
// regression test for the silent fan-out degradation: after retention's
// on-snapshot prune compacts the pre-snapshot events of a main-only
// conversation, the active log is snapshot-only. A snapshot written BEFORE the
// FR-02.32 fix is payload-less (Payload serializes to `null`), so selecting the
// raw last event (events[len(events)-1]) yields a zero-value ConversationPayload
// (Format=="") — renderConversationMarkdown emits an empty code block and the
// native session materializer silently skips (default branch → ("", false, nil)).
//
// The orchestrator must instead route head selection through conversationHead,
// which walks backward to the latest payload-bearing event and — for a
// payload-less snapshot-only active log — falls back to the .compacted layer to
// recover the create/update payload. The conversation must still materialize
// into a real transcript and a non-empty native session.
func TestConversationHead_LegacyPayloadlessSnapshotRecoversFromCompacted(t *testing.T) {
	const (
		userText = "hello from the seed conversation"
		asstText = "and here is the assistant reply"
	)

	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	// Seed a conversation artifact with one canonical create event that carries
	// a real user/assistant transcript.
	evTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "seed conversation",
		CreatedAt:        evTime,
		UpdatedAt:        evTime,
	}))
	createPayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: "turn", Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: userText}}},
			{Type: "turn", Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: asstText}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  evTime,
		Provenance: acf.Provenance{SourceAgent: "src"},
		Payload:    createPayload,
	}))

	// Append a PAYLOAD-LESS snapshot, mirroring the pre-FR-02.32 on-disk shape:
	// Payload:nil (serializes to the literal JSON `null`). ParentHash chains it
	// to the create so the log still VerifyChains.
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     evTime.Add(time.Minute),
		ParentHash:    head,
		SnapshotState: "sha256:deadbeef",
		Payload:       nil,
	}))

	// Prune: the create event moves into .compacted, leaving a snapshot-only
	// active log. Zero graceDeadline → the compacted file is never grace-deleted
	// (mirrors retention's sweepNoGraceDelete), so the fallback can read it.
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindConversation, id, time.Time{})
	require.NoError(t, err)
	require.Equal(t, 1, moved, "the pre-snapshot create event must move to .compacted")

	// Sanity: the active log is now the single legacy payload-less snapshot.
	active, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)
	require.False(t, acf.HasPayload(active[0].Payload), "active head must be the legacy payload-less snapshot")

	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)

	// Production head selection must recover the create payload from .compacted.
	selected, ok, err := conversationHead(store, id, active)
	require.NoError(t, err)
	require.True(t, ok, "a materializable head must be recovered from the compacted layer")

	// convDoc pass: the markdown transcript must contain the real turns, not an
	// empty fenced block.
	md, err := renderConversationMarkdown(art, "src", selected)
	require.NoError(t, err)
	require.Contains(t, md, userText, "transcript lost the user turn (degraded to empty doc)")
	require.Contains(t, md, asstText, "transcript lost the assistant turn (degraded to empty doc)")

	// convSession pass: a real Claude Code adapter must write a non-empty native
	// session (ok==true, not the silent default-branch skip).
	cc := &claudecode.Adapter{HomeDir: t.TempDir()}
	path, ok, err := cc.MaterializeConversationSession(art, selected, "src")
	require.NoError(t, err)
	require.True(t, ok, "native session must materialize, not silently skip")
	require.NotEmpty(t, path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), userText, "native session is empty/missing the transcript")
}

func TestConversationHeadForBranch_NewForkMaterializesSourcePrefix(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "forked conversation",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	create := appendConvHeadEvent(t, store, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Branch:     acf.MainBranch,
		Timestamp:  now,
		Payload:    convHeadPayload(t, "hello", "hi"),
	})
	fork := appendConvHeadEvent(t, store, acf.Event{
		EventID:          acf.NewID(),
		ArtifactID:       id,
		Type:             acf.EventTypeForkOuter,
		Branch:           "experiment",
		Timestamp:        now.Add(time.Minute),
		ParentHash:       create.Hash,
		ForkSourceBranch: acf.MainBranch,
		ForkFromEventID:  create.EventID,
	})

	head, ok, err := conversationHeadForBranch(store, id, "experiment")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fork.EventID, head.EventID)
	require.Equal(t, "experiment", head.Branch)
	payload, err := acf.DecodeConversationPayload(head)
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, payload.Format)
	require.Equal(t, []string{"hello", "hi"}, convHeadTexts(payload))
}

func TestConversationHeadForBranch_MainIgnoresSideBranchUpdates(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "branched conversation",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	create := appendConvHeadEvent(t, store, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Branch:     acf.MainBranch,
		Timestamp:  now,
		Payload:    convHeadPayload(t, "main"),
	})
	fork := appendConvHeadEvent(t, store, acf.Event{
		EventID:          acf.NewID(),
		ArtifactID:       id,
		Type:             acf.EventTypeForkOuter,
		Branch:           "experiment",
		Timestamp:        now.Add(time.Minute),
		ParentHash:       create.Hash,
		ForkSourceBranch: acf.MainBranch,
		ForkFromEventID:  create.EventID,
	})
	appendConvHeadEvent(t, store, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Branch:     "experiment",
		Timestamp:  now.Add(2 * time.Minute),
		ParentHash: fork.Hash,
		Payload:    convHeadDelta(t, "branch-only"),
	})

	mainHead, ok, err := conversationHeadForBranch(store, id, acf.MainBranch)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, create.EventID, mainHead.EventID)
	mainPayload, err := acf.DecodeConversationPayload(mainHead)
	require.NoError(t, err)
	require.Equal(t, []string{"main"}, convHeadTexts(mainPayload))

	branchHead, ok, err := conversationHeadForBranch(store, id, "experiment")
	require.NoError(t, err)
	require.True(t, ok)
	branchPayload, err := acf.DecodeConversationPayload(branchHead)
	require.NoError(t, err)
	require.Equal(t, []string{"main", "branch-only"}, convHeadTexts(branchPayload))
}

func appendConvHeadEvent(t *testing.T, store *acf.Store, event acf.Event) acf.Event {
	t.Helper()
	require.NoError(t, store.AppendEvent(acf.KindConversation, event))
	events, err := store.ReadEvents(acf.KindConversation, event.ArtifactID)
	require.NoError(t, err)
	return events[len(events)-1]
}

func convHeadPayload(t *testing.T, texts ...string) []byte {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: convHeadTurns(texts...),
	})
	require.NoError(t, err)
	return payload
}

func convHeadDelta(t *testing.T, texts ...string) []byte {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: convHeadTurns(texts...),
	})
	require.NoError(t, err)
	return payload
}

func convHeadTurns(texts ...string) []acf.ConversationEvent {
	events := make([]acf.ConversationEvent, 0, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		events = append(events, acf.ConversationEvent{
			Type: acf.EventTypeTurn,
			Role: role,
			Content: []acf.ContentBlock{{
				Type: "text",
				Text: text,
			}},
		})
	}
	return events
}

func convHeadTexts(payload acf.ConversationPayload) []string {
	turns := acf.ExtractTextTurns(payload.Events)
	texts := make([]string, 0, len(turns))
	for _, turn := range turns {
		texts = append(texts, turn.Text)
	}
	return texts
}
