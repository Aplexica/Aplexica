package kilo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip_Skill_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"minimal", "---\nname: x\n---\n# Hi\n"},
		{"empty-frontmatter", "---\n---\n# No fields\n"},
		{"full-frontmatter", "---\nname: full\ndescription: A complete skill\nversion: 1.2.3\n---\n\n# Full Skill\n\nBody.\n"},
		{"unicode-name", "---\nname: ünïcödé\ndescription: hé hé\n---\n# 🎉\n"},
		{"no-frontmatter-just-md", "# Bare skill body\n\nNo frontmatter at all.\n"},
		// ADR-0043: kilo-native allowed-tools normalize+denormalize through
		// the canonical store and still round-trip byte-identical.
		{"allowed-tools-native", "---\nname: tooled\ndescription: d\nallowed-tools: bash read(src/**) grep\n---\n# Uses tools\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "SKILL.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.ImportSkill(context.Background(), s, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "SKILL.md")
			require.NoError(t, a.ExportSkill(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got),
				"kilo skill round-trip MUST be byte-identical")
		})
	}
}

// TestCrossAgent_Skill_AllowedToolsRemap proves the ADR-0043 wiring
// end-to-end through the real adapters + canonical store: a skill authored
// with claude-code tool names fans out to kilo carrying kilo's vocabulary,
// the body and other frontmatter stay verbatim, MCP refs and unmapped names
// survive untouched, and the claude-code re-export reproduces the original
// byte-for-byte.
func TestCrossAgent_Skill_AllowedToolsRemap(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	original := "---\n" +
		"name: deploy-helper\n" +
		"description: Deploys things\n" +
		"allowed-tools: Bash(git:*) Read mcp__github__create_issue Glob\n" +
		"---\n\n# Deploy\n\nUse the Bash tool to run deploy.sh.\n"
	in := filepath.Join(tmp, "in", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
	require.NoError(t, os.WriteFile(in, []byte(original), 0o644))

	cc := claudecode.New()
	ids, err := cc.ImportSkill(context.Background(), s, in)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// kilo export: bash + read translated; MCP ref verbatim; Glob has no
	// kilo mapping so the canonical token rides along; body prose verbatim.
	k := New()
	kiloOut := filepath.Join(tmp, "kilo", "SKILL.md")
	require.NoError(t, k.ExportSkill(context.Background(), s, ids[0], kiloOut))
	kiloGot, err := os.ReadFile(kiloOut)
	require.NoError(t, err)
	require.Contains(t, string(kiloGot), "allowed-tools: bash(git:*) read mcp__github__create_issue acf.glob\n")
	require.Contains(t, string(kiloGot), "Use the Bash tool to run deploy.sh.",
		"body prose must never be rewritten")

	// claude-code re-export: byte-identical to the original (D9).
	ccOut := filepath.Join(tmp, "cc", "SKILL.md")
	require.NoError(t, cc.ExportSkill(context.Background(), s, ids[0], ccOut))
	ccGot, err := os.ReadFile(ccOut)
	require.NoError(t, err)
	require.Equal(t, original, string(ccGot),
		"same-agent round-trip must stay byte-identical with the remap in place")
}
