package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFromOpenClawJSON_BasicLocalServer(t *testing.T) {
	in := `{
		"mcp": {
			"servers": {
				"github": {
					"transport": "stdio",
					"command": "node",
					"args": ["mcp-github.js"],
					"env": {"GITHUB_TOKEN": "abc123"}
				}
			}
		}
	}`
	c, err := FromOpenClawJSON([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	srv, ok := c.Servers["github"]
	require.True(t, ok)
	require.Equal(t, "abc123", srv.Env["GITHUB_TOKEN"])
}

func TestFromOpenClawJSON_HTTPServer(t *testing.T) {
	in := `{
		"mcp": {
			"servers": {
				"remote": {
					"transport": "streamable-http",
					"url": "https://example.com/mcp"
				}
			}
		}
	}`
	c, err := FromOpenClawJSON([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	srv, ok := c.Servers["remote"]
	require.True(t, ok)
	require.Equal(t, "https://example.com/mcp", srv.Other["url"])
}

func TestFromOpenClawJSON_NoMCPKey_EmptyCanonical(t *testing.T) {
	c, err := FromOpenClawJSON([]byte(`{"unrelated": 42}`))
	require.NoError(t, err)
	require.Empty(t, c.Servers)
}

func TestFromOpenClawJSON_MCPKeyWithoutServers_EmptyCanonical(t *testing.T) {
	c, err := FromOpenClawJSON([]byte(`{"mcp": {"otherField": true}}`))
	require.NoError(t, err)
	require.Empty(t, c.Servers)
}

func TestToOpenClawJSON_RoundTrip(t *testing.T) {
	original := Canonical{
		Servers: map[string]Server{
			"github": {
				Env: map[string]string{"GITHUB_TOKEN": "abc"},
				Other: map[string]any{
					"command": "node",
					"args":    []any{"mcp-github.js"},
				},
			},
			"remote": {
				Other: map[string]any{"url": "https://example.com/mcp"},
			},
		},
	}
	out, err := ToOpenClawJSON(original)
	require.NoError(t, err)

	c2, err := FromOpenClawJSON(out)
	require.NoError(t, err)
	require.Len(t, c2.Servers, 2)
	require.Equal(t, "node", c2.Servers["github"].Other["command"])
	require.Equal(t, "https://example.com/mcp", c2.Servers["remote"].Other["url"])
}

func TestToOpenClawJSON_ProducesNestedShape(t *testing.T) {
	c := Canonical{Servers: map[string]Server{"a": {Other: map[string]any{"command": "x"}}}}
	out, err := ToOpenClawJSON(c)
	require.NoError(t, err)
	// Must produce nested {"mcp": {"servers": {...}}}.
	require.Contains(t, string(out), `"mcp"`)
	require.Contains(t, string(out), `"servers"`)
}

func TestMergeIntoOpenClawJSON_PreservesOtherTopLevelKeys(t *testing.T) {
	existing := []byte(`{
		"channels": {"discord": {"enabled": true}},
		"agents": {"defaults": {"workspace": "/home/u/.openclaw/workspace"}},
		"mcp": {
			"servers": {
				"OLD": {"command": "old-bin"}
			},
			"otherField": "preserved-too"
		}
	}`)
	c := Canonical{
		Servers: map[string]Server{
			"NEW": {Other: map[string]any{"command": "new-bin", "args": []any{"--flag"}}},
		},
	}
	out, err := MergeIntoOpenClawJSON(existing, c)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.NotNil(t, got["channels"], "non-mcp top-level keys must survive")
	require.NotNil(t, got["agents"], "non-mcp top-level keys must survive")

	mcpMap := got["mcp"].(map[string]any)
	require.Equal(t, "preserved-too", mcpMap["otherField"], "non-servers keys under mcp must survive")
	servers := mcpMap["servers"].(map[string]any)
	require.Contains(t, servers, "NEW", "new server entry replaces old")
	require.NotContains(t, servers, "OLD", "old server replaced (full overwrite of servers map per ADR-0027 secret-isolation model)")
}

func TestMergeIntoOpenClawJSON_EmptyExistingFile(t *testing.T) {
	c := Canonical{Servers: map[string]Server{"a": {Other: map[string]any{"command": "x"}}}}
	out, err := MergeIntoOpenClawJSON(nil, c)
	require.NoError(t, err)
	require.Contains(t, string(out), `"mcp"`)
	require.Contains(t, string(out), `"servers"`)
	require.Contains(t, string(out), `"a"`)
}

func TestMergeIntoOpenClawJSON_ExistingFileWithoutMCP(t *testing.T) {
	existing := []byte(`{"channels": {"discord": {"enabled": true}}}`)
	c := Canonical{Servers: map[string]Server{"a": {Other: map[string]any{"command": "x"}}}}
	out, err := MergeIntoOpenClawJSON(existing, c)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.NotNil(t, got["channels"])
	require.NotNil(t, got["mcp"])
}
