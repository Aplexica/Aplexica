package mcp

import (
	"encoding/json"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// These tests pin the lossless-replication contract for env values that are
// NOT strings. YAML (Hermes), TOML (Codex), and JSON5 (OpenClaw/Kilo) all
// naturally yield int/bool/float for unquoted scalars, so an env block like
// `env: { MAX_TOKENS: 4096 }` or `env: { DEBUG: true }` must survive import +
// export rather than being silently dropped.

func TestFromMCPJSON_PreservesNonStringEnv(t *testing.T) {
	in := `{"mcpServers":{"gh":{"type":"stdio","command":"uvx","env":{"TOKEN":"abc","MAX_TOKENS":4096,"DEBUG":true}}}}`
	c, err := FromMCPJSON([]byte(in))
	require.NoError(t, err)

	out, err := ToMCPJSON(c)
	require.NoError(t, err)

	gotEnv := envFromMCPJSON(t, out, "gh")
	require.Equal(t, "abc", gotEnv["TOKEN"], "string env values still survive")
	require.EqualValues(t, 4096, gotEnv["MAX_TOKENS"], "numeric env value must not be dropped")
	require.Equal(t, true, gotEnv["DEBUG"], "boolean env value must not be dropped")
}

func TestFromMCPTOML_PreservesNonStringEnv(t *testing.T) {
	in := "[mcp_servers.gh]\ncommand = \"uvx\"\ntype = \"stdio\"\n[mcp_servers.gh.env]\nTOKEN = \"abc\"\nMAX_TOKENS = 4096\nDEBUG = true\n"
	c, err := FromMCPTOML([]byte(in))
	require.NoError(t, err)

	out, err := ToMCPTOML(c)
	require.NoError(t, err)

	var inObj, outObj map[string]any
	require.NoError(t, toml.Unmarshal([]byte(in), &inObj))
	require.NoError(t, toml.Unmarshal(out, &outObj))
	require.Equal(t, inObj["mcp_servers"], outObj["mcp_servers"],
		"non-string env values (int+bool) must round-trip through TOML identically")
}

func TestFromHermesYAML_PreservesNonStringEnv(t *testing.T) {
	in := `
mcp_servers:
  gh:
    command: uvx
    env:
      TOKEN: abc
      MAX_TOKENS: 4096
      DEBUG: true
`
	c, err := FromHermesYAML([]byte(in))
	require.NoError(t, err)

	out, err := ToHermesYAML(c)
	require.NoError(t, err)

	var inObj, outObj map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(in), &inObj))
	require.NoError(t, yaml.Unmarshal(out, &outObj))
	require.Equal(t, inObj["mcp_servers"], outObj["mcp_servers"],
		"non-string env values (int+bool) must round-trip through Hermes YAML identically")
}

func TestFromOpenClawJSON_PreservesNonStringEnv(t *testing.T) {
	// JSON5 with an unquoted numeric + bool env value (OpenClaw reads json5).
	in := `{
		"mcp": {
			"servers": {
				"gh": {
					"type": "stdio",
					"command": "uvx",
					"env": {"TOKEN": "abc", "MAX_TOKENS": 4096, "DEBUG": true}
				}
			}
		}
	}`
	c, err := FromOpenClawJSON([]byte(in))
	require.NoError(t, err)

	out, err := ToOpenClawJSON(c)
	require.NoError(t, err)

	gotEnv := envFromOpenClawJSON(t, out, "gh")
	require.Equal(t, "abc", gotEnv["TOKEN"])
	require.EqualValues(t, 4096, gotEnv["MAX_TOKENS"], "numeric env value must not be dropped")
	require.Equal(t, true, gotEnv["DEBUG"], "boolean env value must not be dropped")
}

func TestFromKiloJSONC_PreservesNonStringEnv(t *testing.T) {
	in := `{
		"mcp": {
			"gh": {
				"type": "local",
				"command": ["uvx"],
				"environment": {"TOKEN": "abc", "MAX_TOKENS": 4096, "DEBUG": true}
			}
		}
	}`
	c, err := FromKiloJSONC([]byte(in))
	require.NoError(t, err)

	out, err := ToKiloJSONC(c)
	require.NoError(t, err)

	gotEnv := envFromKiloJSONC(t, out, "gh")
	require.Equal(t, "abc", gotEnv["TOKEN"])
	require.EqualValues(t, 4096, gotEnv["MAX_TOKENS"], "numeric env value must not be dropped")
	require.Equal(t, true, gotEnv["DEBUG"], "boolean env value must not be dropped")
}

// TestSplitEnv_NonStringDoesNotReachSecrets guards ADR-0027: ExtractSecrets
// only handles the typed string Env, so preserved non-string env values must
// NOT be lifted into the secrets store.
func TestSplitEnv_NonStringDoesNotReachSecrets(t *testing.T) {
	in := `{"mcpServers":{"gh":{"command":"uvx","env":{"TOKEN":"abc","MAX_TOKENS":4096}}}}`
	c, err := FromMCPJSON([]byte(in))
	require.NoError(t, err)

	secrets := ExtractSecrets(&c)
	require.Contains(t, secrets, "gh.TOKEN")
	require.Equal(t, "abc", secrets["gh.TOKEN"])
	require.NotContains(t, secrets, "gh.MAX_TOKENS",
		"non-string env values must not be treated as secrets")

	// After redaction, export must still carry the non-string value verbatim.
	out, err := ToMCPJSON(c)
	require.NoError(t, err)
	gotEnv := envFromMCPJSON(t, out, "gh")
	require.EqualValues(t, 4096, gotEnv["MAX_TOKENS"])
	require.Equal(t, "${secret:gh.TOKEN}", gotEnv["TOKEN"])
}

// --- helpers ---

func envFromMCPJSON(t *testing.T, b []byte, server string) map[string]any {
	t.Helper()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(b, &parsed))
	servers := parsed["mcpServers"].(map[string]any)
	srv := servers[server].(map[string]any)
	env, ok := srv["env"].(map[string]any)
	require.True(t, ok, "server %q must have an env block in the output", server)
	return env
}

func envFromOpenClawJSON(t *testing.T, b []byte, server string) map[string]any {
	t.Helper()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(b, &parsed))
	mcp := parsed["mcp"].(map[string]any)
	servers := mcp["servers"].(map[string]any)
	srv := servers[server].(map[string]any)
	env, ok := srv["env"].(map[string]any)
	require.True(t, ok, "server %q must have an env block in the output", server)
	return env
}

func envFromKiloJSONC(t *testing.T, b []byte, server string) map[string]any {
	t.Helper()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(b, &parsed))
	mcp := parsed["mcp"].(map[string]any)
	srv := mcp[server].(map[string]any)
	env, ok := srv["environment"].(map[string]any)
	require.True(t, ok, "server %q must have an environment block in the output", server)
	return env
}
