// SPDX-License-Identifier: AGPL-3.0-or-later
package claudecode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A conversational row Claude Code's own resume cannot reach must never surface.
// The only shape that makes a row unreachable inside ONE session file is a fork:
// two children of the same node, so no single chain can hold both.
func TestResumableTextTurnsRejectsUnreachableConversationalRows(t *testing.T) {
	raw := []byte(
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s","message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"assistant","uuid":"unreachable","parentUuid":"u1","sessionId":"s","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"must-not-appear"}]}}` + "\n" +
			`{"type":"last-prompt","leafUuid":"a1","sessionId":"s"}` + "\n")
	_, err := ResumableTextTurns(raw)
	require.Error(t, err)
}

// The shape this fixture used to carry — a row BELOW the recorded leaf, on one
// unbranched chain — is not unreachable at all. Claude Code writes the
// last-prompt row when a prompt is submitted and appends what follows after it,
// so the recorded leaf is routinely a strict ancestor of the real tip. Refusing
// the whole projection there discarded every turn written since, and the write
// path then parented its canonical suffix at that stale leaf, which is how a
// real fork would have been manufactured.
func TestResumableTextTurnsResolvesRowsBelowAStaleLeaf(t *testing.T) {
	raw := []byte(
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s","message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"user","uuid":"below","parentUuid":"a1","sessionId":"s","message":{"role":"user","content":"q2"}}` + "\n" +
			`{"type":"last-prompt","leafUuid":"a1","sessionId":"s"}` + "\n")
	turns, err := ResumableTextTurns(raw)
	require.NoError(t, err)
	require.Len(t, turns, 3)
	require.Equal(t, "q2", turns[2].Text)
}

func TestResumableTextTurnsReturnsCompleteSelectedChain(t *testing.T) {
	raw := []byte(
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s","message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"last-prompt","leafUuid":"a1","sessionId":"s"}` + "\n")
	turns, err := ResumableTextTurns(raw)
	require.NoError(t, err)
	require.Equal(t, "q1", turns[0].Text)
	require.Equal(t, "a1", turns[1].Text)
}

func TestResumableTextTurnsTreatsSplitThinkingAsParentBridge(t *testing.T) {
	raw := []byte(
		`{"type":"attachment","uuid":"before","parentUuid":null,"sessionId":"s"}` + "\n" +
			`{"type":"user","uuid":"u1","parentUuid":"before","sessionId":"s","message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"attachment","uuid":"between","parentUuid":"u1","sessionId":"s"}` + "\n" +
			`{"type":"assistant","uuid":"thinking","parentUuid":"between","sessionId":"s","message":{"id":"m1","role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"","signature":"sig"}]}}` + "\n" +
			`{"type":"assistant","uuid":"a1","parentUuid":"thinking","sessionId":"s","message":{"id":"m1","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"last-prompt","leafUuid":"a1","sessionId":"s"}` + "\n")
	turns, err := ResumableTextTurns(raw)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, "q1", turns[0].Text)
	require.Equal(t, "a1", turns[1].Text)
}

func TestResumableTextTurnsIgnoresTrailingMetadataWithoutLastPrompt(t *testing.T) {
	raw := []byte(
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"s","message":{"role":"user","content":"q1"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s","message":{"role":"assistant","content":[{"type":"text","text":"a1"}]}}` + "\n" +
			`{"type":"attachment","uuid":"trailing","parentUuid":"a1","sessionId":"s","content":"must stay hidden"}` + "\n")
	turns, err := ResumableTextTurns(raw)
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, "q1", turns[0].Text)
	require.Equal(t, "a1", turns[1].Text)
}
