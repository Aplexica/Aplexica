package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestTrayEnabledDefault_OnForAllPlatforms pins the product decision that
// the system tray ships ENABLED by default on every OS (overriding the
// original BRD-03 §4.9.4 opt-in-on-macOS/Linux). A user can still opt out
// per box via tray.enabled=false.
func TestTrayEnabledDefault_OnForAllPlatforms(t *testing.T) {
	require.True(t, TrayEnabledDefault(),
		"tray must default ON on all OSes (GOOS=%s)", runtime.GOOS)
}

func TestConfig_LoadMissingReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Empty(t, cfg.Dir, "missing file -> defaults (empty/zero values)")
}

func TestConfig_WriteLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	in := &Config{
		Dir:                 "/home/u/proj",
		StateDir:            "/home/u/.aplexica/state",
		LogDir:              "/home/u/.aplexica/log",
		StoreRoot:           "/home/u/.aplexica/store",
		SecretsRoot:         "/home/u/.aplexica/secrets",
		Quiet:               750 * time.Millisecond,
		GuardWindow:         6 * time.Second,
		Recursive:           true,
		HermesWatch:         true,
		HermesWatchInterval: 10 * time.Second,
		HermesDB:            "/home/u/.hermes/state.db",
		LogLevel:            "debug",
	}
	require.NoError(t, WriteConfig(path, in))

	out, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, in.Dir, out.Dir)
	require.Equal(t, in.Quiet, out.Quiet)
	require.Equal(t, in.LogLevel, out.LogLevel)
	require.Equal(t, in.HermesWatchInterval, out.HermesWatchInterval)
}

func TestConfig_LoadInvalidJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))
	_, err := LoadConfig(path)
	require.Error(t, err)
}

// TestTrayEnabled_NilConfig_UsesPlatformDefault asserts the fallback
// path: when Config is nil or Tray.Enabled is unset, TrayEnabled
// returns TrayEnabledDefault() for every OS.
func TestTrayEnabled_NilConfig_UsesPlatformDefault(t *testing.T) {
	want := TrayEnabledDefault()
	require.Equal(t, want, TrayEnabled(nil))
	require.Equal(t, want, TrayEnabled(&Config{}))
}

// TestTrayEnabled_ExplicitOverridesDefault asserts that an explicit
// user choice (Enabled = &true / &false) supersedes the per-platform
// default — both directions.
func TestTrayEnabled_ExplicitOverridesDefault(t *testing.T) {
	tru, fal := true, false
	require.True(t, TrayEnabled(&Config{Tray: TrayConfig{Enabled: &tru}}))
	require.False(t, TrayEnabled(&Config{Tray: TrayConfig{Enabled: &fal}}))
}

// TestConfig_TrayRoundTrip verifies the Tray.Enabled tri-state
// survives a write→load round-trip.
func TestConfig_TrayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	tru := true
	in := &Config{Tray: TrayConfig{Enabled: &tru}}
	require.NoError(t, WriteConfig(path, in))
	out, err := LoadConfig(path)
	require.NoError(t, err)
	require.NotNil(t, out.Tray.Enabled)
	require.True(t, *out.Tray.Enabled)
}

// TestConfig_RepairForkedMirrorsDefaultsOff pins the Stage-B gate: the
// forked-mirror rebuild authorizes a whole-file rewrite of a session Claude
// Code co-owns, so a config file that never mentions it — including every
// config file written before the key existed — must leave it off.
func TestConfig_RepairForkedMirrorsDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"sync":{"all":true}}`), 0o600))
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.True(t, cfg.Sync.All)
	require.False(t, cfg.Sync.RepairForkedMirrors,
		"an absent sync.repairForkedMirrors must never read as enabled")

	// Nor may enabling it be a side effect of writing any other sync key back.
	require.NoError(t, WriteConfig(path, cfg))
	reloaded, err := LoadConfig(path)
	require.NoError(t, err)
	require.False(t, reloaded.Sync.RepairForkedMirrors)
}

// TestConfig_RepairForkedMirrorsRoundTrip verifies the opt-in survives a
// write→load round-trip under the documented JSON key.
func TestConfig_RepairForkedMirrorsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, WriteConfig(path, &Config{
		Sync: SyncConfig{RepairForkedMirrors: true},
	}))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"repairForkedMirrors": true`)
	out, err := LoadConfig(path)
	require.NoError(t, err)
	require.True(t, out.Sync.RepairForkedMirrors)
}
