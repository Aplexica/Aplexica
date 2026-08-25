package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip_ByteIdentical_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"plain", "Hello world.\n"},
		{"empty", ""},
		{"no-trailing-newline", "no newline"},
		{"multiline-markdown", "# Title\n\nPara 1.\n\n- item a\n- item b\n"},
		{"unicode", "héllo — wörld ✓ 🎉\n"},
		{"crlf", "line1\r\nline2\r\n"},
		{"large", strings1MB()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "CLAUDE.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.Import(context.Background(), s, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "CLAUDE.md")
			require.NoError(t, a.Export(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got),
				"round-trip MUST be byte-identical; failure = ADR-0017 fidelity claim is broken")
		})
	}
}

func strings1MB() string {
	b := make([]byte, 1024*1024)
	for i := range b {
		b[i] = byte('a' + (i % 26))
	}
	return string(b)
}

func TestRoundTrip_Conversation_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"single-line", `{"type":"summary","sessionId":"s1"}` + "\n"},
		{"two-line", `{"type":"summary","sessionId":"s1"}` + "\n" +
			`{"type":"event","uuid":"e1","timestamp":"2026-05-20T12:00:00Z"}` + "\n"},
		{"chained-events", `{"type":"summary","sessionId":"s1"}` + "\n" +
			`{"type":"event","uuid":"e1","parentUuid":null,"timestamp":"2026-05-20T12:00:00Z"}` + "\n" +
			`{"type":"event","uuid":"e2","parentUuid":"e1","timestamp":"2026-05-20T12:01:00Z"}` + "\n" +
			`{"type":"event","uuid":"e3","parentUuid":"e2","timestamp":"2026-05-20T12:02:00Z"}` + "\n"},
		{"unicode-content", `{"type":"event","content":"héllo — wörld ✓ 🎉"}` + "\n"},
		{"no-trailing-newline", `{"type":"summary"}`},
		{"large", strings1MB()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "session-test.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.Import(context.Background(), s, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "session-test.jsonl")
			require.NoError(t, a.Export(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got),
				"conversation round-trip MUST be byte-identical for V0.1.2's opaque-content design")
		})
	}
}

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
		{"large", strings1MB()},
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
			ids, err := a.Import(context.Background(), s, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "SKILL.md")
			require.NoError(t, a.Export(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got),
				"skill round-trip MUST be byte-identical for V0.2's opaque-content design")
		})
	}
}

func TestRoundTrip_Tool_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no-env", `{"mcpServers":{"cf":{"type":"http","url":"https://mcp.cloudflare.com/mcp"}}}`},
		{"single-env", `{"mcpServers":{"gh":{"type":"stdio","command":"uvx","args":["mcp-server-github"],"env":{"TOKEN":"abc123"}}}}`},
		{"multiple-env", `{"mcpServers":{"a":{"env":{"K1":"v1","K2":"v2"}},"b":{"env":{"K3":"v3"}}}}`},
		{"empty-mcpServers", `{"mcpServers":{}}`},
		{"unicode-values", `{"mcpServers":{"x":{"env":{"K":"héllo-—-wörld-🎉"}}}}`},
		{"mixed-types", `{"mcpServers":{"http-srv":{"type":"http","url":"https://x"},"stdio-srv":{"type":"stdio","command":"y","env":{"SECRET":"s"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			store := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, store.Init())
			ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
			require.NoError(t, ss.Init())

			in := filepath.Join(tmp, "in", ".mcp.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
			ids, err := a.Import(context.Background(), store, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", ".mcp.json")
			require.NoError(t, a.Export(context.Background(), store, ids[0], out))

			gotBytes, err := os.ReadFile(out)
			require.NoError(t, err)

			// SEMANTIC equivalence: parse both, compare structures.
			var inObj, outObj any
			require.NoError(t, json.Unmarshal([]byte(tc.content), &inObj))
			require.NoError(t, json.Unmarshal(gotBytes, &outObj))
			require.Equal(t, inObj, outObj,
				"tool round-trip MUST preserve JSON structure semantically (byte-identical not promised due to parse-reserialize)")

			// ADR-0027: the canonical store payload MUST not contain raw secret values.
			events, err := store.ReadEvents(acf.KindTool, ids[0])
			require.NoError(t, err)
			payload, err := acf.DecodeToolPayload(events[0])
			require.NoError(t, err)
			require.NotContains(t, payload.Content, `"v1"`,
				"canonical store MUST NOT contain raw secret values")
			require.NotContains(t, payload.Content, `"abc123"`,
				"canonical store MUST NOT contain raw secret values")
		})
	}
}

func TestCrossAdapter_ClaudeCodeImport_CodexExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	in := filepath.Join(tmp, ".mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
	require.NoError(t, os.WriteFile(in,
		[]byte(`{"mcpServers":{"gh":{"type":"stdio","command":"uvx","env":{"TOKEN":"shh"}}}}`),
		0o644))

	cc := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := cc.ImportTool(context.Background(), store, in)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	cx := codex.New()
	cx.SecretsStore = ss
	out := filepath.Join(tmp, "out", "config.toml")
	require.NoError(t, cx.ExportTool(context.Background(), store, ids[0], out))

	gotBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, toml.Unmarshal(gotBytes, &got))

	servers, ok := got["mcp_servers"].(map[string]any)
	require.True(t, ok, "codex export must contain mcp_servers table")
	gh, ok := servers["gh"].(map[string]any)
	require.True(t, ok, "codex export must contain the gh server")
	require.Equal(t, "stdio", gh["type"])
	require.Equal(t, "uvx", gh["command"])
	env, ok := gh["env"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "shh", env["TOKEN"],
		"cross-adapter export must expand the secret back to its real value")
}
