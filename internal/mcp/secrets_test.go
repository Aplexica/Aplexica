package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractSecrets_MovesEnvValuesToMap(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {Env: map[string]string{"TOKEN": "real-secret", "OTHER": "another"}},
		"cf": {Other: map[string]any{"type": "http", "url": "https://x"}},
	}}
	secrets := ExtractSecrets(&c)
	require.Equal(t, map[string]string{
		"gh.TOKEN": "real-secret",
		"gh.OTHER": "another",
	}, secrets)

	require.Equal(t, "${secret:gh.TOKEN}", c.Servers["gh"].Env["TOKEN"])
	require.Equal(t, "${secret:gh.OTHER}", c.Servers["gh"].Env["OTHER"])
	require.Empty(t, c.Servers["cf"].Env)
}

func TestExtractSecrets_NoEnv(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"x": {Other: map[string]any{"type": "http", "url": "https://x"}},
	}}
	secrets := ExtractSecrets(&c)
	require.Empty(t, secrets)
}

func TestExpandSecrets_ReplacesPlaceholders(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {Env: map[string]string{"TOKEN": "${secret:gh.TOKEN}"}},
	}}
	err := ExpandSecrets(&c, map[string]string{"gh.TOKEN": "real-secret"})
	require.NoError(t, err)
	require.Equal(t, "real-secret", c.Servers["gh"].Env["TOKEN"])
}

func TestExpandSecrets_MissingSecretIsError(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {Env: map[string]string{"TOKEN": "${secret:gh.TOKEN}"}},
	}}
	err := ExpandSecrets(&c, map[string]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "gh.TOKEN")
}

func TestExpandSecrets_MissingSecretsAreSorted(t *testing.T) {
	// Multiple servers/keys missing their secrets: the error message must list
	// the names in deterministic (sorted) order regardless of map iteration.
	c := Canonical{Servers: map[string]Server{
		"zebra": {Env: map[string]string{"TOKEN": "${secret:zebra.TOKEN}"}},
		"alpha": {Env: map[string]string{"KEY": "${secret:alpha.KEY}"}},
		"mid":   {Env: map[string]string{"VAL": "${secret:mid.VAL}"}},
	}}
	err := ExpandSecrets(&c, map[string]string{})
	require.Error(t, err)
	require.Equal(t, "mcp: missing secrets: [alpha.KEY mid.VAL zebra.TOKEN]", err.Error(),
		"missing-secret names must be sorted for a deterministic error message")
}

func TestExpandSecrets_LeavesNonPlaceholderValuesAlone(t *testing.T) {
	c := Canonical{Servers: map[string]Server{
		"gh": {Env: map[string]string{
			"TOKEN": "${secret:gh.TOKEN}",
			"PLAIN": "not-a-secret",
		}},
	}}
	err := ExpandSecrets(&c, map[string]string{"gh.TOKEN": "real-secret"})
	require.NoError(t, err)
	require.Equal(t, "real-secret", c.Servers["gh"].Env["TOKEN"])
	require.Equal(t, "not-a-secret", c.Servers["gh"].Env["PLAIN"])
}

func TestExtractSecrets_ExpandSecrets_RoundTrip(t *testing.T) {
	original := Canonical{Servers: map[string]Server{
		"gh": {Env: map[string]string{"TOKEN": "shh"}, Other: map[string]any{"type": "stdio"}},
	}}
	c := Canonical{Servers: map[string]Server{
		"gh": {Env: map[string]string{"TOKEN": "shh"}, Other: map[string]any{"type": "stdio"}},
	}}
	secrets := ExtractSecrets(&c)
	require.NoError(t, ExpandSecrets(&c, secrets))
	require.Equal(t, original, c, "extract+expand must round-trip to the original canonical")
}
