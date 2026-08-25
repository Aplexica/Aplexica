package acf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMaterializedConversationPayloadFromStore_StopsAtNewestFullAnchor(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	const id = "window-test"
	dir := filepath.Join(store.Root, "events", "conversations")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	full, err := EncodePayload(ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "anchor"}}}},
	})
	require.NoError(t, err)
	delta, err := EncodePayload(ConversationPayload{
		Format: ConversationDeltaFormatV1,
		Events: []ConversationEvent{{Type: EventTypeTurn, Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "tail"}}}},
	})
	require.NoError(t, err)
	anchorLine, err := json.Marshal(Event{EventID: "anchor", ArtifactID: id, Type: EventTypeUpdate, Payload: full})
	require.NoError(t, err)
	deltaLine, err := json.Marshal(Event{EventID: "delta", ArtifactID: id, Type: EventTypeUpdate, Payload: delta})
	require.NoError(t, err)
	// The invalid older line proves the backward reader does not parse history
	// before the newest full-state anchor.
	content := append([]byte("invalid-superseded-history\n"), anchorLine...)
	content = append(content, '\n')
	content = append(content, deltaLine...)
	content = append(content, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+".jsonl"), content, 0o600))

	payload, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, payload.Events, 2)
	require.Equal(t, "anchor", payload.Events[0].Content[0].Text)
	require.Equal(t, "tail", payload.Events[1].Content[0].Text)
}

func TestMaterializedConversationHeadFromStore_IgnoresSideBranchTail(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	const id = "branch-window-test"
	dir := filepath.Join(store.Root, "events", "conversations")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	full, err := EncodePayload(ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "main"}}}},
	})
	require.NoError(t, err)
	sideDelta, err := EncodePayload(ConversationPayload{
		Format: ConversationDeltaFormatV1,
		Events: []ConversationEvent{{Type: EventTypeTurn, Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "side-only"}}}},
	})
	require.NoError(t, err)
	mainLine, err := json.Marshal(Event{EventID: "main-head", ArtifactID: id, Type: EventTypeUpdate, Branch: MainBranch, Payload: full})
	require.NoError(t, err)
	sideLine, err := json.Marshal(Event{EventID: "side-head", ArtifactID: id, Type: EventTypeUpdate, Branch: "experiment", Payload: sideDelta})
	require.NoError(t, err)
	content := append(mainLine, '\n')
	content = append(content, sideLine...)
	content = append(content, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+".jsonl"), content, 0o600))

	payload, head, ok, err := store.MaterializedConversationHeadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "main-head", head.EventID)
	require.Len(t, payload.Events, 1)
	require.Equal(t, "main", payload.Events[0].Content[0].Text)
}

func TestMaterializedConversationHeadFromStore_UsesHeadBoundCache(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	const id = "cache-test"
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindConversation,
		Scope:            ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	want := ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{{
			Type: EventTypeTurn,
			Role: "user",
			Content: []ContentBlock{{
				Type: "text",
				Text: "cached conversation",
			}},
		}},
	}
	payload, err := EncodePayload(want)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Timestamp:  now,
		Payload:    payload,
		Provenance: Provenance{SourceAgent: "codex"},
	}))
	store.PrimeMaterializedConversation(id, want)

	// If the cache is used, a damaged log is never touched. This models the
	// native-import/fan-out handoff: the importer already parsed the complete
	// conversation, so the immediate materializer must reuse that projection.
	require.NoError(t, os.WriteFile(store.eventsPath(KindConversation, id), []byte("not-json\n"), 0o600))
	got, _, ok, err := store.MaterializedConversationHeadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)

	// Returned events are cloned at the slice/struct level and cannot replace
	// the cached event fields.
	got.Events[0].Role = "assistant"
	again, _, ok, err := store.MaterializedConversationHeadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "user", again.Events[0].Role)

	// The artifact head is the cache validity token. A new head must force the
	// normal source-of-truth read rather than returning stale state.
	art, err := store.ReadArtifact(KindConversation, id)
	require.NoError(t, err)
	art.HeadEventHash = "different-head"
	require.NoError(t, store.WriteArtifact(art))
	_, _, _, err = store.MaterializedConversationHeadFromStore(id)
	require.Error(t, err)
}

func TestPrimeMaterializedConversationAtHeadEvent_RejectsConcurrentAppend(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	const id = "cache-concurrent-append-test"
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindConversation,
		Scope:            ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	first := ConversationEvent{
		Type: EventTypeTurn, Role: "user",
		Content: []ContentBlock{{Type: "text", Text: "question"}},
	}
	second := ConversationEvent{
		Type: EventTypeTurn, Role: "assistant",
		Content: []ContentBlock{{Type: "text", Text: "concurrent answer"}},
	}
	firstPayload, err := EncodePayload(ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{first},
	})
	require.NoError(t, err)
	firstEvent := Event{
		EventID: NewID(), ArtifactID: id, Type: EventTypeCreate,
		Timestamp: now, Payload: firstPayload,
	}
	require.NoError(t, store.AppendEvent(KindConversation, firstEvent))
	firstHead, ok, err := store.LastEvent(KindConversation, id)
	require.NoError(t, err)
	require.True(t, ok)
	deltaPayload, err := EncodePayload(ConversationPayload{
		Format: ConversationDeltaFormatV1,
		Events: []ConversationEvent{second},
	})
	require.NoError(t, err)
	secondEvent := Event{
		EventID: NewID(), ArtifactID: id, Type: EventTypeUpdate,
		Timestamp: now.Add(time.Second), ParentHash: firstHead.Hash, Payload: deltaPayload,
	}
	require.NoError(t, store.AppendEvent(KindConversation, secondEvent))

	// The caller parsed/committed firstEvent, but secondEvent landed before it
	// reached the cache handoff. The stale one-turn projection must not be bound
	// to secondEvent's newer artifact head.
	store.PrimeMaterializedConversationAtHeadEvent(id, firstEvent.EventID, ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{first},
	})
	_, cached, err := store.ValidatedCachedMaterializedConversationPayload(id)
	require.NoError(t, err)
	require.False(t, cached)

	complete := ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{first, second},
	}
	store.PrimeMaterializedConversationAtHeadEvent(id, secondEvent.EventID, complete)
	got, cached, err := store.ValidatedCachedMaterializedConversationPayload(id)
	require.NoError(t, err)
	require.True(t, cached)
	require.Equal(t, complete, got)
}

func TestPrimeMaterializedConversation_AcceptsAlignedBaselineHead(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	const id = "aligned-baseline-cache-test"
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindConversation,
		Scope:            ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	want := ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{{
			Type:    EventTypeTurn,
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: "remote baseline"}},
		}},
	}
	payload, err := EncodePayload(want)
	require.NoError(t, err)
	require.NoError(t, store.AdoptBaseline(KindConversation, Event{
		EventID:        NewID(),
		ArtifactID:     id,
		Type:           EventTypeBaseline,
		Timestamp:      now,
		Payload:        payload,
		AlignedHead:    "origin-aligned-head",
		AlignedEventID: NewID(),
	}))
	store.PrimeMaterializedConversation(id, want)
	validated, validatedOK, err := store.ValidatedCachedMaterializedConversationPayload(id)
	require.NoError(t, err)
	require.True(t, validatedOK, "an aligned baseline cache must validate against AlignedHead bookkeeping")
	require.Equal(t, want, validated)

	// The artifact head intentionally differs from the local baseline wrapper
	// hash. A valid aligned cache must still prevent an immediate historical
	// replay after the authenticated baseline was adopted.
	require.NoError(t, os.WriteFile(store.eventsPath(KindConversation, id), []byte("damaged-old-history\n"), 0o600))
	got, _, ok, err := store.MaterializedConversationHeadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)
}

func TestValidatedCachedMaterializedConversationPayload_VerifiesOnlyBoundHead(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	const id = "validated-cache-test"
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindConversation,
		Scope:            ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	first := ConversationEvent{
		Type: EventTypeTurn, Role: "user",
		Content: []ContentBlock{{Type: "text", Text: "old"}},
	}
	second := ConversationEvent{
		Type: EventTypeTurn, Role: "assistant",
		Content: []ContentBlock{{Type: "text", Text: "new"}},
	}
	full, err := EncodePayload(ConversationPayload{Format: ConversationFormatV1, Events: []ConversationEvent{first}})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(KindConversation, Event{
		EventID: NewID(), ArtifactID: id, Type: EventTypeCreate,
		Timestamp: now, Payload: full,
	}))
	head, found, err := store.LastEvent(KindConversation, id)
	require.NoError(t, err)
	require.True(t, found)
	delta, err := EncodePayload(ConversationPayload{Format: ConversationDeltaFormatV1, Events: []ConversationEvent{second}})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(KindConversation, Event{
		EventID: NewID(), ArtifactID: id, Type: EventTypeUpdate,
		Timestamp: now.Add(time.Second), ParentHash: head.Hash, Payload: delta,
	}))
	want := ConversationPayload{Format: ConversationFormatV1, Events: []ConversationEvent{first, second}}
	store.PrimeMaterializedConversation(id, want)

	path := store.eventsPath(KindConversation, id)
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	firstNewline := bytes.IndexByte(original, '\n')
	require.Positive(t, firstNewline)
	validTail := append([]byte(nil), original[firstNewline+1:]...)

	// Older history may be arbitrarily large. A projection parsed from native
	// storage is safe to fan out without touching it when the exact persisted
	// head remains valid and bound to the artifact metadata.
	require.NoError(t, os.WriteFile(path, append([]byte("damaged-old-history\n"), validTail...), 0o600))
	got, ok, err := store.ValidatedCachedMaterializedConversationPayload(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)

	// The fast path never turns a changed newest event into a cache miss. It
	// recomputes that event's hash and reports an integrity error.
	tailLine := bytes.TrimSpace(validTail)
	var tampered Event
	require.NoError(t, json.Unmarshal(tailLine, &tampered))
	tampered.Payload = json.RawMessage(`{"format":"acf.conversation.delta.v1","events":[]}`)
	tamperedLine, err := json.Marshal(tampered)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(append([]byte("damaged-old-history\n"), tamperedLine...), '\n'), 0o600))
	_, _, err = store.ValidatedCachedMaterializedConversationPayload(id)
	require.ErrorContains(t, err, "does not match recomputed")
}
