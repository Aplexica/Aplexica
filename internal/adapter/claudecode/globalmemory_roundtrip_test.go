package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// TestComposeStripInverse_Bodies asserts the loop-safety invariant the whole
// design rests on: Strip(Compose(body, bodies), bodies) == body. If this holds,
// re-importing an exported CLAUDE.md reproduces the pristine body, so the
// skip-if-equal guard fires and no fan-out loop can form.
func TestComposeStripInverse_Bodies(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		bodies []string
	}{
		// Bases mirror real CLAUDE.md files (always read from disk, so they
		// carry their own trailing newline). Compose normalizes a missing
		// trailing newline, so the inverse holds for newline-terminated bases —
		// which is exactly what importGlobalMemory/importProjectMemory feed it.
		{"empty base, one body", "", []string{"A single project fact."}},
		{"pristine base, two bodies", claudeBody, []string{"Fact one.", "Fact two."}},
		{"no bodies (no-op)", claudeBody, nil},
		{"newline-terminated base", "# Memory\n- x\n", []string{"appended line"}},
		{"multi-line body", claudeBody, []string{"Line one.\nLine two."}},
		{"body already present in base", claudeBody, []string{"user name is Example User"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			composed := adapter.ComposeAppendedMemory(tc.base, tc.bodies)
			roundtrip := adapter.StripAppendedMemory(composed, tc.bodies)
			require.Equal(t, tc.base, roundtrip,
				"Strip(Compose(base, bodies), bodies) must reproduce base byte-for-byte")
		})
	}
}

// TestGlobalRoundTrip_LoopSafe drives the global surface end-to-end: import the
// composed global memory, export it back to ~/.claude/CLAUDE.md (which strips
// the type:user bodies), then re-import — the re-imported content must equal the
// first import's content, proving the steady state is a fixed point.
func TestGlobalRoundTrip_LoopSafe(t *testing.T) {
	a, s, claudePath := seedClaudeAutoMemory(t)
	ctx := context.Background()

	ids1, err := a.Import(ctx, s, filepath.Join(homeMemDir(a), "dogs.md"))
	require.NoError(t, err)
	content1 := replayMemoryForTest(t, s, ids1[0])

	// Export strips the type:user bodies → CLAUDE.md must be byte-pristine.
	require.NoError(t, a.ExportMemory(ctx, s, ids1[0], claudePath))
	after, err := os.ReadFile(claudePath)
	require.NoError(t, err)
	require.Equal(t, claudeBody, string(after))

	// Re-import composes the same view → identical content, same artifact id.
	ids2, err := a.Import(ctx, s, claudePath)
	require.NoError(t, err)
	require.Equal(t, ids1[0], ids2[0], "same SourcePath → same artifact (no new id)")
	require.Equal(t, content1, replayMemoryForTest(t, s, ids2[0]), "steady state is a fixed point")
}

// TestProjectRoundTrip_LoopSafe is the integration-style project round-trip: a
// registered project's hand-authored CLAUDE.md + its type:project topic compose
// in on import; exporting back to that same CLAUDE.md strips the topic body,
// reproducing the pristine hand-authored body; re-importing yields identical
// content. No loop.
func TestProjectRoundTrip_LoopSafe(t *testing.T) {
	a, s, _, test123 := seedTypeRouting(t)
	ctx := context.Background()

	projectClaude := filepath.Join(test123, "CLAUDE.md")
	projBody := "# Test123\n\n- build with make\n"
	require.NoError(t, os.WriteFile(projectClaude, []byte(projBody), 0o644))

	ids1, err := a.importProjectMemory(ctx, s, test123)
	require.NoError(t, err)
	content1 := replayMemoryForTest(t, s, ids1[0])
	require.Contains(t, content1, projectTopicBody)
	require.Contains(t, content1, "build with make")

	// Export to the project's own CLAUDE.md strips the type:project body →
	// pristine hand-authored body.
	require.NoError(t, a.ExportMemory(ctx, s, ids1[0], projectClaude))
	after, err := os.ReadFile(projectClaude)
	require.NoError(t, err)
	require.Equal(t, projBody, string(after), "project CLAUDE.md stays pristine after strip")

	// Re-import composes the same view → fixed point, same artifact.
	ids2, err := a.importProjectMemory(ctx, s, test123)
	require.NoError(t, err)
	require.Equal(t, ids1[0], ids2[0])
	require.Equal(t, content1, replayMemoryForTest(t, s, ids2[0]))
}
