package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonical_EncodeDecode_RoundTrip(t *testing.T) {
	in := Canonical{
		Servers: map[string]Server{
			"gh": {
				Env: map[string]string{"TOKEN": "abc"},
				Other: map[string]any{
					"type":    "stdio",
					"command": "uvx",
					"args":    []any{"mcp-server-github"},
				},
			},
			"cf": {
				Other: map[string]any{
					"type": "http",
					"url":  "https://mcp.cloudflare.com",
				},
			},
		},
	}
	b, err := Encode(in)
	require.NoError(t, err)

	out, err := Decode(b)
	require.NoError(t, err)
	require.Equal(t, in.Servers["gh"].Env, out.Servers["gh"].Env)
	require.Equal(t, "stdio", out.Servers["gh"].Other["type"])
	require.Equal(t, "uvx", out.Servers["gh"].Other["command"])
	require.Equal(t, "http", out.Servers["cf"].Other["type"])
}

func TestCanonical_DecodeRejectsUnknownTopLevel(t *testing.T) {
	bad := `{"servers":{"x":{"env":{"K":"v"}}},"unexpected":42}`
	_, err := Decode([]byte(bad))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected")
}

func TestCanonical_DecodeEmptyServers(t *testing.T) {
	c, err := Decode([]byte(`{"servers":{}}`))
	require.NoError(t, err)
	require.Empty(t, c.Servers)
}

func TestCanonical_EncodeProducesStableJSON(t *testing.T) {
	in := Canonical{Servers: map[string]Server{"a": {Env: map[string]string{"K": "v"}}}}
	b, err := Encode(in)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(b, &parsed))
	require.Contains(t, parsed, "servers")
}

func TestCanonical_ServerOmitsEmptyEnvAndOther(t *testing.T) {
	in := Canonical{Servers: map[string]Server{"bare": {}}}
	b, err := Encode(in)
	require.NoError(t, err)
	require.NotContains(t, string(b), `"env"`,
		"empty env must be omitted from the JSON output (omitempty)")
	require.NotContains(t, string(b), `"other"`,
		"empty other must be omitted from the JSON output (omitempty)")
}
