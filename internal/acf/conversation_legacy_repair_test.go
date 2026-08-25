// SPDX-License-Identifier: AGPL-3.0-or-later
package acf

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func legacyRepairTurn(role, text string, ts time.Time) ConversationEvent {
	return ConversationEvent{
		Type: EventTypeTurn, Role: role, Timestamp: ts,
		Content: []ContentBlock{{Type: "text", Text: text}},
	}
}

func legacyRepairTurns(events []ConversationEvent) []TextTurn {
	return ExtractTextTurns(events)
}

func TestRepairLegacyRetimestampedConversation_PrefersNativeOrderAndTimestamps(t *testing.T) {
	t0 := time.Date(2026, 7, 16, 22, 38, 0, 0, time.UTC)
	native := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0),
		legacyRepairTurn("assistant", "a1", t0.Add(time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(2*time.Second)),
		legacyRepairTurn("assistant", "a2", t0.Add(3*time.Second)),
	}
	generated := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0.Add(10*time.Second)),
		legacyRepairTurn("assistant", "a1", t0.Add(10*time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(10*time.Second)),
		legacyRepairTurn("assistant", "a2", t0.Add(10*time.Second)),
	}

	got, ok := RepairLegacyRetimestampedConversation(native, generated)
	require.True(t, ok)
	require.Equal(t, native, got)
	reverse, ok := RepairLegacyRetimestampedConversation(generated, native)
	require.True(t, ok)
	require.Equal(t, got, reverse, "repair must be commutative")
}

func TestRepairLegacyRetimestampedConversation_RepairsPollutedUnion(t *testing.T) {
	t0 := time.Date(2026, 7, 16, 22, 38, 0, 0, time.UTC)
	synthetic := t0.Add(10 * time.Second)
	clean := []ConversationEvent{
		legacyRepairTurn("user", "q1", synthetic),
		legacyRepairTurn("assistant", "a1", synthetic),
		legacyRepairTurn("user", "q2", synthetic),
		legacyRepairTurn("assistant", "a2", synthetic),
	}
	polluted := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0),
		legacyRepairTurn("assistant", "a2", synthetic),
		legacyRepairTurn("assistant", "a1", synthetic),
		legacyRepairTurn("user", "q1", synthetic),
		legacyRepairTurn("user", "q2", synthetic),
		legacyRepairTurn("assistant", "a1", t0.Add(time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(2*time.Second)),
		legacyRepairTurn("assistant", "No response requested.", t0.Add(3*time.Second)),
	}

	got, ok := RepairLegacyRetimestampedConversation(polluted, clean)
	require.True(t, ok)
	require.Equal(t, []TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2"},
		{Role: "assistant", Text: "a2"},
	}, legacyRepairTurns(got))
	reverse, ok := RepairLegacyRetimestampedConversation(clean, polluted)
	require.True(t, ok)
	require.Equal(t, got, reverse)
}

func TestRepairLegacyRetimestampedConversation_PreservesLegitimateRepeatedPrompt(t *testing.T) {
	t0 := time.Date(2026, 7, 16, 22, 38, 0, 0, time.UTC)
	native := []ConversationEvent{
		legacyRepairTurn("user", "continue", t0),
		legacyRepairTurn("assistant", "first", t0.Add(time.Second)),
		legacyRepairTurn("user", "continue", t0.Add(2*time.Second)),
		legacyRepairTurn("assistant", "second", t0.Add(3*time.Second)),
	}
	generated := []ConversationEvent{
		legacyRepairTurn("user", "continue", t0.Add(10*time.Second)),
		legacyRepairTurn("assistant", "first", t0.Add(10*time.Second)),
		legacyRepairTurn("user", "continue", t0.Add(10*time.Second)),
		legacyRepairTurn("assistant", "second", t0.Add(10*time.Second)),
	}

	got, ok := RepairLegacyRetimestampedConversation(native, generated)
	require.True(t, ok)
	require.Equal(t, native, got)
	require.Len(t, legacyRepairTurns(got), 4)
}

func TestRepairLegacyRetimestampedConversation_PreservesLongerSnapshot(t *testing.T) {
	t0 := time.Date(2026, 7, 16, 22, 38, 0, 0, time.UTC)
	full := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0),
		legacyRepairTurn("assistant", "a1", t0.Add(time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(2*time.Second)),
		legacyRepairTurn("assistant", "a2", t0.Add(3*time.Second)),
	}
	legacyPartial := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0.Add(10*time.Second)),
		legacyRepairTurn("assistant", "a1", t0.Add(10*time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(10*time.Second)),
	}

	got, ok := RepairLegacyRetimestampedConversation(full, legacyPartial)
	require.True(t, ok)
	require.Equal(t, full, got)
}

func TestRepairLegacyRetimestampedConversation_RemovesProvenAssistantEdgeEchoes(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	clean := []ConversationEvent{
		legacyRepairTurn("user", "what is capital of Poland", t0),
		legacyRepairTurn("assistant", "Warsaw.", t0),
		legacyRepairTurn("user", "how many people live in warsaw?", t0),
		legacyRepairTurn("assistant", "About 1.87 million.", t0.Add(time.Minute)),
	}
	polluted := append([]ConversationEvent{
		legacyRepairTurn("assistant", "Warsaw.", t0),
	}, clean...)
	polluted = append(polluted,
		legacyRepairTurn("assistant", "About 1.87 million.", t0.Add(2*time.Minute)))

	got, ok := RepairLegacyRetimestampedConversation(clean, polluted)
	require.True(t, ok)
	require.Equal(t, clean, got)
	reverse, ok := RepairLegacyRetimestampedConversation(polluted, clean)
	require.True(t, ok)
	require.Equal(t, got, reverse, "clean-vs-dirty repair must be commutative")
	repeated, ok := RepairLegacyRetimestampedConversation(got, polluted)
	require.True(t, ok)
	require.Equal(t, clean, repeated, "a retained dirty peer must not recontaminate the clean result")
}

func TestIsLegacyAdjacentAssistantEchoCleanup_RecognizesExactHistoricalShape(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	clean := []ConversationEvent{
		legacyRepairTurn("user", "what is capital of France?", t0),
		legacyRepairTurn("assistant", "Paris.", t0.Add(time.Second)),
		legacyRepairTurn("user", "how many people live in Paris?", t0.Add(2*time.Second)),
		legacyRepairTurn("assistant", "About 2.1 million.", t0.Add(3*time.Second)),
	}
	dirty := []ConversationEvent{
		clean[0],
		legacyRepairTurn("assistant", "Paris.", t0.Add(500*time.Millisecond)),
		clean[1], clean[2], clean[3],
	}

	require.True(t, IsLegacyAdjacentAssistantEchoCleanup(clean, dirty))
	require.False(t, IsLegacyAdjacentAssistantEchoCleanup(dirty, clean))
}

func TestIsLegacyAdjacentAssistantEchoConflictDelta_RecognizesExactFranceShape(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	clean := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0),
		legacyRepairTurn("assistant", "a1", t0.Add(time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(2*time.Second)),
		legacyRepairTurn("assistant", "a2", t0.Add(3*time.Second)),
	}
	dirty := []ConversationEvent{
		clean[0], clean[1], legacyRepairTurn("assistant", "a1", t0.Add(4*time.Second)), clean[2], clean[3],
	}
	delta := []ConversationEvent{
		legacyRepairTurn("assistant", "a2", t0.Add(5*time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(6*time.Second)),
	}

	require.True(t, IsLegacyAdjacentAssistantEchoConflictDelta(clean, dirty, delta))
	require.False(t, IsLegacyAdjacentAssistantEchoConflictDelta(clean, dirty, []ConversationEvent{delta[1], delta[0]}))
	changed := append([]ConversationEvent(nil), delta...)
	changed[0].Content = append(changed[0].Content, ContentBlock{Type: "image", Data: "unique"})
	require.False(t, IsLegacyAdjacentAssistantEchoConflictDelta(clean, dirty, changed))
	polluted := append(append([]ConversationEvent(nil), dirty...), delta...)
	require.True(t, IsLegacyAdjacentAssistantEchoMaterializedConflictCleanup(clean, polluted))
	require.True(t, IsLegacyAdjacentAssistantEchoRepairCleanup(clean, polluted))
	require.True(t, IsLegacyAdjacentAssistantEchoRepairCleanup(clean, dirty))
	require.False(t, IsLegacyAdjacentAssistantEchoMaterializedConflictCleanup(clean, append(polluted, delta[0])))
}

func TestIsLegacyAdjacentAssistantEchoCleanup_PreservesUnprovenRepeatsAndPayload(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	clean := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0),
		legacyRepairTurn("assistant", "a1", t0.Add(time.Second)),
		legacyRepairTurn("user", "q2", t0.Add(2*time.Second)),
		legacyRepairTurn("assistant", "a2", t0.Add(3*time.Second)),
	}
	assistantFirst := append([]ConversationEvent{legacyRepairTurn("assistant", "preface", t0)}, clean...)
	withImage := legacyRepairTurn("assistant", "a1", t0.Add(500*time.Millisecond))
	withImage.Content = append(withImage.Content, ContentBlock{Type: "image", Data: "unique"})
	withExtras := legacyRepairTurn("assistant", "a1", t0.Add(500*time.Millisecond))
	withExtras.NativeExtras = []byte(`{"unique":true}`)

	for _, tc := range []struct {
		name   string
		longer []ConversationEvent
	}{
		{name: "distinct adjacent assistant", longer: []ConversationEvent{
			clean[0], clean[1], legacyRepairTurn("assistant", "a distinct reply", t0.Add(1500*time.Millisecond)), clean[2], clean[3],
		}},
		{name: "same text with image", longer: []ConversationEvent{clean[0], withImage, clean[1], clean[2], clean[3]}},
		{name: "same text with native extras", longer: []ConversationEvent{clean[0], withExtras, clean[1], clean[2], clean[3]}},
		{name: "assistant first", longer: append([]ConversationEvent{assistantFirst[0]}, assistantFirst...)},
		{name: "one completed exchange", longer: []ConversationEvent{
			clean[0], clean[1], legacyRepairTurn("assistant", "a1", t0.Add(2*time.Second)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shorter := clean
			if tc.name == "assistant first" {
				shorter = assistantFirst
			} else if tc.name == "one completed exchange" {
				shorter = clean[:2]
			}
			require.False(t, IsLegacyAdjacentAssistantEchoCleanup(shorter, tc.longer))
		})
	}
}

func TestRepairLegacyRetimestampedConversation_PreservesUnprovenSurplusContent(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	clean := []ConversationEvent{
		legacyRepairTurn("user", "q1", t0),
		legacyRepairTurn("assistant", "a1", t0),
		legacyRepairTurn("user", "q2", t0),
		legacyRepairTurn("assistant", "a2", t0.Add(time.Second)),
	}
	for _, tc := range []struct {
		name   string
		longer []ConversationEvent
	}{
		{
			name: "unique assistant answer",
			longer: append(append([]ConversationEvent(nil), clean...),
				legacyRepairTurn("assistant", "a genuine continuation", t0.Add(2*time.Second))),
		},
		{
			name: "repeated user prompt",
			longer: append(append([]ConversationEvent(nil), clean...),
				legacyRepairTurn("user", "q1", t0.Add(2*time.Second))),
		},
		{
			name: "non-edge non-adjacent assistant",
			longer: []ConversationEvent{
				clean[0], clean[1],
				legacyRepairTurn("assistant", "a2", t0.Add(2*time.Second)),
				clean[2], clean[3],
			},
		},
		{
			name: "same text with unique image block",
			longer: func() []ConversationEvent {
				leading := clean[1]
				leading.Content = append(append([]ContentBlock(nil), leading.Content...),
					ContentBlock{Type: "image", Data: "unique-image"})
				out := append([]ConversationEvent{leading}, clean...)
				return append(out, legacyRepairTurn("assistant", "a2", t0.Add(2*time.Second)))
			}(),
		},
		{
			name: "same text with unique native extras",
			longer: func() []ConversationEvent {
				leading := clean[1]
				leading.NativeExtras = []byte(`{"unique":true}`)
				out := append([]ConversationEvent{leading}, clean...)
				return append(out, legacyRepairTurn("assistant", "a2", t0.Add(2*time.Second)))
			}(),
		},
		{
			name: "only one edge echo",
			longer: append([]ConversationEvent{
				legacyRepairTurn("assistant", "a1", t0),
			}, clean...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, IsLegacyAssistantEchoCleanup(clean, tc.longer),
				"the guarded cleanup must not classify distinct content or user multiplicity as an echo")
		})
	}
}

func TestRepairLegacyRetimestampedConversation_PreservesAssistantFirstThread(t *testing.T) {
	t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	assistantFirst := []ConversationEvent{
		legacyRepairTurn("assistant", "preface", t0),
		legacyRepairTurn("user", "q1", t0),
		legacyRepairTurn("assistant", "a1", t0),
		legacyRepairTurn("user", "q2", t0.Add(time.Second)),
	}
	longer := append([]ConversationEvent{assistantFirst[1]}, assistantFirst...)
	longer = append(longer, assistantFirst[len(assistantFirst)-1])
	require.False(t, IsLegacyAssistantEchoCleanup(assistantFirst, longer),
		"assistant-first conversations are supported and must never satisfy the historical repair proof")
}

func TestRepairLegacyRetimestampedConversation_NormalSnapshotsUseNormalMerge(t *testing.T) {
	t0 := time.Date(2026, 7, 16, 22, 38, 0, 0, time.UTC)
	a := []ConversationEvent{legacyRepairTurn("user", "from A", t0)}
	b := []ConversationEvent{legacyRepairTurn("user", "from B", t0.Add(time.Second))}
	got, ok := RepairLegacyRetimestampedConversation(a, b)
	require.False(t, ok)
	require.Nil(t, got)
}
