package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFromMCPJSON_ExtractsServers(t *testing.T) {
	in := `{"mcpServers":{"gh":{"type":"stdio","command":"uvx","env":{"TOKEN":"abc"}}}}`
	c, err := FromMCPJSON([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	require.Equal(t, "abc", c.Servers["gh"].Env["TOKEN"])
	require.Equal(t, "stdio", c.Servers["gh"].Other["type"])
	require.Equal(t, "uvx", c.Servers["gh"].Other["command"])
	require.NotContains(t, c.Servers["gh"].Other, "env",
		"env must be moved out of Other into the typed Env map")
}

func TestFromMCPJSON_NoMcpServers_EmptyCanonical(t *testing.T) {
	in := `{"otherField":42}`
	c, err := FromMCPJSON([]byte(in))
	require.NoError(t, err)
	require.Empty(t, c.Servers)
}

func TestFromMCPJSON_EmptyEnv(t *testing.T) {
	in := `{"mcpServers":{"x":{"type":"http","url":"https://x"}}}`
	c, err := FromMCPJSON([]byte(in))
	require.NoError(t, err)
	require.Empty(t, c.Servers["x"].Env)
	require.Equal(t, "https://x", c.Servers["x"].Other["url"])
}

func TestFromMCPJSON_ParseError(t *testing.T) {
	_, err := FromMCPJSON([]byte("not json"))
	require.Error(t, err)
}

func TestToMCPJSON_WrapsServers(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {
			Env:   map[string]string{"TOKEN": "abc"},
			Other: map[string]any{"type": "stdio", "command": "uvx"},
		},
	}}
	b, err := ToMCPJSON(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(b, &parsed))
	servers := parsed["mcpServers"].(map[string]any)
	require.Contains(t, servers, "gh")
	gh := servers["gh"].(map[string]any)
	require.Equal(t, "stdio", gh["type"])
	require.Equal(t, "uvx", gh["command"])
	env := gh["env"].(map[string]any)
	require.Equal(t, "abc", env["TOKEN"])
}

func TestToMCPJSON_OmitsEmptyEnv(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"http-only": {
			Other: map[string]any{"type": "http", "url": "https://x"},
		},
	}}
	b, err := ToMCPJSON(c)
	require.NoError(t, err)
	require.NotContains(t, string(b), `"env"`)
}

func TestFromMCPJSON_ToMCPJSON_SemanticRoundTrip(t *testing.T) {
	in := `{"mcpServers":{"gh":{"type":"stdio","command":"uvx","args":["mcp-server-github"],"env":{"TOKEN":"abc","OTHER":"xyz"}},"cf":{"type":"http","url":"https://mcp.cloudflare.com"}}}`
	c, err := FromMCPJSON([]byte(in))
	require.NoError(t, err)

	out, err := ToMCPJSON(c)
	require.NoError(t, err)

	var inObj, outObj any
	require.NoError(t, json.Unmarshal([]byte(in), &inObj))
	require.NoError(t, json.Unmarshal(out, &outObj))
	require.Equal(t, inObj, outObj,
		"FromMCPJSON → ToMCPJSON must be JSON-semantically equal to the input")
}
