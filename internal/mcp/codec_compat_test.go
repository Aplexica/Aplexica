package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// OpenClaw parses its config with json5.parse (src/config/io.ts): comments and
// trailing commas are valid and common in real user-edited openclaw.json files.
// The codec must tolerate them, not hard-fail like a strict JSON parser.
func TestFromOpenClawJSON_CommentsAndTrailingCommas(t *testing.T) {
	in := `{
		// the MCP block uses a nested servers map
		"mcp": {
			"servers": {
				"github": {
					"transport": "stdio",
					"command": "node",
					"args": ["mcp-github.js"], /* block comment */
					"env": {"GITHUB_TOKEN": "abc123"},
				},
			},
		},
	}`
	c, err := FromOpenClawJSON([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	srv, ok := c.Servers["github"]
	require.True(t, ok)
	require.Equal(t, "abc123", srv.Env["GITHUB_TOKEN"])
}

func TestMergeIntoOpenClawJSON_ExistingFileWithComments(t *testing.T) {
	c, err := FromOpenClawJSON([]byte(`{"mcp":{"servers":{"gh":{"transport":"stdio","command":"node"}}}}`))
	require.NoError(t, err)
	existing := `{
		// user notes preserved by JSON5
		"theme": "dark",
		"mcp": { "servers": {} },
	}`
	out, err := MergeIntoOpenClawJSON([]byte(existing), c)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(out, &root))
	require.Equal(t, "dark", root["theme"], "non-mcp top-level keys must survive a commented existing file")
}

// Current Kilo Code uses "remote" (not "streamable-http") for HTTP MCP servers
// (kilo.ai/docs/automate/mcp). The codec must map remote<->http.
func TestFromKiloJSONC_RemoteType_MapsToHTTP(t *testing.T) {
	in := `{"mcp": {"cf": {"type": "remote", "url": "https://mcp.cloudflare.com"}}}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)
	require.Equal(t, "http", c.Servers["cf"].Other["type"], "Kilo 'remote' maps to canonical 'http'")
}

func TestToKiloJSONC_HTTPEmitsRemote(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"cf": {Other: map[string]any{"type": "http", "url": "https://mcp.cloudflare.com"}},
	}}
	out, err := ToKiloJSONC(c)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(out, &root))
	mcp := root["mcp"].(map[string]any)
	cf := mcp["cf"].(map[string]any)
	require.Equal(t, "remote", cf["type"], "canonical 'http' emits Kilo 'remote' (current Kilo format)")
}

// Legacy Kilo files used "streamable-http"; keep accepting it on input.
func TestFromKiloJSONC_LegacyStreamableHTTP_StillMapsToHTTP(t *testing.T) {
	in := `{"mcp": {"cf": {"type": "streamable-http", "url": "https://mcp.cloudflare.com"}}}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)
	require.Equal(t, "http", c.Servers["cf"].Other["type"])
}
