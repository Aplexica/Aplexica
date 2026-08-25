package adapter

import (
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestCanonicalConversationAppendPayload_NoChangeNeverEchoesHeadBytes pins the
// fix for the 2026-07-27 duplication: "nothing new" must be signalled with a
// nil payload, never with the current head's own bytes.
//
// Quoting the head was a TOCTOU. The caller resolved "unchanged" by comparing
// the returned bytes against a head it re-read later; when a peer's event
// landed in between, the comparison failed and the quoted head was committed as
// a fresh update. A hot conversation's head is a one-turn delta, so committing
// it replayed that turn verbatim -- canonical timestamp included.
func TestCanonicalConversationAppendPayload_NoChangeNeverEchoesHeadBytes(t *testing.T) {
	at := time.Date(2026, 7, 27, 13, 22, 0, 0, time.UTC)
	turn := func(role, text string, off time.Duration) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: at.Add(off),
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	current := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
		turn("user", "how big is Germany?", 19*time.Second),
	}

	t.Run("byte-identical copy", func(t *testing.T) {
		payload, ok, err := canonicalConversationAppendPayload(current, current)
		require.NoError(t, err)
		require.True(t, ok, "an identical copy is a decided outcome")
		require.Nil(t, payload, "must not echo any payload bytes")
	})

	t.Run("re-stamped copy with identical text turns", func(t *testing.T) {
		// What a materializer produces: same turns, device-local timestamps.
		restamped := make([]acf.ConversationEvent, len(current))
		for i, ev := range current {
			ev.Timestamp = ev.Timestamp.Add(8 * time.Second)
			restamped[i] = ev
		}
		payload, ok, err := canonicalConversationAppendPayload(current, restamped)
		require.NoError(t, err)
		require.True(t, ok)
		require.Nil(t, payload)
	})

	t.Run("stale shorter copy", func(t *testing.T) {
		payload, ok, err := canonicalConversationAppendPayload(current, current[:2])
		require.NoError(t, err)
		require.True(t, ok)
		require.Nil(t, payload)
	})

	t.Run("a genuine continuation still yields its tail", func(t *testing.T) {
		extended := append(append([]acf.ConversationEvent(nil), current...),
			turn("assistant", "357,600 km2", 23*time.Second))
		payload, tail, ok, err := canonicalConversationAppendPayloadWithTail(current, extended)
		require.NoError(t, err)
		require.True(t, ok)
		require.NotNil(t, payload)
		require.Len(t, tail, 1)
		require.Equal(t, "357,600 km2", tail[0].Content[0].Text)
	})

	t.Run("an empty current side is never reported as unchanged", func(t *testing.T) {
		// An empty current is trivially a prefix, so this yields a delta of
		// everything. What must never happen is the nil "nothing new" answer,
		// which would silently drop the whole thread.
		payload, ok, err := canonicalConversationAppendPayload(nil, current)
		require.NoError(t, err)
		require.True(t, ok)
		require.NotNil(t, payload)
	})
}

// TestConversationImportWouldRegress_RejectsStaleSupersetOfARepairedHead closes
// the path-keyed door onto the same hole as the hermes one. A native copy taken
// BEFORE a repair is a strict superset of the repaired head, so the prefix test
// reads it as a continuation and lets it re-assert the removed turns
// immediately after the repair commits.
func TestConversationImportWouldRegress_RejectsStaleSupersetOfARepairedHead(t *testing.T) {
	at := time.Date(2026, 7, 27, 14, 25, 0, 0, time.UTC)
	turn := func(role, text string, off time.Duration) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Timestamp: at.Add(off),
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	repaired := []acf.ConversationEvent{
		turn("user", "q1", 0), turn("assistant", "a1", time.Second),
		turn("user", "q2", 2*time.Second), turn("assistant", "a2", 3*time.Second),
	}

	t.Run("stale copy re-asserting the removed block regresses", func(t *testing.T) {
		stale := append(append([]acf.ConversationEvent(nil), repaired...),
			turn("user", "q1", 10*time.Second), turn("assistant", "a1", 11*time.Second))
		require.True(t, conversationImportWouldRegress(repaired, stale))
	})

	t.Run("a genuine continuation does not regress", func(t *testing.T) {
		cont := append(append([]acf.ConversationEvent(nil), repaired...),
			turn("user", "q3", 10*time.Second), turn("assistant", "a3", 11*time.Second))
		require.False(t, conversationImportWouldRegress(repaired, cont))
	})

	t.Run("a replay plus genuinely new content does not regress", func(t *testing.T) {
		mixed := append(append([]acf.ConversationEvent(nil), repaired...),
			turn("user", "q1", 10*time.Second), turn("user", "q3", 11*time.Second))
		require.False(t, conversationImportWouldRegress(repaired, mixed),
			"suppression is deferral; one new turn must release it")
	})

	t.Run("an equal copy does not regress", func(t *testing.T) {
		require.False(t, conversationImportWouldRegress(repaired, repaired))
	})
}
