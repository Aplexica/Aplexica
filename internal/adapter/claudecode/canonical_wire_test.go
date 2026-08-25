package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
	jsonlPath := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, seed, 0o644))

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]
	initialEvents, err := EncodeCanonical(seed)
	require.NoError(t, err)

	appended := seed
	appended = append(appended, []byte("\n{\"type\":\"user\",\"timestamp\":\"2026-05-21T10:00:06.000Z\",\"content\":\"second question\"}")...)
	appended = append(appended, []byte("\n{\"type\":\"assistant\",\"timestamp\":\"2026-05-21T10:00:07.000Z\",\"content\":\"second answer\"}")...)
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

func TestImportConversation_CanonicalModeAppendsFromOverlappingTail(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := New()
	a.CanonicalConversations = true
	jsonlPath := filepath.Join(t.TempDir(), "session.jsonl")
	full := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00.000Z","content":"q1"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01.000Z","content":"a1"}
{"type":"user","timestamp":"2026-05-21T10:00:02.000Z","content":"q2"}
{"type":"assistant","timestamp":"2026-05-21T10:00:03.000Z","content":"a2"}
{"type":"user","timestamp":"2026-05-21T10:00:04.000Z","content":"q3"}
{"type":"assistant","timestamp":"2026-05-21T10:00:05.000Z","content":"a3"}`)
	require.NoError(t, os.WriteFile(jsonlPath, full, 0o644))

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]

	tail := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:02.000Z","content":"q2"}
{"type":"assistant","timestamp":"2026-05-21T10:00:03.000Z","content":"a2"}
{"type":"user","timestamp":"2026-05-21T10:00:04.000Z","content":"q3"}
{"type":"assistant","timestamp":"2026-05-21T10:00:05.000Z","content":"a3"}
{"type":"user","timestamp":"2026-05-21T10:00:06.000Z","content":"q4"}
{"type":"assistant","timestamp":"2026-05-21T10:00:07.000Z","content":"a4"}`)
	require.NoError(t, os.WriteFile(jsonlPath, tail, 0o644))

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
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2"},
		{Role: "assistant", Text: "a2"},
		{Role: "user", Text: "q3"},
		{Role: "assistant", Text: "a3"},
		{Role: "user", Text: "q4"},
		{Role: "assistant", Text: "a4"},
	}, acf.ExtractTextTurns(materialized.Events))
}

func TestImportConversation_CanonicalModeSkipsShorterDivergentSnapshot(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := New()
	a.CanonicalConversations = true
	jsonlPath := filepath.Join(t.TempDir(), "session.jsonl")
	full := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00.000Z","content":"hello"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01.000Z","content":"hi"}
{"type":"user","timestamp":"2026-05-21T10:00:02.000Z","content":"newer question"}
{"type":"assistant","timestamp":"2026-05-21T10:00:03.000Z","content":"newer answer"}`)
	require.NoError(t, os.WriteFile(jsonlPath, full, 0o644))

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]

	stale := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00.000Z","content":"hello"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01.000Z","content":"older divergent answer"}`)
	require.NoError(t, os.WriteFile(jsonlPath, stale, 0o644))

	ids, err = a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)

	logEvents, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, logEvents, 1, "stale shorter snapshots must not append replacement events")
	materialized, ok, err := acf.MaterializedConversationPayload(logEvents)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "hello"},
		{Role: "assistant", Text: "hi"},
		{Role: "user", Text: "newer question"},
		{Role: "assistant", Text: "newer answer"},
	}, acf.ExtractTextTurns(materialized.Events))
}

func TestImportConversation_CanonicalModeSkipsSameLengthDivergentSnapshot(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := New()
	a.CanonicalConversations = true
	jsonlPath := filepath.Join(t.TempDir(), "session.jsonl")
	full := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00.000Z","content":"q1"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01.000Z","content":"a1"}
{"type":"user","timestamp":"2026-05-21T10:00:02.000Z","content":"q2"}
{"type":"assistant","timestamp":"2026-05-21T10:00:03.000Z","content":"a2"}`)
	require.NoError(t, os.WriteFile(jsonlPath, full, 0o644))

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]

	divergent := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00.000Z","content":"q1"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01.000Z","content":"older a1"}
{"type":"user","timestamp":"2026-05-21T10:00:02.000Z","content":"q2"}
{"type":"assistant","timestamp":"2026-05-21T10:00:03.000Z","content":"a2"}`)
	require.NoError(t, os.WriteFile(jsonlPath, divergent, 0o644))

	ids, err = a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)

	logEvents, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, logEvents, 1, "divergent full snapshots must not append replacement events")
}

func TestImportConversation_LegacyModeStillDefault(t *testing.T) {
	// Without the CanonicalConversations flag, importing must keep producing
	// the legacy opaque payload — backward compatibility with v0.1.2 — v0.14.x.
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := New()
	jsonlPath := filepath.Join("testdata", "session-tiny.jsonl")

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[0].Payload, &payload))
	require.Equal(t, "claude-code.session.jsonl", payload.Format)
	require.NotEmpty(t, payload.Content, "legacy-mode payload must carry the verbatim file bytes")
	require.Empty(t, payload.Events, "legacy-mode payload must not carry structured events")
}

func TestHandlesFormat_AcceptsBothConversationFormats(t *testing.T) {
	a := New()
	require.True(t, a.HandlesFormat(acf.KindConversation, "claude-code.session.jsonl"))
	require.True(t, a.HandlesFormat(acf.KindConversation, acf.ConversationFormatV1))
	require.True(t, a.HandlesFormat(acf.KindConversation, acf.ConversationDeltaFormatV1))
	require.False(t, a.HandlesFormat(acf.KindConversation, "something-else"))
}
