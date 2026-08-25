package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func testTurn(text string) acf.ConversationEvent {
	return acf.ConversationEvent{
		Type:    acf.EventTypeTurn,
		Role:    "assistant",
		Content: []acf.ContentBlock{{Type: "text", Text: text}},
	}
}

func TestConversationTailAfterEventSequence_UsesLastExactAnchor(t *testing.T) {
	anchor := []acf.ConversationEvent{testTurn("repeat")}
	full := []acf.ConversationEvent{
		testTurn("before"), testTurn("repeat"), testTurn("middle"),
		testTurn("repeat"), testTurn("new"),
	}
	tail, ok := conversationTailAfterEventSequence(anchor, full)
	require.True(t, ok)
	require.Equal(t, []acf.ConversationEvent{testTurn("new")}, tail)
}

func TestConversationTailAfterEventSequence_RejectsMissingOrEmptyAnchor(t *testing.T) {
	_, ok := conversationTailAfterEventSequence(nil, []acf.ConversationEvent{testTurn("a")})
	require.False(t, ok)
	_, ok = conversationTailAfterEventSequence([]acf.ConversationEvent{testTurn("x")}, []acf.ConversationEvent{testTurn("a")})
	require.False(t, ok)
}

func TestConversationTailAfterEventSequence_MatchesPersistedWireEquivalentRawJSON(t *testing.T) {
	timestamp := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	parsed := acf.ConversationEvent{
		Type:      acf.EventTypeToolCall,
		Timestamp: timestamp,
		CallID:    "call-1",
		ToolName:  "exec",
		Input:     json.RawMessage(`"x < y"`),
		Tags:      []string{},
	}

	// Round-tripping through the event log rewrites the RawMessage's HTML-
	// sensitive character and collapses the empty omitempty slice to nil. The
	// structs are not reflect.DeepEqual, but their persisted JSON is identical.
	encoded, err := json.Marshal(parsed)
	require.NoError(t, err)
	var persisted acf.ConversationEvent
	require.NoError(t, json.Unmarshal(encoded, &persisted))
	require.NotEqual(t, parsed, persisted)

	newEvent := testTurn("new")
	tail, ok := conversationTailAfterEventSequence([]acf.ConversationEvent{persisted}, []acf.ConversationEvent{parsed, newEvent})
	require.True(t, ok)
	require.Equal(t, []acf.ConversationEvent{newEvent}, tail)
}

func TestReplayCanonicalConversationContent_UsesValidatedNativeProjection(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	const id = "projection-export-test"
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	first, second := testTurn("first"), testTurn("second")
	full, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: []acf.ConversationEvent{first}})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: full,
	}))
	head, found, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, found)
	delta, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationDeltaFormatV1, Events: []acf.ConversationEvent{second}})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), ParentHash: head.Hash, Payload: delta,
	}))
	store.PrimeMaterializedConversation(id, acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{first, second},
	})

	path := filepath.Join(store.Root, "events", "conversations", id+".jsonl")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	firstNewline := bytes.IndexByte(original, '\n')
	require.Positive(t, firstNewline)
	require.NoError(t, os.WriteFile(path, append([]byte("invalid-superseded-history\n"), original[firstNewline+1:]...), 0o600))

	decodeCanonical := func(events []acf.ConversationEvent) ([]byte, error) {
		texts := make([]string, 0, len(events))
		for _, event := range events {
			texts = append(texts, event.Content[0].Text)
		}
		return []byte(strings.Join(texts, ",")), nil
	}
	decodeLegacy := func(acf.Event) (string, error) {
		return "", fmt.Errorf("legacy decoder must not be called")
	}
	content, tombstoned, err := ReplayCanonicalConversationContent(store, id, decodeCanonical, decodeLegacy)
	require.NoError(t, err)
	require.False(t, tombstoned)
	require.Equal(t, "first,second", content)
}

func TestImportCanonicalConversation_DeltaPrimePreservesPrefixAndAttachments(t *testing.T) {
	for _, tc := range []struct {
		name               string
		importConversation func(
			t *testing.T,
			store *acf.Store,
			params OpaqueParams,
			path string,
			parsed []acf.ConversationEvent,
		) ([]string, error)
	}{
		{
			name: "content parser",
			importConversation: func(
				t *testing.T,
				store *acf.Store,
				params OpaqueParams,
				path string,
				parsed []acf.ConversationEvent,
			) ([]string, error) {
				t.Helper()
				return ImportCanonicalConversation(t.Context(), store, params, path,
					func([]byte) ([]acf.ConversationEvent, error) { return parsed, nil })
			},
		},
		{
			name: "path parser",
			importConversation: func(
				t *testing.T,
				store *acf.Store,
				params OpaqueParams,
				path string,
				parsed []acf.ConversationEvent,
			) ([]string, error) {
				t.Helper()
				return ImportCanonicalConversationFile(t.Context(), store, params, path,
					func(string) ([]acf.ConversationEvent, error) { return parsed, nil })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store := &acf.Store{Root: filepath.Join(root, "store")}
			require.NoError(t, store.Init())
			path := filepath.Join(root, "rollout.jsonl")
			require.NoError(t, os.WriteFile(path, []byte("native"), 0o600))
			abs, err := filepath.Abs(path)
			require.NoError(t, err)

			prefix := convTurn("assistant", "portable prefix from another agent")
			question := convTurn("user", "question")
			answer := convTurn("assistant", "answer")
			followup := convTurn("user", "follow-up")
			currentEvents := []acf.ConversationEvent{prefix, question, answer}
			parsedEvents := []acf.ConversationEvent{question, answer, followup}
			attachments := []acf.Attachment{{
				Kind: "image", MimeType: "image/png", ContentHash: "attachment", Bytes: 42, Filename: "proof.png",
			}}
			id := acf.NewID()
			now := time.Now().UTC()
			require.NoError(t, store.WriteArtifact(acf.Artifact{
				AcfSchemaVersion: acf.SchemaVersion,
				ArtifactID:       id,
				Kind:             acf.KindConversation,
				Scope:            acf.ScopeGlobal,
				Name:             filepath.Base(path),
				SourcePath:       abs,
				CreatedAt:        now,
				UpdatedAt:        now,
			}))
			full, err := acf.EncodePayload(acf.ConversationPayload{
				Format: acf.ConversationFormatV1, Events: currentEvents, Attachments: attachments,
			})
			require.NoError(t, err)
			require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
				EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
				Timestamp: now, Payload: full,
			}))

			ids, err := tc.importConversation(t, store,
				OpaqueParams{DeviceID: "local", SourceAgent: "codex", AdapterVersion: "1.0.39"},
				path, parsedEvents,
			)
			require.NoError(t, err)
			require.Equal(t, []string{id}, ids)
			head, ok, err := store.LastEvent(acf.KindConversation, id)
			require.NoError(t, err)
			require.True(t, ok)
			committed, err := acf.DecodeConversationPayload(head)
			require.NoError(t, err)
			require.Equal(t, acf.ConversationDeltaFormatV1, committed.Format)
			require.Equal(t, []acf.ConversationEvent{followup}, committed.Events)

			cached, ok, err := store.ValidatedCachedMaterializedConversationPayload(id)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, []acf.ConversationEvent{prefix, question, answer, followup}, cached.Events,
				"the cache must retain the portable prefix omitted from the native suffix-overlap snapshot")
			require.Equal(t, attachments, cached.Attachments,
				"the cache must retain canonical attachments not represented by native session rows")
		})
	}
}
