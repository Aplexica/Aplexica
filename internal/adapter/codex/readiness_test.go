package codex

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionReadyForImport_StreamsUserBeforeFinalAnswer(t *testing.T) {
	completedBase := strings.Join([]string{
		`{"timestamp":"2026-07-18T20:10:54Z","type":"session_meta","payload":{"id":"thread","aplexica_thread_id":"thread"}}`,
		`{"timestamp":"2026-07-18T20:10:55Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"what is capital of Poland"}]}}`,
		`{"timestamp":"2026-07-18T20:10:56Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Warsaw."}]}}`,
	}, "\n") + "\n"

	ready, err := SessionReadyForImport(strings.NewReader(completedBase))
	require.NoError(t, err)
	require.True(t, ready)

	pending := completedBase + strings.Join([]string{
		`{"timestamp":"2026-07-18T20:11:58Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`,
		`{"timestamp":"2026-07-18T20:11:59Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"how many people live in warsaw?"}]}}`,
		`{"timestamp":"2026-07-18T20:12:02Z","type":"response_item","payload":{"type":"web_search_call","call_id":"search-1","action":{"type":"search","query":"Warsaw population"}}}`,
		`{"timestamp":"2026-07-18T20:12:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Checking the official statistics."}]}}`,
	}, "\n") + "\n"

	ready, err = SessionReadyForImport(strings.NewReader(pending))
	require.NoError(t, err)
	require.True(t, ready,
		"a complete newest user turn must be importable while commentary is still in flight")

	complete := pending + strings.Join([]string{
		`{"timestamp":"2026-07-18T20:12:06Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Warsaw has about 1.87 million residents."}]}}`,
		`{"timestamp":"2026-07-18T20:12:07Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2"}}`,
	}, "\n") + "\n"

	ready, err = SessionReadyForImport(strings.NewReader(complete))
	require.NoError(t, err)
	require.True(t, ready)
}

func TestSessionReadyForImport_IgnoresGeneratedPlaceholder(t *testing.T) {
	raw := strings.Join([]string{
		`{"timestamp":"2026-07-18T20:10:54Z","type":"session_meta","payload":{"id":"thread","aplexica_thread_id":"thread"}}`,
		`{"timestamp":"2026-07-18T20:10:55Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}}`,
		`{"timestamp":"2026-07-18T20:10:56Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"No response requested."}]}}`,
	}, "\n") + "\n"

	ready, err := SessionReadyForImport(strings.NewReader(raw))
	require.NoError(t, err)
	require.True(t, ready, "the real user prompt remains portable even when the placeholder is ignored")
}

func TestSessionReadyForImport_UsesCompletePrefixBeforePartialTrailingRow(t *testing.T) {
	raw := codexConvLine("user", "hello") + codexConvLine("assistant", "hi") + `{"type":"response_item"`
	ready, err := SessionReadyForImport(strings.NewReader(raw))
	require.NoError(t, err)
	require.True(t, ready)

	ready, err = SessionReadyForImport(strings.NewReader(`{"type":"response_item"`))
	require.NoError(t, err)
	require.False(t, ready, "a partial row with no complete portable prefix must still defer")
}
