package skilldialect

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skill(allowedTools string) string {
	return "---\nname: t\ndescription: d\nallowed-tools: " + allowedTools + "\n---\n\n# Body\nUse the Bash tool.\n"
}

func TestNormalize_MapsNativeNamesToCanonical(t *testing.T) {
	in := skill("Bash Read Edit")
	got := string(Normalize("claude-code", []byte(in)))
	assert.Equal(t, skill("acf.exec acf.read acf.edit"), got)
	// Body untouched — "Bash" in prose must survive verbatim.
	assert.Contains(t, got, "Use the Bash tool.")
}

func TestNormalize_PreservesSpecifierSuffix(t *testing.T) {
	in := skill("Bash(git:*) Read(src/**)")
	got := string(Normalize("claude-code", []byte(in)))
	assert.Equal(t, skill("acf.exec(git:*) acf.read(src/**)"), got)
}

func TestNormalize_UnknownAndMCPTokensPassThrough(t *testing.T) {
	in := skill("Bash mcp__github__create_issue FrobnicateTool")
	got := string(Normalize("claude-code", []byte(in)))
	assert.Equal(t, skill("acf.exec mcp__github__create_issue FrobnicateTool"), got)
}

func TestNormalize_Idempotent(t *testing.T) {
	in := skill("acf.exec acf.read")
	got := string(Normalize("claude-code", []byte(in)))
	assert.Equal(t, in, got, "already-canonical tokens must not be re-translated")
}

func TestDenormalize_PerAgentDialects(t *testing.T) {
	canonical := skill("acf.exec acf.read acf.edit")
	assert.Equal(t, skill("Bash Read Edit"), string(Denormalize("claude-code", []byte(canonical))))
	assert.Equal(t, skill("bash read apply_patch"), string(Denormalize("kilo", []byte(canonical))))
	assert.Equal(t, skill("exec_command acf.read apply_patch"), string(Denormalize("codex", []byte(canonical))),
		"codex has no read mapping — canonical token is emitted verbatim (lossless multi-hop)")
	assert.Equal(t, canonical, string(Denormalize("openclaw", []byte(canonical))),
		"openclaw has an empty map — pure passthrough")
}

func TestRoundTrip_SameAgent_ByteStable(t *testing.T) {
	// The byte-stability contract (ADR-0043 D9) holds for inputs free of
	// canonical tokens the agent can map — i.e. every natively-authored file.
	// (A mappable canonical token in a native file is the multi-hop
	// intermediate case and intentionally resolves; see the next test.)
	for _, agent := range []string{"claude-code", "codex", "kilo", "hermes", "openclaw"} {
		for _, val := range []string{"Bash Read", "exec_command apply_patch", "bash grep todowrite", "weird_tool x(y) acf.unknown-tool"} {
			in := skill(val)
			out := string(Denormalize(agent, Normalize(agent, []byte(in))))
			assert.Equal(t, in, out, "agent=%s value=%q: normalize∘denormalize must be identity", agent, val)
		}
	}
}

// TestRoundTrip_CanonicalTokenResolvesOnExport documents the intentional
// exception to byte-stability: a canonical token sitting in a native file —
// the artifact of an earlier hop through an agent that had no mapping (D4) —
// is translated to the native name on the next export that CAN map it.
// That late recovery is the feature, not a round-trip bug.
func TestRoundTrip_CanonicalTokenResolvesOnExport(t *testing.T) {
	in := skill("acf.exec weird_tool")
	out := string(Denormalize("claude-code", Normalize("claude-code", []byte(in))))
	assert.Equal(t, skill("Bash weird_tool"), out)
}

func TestMultiHop_ClaudeToKilo(t *testing.T) {
	in := skill("Bash Read(docs/**) mcp__jira__search Glob")
	canonical := Normalize("claude-code", []byte(in))
	kilo := string(Denormalize("kilo", canonical))
	// Glob has no kilo mapping → canonical token survives; MCP ref verbatim.
	assert.Equal(t, skill("bash read(docs/**) mcp__jira__search acf.glob"), kilo)
	// Hop onward: kilo re-import + claude export reconstructs the original exactly.
	back := string(Denormalize("claude-code", Normalize("kilo", []byte(kilo))))
	assert.Equal(t, in, back, "claude → kilo → claude must reconstruct the original")
}

func TestSafeBail_LeavesContentVerbatim(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":   "# Just a body\nallowed-tools: Bash\n",
		"no allowed-tools": "---\nname: t\ndescription: d\n---\nbody\n",
		"block list":       "---\nname: t\nallowed-tools:\n  - Bash\n  - Read\n---\nbody\n",
		"flow list":        "---\nname: t\nallowed-tools: [Bash, Read]\n---\nbody\n",
		"quoted value":     "---\nname: t\nallowed-tools: \"Bash Read\"\n---\nbody\n",
		"comment":          "---\nname: t\nallowed-tools: Bash # trusted\n---\nbody\n",
		"unclosed fm":      "---\nname: t\nallowed-tools: Bash\n",
		"duplicate key":    "---\nallowed-tools: Bash\nallowed-tools: Read\n---\nbody\n",
		"empty value":      "---\nname: t\nallowed-tools:\n---\nbody\n",
	}
	for name, in := range cases {
		assert.Equal(t, in, string(Normalize("claude-code", []byte(in))), "normalize bail case %q", name)
		assert.Equal(t, in, string(Denormalize("kilo", []byte(in))), "denormalize bail case %q", name)
	}
}

func TestNormalize_AllowedToolsOutsideFrontmatterUntouched(t *testing.T) {
	// An allowed-tools-looking line in the BODY (e.g. inside a fenced docs
	// example) must never be transformed.
	in := "---\nname: t\ndescription: d\n---\n\n```yaml\nallowed-tools: Bash Read\n```\n"
	assert.Equal(t, in, string(Normalize("claude-code", []byte(in))))
}

func TestNormalize_PreservesSpacingAndCRLFBail(t *testing.T) {
	// Multiple spaces between tokens are preserved exactly.
	in := skill("Bash  Read")
	assert.Equal(t, skill("acf.exec  acf.read"), string(Normalize("claude-code", []byte(in))))
	// CRLF files bail verbatim (frontmatter scanner is \n-based by design).
	crlf := "---\r\nname: t\r\nallowed-tools: Bash\r\n---\r\nbody\r\n"
	assert.Equal(t, crlf, string(Normalize("claude-code", []byte(crlf))))
}

// TestMaps_BijectivePerAgent guards D4: each agent's table must be 1:1 in
// both directions or same-agent round-trips lose bytes.
func TestMaps_BijectivePerAgent(t *testing.T) {
	for agent, m := range agentMaps() {
		seenNative := map[string]string{}
		for canon, native := range m {
			require.NotEmpty(t, native, "agent %s canonical %s", agent, canon)
			if prev, dup := seenNative[native]; dup {
				t.Fatalf("agent %s: native %q mapped from both %q and %q (not bijective)", agent, native, prev, canon)
			}
			seenNative[native] = canon
		}
	}
}
