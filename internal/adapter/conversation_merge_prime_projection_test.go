package adapter

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Synthetic timestamps model a native prefix whose generated copy re-stamps
// the base turns before adding a continuation.
var (
	tsU1 = time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)  // codex native
	tsA1 = time.Date(2026, 1, 2, 3, 4, 6, 456000000, time.UTC)  // codex native
	tsU2 = time.Date(2026, 1, 2, 3, 4, 10, 789000000, time.UTC) // claude-code native
	tsE1 = time.Date(2026, 1, 2, 3, 4, 7, 234000000, time.UTC)  // re-stamped echo of U1
	tsE2 = time.Date(2026, 1, 2, 3, 4, 8, 567000000, time.UTC)  // re-stamped echo of A1
)

func stampedTurn(ts time.Time, role, text string) acf.ConversationEvent {
	e := convTurn(role, text)
	e.Timestamp = ts
	return e
}

func stamps(evs []acf.ConversationEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Timestamp.UTC().Format(time.RFC3339Nano)+"|"+e.Role)
	}
	return out
}

// TestMergeConversationByThreadRef_PrimesCommittedProjectionNotRawParse proves the ORIGIN-side half of the
// root cause: MergeConversationByThreadRef commits a text-prefix DELTA (so the
// event LOG stays clean, exactly like any peer that chains the live delta) but
// primes the head-bound materialization cache with the adapter's RAW re-stamped
// parse. MaterializedConversationPayloadFromStore — the single call
// syncd.retainedConversationEvent uses to build the lane=retained full-state
// baseline — then serves that poisoned projection.
func TestMergeConversationByThreadRef_PrimesCommittedProjectionNotRawParse(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "019e0000-0000-7000-8000-0000000000aa"

	// Origin state after the two codex turns: log carries the NATIVE stamps.
	seedConversation(t, store, id, []acf.ConversationEvent{
		stampedTurn(tsU1, "user", "What is two plus two?"),
		stampedTurn(tsA1, "assistant", "Four."),
	})

	logClean, ok, err := logProjection(store, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{
		"2026-01-02T03:04:05.123Z|user",
		"2026-01-02T03:04:06.456Z|assistant",
	}, stamps(logClean.Events))

	// The user continues the thread in Claude Code. The claude-code adapter
	// parses its own GENERATED session, whose base rows were written by
	// session_transcode.go with base.Add(index*time.Second) — i.e. re-stamped.
	claudeParse := []acf.ConversationEvent{
		stampedTurn(tsE1, "user", "What is two plus two?"),
		stampedTurn(tsE2, "assistant", "Four."),
		stampedTurn(tsU2, "user", "What is three plus three?"),
	}
	ids, handled, err := MergeConversationByThreadRef(
		context.Background(), store,
		OpaqueParams{DeviceID: "11111111-1111-4111-8111-111111111111", SourceAgent: "claude-code", AdapterVersion: "0.14.2"},
		ThreadRef{ArtifactID: id, BranchID: acf.MainBranch},
		claudeParse, EncodeCanonicalConversationPayload,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, []string{id}, ids)

	// (1) The COMMITTED event is a 1-event delta (fmt=delta.v1 n=1),
	// which is what the live lane ships verbatim.
	head, ok, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, ok)
	headPayload, err := acf.DecodeConversationPayload(head)
	require.NoError(t, err)
	require.Equal(t, acf.ConversationDeltaFormatV1, headPayload.Format)
	require.Len(t, headPayload.Events, 1)
	require.Equal(t, tsU2, headPayload.Events[0].Timestamp.UTC())

	// (2) The LOG projection is CLEAN — the origin's own store is not corrupt,
	// and a receiver that chains the verbatim live delta stays clean too.
	logAfter, ok, err := logProjection(store, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{
		"2026-01-02T03:04:05.123Z|user",
		"2026-01-02T03:04:06.456Z|assistant",
		"2026-01-02T03:04:10.789Z|user",
	}, stamps(logAfter.Events))

	// (3) THE REGRESSION GUARD: the exact call syncd.retainedConversationEvent
	// makes must agree with the log. Before the fix it served the POISONED
	// cache carrying the materializer's re-stamped base.
	retained, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.ConversationFormatV1, retained.Format)
	require.Equal(t, stamps(logAfter.Events), stamps(retained.Events),
		"lane=retained full state must not carry the materializer's re-stamped base")
	t.Logf("LOG      = %v", stamps(logAfter.Events))
	t.Logf("RETAINED = %v", stamps(retained.Events))
}

func logProjection(store *acf.Store, id string) (acf.ConversationPayload, bool, error) {
	evs, err := store.ReadEvents(acf.KindConversation, id)
	if err != nil {
		return acf.ConversationPayload{}, false, err
	}
	return acf.MaterializedConversationPayload(evs)
}
