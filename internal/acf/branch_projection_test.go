package acf

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testConversationPayload(t *testing.T, texts ...string) []byte {
	t.Helper()
	events := make([]ConversationEvent, 0, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		events = append(events, ConversationEvent{
			Type: EventTypeTurn,
			Role: role,
			Content: []ContentBlock{{
				Type: "text",
				Text: text,
			}},
		})
	}
	payload, err := EncodePayload(ConversationPayload{
		Format: ConversationFormatV1,
		Events: events,
	})
	require.NoError(t, err)
	return payload
}

func testConversationDelta(t *testing.T, texts ...string) []byte {
	t.Helper()
	events := make([]ConversationEvent, 0, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		events = append(events, ConversationEvent{
			Type: EventTypeTurn,
			Role: role,
			Content: []ContentBlock{{
				Type: "text",
				Text: text,
			}},
		})
	}
	payload, err := EncodePayload(ConversationPayload{
		Format: ConversationDeltaFormatV1,
		Events: events,
	})
	require.NoError(t, err)
	return payload
}

func seedProjectionArtifact(t *testing.T, store *Store, id string) time.Time {
	t.Helper()
	now := time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindConversation,
		Scope:            ScopeGlobal,
		Name:             "projection",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	return now
}

func appendProjectionEvent(t *testing.T, store *Store, event Event) Event {
	t.Helper()
	require.NoError(t, store.AppendEvent(KindConversation, event))
	events, err := store.ReadEvents(KindConversation, event.ArtifactID)
	require.NoError(t, err)
	return events[len(events)-1]
}

func projectionEventIDs(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.EventID)
	}
	return out
}

func projectionTexts(t *testing.T, payload ConversationPayload) []string {
	t.Helper()
	turns := ExtractTextTurns(payload.Events)
	out := make([]string, 0, len(turns))
	for _, turn := range turns {
		out = append(out, turn.Text)
	}
	return out
}

func TestProjectEventsForBranch_IncludesSourcePrefixThroughForkPoint(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019f0000-0000-7000-8000-000000000101"
	now := seedProjectionArtifact(t, store, id)

	create := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("create"),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Branch:     MainBranch,
		Timestamp:  now,
		Payload:    testConversationPayload(t, "root user", "root assistant"),
	})
	mainUpdate := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("main-update"),
		ArtifactID: id,
		Type:       EventTypeUpdate,
		Branch:     MainBranch,
		Timestamp:  now.Add(time.Minute),
		ParentHash: create.Hash,
		Payload:    testConversationDelta(t, "main-only"),
	})
	fork := appendProjectionEvent(t, store, Event{
		EventID:          acfTestID("fork-exp"),
		ArtifactID:       id,
		Type:             EventTypeForkOuter,
		Branch:           "experiment",
		Timestamp:        now.Add(2 * time.Minute),
		ParentHash:       create.Hash,
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  create.EventID,
	})
	branchUpdate := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("branch-update"),
		ArtifactID: id,
		Type:       EventTypeUpdate,
		Branch:     "experiment",
		Timestamp:  now.Add(3 * time.Minute),
		ParentHash: fork.Hash,
		Payload:    testConversationDelta(t, "branch-only"),
	})

	projected, err := store.ProjectEventsForBranch(KindConversation, id, "experiment", BranchProjectionOpts{})
	require.NoError(t, err)
	require.Equal(t, []string{create.EventID, fork.EventID, branchUpdate.EventID}, projectionEventIDs(projected))

	payload, _, ok, err := store.ProjectConversationPayloadForBranch(id, "experiment", BranchProjectionOpts{})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"root user", "root assistant", "branch-only"}, projectionTexts(t, payload))

	mainProjected, err := store.ProjectEventsForBranch(KindConversation, id, MainBranch, BranchProjectionOpts{})
	require.NoError(t, err)
	require.Equal(t, []string{create.EventID, mainUpdate.EventID}, projectionEventIDs(mainProjected))
}

func TestProjectConversationPayloadForBranch_NewForkMaterializesSourcePrefix(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019f0000-0000-7000-8000-000000000102"
	now := seedProjectionArtifact(t, store, id)

	create := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("create"),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Branch:     MainBranch,
		Timestamp:  now,
		Payload:    testConversationPayload(t, "hello", "hi"),
	})
	appendProjectionEvent(t, store, Event{
		EventID:          acfTestID("fork-empty"),
		ArtifactID:       id,
		Type:             EventTypeForkOuter,
		Branch:           "codex-test",
		Timestamp:        now.Add(time.Minute),
		ParentHash:       create.Hash,
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  create.EventID,
	})

	payload, events, ok, err := store.ProjectConversationPayloadForBranch(id, "codex-test", BranchProjectionOpts{})
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, events, 2)
	require.Equal(t, []string{"hello", "hi"}, projectionTexts(t, payload))
}

func TestProjectEventsForBranch_ForkFromForkBranch(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019f0000-0000-7000-8000-000000000103"
	now := seedProjectionArtifact(t, store, id)

	create := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("create"),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Branch:     MainBranch,
		Timestamp:  now,
		Payload:    testConversationPayload(t, "root"),
	})
	altFork := appendProjectionEvent(t, store, Event{
		EventID:          acfTestID("fork-alt"),
		ArtifactID:       id,
		Type:             EventTypeForkOuter,
		Branch:           "alt",
		Timestamp:        now.Add(time.Minute),
		ParentHash:       create.Hash,
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  create.EventID,
	})
	altUpdate := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("alt-update"),
		ArtifactID: id,
		Type:       EventTypeUpdate,
		Branch:     "alt",
		Timestamp:  now.Add(2 * time.Minute),
		ParentHash: altFork.Hash,
		Payload:    testConversationDelta(t, "alt-only"),
	})
	childFork := appendProjectionEvent(t, store, Event{
		EventID:          acfTestID("fork-child"),
		ArtifactID:       id,
		Type:             EventTypeForkOuter,
		Branch:           "child",
		Timestamp:        now.Add(3 * time.Minute),
		ParentHash:       altUpdate.Hash,
		ForkSourceBranch: "alt",
		ForkFromEventID:  altUpdate.EventID,
	})

	projected, err := store.ProjectEventsForBranch(KindConversation, id, "child", BranchProjectionOpts{})
	require.NoError(t, err)
	require.Equal(t, []string{create.EventID, altFork.EventID, altUpdate.EventID, childFork.EventID}, projectionEventIDs(projected))
}

func TestProjectEventsForBranch_MismatchedForkSourceFailsClearly(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019f0000-0000-7000-8000-000000000104"
	now := seedProjectionArtifact(t, store, id)

	create := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("create"),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Branch:     MainBranch,
		Timestamp:  now,
		Payload:    testConversationPayload(t, "root"),
	})
	altFork := appendProjectionEvent(t, store, Event{
		EventID:          acfTestID("fork-alt"),
		ArtifactID:       id,
		Type:             EventTypeForkOuter,
		Branch:           "alt",
		Timestamp:        now.Add(time.Minute),
		ParentHash:       create.Hash,
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  create.EventID,
	})
	altUpdate := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("alt-update"),
		ArtifactID: id,
		Type:       EventTypeUpdate,
		Branch:     "alt",
		Timestamp:  now.Add(2 * time.Minute),
		ParentHash: altFork.Hash,
		Payload:    testConversationDelta(t, "alt"),
	})
	appendProjectionEvent(t, store, Event{
		EventID:          acfTestID("fork-child"),
		ArtifactID:       id,
		Type:             EventTypeForkOuter,
		Branch:           "child",
		Timestamp:        now.Add(3 * time.Minute),
		ParentHash:       altUpdate.Hash,
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  altUpdate.EventID,
	})

	_, err := store.ProjectEventsForBranch(KindConversation, id, "child", BranchProjectionOpts{})
	require.ErrorIs(t, err, ErrForkPointMissing)
}

func TestProjectEventsForBranch_RejectsForkBeforeGlobalRedaction(t *testing.T) {
	store := &Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019f0000-0000-7000-8000-000000000105"
	now := seedProjectionArtifact(t, store, id)

	create := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("create"),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Branch:     MainBranch,
		Timestamp:  now,
		Payload:    testConversationPayload(t, "secret"),
	})
	redaction := appendProjectionEvent(t, store, Event{
		EventID:    acfTestID("redaction"),
		ArtifactID: id,
		Type:       EventTypeRedaction,
		Branch:     MainBranch,
		Timestamp:  now.Add(time.Minute),
		ParentHash: create.Hash,
	})
	require.NotEmpty(t, redaction.Hash)
	appendProjectionEvent(t, store, Event{
		EventID:          acfTestID("fork-before-redaction"),
		ArtifactID:       id,
		Type:             EventTypeForkOuter,
		Branch:           "unsafe",
		Timestamp:        now.Add(2 * time.Minute),
		ParentHash:       create.Hash,
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  create.EventID,
	})

	_, err := store.ProjectEventsForBranch(KindConversation, id, "unsafe", BranchProjectionOpts{})
	require.True(t, errors.Is(err, ErrRedactionBarrier), "got %v", err)
}

func acfTestID(label string) string {
	return "test-" + label
}
