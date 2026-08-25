package openclaw

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestEncodeCanonical_BasicConversation(t *testing.T) {
	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00Z","content":"hi"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01Z","content":"hello"}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(events), 2, "user + assistant turns should be present")
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "hi", events[0].Content[0].Text)
	require.Equal(t, acf.EventTypeTurn, events[1].Type)
	require.Equal(t, "assistant", events[1].Role)
}

func TestDecodeCanonical_RoundTrip(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "x"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "y"}}},
	}
	jsonl, err := DecodeCanonical(events)
	require.NoError(t, err)
	got, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "user", got[0].Role)
	require.Equal(t, "assistant", got[1].Role)
}

func TestImportConversation_CanonicalMode(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00Z","content":"openclaw turn"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01Z","content":"reply"}
`)
	jsonlPath := filepath.Join(tmp, "session.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, jsonl, 0o644))

	a := New()
	a.HomeDir = tmp
	a.CanonicalConversations = true

	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	var p acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[0].Payload, &p))
	require.Equal(t, acf.ConversationFormatV1, p.Format)
	require.NotEmpty(t, p.Events)
	require.Empty(t, p.Content, "structured-mode payload must not carry the legacy Content field")
}

func TestImportConversation_LegacyModeDefault(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	jsonl := []byte(`{"type":"user","content":"x"}` + "\n")
	jsonlPath := filepath.Join(tmp, "session.jsonl")
	require.NoError(t, os.WriteFile(jsonlPath, jsonl, 0o644))

	a := New() // CanonicalConversations false
	a.HomeDir = tmp
	ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	var p acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[0].Payload, &p))
	require.Equal(t, SessionJSONLFormat, p.Format)
	require.Empty(t, p.Events, "legacy-mode payload must not carry structured events")
	require.NotEmpty(t, p.Content, "legacy-mode payload must carry the verbatim file bytes")
}

func TestHandlesFormat_Conversation_AcceptsBoth(t *testing.T) {
	a := New()
	require.True(t, a.HandlesFormat(acf.KindConversation, SessionJSONLFormat))
	require.True(t, a.HandlesFormat(acf.KindConversation, acf.ConversationFormatV1))
	require.True(t, a.HandlesFormat(acf.KindConversation, acf.ConversationDeltaFormatV1))
	require.False(t, a.HandlesFormat(acf.KindConversation, "claude-code.session.jsonl"))
}

// TestCrossAgent_ClaudeCodeJSONLToOpenClawJSONL proves the full canonical
// loop: import a Claude Code session.jsonl in canonical mode, then export
// the same artifact via openclaw — both turns must survive the
// claudecode-encode → acf.conversation.v1 store → openclaw-decode pipeline.
func TestCrossAgent_ClaudeCodeJSONLToOpenClawJSONL(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	// Import via claudecode in canonical mode.
	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, cc.SecretsStore.Init())
	cc.CanonicalConversations = true

	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00Z","content":"shared turn"}
{"type":"assistant","timestamp":"2026-05-21T10:00:01Z","content":"shared reply"}
`)
	claudePath := filepath.Join(tmp, "claude.jsonl")
	require.NoError(t, os.WriteFile(claudePath, jsonl, 0o644))

	ids, err := cc.ImportConversation(t.Context(), store, claudePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Export via openclaw (which decodes the canonical payload).
	oc := New()
	oc.HomeDir = tmp
	oc.SecretsStore = cc.SecretsStore

	outPath := filepath.Join(tmp, "openclaw-out.jsonl")
	require.NoError(t, oc.ExportConversation(t.Context(), store, ids[0], outPath))

	out, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.Contains(t, string(out), "shared turn")
	require.Contains(t, string(out), "shared reply")
}
