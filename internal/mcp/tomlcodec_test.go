package mcp

import (
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

func TestFromMCPTOML_ExtractsServers(t *testing.T) {
	in := "[mcp_servers.gh]\ncommand = \"uvx\"\ntype = \"stdio\"\n[mcp_servers.gh.env]\nTOKEN = \"abc\"\n"
	c, err := FromMCPTOML([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	require.Equal(t, "abc", c.Servers["gh"].Env["TOKEN"])
	require.Equal(t, "stdio", c.Servers["gh"].Other["type"])
	require.Equal(t, "uvx", c.Servers["gh"].Other["command"])
	require.NotContains(t, c.Servers["gh"].Other, "env",
		"env must be moved out of Other into the typed Env map")
}

func TestFromMCPTOML_NoMcpServers_EmptyCanonical(t *testing.T) {
	c, err := FromMCPTOML([]byte(`model = "gpt"` + "\n"))
	require.NoError(t, err)
	require.Empty(t, c.Servers)
}

func TestFromMCPTOML_DropsNonMcpServersContent(t *testing.T) {
	in := `model = "gpt"

[plugins.something]
enabled = true

[mcp_servers.cf]
type = "http"
url = "https://x"
`
	c, err := FromMCPTOML([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	require.Contains(t, c.Servers, "cf")
}

func TestFromMCPTOML_ParseError(t *testing.T) {
	_, err := FromMCPTOML([]byte("[[ unclosed"))
	require.Error(t, err)
}

func TestToMCPTOML_WrapsServers(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {
			Env:   map[string]string{"TOKEN": "abc"},
			Other: map[string]any{"type": "stdio", "command": "uvx"},
		},
	}}
	b, err := ToMCPTOML(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, toml.Unmarshal(b, &parsed))
	servers := parsed["mcp_servers"].(map[string]any)
	require.Contains(t, servers, "gh")
	gh := servers["gh"].(map[string]any)
	require.Equal(t, "stdio", gh["type"])
	require.Equal(t, "uvx", gh["command"])
	env := gh["env"].(map[string]any)
	require.Equal(t, "abc", env["TOKEN"])
}

func TestFromMCPTOML_ToMCPTOML_SemanticRoundTrip(t *testing.T) {
	in := "[mcp_servers.gh]\ncommand = \"uvx\"\ntype = \"stdio\"\nargs = [\"mcp-server-github\"]\n[mcp_servers.gh.env]\nTOKEN = \"abc\"\nOTHER = \"xyz\"\n\n[mcp_servers.cf]\ntype = \"http\"\nurl = \"https://mcp.cloudflare.com\"\n"
	c, err := FromMCPTOML([]byte(in))
	require.NoError(t, err)

	out, err := ToMCPTOML(c)
	require.NoError(t, err)

	var inObj, outObj map[string]any
	require.NoError(t, toml.Unmarshal([]byte(in), &inObj))
	require.NoError(t, toml.Unmarshal(out, &outObj))
	require.Equal(t, inObj["mcp_servers"], outObj["mcp_servers"],
		"FromMCPTOML → ToMCPTOML must be TOML-semantically equal on the mcp_servers subtree")
}
