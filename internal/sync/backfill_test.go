package syncd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// capConvBackfill is the core of the per-agent conversation-backfill cap: given
// conversations newest-first (by source agent) and a per-target limit, it
// decides which target agents each conversation may materialize into, so each
// target receives at most `limit` of the most-recent conversations. A negative
// limit means unlimited; a conversation never backfills into its own source.
func TestCapConvBackfill_PerAgentLimitsNewestFirst(t *testing.T) {
	// 5 conversations, all authored by claude-code, newest first.
	sources := []string{"claude-code", "claude-code", "claude-code", "claude-code", "claude-code"}
	targets := []string{"codex", "kilo"}
	limit := func(_ int, a string) int {
		if a == "codex" {
			return 2 // codex: only the 2 most-recent
		}
		return -1 // kilo: unlimited ("all")
	}

	plan := capConvBackfill(sources, targets, limit)
	require.Len(t, plan, 5)

	codex, kilo := 0, 0
	for _, allowed := range plan {
		for _, tg := range allowed {
			switch tg {
			case "codex":
				codex++
			case "kilo":
				kilo++
			}
		}
	}
	require.Equal(t, 2, codex, "codex capped at 2")
	require.Equal(t, 5, kilo, "kilo unlimited")

	// codex appears only in the two NEWEST conversations (index 0,1).
	require.Contains(t, plan[0], "codex")
	require.Contains(t, plan[1], "codex")
	require.NotContains(t, plan[2], "codex")
	require.NotContains(t, plan[4], "codex")
}

// A conversation must never be backfilled into the agent that authored it
// (that materialization is a no-op and shouldn't consume the cap budget).
func TestCapConvBackfill_ExcludesSourceAgent(t *testing.T) {
	// 3 convs: two from claude-code, one from codex. Target both with cap 1.
	sources := []string{"codex", "claude-code", "claude-code"}
	targets := []string{"codex", "claude-code"}
	limit := func(int, string) int { return 1 }

	plan := capConvBackfill(sources, targets, limit)

	// conv[0] is from codex → only claude-code is a valid target.
	require.Equal(t, []string{"claude-code"}, plan[0])
	// claude-code now at its cap (1); conv[1] (from claude-code) → only codex.
	require.Equal(t, []string{"codex"}, plan[1])
	// both targets now capped; conv[2] → nothing.
	require.Empty(t, plan[2])
}

func TestCapConvBackfillWithSourcePolicy_IncludesRemoteSameAgent(t *testing.T) {
	sources := []string{"codex", "codex", "codex"}
	targets := []string{"codex"}

	plan := capConvBackfillWithSourcePolicy(sources, targets, true, func(int, string) int { return 2 })

	require.Equal(t, []string{"codex"}, plan[0])
	require.Equal(t, []string{"codex"}, plan[1])
	require.Empty(t, plan[2])
}
