package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestFromHermesYAML_ExtractsServers(t *testing.T) {
	in := `
mcp_servers:
  github:
    command: uvx
    args:
      - mcp-server-github
    env:
      TOKEN: abc
`
	c, err := FromHermesYAML([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	gh := c.Servers["github"]
	require.Equal(t, "abc", gh.Env["TOKEN"])
	require.Equal(t, "uvx", gh.Other["command"])
	args := gh.Other["args"].([]any)
	require.Equal(t, []any{"mcp-server-github"}, args)
	require.NotContains(t, gh.Other, "env",
		"env must be moved into the typed Env map, not left under Other")
}

func TestFromHermesYAML_HTTPServer(t *testing.T) {
	in := `
mcp_servers:
  cf:
    url: https://mcp.cloudflare.com
    headers:
      Authorization: "Bearer abc"
`
	c, err := FromHermesYAML([]byte(in))
	require.NoError(t, err)
	cf := c.Servers["cf"]
	require.Equal(t, "https://mcp.cloudflare.com", cf.Other["url"])
	require.NotNil(t, cf.Other["headers"])
}

func TestFromHermesYAML_NoMCPSection_EmptyCanonical(t *testing.T) {
	c, err := FromHermesYAML([]byte("memory_enabled: true\nother_unrelated: 42\n"))
	require.NoError(t, err)
	require.Empty(t, c.Servers)
}

func TestFromHermesYAML_ParseError(t *testing.T) {
	_, err := FromHermesYAML([]byte("not: : valid yaml: ["))
	require.Error(t, err)
}

func TestToHermesYAML_StdioServer(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {
			Env: map[string]string{"TOKEN": "abc"},
			Other: map[string]any{
				"command": "uvx",
				"args":    []any{"mcp-server-github"},
			},
		},
	}}
	out, err := ToHermesYAML(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, yaml.Unmarshal(out, &parsed))
	mcp := parsed["mcp_servers"].(map[string]any)
	gh := mcp["gh"].(map[string]any)
	require.Equal(t, "uvx", gh["command"])
	env := gh["env"].(map[string]any)
	require.Equal(t, "abc", env["TOKEN"])
}

func TestToHermesYAML_OmitsEmptyEnv(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"x": {Other: map[string]any{"command": "x"}},
	}}
	out, err := ToHermesYAML(c)
	require.NoError(t, err)
	require.NotContains(t, string(out), "env:",
		"empty Env must not produce an env: key in the Hermes output")
}

func TestFromHermesYAML_ToHermesYAML_SemanticRoundTrip(t *testing.T) {
	in := `
mcp_servers:
  github:
    command: uvx
    args:
      - mcp-server-github
    env:
      TOKEN: abc
      OTHER: xyz
  cf:
    url: https://mcp.cloudflare.com
    headers:
      Authorization: Bearer x
`
	c, err := FromHermesYAML([]byte(in))
	require.NoError(t, err)

	out, err := ToHermesYAML(c)
	require.NoError(t, err)

	var inObj, outObj map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(in), &inObj))
	require.NoError(t, yaml.Unmarshal(out, &outObj))
	require.Equal(t, inObj["mcp_servers"], outObj["mcp_servers"],
		"Hermes YAML → canonical → Hermes YAML must be semantically equal on mcp_servers")
}

func TestFromHermesYAML_ClaudecodeCompatibleAfterRoundtrip(t *testing.T) {
	// Cross-codec: Hermes YAML → canonical → claudecode .mcp.json
	in := `
mcp_servers:
  gh:
    command: uvx
    args: [x]
    env:
      T: v
`
	c, err := FromHermesYAML([]byte(in))
	require.NoError(t, err)
	out, err := ToMCPJSON(c)
	require.NoError(t, err)
	// Verify the claudecode JSON is well-formed and contains the expected fields.
	require.Contains(t, string(out), `"mcpServers"`)
	require.Contains(t, string(out), `"uvx"`)
	require.Contains(t, string(out), `"T"`)
}
