package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/stretchr/testify/require"
)

func codexConvLine(role, text string) string {
	payload := fmt.Sprintf(`{"type":"message","role":%q,"content":[{"type":"%s","text":%q}]}`,
		role, map[bool]string{true: "output_text", false: "input_text"}[role == "assistant"], text)
	return fmt.Sprintf(`{"timestamp":"2026-01-01T00:00:00Z","type":"response_item","payload":%s}`+"\n", payload)
}

func TestImportConversation_IncrementalFileTailMatchesFullParse(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	a := New()
	a.CanonicalConversations = true
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	v1 := codexConvLine("user", "one") + codexConvLine("assistant", "two")
	v2 := v1 + codexConvLine("user", "three")
	v3 := v2 + codexConvLine("assistant", "four")

	var id string
	for _, content := range []string{v1, v2, v3} {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		ids, err := a.ImportConversation(t.Context(), store, path)
		require.NoError(t, err)
		require.Len(t, ids, 1)
		id = ids[0]
	}
	require.NotNil(t, a.convCache)
	require.Equal(t, uint64(1), a.convCache.fullParses)
	require.Equal(t, uint64(2), a.convCache.incParses)

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	materialized, ok, err := acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)
	want, err := EncodeCanonical([]byte(v3))
	require.NoError(t, err)
	require.Equal(t, want, materialized.Events)

	var head acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[len(events)-1].Payload, &head))
	require.Equal(t, acf.ConversationDeltaFormatV1, head.Format)
}

func TestConvEncodeCache_FileRewriteForcesFullParse(t *testing.T) {
	c := newConvEncodeCache(4, defaultConvCacheMaxBytes)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	v1 := codexConvLine("user", "one") + codexConvLine("assistant", "two")
	require.NoError(t, os.WriteFile(path, []byte(v1), 0o600))
	_, err := c.encodeFile(path)
	require.NoError(t, err)

	v2 := codexConvLine("user", "different") + codexConvLine("assistant", "reply") + codexConvLine("user", "longer")
	require.NoError(t, os.WriteFile(path, []byte(v2), 0o600))
	got, err := c.encodeFile(path)
	require.NoError(t, err)
	want, err := EncodeCanonical([]byte(v2))
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, uint64(2), c.fullParses)
	require.Zero(t, c.incParses)
}

func TestConvEncodeCache_EvictionNeverDetachesActiveEntry(t *testing.T) {
	c := newConvEncodeCache(1, defaultConvCacheMaxBytes)
	active := c.entry("active")
	other := c.entry("other")

	c.releaseEntry(other)
	require.Same(t, active, c.m["active"],
		"an entry already handed to a parser must remain map-owned until release")
	require.NotContains(t, c.m, "other")

	c.releaseEntry(active)
	require.Len(t, c.m, 1)
}

func TestConvEncodeCache_RawRolloutSizeDoesNotEvictSmallPortableProjection(t *testing.T) {
	const cacheBudget = int64(64 << 10)
	c := newConvEncodeCache(4, cacheBudget)
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "rollout-one.jsonl"),
		filepath.Join(dir, "rollout-two.jsonl"),
	}
	largeFilteredHarness := strings.Repeat("private execution policy ", 1<<14)
	initial := make([]string, len(paths))
	for i, path := range paths {
		initial[i] = strings.Join([]string{
			fmt.Sprintf(`{"timestamp":"2026-07-19T10:00:00Z","type":"session_meta","payload":{"id":"native-%d"}}`, i),
			fmt.Sprintf(`{"timestamp":"2026-07-19T10:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":%q}]}}`, largeFilteredHarness),
			codexConvLine("user", fmt.Sprintf("portable prompt %d", i)),
		}, "\n")
		require.NoError(t, os.WriteFile(path, []byte(initial[i]), 0o600))
		got, err := c.encodeFile(path)
		require.NoError(t, err)
		require.Len(t, got, 1)
	}

	rawBytes := int64(len(initial[0]) + len(initial[1]))
	require.Greater(t, rawBytes, cacheBudget,
		"the regression requires raw rollouts that cannot both fit under the old accounting")
	require.Less(t, c.totalBytes, cacheBudget,
		"only the small portable projections and parser state should count against the cache")
	require.Contains(t, c.m, paths[0])
	require.Contains(t, c.m, paths[1])
	require.Equal(t, uint64(2), c.fullParses)
	require.Equal(t, uint64(rawBytes), c.fullBytes)

	var appendedBytes uint64
	for i, path := range paths {
		appendText := codexConvLine("assistant", fmt.Sprintf("portable answer %d", i))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		require.NoError(t, err)
		_, err = f.WriteString(appendText)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		appendedBytes += uint64(len(appendText))

		got, err := c.encodeFile(path)
		require.NoError(t, err)
		require.Len(t, got, 2)
	}
	require.Equal(t, uint64(2), c.fullParses,
		"alternating hot rollouts must not be reconstructed from byte zero")
	require.Equal(t, uint64(2), c.incParses)
	require.Equal(t, appendedBytes, c.incBytes,
		"the warm calls must read exactly the newly appended bytes")
}

func TestConvEncodeCache_IncrementalSyntheticReplyStaysFiltered(t *testing.T) {
	c := newConvEncodeCache(4, defaultConvCacheMaxBytes)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	generated := `{"timestamp":"2026-07-16T22:40:15Z","type":"session_meta","payload":{"cli_version":"0.0.0-aplexica"}}` + "\n" +
		codexConvLine("user", "how many planets in solar system?")
	require.NoError(t, os.WriteFile(path, []byte(generated), 0o600))
	got, err := c.encodeFile(path)
	require.NoError(t, err)
	require.Len(t, got, 1)

	generated += codexConvLine("assistant", "No response requested.")
	require.NoError(t, os.WriteFile(path, []byte(generated), 0o600))
	got, err = c.encodeFile(path)
	require.NoError(t, err)
	require.Len(t, got, 1, "incremental parsing must retain generated-session context")
	require.Equal(t, uint64(1), c.fullParses)
	require.Equal(t, uint64(1), c.incParses)
}

func TestConvEncodeCache_OversizedUnterminatedRowKeepsBoundedPending(t *testing.T) {
	c := newConvEncodeCache(4, defaultConvCacheMaxBytes)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	prefix := `{"timestamp":"2026-07-19T10:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"`
	first := prefix + strings.Repeat("x", convPendingMaxBytes+1024)
	require.NoError(t, os.WriteFile(path, []byte(first), 0o600))
	got, err := c.encodeFile(path)
	require.NoError(t, err)
	require.Empty(t, got)
	e := c.m[path]
	require.NotNil(t, e)
	require.True(t, e.pendingOversized)
	require.Len(t, e.pending, convPendingMaxBytes)

	middle := strings.Repeat("y", convPendingMaxBytes+2048)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.WriteString(middle)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	got, err = c.encodeFile(path)
	require.NoError(t, err)
	require.Empty(t, got)
	require.True(t, e.pendingOversized)
	require.Len(t, e.pending, convPendingMaxBytes,
		"an active unterminated row must not grow retained memory")
	require.Equal(t, uint64(1), c.fullParses)
	require.Equal(t, uint64(1), c.incParses)

	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`"}]}}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	got, err = c.encodeFile(path)
	require.NoError(t, err)
	require.Empty(t, got, "completed commentary remains nonportable")
	require.False(t, e.pendingOversized)
	require.Equal(t, []byte("\n"), e.pending,
		"only the decoder's trailing row separator may remain pending")
	require.Equal(t, uint64(1), c.fullParses,
		"completing an oversized row must not force a whole-rollout reconstruction")
	require.Equal(t, uint64(2), c.incParses)
	require.Empty(t, e.legacyEvents,
		"ordinary imports must not build and retain the legacy projection used only by one-time repair")
	require.False(t, e.legacyKnown)
}

func TestConvEncodeCache_OversizedCompletedAtEOFWithoutNewlineIsParsed(t *testing.T) {
	c := newConvEncodeCache(4, defaultConvCacheMaxBytes)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	prefix := `{"timestamp":"2026-07-19T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`
	first := prefix + strings.Repeat("x", convPendingMaxBytes+1024)
	require.NoError(t, os.WriteFile(path, []byte(first), 0o600))
	got, err := c.encodeFile(path)
	require.NoError(t, err)
	require.Empty(t, got)
	e := c.m[path]
	require.NotNil(t, e)
	require.True(t, e.pendingOversized)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`"}]}}`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	got, err = c.encodeFile(path)
	require.NoError(t, err)
	require.Len(t, got, 1,
		"a complete JSON row at EOF must not wait forever for a newline")
	require.Equal(t, "user", got[0].Role)
	require.Equal(t, strings.Repeat("x", convPendingMaxBytes+1024), got[0].Content[0].Text)
	require.False(t, e.pendingOversized)
	require.Empty(t, e.pending)
	require.Equal(t, uint64(1), c.fullParses)
	require.Equal(t, uint64(1), c.incParses)
}

func TestRepairLegacyNativeProjection_OldCleanHeadDoesNotReadSource(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	// A directory is an intentional read-failure sentinel. Version drift used
	// to reach os.ReadFile here; the residue preflight must now return first.
	source := t.TempDir()
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: filepath.Base(source), SourcePath: source,
		CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "question"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "answer"}}},
		},
	})
	require.NoError(t, err)
	a := New()
	a.DeviceID = "local-device"
	a.CanonicalConversations = true
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
		Provenance: acf.Provenance{
			DeviceID: a.DeviceID, SourceAgent: a.Name(), AdapterVersion: "0.9.2",
		},
	}))

	ids, repaired, err := a.repairLegacyNativeProjection(t.Context(), store, source)
	require.NoError(t, err)
	require.False(t, repaired)
	require.Nil(t, ids)
	require.Nil(t, a.convCache, "clean old-version heads must not initialize or read the source cache")
}

func TestRepairLegacyNativeProjection_ToolOnlyResidueIsCleanedOnce(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	raw := codexConvLine("user", "question") +
		`{"timestamp":"2026-07-19T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{}","call_id":"call-1"}}` + "\n" +
		`{"timestamp":"2026-07-19T10:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"private output"}}` + "\n" +
		codexConvLine("assistant", "answer")
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
	legacy := encodeCanonicalLegacyNativeForRepair([]byte(raw))
	require.Len(t, legacy, 4)

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
	a := New()
	a.DeviceID = "local-device"
	a.CanonicalConversations = true
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
		Provenance: acf.Provenance{
			DeviceID: a.DeviceID, SourceAgent: a.Name(), AdapterVersion: "0.9.2",
		},
	}))

	ids, err := a.ImportConversation(t.Context(), store, path)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)
	current, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "question"}, {Role: "assistant", Text: "answer"}},
		acf.ExtractTextTurns(current.Events))
	require.Len(t, current.Events, 2)
	for _, event := range current.Events {
		require.Equal(t, acf.EventTypeTurn, event.Type)
	}

	ids, repaired, err := a.repairLegacyNativeProjection(t.Context(), store, path)
	require.NoError(t, err)
	require.False(t, repaired, "a cleaned tool-only head must not loop through migration")
	require.Nil(t, ids)
}

func TestImportConversation_GrowingLargeLegacySourceReadsFullPrefixOnce(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	path := filepath.Join(t.TempDir(), "rollout-large.jsonl")
	largeHarness := strings.Repeat("private execution policy ", 1<<16)
	initial := strings.Join([]string{
		`{"timestamp":"2026-07-19T10:00:00Z","type":"session_meta","payload":{"id":"native-large"}}`,
		fmt.Sprintf(`{"timestamp":"2026-07-19T10:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":%q}]}}`, largeHarness),
		`{"timestamp":"2026-07-19T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"what is capital of France?"}]}}`,
		`{"timestamp":"2026-07-19T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Searching private context."}]}}`,
		`{"timestamp":"2026-07-19T10:00:04Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{}","call_id":"call-1"}}`,
		`{"timestamp":"2026-07-19T10:00:05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"private tool output"}}`,
		`{"timestamp":"2026-07-19T10:00:06Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Paris."}]}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))

	legacy := encodeCanonicalLegacyNativeForRepair([]byte(initial))
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
	a := New()
	a.DeviceID = "local-device"
	a.CanonicalConversations = true
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
		Provenance: acf.Provenance{
			DeviceID: a.DeviceID, SourceAgent: a.Name(), AdapterVersion: "0.9.2",
		},
	}))

	appended := codexConvLine("user", "how many people live in Paris?") +
		codexConvLine("assistant", "About 2.1 million.")
	cache := a.conversationCache()
	var appendErr error
	cache.afterSnapshotRead = func(gotPath string, _ int64) {
		cache.afterSnapshotRead = nil
		var f *os.File
		f, appendErr = os.OpenFile(gotPath, os.O_WRONLY|os.O_APPEND, 0)
		if appendErr != nil {
			return
		}
		_, appendErr = f.WriteString(appended)
		if closeErr := f.Close(); appendErr == nil {
			appendErr = closeErr
		}
	}

	ids, err := a.ImportConversation(t.Context(), store, path)
	require.NoError(t, err)
	require.NoError(t, appendErr)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, uint64(1), cache.fullParses)
	require.Zero(t, cache.incParses)
	require.Equal(t, uint64(len(initial)), cache.fullBytes,
		"the cold snapshot must stop at its initial descriptor size even when the file grows")

	ids, err = a.ImportConversation(t.Context(), store, path)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, uint64(1), cache.fullParses, "the growing source must never be reconstructed in full again")
	require.Equal(t, uint64(1), cache.incParses)
	require.Equal(t, uint64(len(appended)), cache.incBytes, "only the appended tail should be read")

	current, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in Paris?"},
		{Role: "assistant", Text: "About 2.1 million."},
	}, acf.ExtractTextTurns(current.Events), "prompt and final answer must not be starved by legacy repair")

	portable := hermes.DecodePortableBundleFromCanonical(id, a.Name(), current.Events)
	require.Len(t, portable.Messages, 4)
	for _, message := range portable.Messages {
		require.Contains(t, []string{"user", "assistant"}, message.Role)
		require.NotNil(t, message.Content)
		require.NotContains(t, *message.Content, "private")
		require.Nil(t, message.ToolCalls, "Codex tool traffic must not enter the Hermes portable projection")
		require.Nil(t, message.ToolCallID)
	}
}
