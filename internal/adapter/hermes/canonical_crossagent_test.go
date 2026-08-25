package hermes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

// TestCrossAgent_ClaudeCodeJSONLToHermesDB validates the cross-agent
// claim: a Claude Code session imported in canonical mode produces ACF
// conversation events that the Hermes adapter can replay into a Hermes
// state.db. The session ends up as Hermes messages with the same content.
func TestCrossAgent_ClaudeCodeJSONLToHermesDB(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.CanonicalConversations = true

	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00.000Z","content":"hi"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01.000Z","content":"hello there"}
`)
	jsonlPath := filepath.Join(t.TempDir(), "in.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, jsonl, 0o644))

	ids, err := cc.ImportConversation(context.Background(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	dbPath := filepath.Join(t.TempDir(), "out.db")
	dbInit, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, dbInit.Close())

	hr := New()
	hr.HomeDir = t.TempDir()
	require.NoError(t, hr.ExportConversationsToDB(context.Background(), store, ids[0], dbPath))

	sessions, err := hermesdb.ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.GreaterOrEqual(t, len(sessions[0].Messages), 2, "user + assistant turns should land in the Hermes DB")

	roles := []string{}
	contents := []string{}
	for _, m := range sessions[0].Messages {
		roles = append(roles, m.Role)
		if m.Content != nil {
			contents = append(contents, *m.Content)
		}
	}
	require.Contains(t, roles, "user")
	require.Contains(t, roles, "assistant")
	require.Contains(t, contents, "hi")
	require.Contains(t, contents, "hello there")
}

func TestCrossAgent_CodexInternalsDoNotRenderInHermes(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Date(2026, 7, 18, 21, 48, 30, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, CreatedAt: now, UpdatedAt: now,
	}))
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: now, Content: []acf.ContentBlock{{Type: "text", Text: "what is capital of France?"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "Paris."}}},
		{Type: acf.EventTypeTurn, Role: "system", Timestamp: now.Add(2 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "<permissions instructions>private harness"}}},
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: now.Add(3 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "how many people live in paris?"}}},
		{Type: acf.EventTypeToolCall, Timestamp: now.Add(4 * time.Second), CallID: "call-1", ToolName: "exec", Input: []byte(`{"cmd":"search"}`)},
		{Type: acf.EventTypeToolResult, Timestamp: now.Add(5 * time.Second), CallID: "call-1", Content: []acf.ContentBlock{{Type: "text", Text: "large unrelated tool output"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(6 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "About 2.1 million."}}},
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now,
		Provenance: acf.Provenance{SourceAgent: "codex"}, Payload: payload,
	}))

	dbPath := filepath.Join(t.TempDir(), "hermes.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	hr := New()
	require.NoError(t, hr.ExportConversationsToDB(context.Background(), store, id, dbPath))

	sessions, err := hermesdb.ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Len(t, sessions[0].Messages, 4, "Hermes must receive only visible user/assistant turns")
	got := make([]string, 0, 4)
	for _, message := range sessions[0].Messages {
		require.Contains(t, []string{"user", "assistant"}, message.Role)
		require.Nil(t, message.ToolCalls)
		require.Nil(t, message.ToolCallID)
		if message.Content != nil {
			got = append(got, *message.Content)
		}
	}
	require.Equal(t, []string{
		"what is capital of France?", "Paris.", "how many people live in paris?", "About 2.1 million.",
	}, got)
}

func TestCrossAgent_PortableProjectionIsStableAcrossLatestUpdaterAndRepairsOwnedRows(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Date(2026, 7, 18, 22, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, CreatedAt: now, UpdatedAt: now,
	}))

	question, answer := "what is capital of France?", "Paris."
	harness := "<permissions instructions>private Codex harness"
	canonical := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: now, Content: []acf.ContentBlock{{Type: "text", Text: question}}},
		{Type: acf.EventTypeSystemNote, Timestamp: now.Add(time.Second), Content: []acf.ContentBlock{{Type: "text", Text: harness}}},
		{Type: acf.EventTypeToolCall, Timestamp: now.Add(2 * time.Second), CallID: "call-1", ToolName: "exec"},
		{Type: acf.EventTypeToolResult, Timestamp: now.Add(3 * time.Second), CallID: "call-1", Content: []acf.ContentBlock{{Type: "text", Text: "private output"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(4 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: answer}}},
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: canonical})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now,
		// A later Hermes continuation can make Hermes the latest updater. That
		// must not switch the same canonical payload back to full-fidelity mode.
		Provenance: acf.Provenance{SourceAgent: "hermes"}, Payload: payload,
	}))

	dbPath := filepath.Join(t.TempDir(), "hermes.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	// Seed the exact full projection written by the previous adapter. The
	// upgrade repair deliberately removes only identities it can prove came
	// from that projection.
	require.NoError(t, hermesdb.InsertSession(dbPath, DecodeBundleFromCanonical(id, "codex", canonical)))

	hr := New()
	require.NoError(t, hr.ExportConversationsToDB(context.Background(), store, id, dbPath))
	bundles, err := hermesdb.ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Len(t, bundles[0].Messages, 2)
	require.Equal(t, "user", bundles[0].Messages[0].Role)
	require.Equal(t, question, *bundles[0].Messages[0].Content)
	require.Equal(t, "assistant", bundles[0].Messages[1].Role)
	require.Equal(t, answer, *bundles[0].Messages[1].Content)
}

func TestCrossAgent_PortableExportRepairsOwnedLegacyUAAUAAUProjection(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	q1, a1 := "what is capital of France?", "The capital of France is Paris."
	q2, a2 := "how many people live in Paris?", "Paris has about 2.1 million residents."
	canonical := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: now, Content: []acf.ContentBlock{{Type: "text", Text: q1}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(time.Second), Content: []acf.ContentBlock{{Type: "text", Text: a1}}},
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: now.Add(2 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: q2}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(3 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: a2}}},
	}
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: canonical})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Provenance: acf.Provenance{SourceAgent: "codex"}, Payload: payload,
	}))

	dbPath := filepath.Join(t.TempDir(), "hermes.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	clean := DecodePortableBundleFromCanonical(id, "codex", canonical)
	polluted := clean
	polluted.Messages = []hermesdb.MessageRow{
		clean.Messages[0], clean.Messages[1],
		{Role: "assistant", Content: &a1, Timestamp: clean.Messages[1].Timestamp + 0.5},
		clean.Messages[2], clean.Messages[3],
		{Role: "assistant", Content: &a2, Timestamp: clean.Messages[3].Timestamp + 100},
		{Role: "user", Content: &q2, Timestamp: clean.Messages[3].Timestamp + 101},
	}
	polluted.Session.MessageCount = int64(len(polluted.Messages))
	require.NoError(t, hermesdb.InsertSession(dbPath, polluted))

	hr := New()
	require.NoError(t, hr.ExportConversationsToDB(t.Context(), store, id, dbPath))
	bundles, err := hermesdb.ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, clean.Messages, bundles[0].Messages)
	require.Equal(t, int64(len(clean.Messages)), bundles[0].Session.MessageCount)
}

func TestCrossAgent_PortableRepairUsesHistoricalCanonicalIdentities(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pollution string
		compact   bool
	}{
		{name: "polluted full payload remains active", pollution: "full"},
		{name: "polluted delta moved to compacted history", pollution: "delta", compact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
			require.NoError(t, store.Init())
			id := acf.NewID()
			now := time.Date(2026, 7, 18, 23, 0, 0, 0, time.UTC)
			require.NoError(t, store.WriteArtifact(acf.Artifact{
				AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
				Scope: acf.ScopeGlobal, CreatedAt: now, UpdatedAt: now,
			}))

			question, answer := "what is capital of France?", "Paris."
			commentary := "Searching the web and checking several sources."
			harness := "<permissions instructions>private Codex harness"
			privateOutput := "large unrelated tool output"
			q := acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "user", Timestamp: now,
				Content: []acf.ContentBlock{{Type: "text", Text: question}}}
			progress := acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(time.Second),
				Content: []acf.ContentBlock{{Type: "text", Text: commentary}}}
			system := acf.ConversationEvent{Type: acf.EventTypeSystemNote, Timestamp: now.Add(2 * time.Second),
				Content: []acf.ContentBlock{{Type: "text", Text: harness}}}
			call := acf.ConversationEvent{Type: acf.EventTypeToolCall, Timestamp: now.Add(3 * time.Second),
				CallID: "call-1", ToolName: "exec", Input: []byte(`{"cmd":"search"}`)}
			result := acf.ConversationEvent{Type: acf.EventTypeToolResult, Timestamp: now.Add(4 * time.Second),
				CallID: "call-1", Content: []acf.ContentBlock{{Type: "text", Text: privateOutput}}}
			a := acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(5 * time.Second),
				Content: []acf.ContentBlock{{Type: "text", Text: answer}}}
			polluted := []acf.ConversationEvent{q, progress, system, call, result, a}
			cleanQ, cleanA := q, a
			cleanQ.Timestamp = now.Add(20 * time.Second)
			cleanA.Timestamp = now.Add(21 * time.Second)
			clean := []acf.ConversationEvent{cleanQ, cleanA}

			appendPayload := func(eventType acf.EventType, format string, canonical []acf.ConversationEvent, at time.Time) {
				t.Helper()
				payload, err := acf.EncodePayload(acf.ConversationPayload{Format: format, Events: canonical})
				require.NoError(t, err)
				artifact, err := store.ReadArtifact(acf.KindConversation, id)
				require.NoError(t, err)
				require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
					EventID: acf.NewID(), ArtifactID: id, Type: eventType, Timestamp: at,
					ParentHash: artifact.HeadEventHash, Provenance: acf.Provenance{SourceAgent: "codex"}, Payload: payload,
				}))
			}

			if tc.pollution == "full" {
				appendPayload(acf.EventTypeCreate, acf.ConversationFormatV1, polluted, now)
			} else {
				appendPayload(acf.EventTypeCreate, acf.ConversationFormatV1, []acf.ConversationEvent{q}, now)
				appendPayload(acf.EventTypeUpdate, acf.ConversationDeltaFormatV1, polluted[1:], now.Add(time.Second))
			}

			dbPath := filepath.Join(t.TempDir(), "hermes.db")
			db, err := hermesdb.InitTestDB(dbPath)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			// This is the row set written by the pre-fix exporter before the clean
			// canonical replacement arrived.
			require.NoError(t, hermesdb.InsertSession(dbPath, DecodeBundleFromCanonical(id, "codex", polluted)))

			appendPayload(acf.EventTypeUpdate, acf.ConversationFormatV1, clean, now.Add(10*time.Second))
			if tc.compact {
				_, err := retention.CreateSnapshot(context.Background(), store, acf.KindConversation, id)
				require.NoError(t, err)
				moved, _, err := retention.PruneArtifact(
					context.Background(), store, acf.KindConversation, id, time.Now().Add(-time.Hour),
				)
				require.NoError(t, err)
				require.Positive(t, moved)
			}

			hr := New()
			require.NoError(t, hr.ExportConversationsToDB(context.Background(), store, id, dbPath))
			bundles, err := hermesdb.ListSessions(dbPath, 0)
			require.NoError(t, err)
			require.Len(t, bundles, 1)
			require.Len(t, bundles[0].Messages, 2,
				"historical commentary, harness, tool call, and tool result must all be removed")
			require.Equal(t, "user", bundles[0].Messages[0].Role)
			require.Equal(t, question, *bundles[0].Messages[0].Content)
			require.Equal(t, "assistant", bundles[0].Messages[1].Role)
			require.Equal(t, answer, *bundles[0].Messages[1].Content)
		})
	}
}
