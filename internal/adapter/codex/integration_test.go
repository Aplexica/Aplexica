package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip_Memory_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"plain", "Hello world.\n"},
		{"empty", ""},
		{"no-trailing-newline", "no newline"},
		{"multiline-markdown", "# Agent Instructions\n\nDo X.\n\n- item a\n- item b\n"},
		{"unicode", "héllo — wörld ✓ 🎉\n"},
		{"crlf", "line1\r\nline2\r\n"},
		{"large", strings1MB()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "AGENTS.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.Import(context.Background(), s, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "AGENTS.md")
			require.NoError(t, a.Export(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got),
				"AGENTS.md round-trip MUST be byte-identical (validates ACF-as-hub-format claim across two adapters)")
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

func TestRoundTrip_Skill_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"minimal", "---\nname: x\n---\n# Hi\n"},
		{"empty-frontmatter", "---\n---\n# No fields\n"},
		{"full-frontmatter", "---\nname: full\ndescription: A complete skill\nversion: 1.2.3\n---\n\n# Full Skill\n\nBody.\n"},
		{"unicode", "---\nname: ünïcödé\n---\n# 🎉\n"},
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

			out := filepath.Join(tmp, "out", "SKILL.md")
			require.NoError(t, a.Export(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got))
		})
	}
}

func TestRoundTrip_Conversation_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"single-line", `{"timestamp":"2026-05-20T12:00:00Z","type":"event"}` + "\n"},
		{"multi-line", `{"timestamp":"2026-05-20T12:00:00Z","type":"user","payload":{"text":"hi"}}` + "\n" +
			`{"timestamp":"2026-05-20T12:00:01Z","type":"assistant","payload":{"text":"hello"}}` + "\n"},
		{"unicode-payload", `{"type":"event","payload":{"text":"héllo — wörld 🎉"}}` + "\n"},
		{"no-trailing-newline", `{"type":"event"}`},
		{"large", strings1MB()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			s := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, s.Init())

			in := filepath.Join(tmp, "in", "rollout.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := New()
			ids, err := a.Import(context.Background(), s, in)
			require.NoError(t, err)

			out := filepath.Join(tmp, "out", "rollout.jsonl")
			require.NoError(t, a.Export(context.Background(), s, ids[0], out))

			got, err := os.ReadFile(out)
			require.NoError(t, err)
			require.Equal(t, tc.content, string(got))
		})
	}
}

func TestRoundTrip_Tool_VariousInputs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no-env", `[mcp_servers.cf]
type = "http"
url = "https://mcp.cloudflare.com/mcp"
`},
		{"single-env", `[mcp_servers.gh]
command = "uvx"
args = ["mcp-server-github"]

[mcp_servers.gh.env]
TOKEN = "abc123"
`},
		{"multiple-env", `[mcp_servers.a]

[mcp_servers.a.env]
K1 = "v1"
K2 = "v2"

[mcp_servers.b]

[mcp_servers.b.env]
K3 = "v3"
`},
		{"unicode-values", `[mcp_servers.x]

[mcp_servers.x.env]
K = "héllo-wörld-🎉"
`},
		{"ignored-extra-sections", `model = "gpt-5"
notify = ["foo"]

[marketplaces.bundled]
source = "/tmp"

[mcp_servers.gh]
command = "x"

[mcp_servers.gh.env]
SECRET = "s"
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			store := &acf.Store{Root: filepath.Join(tmp, "store")}
			require.NoError(t, store.Init())
			ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
			require.NoError(t, ss.Init())

			in := filepath.Join(tmp, "in", "config.toml")
			require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
			require.NoError(t, os.WriteFile(in, []byte(tc.content), 0o644))

			a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
			ids, err := a.Import(context.Background(), store, in)
			require.NoError(t, err)
			require.Len(t, ids, 1)

			out := filepath.Join(tmp, "out", "config.toml")
			require.NoError(t, a.Export(context.Background(), store, ids[0], out))

			gotBytes, err := os.ReadFile(out)
			require.NoError(t, err)

			// SEMANTIC equivalence on mcp_servers only (non-mcp_servers content
			// is dropped on import by design).
			var inObj, outObj map[string]any
			require.NoError(t, toml.Unmarshal([]byte(tc.content), &inObj))
			require.NoError(t, toml.Unmarshal(gotBytes, &outObj))
			require.Equal(t, inObj["mcp_servers"], outObj["mcp_servers"],
				"tool round-trip MUST preserve mcp_servers structure semantically")

			// ADR-0027: the canonical store payload MUST NOT contain raw secret values.
			events, err := store.ReadEvents(acf.KindTool, ids[0])
			require.NoError(t, err)
			payload, err := acf.DecodeToolPayload(events[0])
			require.NoError(t, err)
			require.NotContains(t, payload.Content, "v1",
				"canonical store MUST NOT contain raw secret values")
			require.NotContains(t, payload.Content, "abc123",
				"canonical store MUST NOT contain raw secret values")
			require.NotContains(t, payload.Content, `"s"`,
				"canonical store MUST NOT contain raw secret values")
		})
	}
}

func TestCrossAdapter_CodexImport_ClaudeCodeExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	in := filepath.Join(tmp, "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
	require.NoError(t, os.WriteFile(in,
		[]byte("[mcp_servers.gh]\ncommand = \"uvx\"\ntype = \"stdio\"\n[mcp_servers.gh.env]\nTOKEN = \"shh\"\n"),
		0o644))

	cx := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := cx.ImportTool(context.Background(), store, in)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	cc := claudecode.New()
	cc.SecretsStore = ss
	out := filepath.Join(tmp, "out", ".mcp.json")
	require.NoError(t, cc.ExportTool(context.Background(), store, ids[0], out))

	gotBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(gotBytes, &got))

	servers, ok := got["mcpServers"].(map[string]any)
	require.True(t, ok, "claude-code export must contain mcpServers object")
	gh, ok := servers["gh"].(map[string]any)
	require.True(t, ok, "claude-code export must contain the gh server")
	require.Equal(t, "stdio", gh["type"])
	require.Equal(t, "uvx", gh["command"])
	env, ok := gh["env"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "shh", env["TOKEN"],
		"cross-adapter export must expand the secret back to its real value")
}
