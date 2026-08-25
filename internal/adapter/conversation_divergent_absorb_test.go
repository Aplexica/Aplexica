package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func absorbTurn(at time.Time) func(role, text string, off time.Duration) acf.ConversationEvent {
	return func(role, text string, off time.Duration) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: at.Add(off),
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
}

func absorbTurnTexts(events []acf.ConversationEvent) []string {
	turns := acf.ExtractTextTurns(events)
	out := make([]string, 0, len(turns))
	for _, turn := range turns {
		out = append(out, turn.Text)
	}
	return out
}

// The reproduction: a thread started in Claude Code, continued in Codex, then
// continued AGAIN in Claude Code without resuming. Canonical holds the codex
// turns; the native file holds the two later Claude turns; neither side is a
// prefix of the other. Before this, the anti-regression guard refused on every
// pass and those two turns were never learned — the import returned the existing
// artifact id and a nil error, so nothing anywhere recorded that they existed.
func TestConversationDivergentNativeTail_AbsorbsTheNativeOnlyTurns(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	canonical := []acf.ConversationEvent{
		turn("user", "how big is Neptune?", 0),
		turn("assistant", "About four times Earth's diameter.", time.Second),
		turn("user", "what is its temperature?", 2*time.Second),
		turn("assistant", "About -214 C.", 3*time.Second),
		turn("user", "what is the distance to Neptune?", 4*time.Second),
		// The reproduction thread genuinely holds a duplicated question. Every
		// proof here has to stay correct in its presence.
		turn("user", "what is the distance to Neptune?", 5*time.Second),
		turn("assistant", "About 4.3 billion km.", 6*time.Second),
	}
	native := []acf.ConversationEvent{
		turn("user", "how big is Neptune?", 0),
		turn("assistant", "About four times Earth's diameter.", time.Second),
		turn("user", "what is the closest planet to Neptune?", 10*time.Second),
		turn("assistant", "Uranus.", 11*time.Second),
	}

	require.True(t, conversationImportWouldRegress(canonical, native),
		"the anti-regression guard must still refuse this import first")

	tail, ok := conversationDivergentNativeTail(canonical, native)
	require.True(t, ok)
	require.Equal(t, []string{"what is the closest planet to Neptune?", "Uranus."}, absorbTurnTexts(tail))

	absorbed := append(append([]acf.ConversationEvent(nil), canonical...), tail...)
	require.Equal(t, []string{
		"how big is Neptune?",
		"About four times Earth's diameter.",
		"what is its temperature?",
		"About -214 C.",
		"what is the distance to Neptune?",
		"what is the distance to Neptune?",
		"About 4.3 billion km.",
		"what is the closest planet to Neptune?",
		"Uranus.",
	}, absorbTurnTexts(absorbed), "canonical's own events are preserved verbatim and only the absent turns are added")
}

// THE FIXED POINT. Appending the tail contiguously makes the native turns an
// in-order subsequence of canonical, which is the inverse of this route's own
// precondition, so a second pass over the same unchanged file absorbs nothing.
// Without this the same turns would be re-appended on every import forever.
func TestConversationDivergentNativeTail_IsAFixedPoint(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	canonical := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
		turn("user", "codex-q", 2*time.Second), turn("assistant", "codex-a", 3*time.Second),
	}
	native := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
		turn("user", "claude-q", 10*time.Second), turn("assistant", "claude-a", 11*time.Second),
	}

	tail, ok := conversationDivergentNativeTail(canonical, native)
	require.True(t, ok)
	absorbed := append(append([]acf.ConversationEvent(nil), canonical...), tail...)

	secondTail, secondOK := conversationDivergentNativeTail(absorbed, native)
	require.False(t, secondOK, "the same unchanged native file must absorb nothing on a second pass")
	require.Nil(t, secondTail)
	require.Len(t, absorbed, 6, "the artifact must not grow on a repeated import")
}

// The absorb must never re-assert turns a repair deliberately removed, and must
// never append a turn canonical already holds — the reproduction thread holds a
// duplicate, so "already holds" has to be checked turn by turn rather than
// assumed away.
func TestConversationDivergentNativeTail_DeclinesWhatWouldRevertARepair(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	repaired := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
		turn("user", "q2", 2*time.Second), turn("assistant", "a2", 3*time.Second),
	}

	t.Run("a pre-repair copy re-asserting a removed duplicate", func(t *testing.T) {
		stale := []acf.ConversationEvent{
			turn("user", "q1", 0), turn("assistant", "a1", time.Second),
			turn("assistant", "a1", 2*time.Second),
			turn("user", "q2", 3*time.Second), turn("assistant", "a2", 4*time.Second),
		}
		require.True(t, conversationImportWouldRegress(repaired, stale))
		_, ok := conversationDivergentNativeTail(repaired, stale)
		require.False(t, ok, "absorbing this would restore the duplicate the repair removed")
	})

	t.Run("a pre-repair copy that also carries a genuinely new turn", func(t *testing.T) {
		stale := []acf.ConversationEvent{
			turn("user", "q1", 0), turn("assistant", "a1", time.Second),
			turn("assistant", "a1", 2*time.Second),
			turn("user", "q2", 3*time.Second), turn("assistant", "a2", 4*time.Second),
			turn("user", "q3", 5*time.Second),
		}
		_, ok := conversationDivergentNativeTail(repaired, stale)
		require.False(t, ok,
			"one new turn does not license re-appending the removed duplicate alongside it")
	})

	t.Run("two threads that share no first turn", func(t *testing.T) {
		unrelated := []acf.ConversationEvent{
			turn("user", "totally different", 0), turn("assistant", "different too", time.Second),
		}
		_, ok := conversationDivergentNativeTail(repaired, unrelated)
		require.False(t, ok, "agreement on the first turn is what proves this is one conversation")
	})

	t.Run("an empty canonical side", func(t *testing.T) {
		_, ok := conversationDivergentNativeTail(nil, repaired)
		require.False(t, ok)
	})
}

// The turn relations alone cannot separate "the file was continued in its own
// agent since canonical last saw it" from "this is a stale re-read of a snapshot
// the file has since moved past". Both are shorter-and-divergent. Time does
// separate them, and absorbing the stale one would re-assert content the file no
// longer holds.
func TestConversationDivergentNativeTail_DeclinesAStaleOlderSnapshot(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	canonical := []acf.ConversationEvent{
		turn("user", "hello", 0),
		turn("assistant", "hi", time.Second),
		turn("user", "newer question", 2*time.Second),
		turn("assistant", "newer answer", 3*time.Second),
	}

	stale := []acf.ConversationEvent{
		turn("user", "hello", 0),
		turn("assistant", "older divergent answer", time.Second),
	}
	require.True(t, conversationImportWouldRegress(canonical, stale))
	_, ok := conversationDivergentNativeTail(canonical, stale)
	require.False(t, ok, "a turn older than everything canonical holds is a stale snapshot, not a continuation")

	// The same shape with the divergent turn stamped AFTER canonical's newest is
	// the continuation this route exists for, and it is absorbed.
	continued := []acf.ConversationEvent{
		turn("user", "hello", 0),
		turn("assistant", "hi", time.Second),
		turn("user", "asked in this agent later", 30*time.Second),
	}
	tail, ok := conversationDivergentNativeTail(canonical, continued)
	require.True(t, ok)
	require.Equal(t, []string{"asked in this agent later"}, absorbTurnTexts(tail))
}

// An unstamped turn on either side leaves the ordering unprovable, so the absorb
// must decline rather than assume.
func TestConversationDivergentNativeTail_DeclinesUnstampedTurns(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	canonical := []acf.ConversationEvent{
		turn("user", "hello", 0),
		turn("assistant", "hi", time.Second),
		turn("user", "codex question", 2*time.Second),
	}
	unstamped := []acf.ConversationEvent{
		turn("user", "hello", 0),
		turn("assistant", "hi", time.Second),
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "no timestamp"}}},
	}
	_, ok := conversationDivergentNativeTail(canonical, unstamped)
	require.False(t, ok)
}

// The ordinary relations must be byte-identical to what they produce today: the
// absorb runs only after conversationImportWouldRegress has already refused, so
// it may never claim one of these.
func TestConversationDivergentNativeTail_LeavesTheOrdinaryRelationsAlone(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	current := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
	}

	t.Run("native is a continuation", func(t *testing.T) {
		extended := append(append([]acf.ConversationEvent(nil), current...),
			turn("user", "q2", 2*time.Second))
		require.False(t, conversationImportWouldRegress(current, extended))
		_, ok := conversationDivergentNativeTail(current, extended)
		require.False(t, ok)
	})

	t.Run("native equals canonical", func(t *testing.T) {
		_, ok := conversationDivergentNativeTail(current, current)
		require.False(t, ok)
	})

	t.Run("native is behind", func(t *testing.T) {
		_, ok := conversationDivergentNativeTail(current, current[:1])
		require.False(t, ok,
			"a native side that is merely behind must never have its own past appended")
	})
}

// End to end through the real import entry point: canonical learns the native
// turns it was missing, a second import of the same unchanged file commits
// nothing, and both calls still return the artifact id with a nil error.
func TestImportCanonicalConversation_AbsorbsADivergentNativeTail(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	seedConversation(t, store, id, []acf.ConversationEvent{
		turn("user", "how big is Neptune?", 0),
		turn("assistant", "About four times Earth's diameter.", time.Second),
		turn("user", "what is its temperature?", 2*time.Second),
		turn("assistant", "About -214 C.", 3*time.Second),
	})
	native := []acf.ConversationEvent{
		turn("user", "how big is Neptune?", 0),
		turn("assistant", "About four times Earth's diameter.", time.Second),
		turn("user", "what is the closest planet to Neptune?", 10*time.Second),
		turn("assistant", "Uranus.", 11*time.Second),
	}
	path := conversationTestSourcePath(store, id)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("native session bytes\n"), 0o644))
	parse := func([]byte) ([]acf.ConversationEvent, error) { return native, nil }
	params := OpaqueParams{DeviceID: "dev", SourceAgent: "claude-code", AdapterVersion: "test"}

	ids, err := ImportCanonicalConversation(context.Background(), store, params, path, parse)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)
	require.Equal(t, []string{
		"how big is Neptune?",
		"About four times Earth's diameter.",
		"what is its temperature?",
		"About -214 C.",
		"what is the closest planet to Neptune?",
		"Uranus.",
	}, turnTextsOf(threadTurns(t, store, id)))

	before, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	ids, err = ImportCanonicalConversation(context.Background(), store, params, path, parse)
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids)
	after, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, after, len(before), "a repeated import of the same file must commit nothing")
}

// When the divergence cannot be absorbed the refusal keeps its identity-
// preserving no-op contract, but stops being anonymous: it names itself so a
// caller can tell "canonical and this file each hold turns the other lacks"
// apart from an ordinary stale re-read.
func TestCanonicalConversationPayload_NamesAnUnabsorbableDivergence(t *testing.T) {
	turn := absorbTurn(time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC))
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	repaired := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
		turn("user", "q2", 2*time.Second), turn("assistant", "a2", 3*time.Second),
	}
	seedConversation(t, store, id, repaired)
	stale := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
		turn("assistant", "a1", 2*time.Second),
		turn("user", "q2", 3*time.Second), turn("assistant", "a2", 4*time.Second),
	}

	_, err := canonicalConversationPayloadForEvents(
		store, conversationTestSourcePath(store, id), stale, "claude-code")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrConversationImportDiverged)
	require.ErrorIs(t, err, ErrConversationImportNoop,
		"every existing errors.Is caller must be unaffected")
}

func turnTextsOf(turns []acf.TextTurn) []string {
	out := make([]string, 0, len(turns))
	for _, turn := range turns {
		out = append(out, turn.Text)
	}
	return out
}

func TestTextTurnsSubsequence_IsOrderedAndDuplicateAware(t *testing.T) {
	turns := func(texts ...string) []acf.TextTurn {
		out := make([]acf.TextTurn, 0, len(texts))
		for _, text := range texts {
			out = append(out, acf.TextTurn{Role: "user", Text: text})
		}
		return out
	}
	require.True(t, textTurnsSubsequence(turns("a", "c"), turns("a", "b", "c")))
	require.False(t, textTurnsSubsequence(turns("c", "a"), turns("a", "b", "c")))
	require.False(t, textTurnsSubsequence(turns("a", "a"), turns("a", "b")),
		"a duplicate must not be satisfied by a single occurrence")
	require.True(t, textTurnsSubsequence(turns("a", "a"), turns("a", "b", "a")))
}
