package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportConversation_CanonicalMode(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := New()
	a.CanonicalConversations = true
	jsonlPath := filepath.Join("testdata", "session-tiny.jsonl")

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	require.Equal(t, acf.ConversationFormatV1, payload.Format)
	require.NotEmpty(t, payload.Events)
	require.Empty(t, payload.Content, "structured-mode payload must not carry the legacy Content field")
}

func TestImportConversation_CanonicalModeAppendsDelta(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := New()
	a.CanonicalConversations = true
	seed, err := os.ReadFile(filepath.Join("testdata", "session-tiny.jsonl"))
	require.NoError(t, err)
	jsonlPath := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, seed, 0o644))

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]
	initialEvents, _ := encodePortableCanonicalFromMode(seed, 0, generatedCodexSession(seed))

	appended := seed
	appended = append(appended, []byte("\n{\"timestamp\":\"2026-05-21T10:00:06.000Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"second question\"}]}}")...)
	appended = append(appended, []byte("\n{\"timestamp\":\"2026-05-21T10:00:07.000Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"second answer\"}]}}")...)
	require.NoError(t, os.WriteFile(jsonlPath, appended, 0o644))

	ids, err = a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)

	logEvents, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, logEvents, 2)
	var delta acf.ConversationPayload
	require.NoError(t, json.Unmarshal(logEvents[1].Payload, &delta))
	require.Equal(t, acf.ConversationDeltaFormatV1, delta.Format)
	require.Len(t, delta.Events, 2)

	materialized, ok, err := acf.MaterializedConversationPayload(logEvents)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.ConversationFormatV1, materialized.Format)
	require.Len(t, materialized.Events, len(initialEvents)+2)

	dest := filepath.Join(t.TempDir(), "exported.jsonl")
	require.NoError(t, a.ExportConversation(t.Context(), store, id, dest))
	exported, err := os.ReadFile(dest)
	require.NoError(t, err)
	exportedEvents, err := EncodeCanonical(exported)
	require.NoError(t, err)
	require.Equal(t, materialized.Events, exportedEvents)
}

func TestImportConversation_RepairsLegacyNativeCodexHarnessAndCommentary(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	raw := []byte(`{"timestamp":"2026-07-18T21:47:30Z","type":"session_meta","payload":{"id":"native-codex"}}
{"timestamp":"2026-07-18T21:47:31Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"private execution harness"}]}}
{"timestamp":"2026-07-18T21:47:32Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"what is capital of France?"}]}}
{"timestamp":"2026-07-18T21:47:33Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Searching internal context."}]}}
{"timestamp":"2026-07-18T21:47:34Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{\"cmd\":\"lookup\"}","call_id":"call-1"}}
{"timestamp":"2026-07-18T21:47:35Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"tool output"}}
{"timestamp":"2026-07-18T21:47:36Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Paris."}]}}
`)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	legacy := encodeCanonicalLegacyNativeForRepair(raw)
	require.Contains(t, acf.ExtractTextTurns(legacy), acf.TextTurn{Role: "assistant", Text: "Searching internal context."})
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             filepath.Base(path),
		SourcePath:       path,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: legacy})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: id + "-legacy", ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
	}))

	a := New()
	a.CanonicalConversations = true
	ids, err := a.ImportConversation(context.Background(), store, path)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)

	materialized, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
	}, acf.ExtractTextTurns(materialized.Events))
	require.Len(t, materialized.Events, 2, "portable Codex retains only the prompt and final answer")
	for _, event := range materialized.Events {
		require.NotEqual(t, "system", event.Role)
	}
	logEvents, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, logEvents, 2)
	var repaired acf.ConversationPayload
	require.NoError(t, json.Unmarshal(logEvents[1].Payload, &repaired))
	require.Equal(t, acf.ConversationFormatV1, repaired.Format,
		"repair must be a self-contained replacement, not a delta after polluted rows")
}

func TestRepairLegacyNativeCodexProjection_PreservesUnprovenRemoteSuffix(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	raw := []byte(`{"timestamp":"2026-07-18T21:47:30Z","type":"session_meta","payload":{"id":"native-codex"}}
{"timestamp":"2026-07-18T21:47:31Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"private execution harness"}]}}
{"timestamp":"2026-07-18T21:47:32Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}}
{"timestamp":"2026-07-18T21:47:36Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"local answer"}]}}
`)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	legacy := encodeCanonicalLegacyNativeForRepair(raw)
	legacy = append(legacy, acf.ConversationEvent{
		Type: acf.EventTypeTurn, Role: "assistant",
		Content: []acf.ContentBlock{{Type: "text", Text: "remote continuation"}},
	})
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: filepath.Base(path), SourcePath: path,
		CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: legacy})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: id + "-legacy", ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
	}))

	a := New()
	a.CanonicalConversations = true
	ids, repaired, err := a.repairLegacyNativeProjection(t.Context(), store, path)
	require.NoError(t, err)
	require.True(t, repaired)
	require.Equal(t, []string{id}, ids)
	current, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, acf.ExtractTextTurns(current.Events),
		acf.TextTurn{Role: "assistant", Text: "remote continuation"})
}

func TestRepairLegacyNativeCodexProjection_ReplacesComplexLocalOldHead(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	raw := []byte(`{"timestamp":"2026-07-18T21:47:30Z","type":"session_meta","payload":{"id":"native-codex"}}
{"timestamp":"2026-07-18T21:47:31Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"private execution harness"}]}}
{"timestamp":"2026-07-18T21:47:32Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}}
{"timestamp":"2026-07-18T21:47:33Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"working"}]}}
{"timestamp":"2026-07-18T21:47:34Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{}","call_id":"call-1"}}
{"timestamp":"2026-07-18T21:47:36Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"final answer"}]}}
`)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	legacy := encodeCanonicalLegacyNativeForRepair(raw)
	// Model the real upgraded store: unrelated historical prefix plus an old
	// retimestamped/native-wrapper projection that is not an exact raw prefix.
	complex := []acf.ConversationEvent{
		{Type: acf.EventTypeSystemNote, Content: []acf.ContentBlock{{Type: "text", Text: "private execution harness"}}},
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "duplicate title prompt"}}},
	}
	complex = append(complex, legacy...)
	complex[3].NativeExtras = json.RawMessage(`{"old_wrapper":true}`)
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: filepath.Base(path), SourcePath: path,
		CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: complex})
	require.NoError(t, err)
	a := New()
	a.DeviceID = "local-device"
	a.CanonicalConversations = true
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: id + "-legacy", ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
		Provenance: acf.Provenance{DeviceID: a.DeviceID, SourceAgent: a.Name(), AdapterVersion: "0.9.2"},
	}))

	ids, repaired, err := a.repairLegacyNativeProjection(t.Context(), store, path)
	require.NoError(t, err)
	require.True(t, repaired)
	require.Equal(t, []string{id}, ids)
	current, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, current.Events, 3)
	require.Equal(t, complex[1], current.Events[0],
		"unclassified user prefix events must be retained byte-for-byte")
	require.Equal(t, json.RawMessage(`{"old_wrapper":true}`), current.Events[1].NativeExtras,
		"source wrappers that are not proven execution messages must be retained")
	turns := acf.ExtractTextTurns(current.Events)
	require.NotContains(t, turns, acf.TextTurn{Role: "assistant", Text: "working"})
	require.Contains(t, turns, acf.TextTurn{Role: "assistant", Text: "final answer"})
	for _, event := range current.Events {
		for _, block := range event.Content {
			require.NotEqual(t, "private execution harness", block.Text)
		}
	}
}

func TestSanitizeLegacyCodexExecutionEvents_BoundsDuplicateCommentaryText(t *testing.T) {
	base := time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC)
	user := acf.ConversationEvent{Type: acf.EventTypeTurn, Timestamp: base, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "question"}}}
	commentary := acf.ConversationEvent{Type: acf.EventTypeTurn, Timestamp: base.Add(time.Second), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "same text"}}}
	final := acf.ConversationEvent{Type: acf.EventTypeTurn, Timestamp: base.Add(2 * time.Second), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "same text"}}}
	remote := acf.ConversationEvent{Type: acf.EventTypeTurn, Timestamp: base.Add(-time.Second), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "same text"}}}
	legacy := []acf.ConversationEvent{user, commentary, final}
	clean := []acf.ConversationEvent{user, final}
	current := []acf.ConversationEvent{remote, user, commentary, final}

	got, changed := sanitizeLegacyCodexExecutionEvents(current, legacy, clean)
	require.True(t, changed)
	require.Len(t, got, len(current)-1)
	require.Contains(t, got, remote, "a same-text remote prefix must not be deleted")
	require.Contains(t, got, final, "the legitimate same-text final answer must not be deleted")
	require.NotContains(t, got, commentary, "the exact source commentary identity must be removed")
}

func TestImportConversation_LegacyModeDefault(t *testing.T) {
	// Without the CanonicalConversations flag, importing must keep producing
	// the legacy opaque payload — backward compatibility with v0.1.6 — v0.15.x.
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := New() // CanonicalConversations false by default
	jsonlPath := filepath.Join("testdata", "session-tiny.jsonl")
	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	require.Equal(t, "codex.session.jsonl", payload.Format)
	require.NotEmpty(t, payload.Content, "legacy-mode payload must carry the verbatim file bytes")
	require.Empty(t, payload.Events, "legacy-mode payload must not carry structured events")
}

func TestHandlesFormat_AcceptsBoth(t *testing.T) {
	a := New()
	require.True(t, a.HandlesFormat(acf.KindConversation, "codex.session.jsonl"))
	require.True(t, a.HandlesFormat(acf.KindConversation, acf.ConversationFormatV1))
	require.True(t, a.HandlesFormat(acf.KindConversation, acf.ConversationDeltaFormatV1))
	require.False(t, a.HandlesFormat(acf.KindConversation, "claude-code.session.jsonl"))
}
