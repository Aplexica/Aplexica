package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// runConfigCmd invokes `aplexica config …` via rootCmd. The test fixture
// pins --user-path / --system-path / --project-path to the supplied
// values so the real ~/.aplexica/config.toml is never touched.
func runConfigCmd(t *testing.T, userPath string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	// Always inject --user-path so tests are hermetic.
	full := append([]string{"config", "--user-path", userPath}, args...)
	rootCmd.SetArgs(full)
	t.Cleanup(func() {
		configSystemPath = ""
		configUserPath = ""
		configProjectPath = ""
		configShowKey = ""
		configSetLayer = "user"
		configUnsetLayer = "user"
		configCLISets = nil
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestConfigShow_DefaultsOnly(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml") // doesn't exist
	out, err := runConfigCmd(t, userPath, "show")
	require.NoError(t, err)
	// Spot-check a few shipped keys appear with provenance = shipped.
	require.Contains(t, out, "daemon.project_scan_interval")
	require.Contains(t, out, "shipped")
}

func TestConfigShow_KeyFilter(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	out, err := runConfigCmd(t, userPath,
		"show", "--key", "retention.snapshot_min_events")
	require.NoError(t, err)
	require.Contains(t, out, "retention.snapshot_min_events = 100")
	require.Contains(t, out, "from shipped")
}

func TestConfigShow_UnknownKey(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	_, err := runConfigCmd(t, userPath, "show", "--key", "no.such.key")
	require.Error(t, err)
}

func TestConfigSet_RoundTripsThroughShow(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")

	_, err := runConfigCmd(t, userPath,
		"set", "daemon.project_scan_interval", "30m")
	require.NoError(t, err)

	out, err := runConfigCmd(t, userPath,
		"show", "--key", "daemon.project_scan_interval")
	require.NoError(t, err)
	require.Contains(t, out, "= 30m")
	require.Contains(t, out, "from user")
}

func TestConfigSet_BadKey(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	_, err := runConfigCmd(t, userPath, "set", "no-dot-here", "x")
	require.Error(t, err)
}

func TestConfigUnset_RestoresShipped(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")

	// First override, then unset.
	_, err := runConfigCmd(t, userPath,
		"set", "daemon.project_scan_interval", "30m")
	require.NoError(t, err)
	_, err = runConfigCmd(t, userPath,
		"unset", "daemon.project_scan_interval")
	require.NoError(t, err)

	out, err := runConfigCmd(t, userPath,
		"show", "--key", "daemon.project_scan_interval")
	require.NoError(t, err)
	require.Contains(t, out, "from shipped",
		"after unset, provenance must fall back to shipped")
}

func TestConfigDiff_NoUserOverrides(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml") // empty/missing
	out, err := runConfigCmd(t, userPath, "diff")
	require.NoError(t, err)
	require.Contains(t, out, "0 keys differ from shipped defaults")
}

func TestConfigDiff_WithOverride(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	_, err := runConfigCmd(t, userPath,
		"set", "tray.refresh_interval", "1s")
	require.NoError(t, err)

	out, err := runConfigCmd(t, userPath, "diff")
	require.NoError(t, err)
	require.Contains(t, out, "tray.refresh_interval")
	require.Contains(t, out, "1 keys differ")
}

func TestConfigValidate_GoodFile(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	good := filepath.Join(tmp, "good.toml")
	require.NoError(t, os.WriteFile(good,
		[]byte("[daemon]\nproject_scan_interval = \"5m\"\n"), 0o644))

	out, err := runConfigCmd(t, userPath, "validate", good)
	require.NoError(t, err)
	require.Contains(t, out, "ok")
}

func TestConfigValidate_BadFile(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	bad := filepath.Join(tmp, "bad.toml")
	require.NoError(t, os.WriteFile(bad,
		[]byte("not valid toml [[["), 0o644))

	_, err := runConfigCmd(t, userPath, "validate", bad)
	require.Error(t, err)
}

// ─────────────────────────────────────────────────────────────────────
// v0.70.0 — schema, docs, --config-set
// ─────────────────────────────────────────────────────────────────────

func TestConfigSchema_EmitsJSON(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	out, err := runConfigCmd(t, userPath, "schema")
	require.NoError(t, err)
	// Spot-check JSON structure.
	require.Contains(t, out, `"key": "daemon.project_scan_interval"`)
	require.Contains(t, out, `"type": "duration"`)
	require.Contains(t, out, `"default": "60m"`)
}

func TestConfigDocs_EmitsMarkdown(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	out, err := runConfigCmd(t, userPath, "docs")
	require.NoError(t, err)
	require.Contains(t, out, "# Aplexica configuration schema")
	require.Contains(t, out, "| Key | Type |")
}

func TestConfigValidate_RangeViolation(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	bad := filepath.Join(tmp, "user-bad.toml")
	require.NoError(t, os.WriteFile(bad,
		[]byte("[daemon]\nproject_scan_max_depth = 99\n"), 0o644))
	_, err := runConfigCmd(t, userPath, "validate", bad)
	require.Error(t, err, "out-of-range value must be rejected")
}

func TestConfigValidate_UnknownKeyWarnsButPasses(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	f := filepath.Join(tmp, "user-typo.toml")
	require.NoError(t, os.WriteFile(f,
		[]byte("[daemon]\nproject_scan_interval = \"5m\"\ntypo_key = 42\n"), 0o644))
	out, err := runConfigCmd(t, userPath, "validate", f)
	require.NoError(t, err, "unknown key is a warning per BRD-10 §10.2")
	require.Contains(t, out, "WARN")
	require.Contains(t, out, "typo_key")
}

func TestConfigShow_CLIOverrideBeatsUser(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	_, err := runConfigCmd(t, userPath,
		"set", "tray.refresh_interval", "3s")
	require.NoError(t, err)

	out, err := runConfigCmd(t, userPath,
		"-c", "tray.refresh_interval=2s",
		"show", "--key", "tray.refresh_interval")
	require.NoError(t, err)
	require.Contains(t, out, "= 2s")
	require.Contains(t, out, "from cli")
}
