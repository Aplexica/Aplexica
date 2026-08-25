package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
	"github.com/stretchr/testify/require"
)

func TestConversationsWebAccessor_SearchesVisibleDescription(t *testing.T) {
	store := newConversationSearchStore(t)
	seedConversationSearchArtifact(t, store, "older", time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC), "claude-code", "What is the luna size?")
	seedConversationSearchArtifact(t, store, "newer", time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC), "codex", "What is the capital of France?")

	acc := &conversationsWebAccessor{deps: &webAPIDeps{store: store}}
	got, err := acc.SearchConversations(apiweb.ConversationSearchQuery{Query: "luna", Limit: 10})

	require.NoError(t, err)
	require.Len(t, got.Conversations, 1)
	require.Equal(t, "older", got.Conversations[0].ArtifactID)
	require.Equal(t, "What is the luna size?", got.Conversations[0].Title)
	require.Equal(t, "claude-code", got.Conversations[0].SourceAgent)
	require.Equal(t, 2, got.Conversations[0].TurnCount)
}

func TestConversationsWebAccessor_RecentConversationsAreNewestFirstAndBounded(t *testing.T) {
	store := newConversationSearchStore(t)
	seedConversationSearchArtifact(t, store, "old", time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC), "claude-code", "old prompt")
	seedConversationSearchArtifact(t, store, "middle", time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC), "codex", "middle prompt")
	seedConversationSearchArtifact(t, store, "new", time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC), "kilo", "new prompt")

	reads := 0
	acc := &conversationsWebAccessor{
		deps: &webAPIDeps{store: store},
		readFirstEvent: func(k acf.Kind, id string) (acf.Event, bool, error) {
			reads++
			return store.ReadFirstEvent(k, id)
		},
	}
	got, err := acc.SearchConversations(apiweb.ConversationSearchQuery{Limit: 2})

	require.NoError(t, err)
	require.Equal(t, []string{"new", "middle"}, []string{got.Conversations[0].ArtifactID, got.Conversations[1].ArtifactID})
	require.Equal(t, 2, reads)
}

func TestConversationsWebAccessor_DeduplicatesNativeSourcePath(t *testing.T) {
	store := newConversationSearchStore(t)
	sourcePath := "/Users/exampleuser/.claude/projects/demo/986889d6-01d3-4a25-b158-23e4b9def160.jsonl"
	seedConversationSearchArtifactAtPath(t, store, "stale-copy", sourcePath, time.Date(2026, 7, 6, 13, 13, 47, 0, time.UTC), "claude-code", "Example User is working with a company Example Company")
	seedConversationSearchArtifactAtPath(t, store, "live-copy", sourcePath, time.Date(2026, 7, 6, 14, 8, 53, 0, time.UTC), "claude-code", "Example User is working with a company Example Company")
	seedConversationSearchArtifact(t, store, "other", time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC), "codex", "other prompt")

	acc := &conversationsWebAccessor{deps: &webAPIDeps{store: store}}
	got, err := acc.SearchConversations(apiweb.ConversationSearchQuery{Limit: 10})

	require.NoError(t, err)
	require.Equal(t, []string{"live-copy", "other"}, []string{got.Conversations[0].ArtifactID, got.Conversations[1].ArtifactID})
}

func TestConversationsWebAccessor_UsesLatestSnapshotWhenFirstEventIsEmptyShell(t *testing.T) {
	store := newConversationSearchStore(t)
	at := time.Date(2026, 7, 6, 13, 40, 54, 0, time.UTC)
	id := "codex-rollout"
	rolloutName := "rollout-2026-07-05T14-18-55-019f3381-3e01-7e32-b71b-4243710a8638.jsonl"
	prompt := "# Files mentioned by the user:\n\n" +
		"## codex-clipboard.png: /var/folders/example/codex-clipboard.png\n\n" +
		"## My request for Codex:\n" +
		"Example User is working with a company Example Company to design new pages for Sample App.\n" +
		"<image name=[Image #1] path=\"/var/folders/example/codex-clipboard.png\"></image>"
	wantTitle := "Example User is working with a company Example Company to design new pages for Sample App."
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             rolloutName,
		SourcePath:       filepath.Join("/Users/exampleuser/.codex/sessions/2026/07/05", rolloutName),
		CreatedAt:        at.Add(-time.Minute),
		UpdatedAt:        at,
	}))
	shellPayload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  at.Add(-time.Minute),
		Provenance: acf.Provenance{SourceAgent: "codex"},
		Payload:    shellPayload,
	}))
	head, ok, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, ok)

	fullPayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{
				Type:      acf.EventTypeTurn,
				Timestamp: at,
				Role:      "user",
				Content:   []acf.ContentBlock{{Type: "text", Text: prompt}},
			},
			{
				Type:      acf.EventTypeTurn,
				Timestamp: at.Add(time.Second),
				Role:      "assistant",
				Content:   []acf.ContentBlock{{Type: "text", Text: "Here are the implementation notes."}},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  at,
		Provenance: acf.Provenance{SourceAgent: "codex"},
		Payload:    fullPayload,
		ParentHash: head.Hash,
	}))

	acc := &conversationsWebAccessor{deps: &webAPIDeps{store: store}}
	got, err := acc.SearchConversations(apiweb.ConversationSearchQuery{Query: "Example Company", Limit: 10})

	require.NoError(t, err)
	require.Len(t, got.Conversations, 1)
	require.Equal(t, id, got.Conversations[0].ArtifactID)
	require.Equal(t, wantTitle, got.Conversations[0].Title)
	require.Equal(t, "codex", got.Conversations[0].SourceAgent)
	require.Equal(t, 2, got.Conversations[0].TurnCount)
	require.NotContains(t, got.Conversations[0].Title, "rollout-")
	require.NotContains(t, got.Conversations[0].Title, "Files mentioned")
}

func newConversationSearchStore(t *testing.T) *acf.Store {
	t.Helper()
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	return store
}

func seedConversationSearchArtifact(t *testing.T, store *acf.Store, id string, at time.Time, agent, prompt string) {
	t.Helper()
	seedConversationSearchArtifactAtPath(t, store, id, filepath.Join("/Users/exampleuser/.claude/projects/demo", id+".jsonl"), at, agent, prompt)
}

func seedConversationSearchArtifactAtPath(t *testing.T, store *acf.Store, id, sourcePath string, at time.Time, agent, prompt string) {
	t.Helper()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             id + ".jsonl",
		SourcePath:       sourcePath,
		CreatedAt:        at,
		UpdatedAt:        at,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{
				Type:      acf.EventTypeTurn,
				Timestamp: at,
				Role:      "user",
				Content:   []acf.ContentBlock{{Type: "text", Text: prompt}},
			},
			{
				Type:      acf.EventTypeTurn,
				Timestamp: at.Add(time.Second),
				Role:      "assistant",
				Content:   []acf.ContentBlock{{Type: "text", Text: "answer"}},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  at,
		Provenance: acf.Provenance{SourceAgent: agent},
		Payload:    payload,
	}))
}
