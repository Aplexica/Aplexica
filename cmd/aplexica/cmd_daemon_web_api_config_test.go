package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/stretchr/testify/require"
)

// TestConfigPatch_DurationGetPatchRoundTrip asserts the GET->PATCH
// round-trip preserves time.Duration config fields. GET serializes a
// time.Duration as a nanosecond integer; PATCH must read that same wire
// shape back without rescaling. The naive "treat numbers as seconds"
// reader multiplied stored max-ages by ~1e9 on a read-modify-write.
func TestConfigPatch_DurationGetPatchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	const conv = 7 * 24 * time.Hour
	const hermes = 5 * time.Second
	require.NoError(t, daemon.WriteConfig(cfgPath, &daemon.Config{
		SnapshotMaxAgeConversation: conv,
		HermesWatchInterval:        hermes,
	}))

	acc := &configWebAccessor{deps: &webAPIDeps{configPath: cfgPath}}

	// GET: load + marshal to the JSON wire shape the SPA would receive.
	got, err := acc.Load()
	require.NoError(t, err)
	wire, err := json.Marshal(got)
	require.NoError(t, err)
	var asMap map[string]any
	require.NoError(t, json.Unmarshal(wire, &asMap))

	// PATCH: feed the same field values straight back (read-modify-write).
	require.NoError(t, acc.Patch(map[string]any{
		"snapshotMaxAgeConversation": asMap["snapshotMaxAgeConversation"],
		"hermesWatchInterval":        asMap["hermesWatchInterval"],
	}))

	after, err := acc.Load()
	require.NoError(t, err)
	require.Equal(t, conv, after.SnapshotMaxAgeConversation,
		"GET->PATCH must not rescale the conversation max-age")
	require.Equal(t, hermes, after.HermesWatchInterval,
		"GET->PATCH must not rescale the hermes watch interval")
}

// TestDurationFromAny_HumanStringStillWorks keeps the ergonomic
// human-readable string path intact.
func TestDurationFromAny_HumanStringStillWorks(t *testing.T) {
	d, ok := durationFromAny("90m")
	require.True(t, ok)
	require.Equal(t, 90*time.Minute, d)
}
