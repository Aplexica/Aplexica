package daemon

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type captureLogger struct {
	*slog.Logger
	buf *bytes.Buffer
}

func newCaptureLogger() *captureLogger {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	return &captureLogger{Logger: slog.New(h), buf: buf}
}

func TestApplyReload_HotFieldsApply(t *testing.T) {
	current := &Config{Quiet: 500 * time.Millisecond, GuardWindow: 5 * time.Second}
	next := &Config{Quiet: 750 * time.Millisecond, GuardWindow: 5 * time.Second, HermesWatchInterval: 10 * time.Second}

	lg := newCaptureLogger()
	diff := ApplyReload(current, next, lg.Logger)
	require.True(t, diff.QuietChanged)
	require.False(t, diff.GuardWindowChanged)
	require.True(t, diff.HermesWatchIntervalChanged)
	require.Empty(t, diff.RestartRequired, "all changed fields are hot-reloadable")
}

func TestApplyReload_NonHotFieldsRequireRestart(t *testing.T) {
	current := &Config{Dir: "/old"}
	next := &Config{Dir: "/new"}
	lg := newCaptureLogger()
	diff := ApplyReload(current, next, lg.Logger)
	require.Contains(t, diff.RestartRequired, "dir")
	require.Contains(t, lg.buf.String(), "restart required")
}

func TestApplyReload_NoChangeNoLog(t *testing.T) {
	current := &Config{Dir: "/same"}
	next := &Config{Dir: "/same"}
	lg := newCaptureLogger()
	diff := ApplyReload(current, next, lg.Logger)
	require.False(t, diff.DirChanged)
	require.Empty(t, diff.RestartRequired)
}

func TestApplyReload_SyncGateChangeIsHot(t *testing.T) {
	current := &Config{Sync: SyncConfig{All: false, Agents: map[string]bool{"hermes": false}}}
	next := &Config{Sync: SyncConfig{All: false, Agents: map[string]bool{"hermes": true}}}
	lg := newCaptureLogger()
	diff := ApplyReload(current, next, lg.Logger)
	require.True(t, diff.SyncChanged, "per-agent gate flip must be detected")
	require.Empty(t, diff.RestartRequired, "sync gate is hot-reloadable, not restart-required")
}

func TestApplyReload_SyncAllFlipIsHot(t *testing.T) {
	current := &Config{Sync: SyncConfig{All: false}}
	next := &Config{Sync: SyncConfig{All: true}}
	lg := newCaptureLogger()
	diff := ApplyReload(current, next, lg.Logger)
	require.True(t, diff.SyncChanged)
}

func TestApplyReload_SyncNilVsEmptyAgentsIsNoChange(t *testing.T) {
	current := &Config{Sync: SyncConfig{Agents: nil}}
	next := &Config{Sync: SyncConfig{Agents: map[string]bool{}}}
	lg := newCaptureLogger()
	diff := ApplyReload(current, next, lg.Logger)
	require.False(t, diff.SyncChanged, "nil and empty agent maps are equivalent")
}

func TestSyncEnablementExpanded(t *testing.T) {
	cases := []struct {
		name string
		prev SyncConfig
		next SyncConfig
		want bool
	}{
		{"agent enabled", SyncConfig{}, SyncConfig{Agents: map[string]bool{"hermes": true}}, true},
		{"agent disabled", SyncConfig{Agents: map[string]bool{"hermes": true}}, SyncConfig{Agents: map[string]bool{"hermes": false}}, false},
		{"all flipped on", SyncConfig{}, SyncConfig{All: true}, true},
		{"all flipped off", SyncConfig{All: true}, SyncConfig{}, false},
		{"no change", SyncConfig{Agents: map[string]bool{"kilo": true}}, SyncConfig{Agents: map[string]bool{"kilo": true}}, false},
		{"false override removed under all", SyncConfig{All: true, Agents: map[string]bool{"hermes": false}}, SyncConfig{All: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, SyncEnablementExpanded(tc.prev, tc.next))
		})
	}
}

func TestLoadConfigOverlay_AbsentFieldsKeepBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Sparse file: only logLevel + sync — the shape `aplexica setup` +
	// `aplexica sync enable` actually persist. No quiet/guardWindow/etc.
	require.NoError(t, os.WriteFile(path, []byte(`{"logLevel":"debug","sync":{"agents":{"hermes":true}}}`), 0o644))

	base := Config{
		Quiet:                 500 * time.Millisecond,
		GuardWindow:           5 * time.Second,
		HermesWatchInterval:   5 * time.Second,
		HermesDB:              "/home/u/.hermes/state.db",
		Dir:                   "/watched",
		SnapshotCadenceMemory: 50,
	}
	got, err := LoadConfigOverlay(path, base)
	require.NoError(t, err)
	require.Equal(t, "debug", got.LogLevel, "present field overrides")
	require.Equal(t, map[string]bool{"hermes": true}, got.Sync.Agents)
	require.Equal(t, 500*time.Millisecond, got.Quiet, "absent field keeps base — NOT zero")
	require.Equal(t, 5*time.Second, got.GuardWindow)
	require.Equal(t, 5*time.Second, got.HermesWatchInterval)
	require.Equal(t, "/home/u/.hermes/state.db", got.HermesDB)
	require.Equal(t, "/watched", got.Dir)
	require.Equal(t, 50, got.SnapshotCadenceMemory)
}

func TestLoadConfigOverlay_SyncReplacedWholesaleWhenPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"sync":{"agents":{"kilo":true}}}`), 0o644))

	base := Config{Sync: SyncConfig{All: true, Agents: map[string]bool{"hermes": false, "stale": true}}}
	got, err := LoadConfigOverlay(path, base)
	require.NoError(t, err)
	require.False(t, got.Sync.All, "sync object in file replaces the whole section")
	require.Equal(t, map[string]bool{"kilo": true}, got.Sync.Agents, "no stale merge leftovers")
}

func TestLoadConfigOverlay_NoSyncKeyKeepsBaseSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"logLevel":"warn"}`), 0o644))

	base := Config{Sync: SyncConfig{Agents: map[string]bool{"codex": true}}}
	got, err := LoadConfigOverlay(path, base)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"codex": true}, got.Sync.Agents)
}

func TestLoadConfigOverlay_MissingFileReturnsBase(t *testing.T) {
	base := Config{Quiet: time.Second}
	got, err := LoadConfigOverlay(filepath.Join(t.TempDir(), "nope.json"), base)
	require.NoError(t, err)
	require.Equal(t, base, *got)
}

// Regression: LoadConfigOverlay must never mutate the caller's base —
// next := base shallow-copied Sync.Agents, so unmarshalling the file
// merged new agents into the BASELINE map; the subsequent diff compared
// the polluted baseline against the file and reported "no change".
func TestLoadConfigOverlay_DoesNotMutateBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"sync":{"agents":{"claude-code":true,"openclaw":true}}}`), 0o644))

	base := Config{Sync: SyncConfig{Agents: map[string]bool{"claude-code": true}}}
	got, err := LoadConfigOverlay(path, base)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"claude-code": true, "openclaw": true}, got.Sync.Agents)
	require.Equal(t, map[string]bool{"claude-code": true}, base.Sync.Agents,
		"base must be untouched — a polluted baseline makes the reload diff a no-op")
	require.False(t, syncConfigsEqual(base.Sync, got.Sync),
		"diff against the original base must register the change")
}
