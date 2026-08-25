package syncd

import (
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func turnEv(role, text string, ts time.Time) acf.ConversationEvent {
	return acf.ConversationEvent{
		Type: acf.EventTypeTurn, Role: role, Timestamp: ts,
		Content: []acf.ContentBlock{{Type: "text", Text: text}},
	}
}

func TestClassifyConversationEvents(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	base := []acf.ConversationEvent{turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second))}
	longer := append(append([]acf.ConversationEvent{}, base...), turnEv("user", "q2", t0.Add(2*time.Second)))
	divergedA := append(append([]acf.ConversationEvent{}, base...), turnEv("user", "q2-from-A", t0.Add(2*time.Second)))
	divergedB := append(append([]acf.ConversationEvent{}, base...), turnEv("user", "q2-from-B", t0.Add(3*time.Second)))

	require.Equal(t, convEqual, classifyConversationEvents(base, base))
	require.Equal(t, convInboundStale, classifyConversationEvents(longer, base))
	require.Equal(t, convInboundExtends, classifyConversationEvents(base, longer))
	require.Equal(t, convDiverged, classifyConversationEvents(divergedA, divergedB))
}

func TestUnionConversationEvents_DeterministicAndCommutative(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	base := []acf.ConversationEvent{turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second))}
	a := append(append([]acf.ConversationEvent{}, base...), turnEv("user", "from A", t0.Add(2*time.Second)))
	b := append(append([]acf.ConversationEvent{}, base...), turnEv("user", "from B", t0.Add(3*time.Second)))

	ab := unionConversationEvents(a, b)
	ba := unionConversationEvents(b, a)
	require.Equal(t, ab, ba, "union must be commutative — both devices must compute the identical merge")
	require.Len(t, ab, 4)
	require.Equal(t, "from A", ab[2].Content[0].Text, "chronological order")
	require.Equal(t, "from B", ab[3].Content[0].Text)

	// Idempotent: merging the merge with either input adds nothing.
	require.Equal(t, ab, unionConversationEvents(ab, b))
	require.Equal(t, convEqual, classifyConversationEvents(ab, unionConversationEvents(ab, a)))
}

func TestUnionConversationEvents_EqualTimestampsOrderByKey(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	a := []acf.ConversationEvent{turnEv("user", "zzz", t0)}
	b := []acf.ConversationEvent{turnEv("user", "aaa", t0)}
	require.Equal(t, unionConversationEvents(a, b), unionConversationEvents(b, a),
		"equal-timestamp events must still order deterministically")
}

func TestUnionConversationAttachments_DeterministicCommutativeAndLossless(t *testing.T) {
	a := acf.Attachment{
		Kind: "image", MimeType: "image/png", ContentHash: "a-hash", Bytes: 10, Filename: "a.png",
	}
	b := acf.Attachment{
		Kind: "file", MimeType: "text/plain", ContentHash: "b-hash", Bytes: 20, Filename: "b.txt",
	}
	ab := unionConversationAttachments([]acf.Attachment{a, a}, []acf.Attachment{b})
	ba := unionConversationAttachments([]acf.Attachment{b}, []acf.Attachment{a, a})
	require.Equal(t, ab, ba)
	require.Len(t, ab, 3, "multiset union must preserve repeated attachment references")
	require.Equal(t, ab, unionConversationAttachments(ab, []acf.Attachment{b}),
		"attachment union must be idempotent")
	require.Equal(t,
		conversationAttachmentKeys([]acf.Attachment{a, a, b}),
		conversationAttachmentKeys(ab),
	)
}

func TestUnionConversationEvents_LegacyRetimestampedCopyDoesNotDuplicateOrReorder(t *testing.T) {
	t0 := time.Date(2026, 7, 16, 22, 38, 0, 0, time.UTC)
	native := []acf.ConversationEvent{
		turnEv("user", "q1", t0),
		turnEv("assistant", "a1", t0.Add(time.Second)),
		turnEv("user", "q2", t0.Add(2*time.Second)),
		turnEv("assistant", "a2", t0.Add(3*time.Second)),
	}
	legacyTS := t0.Add(10 * time.Second)
	legacy := []acf.ConversationEvent{
		turnEv("user", "q1", legacyTS),
		turnEv("assistant", "a1", legacyTS),
		turnEv("user", "q2", legacyTS),
		turnEv("assistant", "a2", legacyTS),
	}

	got := unionConversationEvents(native, legacy)
	require.Equal(t, native, got)
	require.Equal(t, got, unionConversationEvents(legacy, native))
}

func TestUnionConversationEvents_LegacyAssistantEdgeEchoConvergesClean(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	clean := []acf.ConversationEvent{
		turnEv("user", "what is capital of Poland", t0),
		turnEv("assistant", "Warsaw.", t0),
		turnEv("user", "how many people live in warsaw?", t0),
		turnEv("assistant", "About 1.87 million.", t0.Add(time.Minute)),
	}
	dirty := append([]acf.ConversationEvent{turnEv("assistant", "Warsaw.", t0)}, clean...)
	dirty = append(dirty, turnEv("assistant", "About 1.87 million.", t0.Add(2*time.Minute)))

	cleanDirty := unionConversationEvents(clean, dirty)
	dirtyClean := unionConversationEvents(dirty, clean)
	require.Equal(t, clean, cleanDirty)
	require.Equal(t, cleanDirty, dirtyClean, "peer argument order must not change the repair")
	require.Equal(t, clean, unionConversationEvents(cleanDirty, dirty),
		"a retained dirty head must not recontaminate a repaired peer")
}

// TestInboundOnlyReplaysLocalTurns pins the convInboundExtends re-infection
// guard. A repaired device must not adopt a still-duplicated peer head just
// because the duplicates make it strictly longer.
func TestInboundOnlyReplaysLocalTurns(t *testing.T) {
	at := func(role, text string, sec int) acf.ConversationEvent {
		return acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Role:      role,
			Timestamp: time.Date(2026, 7, 27, 2, 40, sec, 0, time.UTC),
			Content:   []acf.ContentBlock{{Type: "text", Text: text}},
		}
	}
	u1, a1 := at("user", "q1", 1), at("assistant", "a1", 2)
	u2, a2 := at("user", "q2", 3), at("assistant", "a2", 4)
	u3, a3 := at("user", "q3", 5), at("assistant", "a3", 6)
	clean := []acf.ConversationEvent{u1, a1, u2, a2}

	t.Run("trailing duplicate block is a replay", func(t *testing.T) {
		dirty := []acf.ConversationEvent{u1, a1, u2, a2, u2, a2}
		require.Equal(t, convInboundExtends, classifyConversationEvents(clean, dirty),
			"precondition: the prefix classifier still calls this a fast-forward")
		require.True(t, inboundOnlyReplaysLocalTurns(clean, dirty))
		// The union arm this now routes to is what actually heals it.
		require.Equal(t, clean, unionConversationEvents(clean, dirty))
	})

	t.Run("a genuine continuation is never a replay", func(t *testing.T) {
		extended := []acf.ConversationEvent{u1, a1, u2, a2, u3, a3}
		require.Equal(t, convInboundExtends, classifyConversationEvents(clean, extended))
		require.False(t, inboundOnlyReplaysLocalTurns(clean, extended))
	})

	t.Run("a duplicate mixed with new content is not a replay", func(t *testing.T) {
		mixed := []acf.ConversationEvent{u1, a1, u2, a2, u2, a2, u3, a3}
		require.False(t, inboundOnlyReplaysLocalTurns(clean, mixed),
			"new turns are present, so the payload must still fast-forward normally")
	})

	t.Run("equal-length and shorter inbound are out of scope", func(t *testing.T) {
		require.False(t, inboundOnlyReplaysLocalTurns(clean, clean))
		require.False(t, inboundOnlyReplaysLocalTurns(clean, clean[:2]))
	})

	t.Run("a repeated text turn with a distinct timestamp is real content", func(t *testing.T) {
		retyped := []acf.ConversationEvent{u1, a1, u2, a2, at("user", "q2", 30)}
		require.False(t, inboundOnlyReplaysLocalTurns(clean, retyped),
			"identity includes the timestamp, so a genuinely re-sent prompt survives")
	})

	t.Run("an unseen non-turn row is real content", func(t *testing.T) {
		withTool := append(append([]acf.ConversationEvent(nil), clean...),
			acf.ConversationEvent{
				Type: acf.EventTypeToolCall, Timestamp: u2.Timestamp,
				CallID: "call_1", ToolName: "Read", Input: []byte(`{"p":"a"}`),
			})
		require.False(t, inboundOnlyReplaysLocalTurns(clean, withTool))
	})
}
