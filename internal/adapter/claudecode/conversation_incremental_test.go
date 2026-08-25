package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Importing an append-only session.jsonl repeatedly must (1) store exactly the
// canonical events a full re-parse would produce, and (2) actually exercise the
// incremental cache (parse only the appended tail) rather than re-parsing the
// whole file each time. This is the wiring guard for the idle-CPU fix.
func TestImportConversation_IncrementalAppend_MatchesFullParse(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	a := New()
	a.CanonicalConversations = true

	jsonlPath := filepath.Join(t.TempDir(), "session.jsonl")
	v1 := convLine("user", "one") + convLine("assistant", "two")
	v2 := v1 + convLine("user", "three")
	v3 := v2 + convLine("assistant", "four")

	importNow := func(content string) string {
		require.NoError(t, os.WriteFile(jsonlPath, []byte(content), 0o644))
		ids, err := a.ImportConversation(t.Context(), store, jsonlPath)
		require.NoError(t, err)
		require.Len(t, ids, 1)
		return ids[0]
	}

	id := importNow(v1) // cold: full parse
	importNow(v2)       // append: incremental
	importNow(v3)       // append: incremental

	// The materialized conversation must equal a fresh full parse of v3 even
	// though append updates now store only the delta payload.
	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(events), 1)

	var got acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[len(events)-1].Payload, &got))
	require.Equal(t, acf.ConversationDeltaFormatV1, got.Format)

	wantEvents, _ := encodeCanonicalFrom([]byte(v3), 0)
	materialized, ok, err := acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.ConversationFormatV1, materialized.Format)
	require.Equal(t, wantEvents, materialized.Events,
		"incrementally-built conversation must match a full re-parse of the whole file")

	// The two appends must have gone through the incremental path, proving the
	// cache is wired (the pre-fix code re-parsed from byte 0 every time).
	require.NotNil(t, a.convCache)
	require.GreaterOrEqual(t, a.convCache.incParses, uint64(2),
		"appends should parse only the tail, not re-parse the whole file")
}
