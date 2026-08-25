package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFromKiloJSONC_BasicLocalServer(t *testing.T) {
	in := `{
		"mcp": {
			"gh": {
				"type": "local",
				"command": ["uvx", "mcp-server-github"],
				"environment": {"TOKEN": "abc"},
				"enabled": true
			}
		}
	}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)

	gh := c.Servers["gh"]
	require.Equal(t, "abc", gh.Env["TOKEN"])
	require.Equal(t, "stdio", gh.Other["type"], "Kilo 'local' type maps to canonical 'stdio'")
	require.Equal(t, "uvx", gh.Other["command"])
	args, ok := gh.Other["args"].([]any)
	require.True(t, ok, "args must be present when command had multiple elements")
	require.Equal(t, []any{"mcp-server-github"}, args)
	require.Equal(t, true, gh.Other["enabled"], "vendor-specific keys preserved verbatim")
}

func TestFromKiloJSONC_RemoteHTTPServer(t *testing.T) {
	in := `{
		"mcp": {
			"cf": {
				"type": "streamable-http",
				"url": "https://mcp.cloudflare.com",
				"headers": {"Authorization": "Bearer abc"}
			}
		}
	}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)
	cf := c.Servers["cf"]
	require.Equal(t, "http", cf.Other["type"], "Kilo 'streamable-http' type maps to canonical 'http'")
	require.Equal(t, "https://mcp.cloudflare.com", cf.Other["url"])
	require.NotNil(t, cf.Other["headers"])
}

func TestFromKiloJSONC_SingleElementCommand_NoArgs(t *testing.T) {
	in := `{"mcp": {"x": {"command": ["bin-only"]}}}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)
	x := c.Servers["x"]
	require.Equal(t, "bin-only", x.Other["command"])
	require.NotContains(t, x.Other, "args", "single-element command must NOT emit an args key")
}

func TestFromKiloJSONC_NoMCPKey_EmptyCanonical(t *testing.T) {
	c, err := FromKiloJSONC([]byte(`{"unrelated": 42}`))
	require.NoError(t, err)
	require.Empty(t, c.Servers)
}

func TestFromKiloJSONC_WithComments(t *testing.T) {
	in := `{
		// top comment
		"mcp": {
			"gh": {
				"type": "local", /* block */
				"command": ["uvx"]
			}
		}
	}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)
	require.Len(t, c.Servers, 1)
	require.Equal(t, "stdio", c.Servers["gh"].Other["type"])
}

func TestFromKiloJSONC_ParseError(t *testing.T) {
	_, err := FromKiloJSONC([]byte("{[ not json"))
	require.Error(t, err)
}

func TestToKiloJSONC_StdioServer(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {
			Env: map[string]string{"TOKEN": "abc"},
			Other: map[string]any{
				"type":    "stdio",
				"command": "uvx",
				"args":    []any{"mcp-server-github"},
				"enabled": true,
			},
		},
	}}
	out, err := ToKiloJSONC(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	mcp := parsed["mcp"].(map[string]any)
	gh := mcp["gh"].(map[string]any)
	require.Equal(t, "local", gh["type"], "canonical 'stdio' maps to Kilo 'local'")
	cmd := gh["command"].([]any)
	require.Equal(t, []any{"uvx", "mcp-server-github"}, cmd,
		"canonical command+args merged into a single command array for Kilo")
	env := gh["environment"].(map[string]any)
	require.Equal(t, "abc", env["TOKEN"], "canonical Env maps to Kilo 'environment' key")
	require.Equal(t, true, gh["enabled"])
}

func TestToKiloJSONC_HTTPServer(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"cf": {Other: map[string]any{
			"type": "http",
			"url":  "https://mcp.cloudflare.com",
		}},
	}}
	out, err := ToKiloJSONC(c)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	cf := parsed["mcp"].(map[string]any)["cf"].(map[string]any)
	require.Equal(t, "remote", cf["type"], "canonical 'http' maps to Kilo 'remote' (current Kilo format)")
}

func TestToKiloJSONC_CommandWithoutArgs(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"x": {Other: map[string]any{"command": "bin-only"}},
	}}
	out, err := ToKiloJSONC(c)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	x := parsed["mcp"].(map[string]any)["x"].(map[string]any)
	cmd := x["command"].([]any)
	require.Equal(t, []any{"bin-only"}, cmd,
		"canonical command-only emits a single-element Kilo command array")
}

func TestToKiloJSONC_CommandBackedCrossAdapterServerGetsRequiredDefaults(t *testing.T) {
	// Claude-style MCP JSON permits command-backed servers to omit both type
	// and enabled. Kilo requires them, so its emission boundary must add the
	// native local/true defaults without losing the source's other fields.
	c, err := FromMCPJSON([]byte(`{
		"mcpServers": {
			"node_repl": {
				"command": "/opt/node",
				"args": ["--interactive"],
				"env": {"MODE": "safe"},
				"startup_timeout_sec": 17
			}
		}
	}`))
	require.NoError(t, err)

	out, err := ToKiloJSONC(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	node := parsed["mcp"].(map[string]any)["node_repl"].(map[string]any)
	require.Equal(t, "local", node["type"], "command-backed Kilo servers default to the local/stdio transport")
	require.Equal(t, true, node["enabled"], "command-backed Kilo servers default to enabled")
	require.Equal(t, []any{"/opt/node", "--interactive"}, node["command"])
	require.Equal(t, map[string]any{"MODE": "safe"}, node["environment"])
	require.Equal(t, float64(17), node["startup_timeout_sec"], "unrelated vendor fields must survive emission")
}

func TestToKiloJSONC_CommandBackedPreservesExplicitRequiredFields(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"disabled": {Other: map[string]any{
			"type":    "stdio",
			"command": "uvx",
			"enabled": false,
			"timeout": float64(2500),
		}},
	}}

	out, err := ToKiloJSONC(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	server := parsed["mcp"].(map[string]any)["disabled"].(map[string]any)
	require.Equal(t, "local", server["type"], "explicit canonical stdio still maps to Kilo local")
	require.Equal(t, false, server["enabled"], "an explicit disabled server must never be re-enabled")
	require.Equal(t, float64(2500), server["timeout"], "vendor fields must remain untouched")
}

func TestToKiloJSONC_NonCommandServerDoesNotInventLocalDefaults(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"remote": {Other: map[string]any{"url": "https://example.test/mcp"}},
	}}

	out, err := ToKiloJSONC(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	server := parsed["mcp"].(map[string]any)["remote"].(map[string]any)
	require.NotContains(t, server, "type", "the compatibility default is intentionally limited to command-backed servers")
	require.NotContains(t, server, "enabled", "the compatibility default is intentionally limited to command-backed servers")
}

func TestToKiloJSONC_ArgsWithoutCommand_PreservesArgs(t *testing.T) {
	// A canonical server can carry args without a command (reachable
	// cross-adapter: a claudecode .mcp.json {"x":{"type":"stdio","args":["--flag"]}}
	// parses to Other{args,type} with no command). The Kilo exporter must not
	// silently drop the args — the claudecode/codex/hermes exporters preserve
	// them verbatim, so Kilo must too.
	c := Canonical{Servers: map[string]Server{
		"x": {Other: map[string]any{"type": "stdio", "args": []any{"--flag"}}},
	}}
	out, err := ToKiloJSONC(c)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	x := parsed["mcp"].(map[string]any)["x"].(map[string]any)
	cmd := x["command"].([]any)
	require.Equal(t, []any{"--flag"}, cmd,
		"args must survive Kilo export even when there is no command")
}

func TestToKiloJSONC_OmitsEmptyEnvironment(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"x": {Other: map[string]any{"type": "http", "url": "https://x"}},
	}}
	out, err := ToKiloJSONC(c)
	require.NoError(t, err)
	require.NotContains(t, string(out), `"environment"`,
		"empty Env must not produce an environment key in the Kilo output")
}

func TestFromKiloJSONC_ToKiloJSONC_SemanticRoundTrip(t *testing.T) {
	in := `{
		"mcp": {
			"gh": {
				"type": "local",
				"command": ["uvx", "mcp-server-github"],
				"environment": {"TOKEN": "abc", "OTHER": "xyz"},
				"enabled": true,
				"timeout": 10000
			},
			"cf": {
				"type": "remote",
				"url": "https://mcp.cloudflare.com",
				"headers": {"Auth": "Bearer x"}
			}
		}
	}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)

	out, err := ToKiloJSONC(c)
	require.NoError(t, err)

	var inObj, outObj map[string]any
	require.NoError(t, json.Unmarshal(StripComments([]byte(in)), &inObj))
	require.NoError(t, json.Unmarshal(out, &outObj))
	require.Equal(t, inObj, outObj,
		"Kilo JSONC → canonical → Kilo JSON must be semantically equal (modulo comments)")
}

func TestFromKiloJSONC_ClaudecodeCompatibleAfterRoundtrip(t *testing.T) {
	in := `{"mcp": {"gh": {"type": "local", "command": ["uvx", "x"], "environment": {"T": "v"}}}}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)

	out, err := ToMCPJSON(c)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	servers := parsed["mcpServers"].(map[string]any)
	gh := servers["gh"].(map[string]any)
	require.Equal(t, "stdio", gh["type"], "kilo 'local' surfaces as 'stdio' in claudecode output")
	require.Equal(t, "uvx", gh["command"], "command splits into string for claudecode")
	require.Equal(t, []any{"x"}, gh["args"])
	env := gh["env"].(map[string]any)
	require.Equal(t, "v", env["T"], "env preserved across the rename")
}
