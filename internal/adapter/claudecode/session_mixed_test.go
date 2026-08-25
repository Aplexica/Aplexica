// SPDX-License-Identifier: AGPL-3.0-or-later
package claudecode

import (
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// After Aplexica rewrites the original session file (materialization of a
// peer's turns), a still-open Claude Code session appends its next line with
// its ORIGINAL sessionId and a parentUuid that no longer exists in the file.
// The parser must still surface every turn, and the file must still resolve
// to the canonical thread id (first line wins).
func TestEncodeCanonical_MixedMaterializedAndNativeAppend(t *testing.T) {
	turns := []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "peer-q2"},
		{Role: "assistant", Text: "peer-a2"},
	}
	materialized := transcodeToClaudeSession(turns, "thread-artifact-id", "/Users/x", "claude-code", "f",
		time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))

	nativeAppend := strings.Join([]string{
		`{"parentUuid":"dangling-uuid-from-claudes-memory","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":"local-q3-typed-while-open"}]},"uuid":"native-u3","timestamp":"2026-07-01T12:10:00Z","sessionId":"original-random-session-uuid","cwd":"/Users/x","version":"2.0.0","gitBranch":""}`,
		`{"parentUuid":"native-u3","isSidechain":false,"type":"assistant","message":{"role":"assistant","type":"message","model":"claude-opus-4-8","content":[{"type":"text","text":"local-a3"}],"id":"msg_x","stop_reason":"end_turn"},"uuid":"native-a3","timestamp":"2026-07-01T12:10:05Z","sessionId":"original-random-session-uuid","cwd":"/Users/x","version":"2.0.0","gitBranch":""}`,
		"",
	}, "\n")
	mixed := materialized + nativeAppend

	require.Equal(t, "thread-artifact-id", claudeSessionThreadID([]byte(mixed)),
		"thread identity must remain the canonical artifact id")

	events, err := EncodeCanonical([]byte(mixed))
	require.NoError(t, err, "a dangling parentUuid / mixed sessionId file must still parse")
	got := acf.ExtractTextTurns(events)
	require.Equal(t, append(turns,
		acf.TextTurn{Role: "user", Text: "local-q3-typed-while-open"},
		acf.TextTurn{Role: "assistant", Text: "local-a3"},
	), got, "no turn may be dropped from a mixed-format session file")
}
