package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
	"github.com/stretchr/testify/require"
)

const pristineAgents = "# Project memory\n\n## Conventions\n- Language: Go 1.23.\n- Never commit secrets.\n\n## Preferences\n- user name is Example User\n"
const cometMem = "# Personal memory\n\n- Example User's dog's name is Comet.\n"

func TestComposeGlobalMemory_AppendsNewEntries(t *testing.T) {
	got := composeGlobalMemory(pristineAgents, []string{cometMem})
	// Body is preserved verbatim and the memories entries are appended.
	require.Contains(t, got, pristineAgents)
	require.Contains(t, got, "- Example User's dog's name is Comet.")
	require.Contains(t, got, "# Personal memory")
	require.True(t, len(got) > len(pristineAgents), "composed memory must grow")
}

func TestComposeGlobalMemory_NoMemories_ByteIdentical(t *testing.T) {
	require.Equal(t, pristineAgents, composeGlobalMemory(pristineAgents, nil))
	require.Equal(t, pristineAgents, composeGlobalMemory(pristineAgents, []string{"", "   \n"}))
}

func TestComposeGlobalMemory_DoesNotDuplicateEntryAlreadyInBody(t *testing.T) {
	body := "# Project memory\n\n- Example User's dog's name is Comet.\n"
	got := composeGlobalMemory(body, []string{cometMem})
	// The Comet line is already in the body, so the only NEW entry the
	// memories layer contributes is its "# Personal memory" header.
	require.Equal(t, 1, strings.Count(got, "- Example User's dog's name is Comet."),
		"a memory present in AGENTS.md body must not be duplicated by the memories layer")
}

func TestStripIsInverseOfCompose(t *testing.T) {
	// The core round-trip invariant: writing a composed memory back to
	// AGENTS.md reproduces the pristine body byte-for-byte, so AGENTS.md never
	// gains a memory it already holds in memories/.
	composed := composeGlobalMemory(pristineAgents, []string{cometMem})
	require.Equal(t, pristineAgents, stripMemoriesEntries(composed, []string{cometMem}))
}

func TestStrip_KeepsGenuinelyNewMemory(t *testing.T) {
	// A memory that came from ANOTHER agent (not in memories/) survives the
	// strip and lands in AGENTS.md.
	incoming := pristineAgents + "\n# Personal memory\n- Example User's dog's name is Comet.\n- Favorite editor is vim.\n"
	got := stripMemoriesEntries(incoming, []string{cometMem})
	require.NotContains(t, got, "Comet", "memories-layer entry must be stripped")
	require.Contains(t, got, "- Favorite editor is vim.", "new memory not in memories/ must be kept")
}

func TestStrip_NoMemories_Unchanged(t *testing.T) {
	require.Equal(t, pristineAgents, stripMemoriesEntries(pristineAgents, nil))
}

// --- integration: the live memories→AGENTS.md round-trip on a real store ---

func TestGlobalMemory_RoundTrip_AgentsStaysPristine(t *testing.T) {
	home := t.TempDir()
	s := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, s.Init())

	codexDir := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(filepath.Join(codexDir, "memories"), 0o755))
	agentsPath := filepath.Join(codexDir, "AGENTS.md")
	require.NoError(t, os.WriteFile(agentsPath, []byte(pristineAgents), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "memories", "personal.md"), []byte(cometMem), 0o644))

	a := &Adapter{HomeDir: home, DeviceID: "dev"}
	ctx := context.Background()

	// Import via the dispatch path, triggered by the memories file — must
	// update the single AGENTS.md-keyed artifact with the COMPOSED content.
	ids, err := a.Import(ctx, s, filepath.Join(codexDir, "memories", "personal.md"))
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// The artifact (what fans out to other agents) carries the merged view.
	content, tomb, err := replayForTest(s, ids[0])
	require.NoError(t, err)
	require.False(t, tomb)
	require.Contains(t, content, "Comet")
	require.Contains(t, content, "user name is Example User")

	// AGENTS.md is the artifact's SourcePath (NOT memories/personal.md), so a
	// memories edit updates the one canonical memory.
	got, _ := s.ReadArtifact(acf.KindMemory, ids[0])
	require.Equal(t, agentsPath, got.SourcePath)
	require.Equal(t, acf.ScopeGlobal, got.Scope)

	// Exporting that artifact BACK to ~/.codex/AGENTS.md strips the memories
	// entries → AGENTS.md is unchanged (still pristine, byte-identical).
	require.NoError(t, a.ExportMemory(ctx, s, ids[0], agentsPath))
	after, err := os.ReadFile(agentsPath)
	require.NoError(t, err)
	require.Equal(t, pristineAgents, string(after),
		"AGENTS.md MUST stay pristine — memories already in memories/ are not duplicated into it")

	// Exporting to ANOTHER agent's file keeps the merged view (Comet included).
	claudePath := filepath.Join(home, ".claude", "CLAUDE.md")
	require.NoError(t, a.ExportMemory(ctx, s, ids[0], claudePath))
	claude, err := os.ReadFile(claudePath)
	require.NoError(t, err)
	require.Contains(t, string(claude), "Comet", "other agents receive the merged memory")
}

func replayForTest(s *acf.Store, id string) (string, bool, error) {
	events, err := s.ReadEvents(acf.KindMemory, id)
	if err != nil {
		return "", false, err
	}
	p, err := acf.DecodeMemoryPayload(events[len(events)-1])
	return p.Content, false, err
}

func TestDiscover_IncludesMemoriesRoot(t *testing.T) {
	codexPath := adaptertest.WithCommand(t, "codex")
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "memories"), 0o755))
	d, err := (&Adapter{HomeDir: home, CLIExecutablePaths: []string{codexPath}}).Discover()
	require.NoError(t, err)
	require.True(t, d.Installed)
	require.Contains(t, d.GlobalRoots, filepath.Join(home, ".codex"))
	require.Contains(t, d.GlobalRoots, filepath.Join(home, ".codex", "memories"))
}
