package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/aplexica/aplexica/internal/syncgate"
)

// Diff summarizes what changed between two Configs in an ApplyReload call.
// Fields ending in "Changed" indicate hot-applied fields (the caller is
// expected to update the live orchestrator / watcher / logger references
// when these are true). RestartRequired lists the human-readable names of
// non-hot fields that changed and need a daemon restart to take effect;
// ApplyReload also emits one INFO log entry per restart-required field.
type Diff struct {
	// Hot-reloadable
	QuietChanged               bool
	GuardWindowChanged         bool
	HermesWatchIntervalChanged bool
	LogLevelChanged            bool

	// SnapshotCadenceChanged is true when ANY of the four per-kind
	// cadence thresholds (Conversation / Memory / Skill / Tool) changed.
	// The SIGHUP handler reads all four and rebuilds the orchestrator's
	// SnapshotCadence map in one shot, so a single boolean suffices for
	// the per-kind family. v0.29.2 replaced v0.29.1's single
	// SnapshotCadenceEventsChanged with this aggregate.
	SnapshotCadenceChanged bool

	// SnapshotMaxAgeChanged is true when ANY of the four per-kind
	// time-based snapshot thresholds (SnapshotMaxAgeConversation/Memory/
	// Skill/Tool) changed. v0.34.0 introduced retention.Runner with a
	// thread-safe SetMaxAge so SIGHUP can hot-apply the new map; the
	// daemon serve loop wires this through to snapRunner.SetMaxAge.
	// Previously this was a restart-required field.
	SnapshotMaxAgeChanged bool

	// SyncChanged is true when the FR-03.3 await-config fan-out gate
	// (Sync.All or any per-agent override) changed. Hot-applied by
	// rebuilding the orchestrator's SyncGate — the same mechanism the
	// portal toggle uses — so `aplexica sync enable` + reload works
	// without a daemon restart.
	SyncChanged bool

	// Restart-required
	DirChanged         bool
	RecursiveChanged   bool
	StateDirChanged    bool
	LogDirChanged      bool
	StoreRootChanged   bool
	SecretsRootChanged bool
	HermesWatchChanged bool
	HermesDBChanged    bool
	RestartRequired    []string
}

// ApplyReload compares current vs next and returns a Diff. It does NOT
// apply the changes itself — the daemon's serve loop owns the
// orchestrator + watcher references that need updating. ApplyReload only:
//  1. Identifies what changed.
//  2. Logs a "restart required" entry per non-hot field that changed.
//
// Hot fields are applied by the caller after ApplyReload returns by
// reading Diff.*Changed booleans and updating the live orchestrator /
// watcher / logger references accordingly.
//
// The hot fields are all wired through to their runtime owners by the
// reload consumer when their live owners exist: LogLevel via
// RotatingLogger.SetLevel (slog.LevelVar), Quiet via
// orch.Debouncer().SetQuietPeriod, GuardWindow via orch.Guard().SetWindow,
// and HermesWatchInterval via hw.SetInterval. The consumer reads the
// *Changed booleans set here to drive that wiring and appends each applied
// field to its applied list; when an owner is absent (e.g. hermeswatch not
// running) it logs the skip. Non-hot fields are reported as
// "restart required" instead.
func ApplyReload(current, next *Config, lg *slog.Logger) Diff {
	d := Diff{}

	// Hot-reloadable
	if current.Quiet != next.Quiet {
		d.QuietChanged = true
	}
	if current.GuardWindow != next.GuardWindow {
		d.GuardWindowChanged = true
	}
	if current.HermesWatchInterval != next.HermesWatchInterval {
		d.HermesWatchIntervalChanged = true
	}
	if current.LogLevel != next.LogLevel {
		d.LogLevelChanged = true
	}
	if current.SnapshotCadenceConversation != next.SnapshotCadenceConversation ||
		current.SnapshotCadenceMemory != next.SnapshotCadenceMemory ||
		current.SnapshotCadenceSkill != next.SnapshotCadenceSkill ||
		current.SnapshotCadenceTool != next.SnapshotCadenceTool {
		d.SnapshotCadenceChanged = true
	}
	if current.SnapshotMaxAgeConversation != next.SnapshotMaxAgeConversation ||
		current.SnapshotMaxAgeMemory != next.SnapshotMaxAgeMemory ||
		current.SnapshotMaxAgeSkill != next.SnapshotMaxAgeSkill ||
		current.SnapshotMaxAgeTool != next.SnapshotMaxAgeTool {
		d.SnapshotMaxAgeChanged = true
	}
	if !syncConfigsEqual(current.Sync, next.Sync) {
		d.SyncChanged = true
	}

	// Restart-required
	if current.Dir != next.Dir {
		d.DirChanged = true
		d.RestartRequired = append(d.RestartRequired, "dir")
	}
	if current.Recursive != next.Recursive {
		d.RecursiveChanged = true
		d.RestartRequired = append(d.RestartRequired, "recursive")
	}
	if current.StateDir != next.StateDir {
		d.StateDirChanged = true
		d.RestartRequired = append(d.RestartRequired, "stateDir")
	}
	if current.LogDir != next.LogDir {
		d.LogDirChanged = true
		d.RestartRequired = append(d.RestartRequired, "logDir")
	}
	if current.StoreRoot != next.StoreRoot {
		d.StoreRootChanged = true
		d.RestartRequired = append(d.RestartRequired, "storeRoot")
	}
	if current.SecretsRoot != next.SecretsRoot {
		d.SecretsRootChanged = true
		d.RestartRequired = append(d.RestartRequired, "secretsRoot")
	}
	if current.HermesWatch != next.HermesWatch {
		d.HermesWatchChanged = true
		d.RestartRequired = append(d.RestartRequired, "hermesWatch")
	}
	if current.HermesDB != next.HermesDB {
		d.HermesDBChanged = true
		d.RestartRequired = append(d.RestartRequired, "hermesDB")
	}

	for _, field := range d.RestartRequired {
		lg.Info("config field changed but restart required to apply", "field", field)
	}
	return d
}

// LoadConfigOverlay reads the config file at path and overlays it on a COPY
// of base: fields present in the file override, fields absent keep base's
// values. A missing file returns base unchanged.
//
// This exists because <state-dir>/config.json is SPARSE — `aplexica setup`
// and the CLI mutators persist only the fields they own (logLevel, tray,
// web, remote, sync), while runtime tunables (quiet, guardWindow, hermes*,
// snapshot*) usually arrive via flags and are never written back. The
// reload paths must diff the file against the RUNNING config; loading the
// sparse file bare (LoadConfig) made every absent field look like a change
// to its zero value, so a reload would hot-apply quiet=0 / guardWindow=0 /
// cadence=0 and log phantom "restart required" entries for dir/store/etc.
//
// The "sync" section is replaced wholesale when present (JSON map
// unmarshalling otherwise MERGES into base's map, resurrecting deleted
// per-agent overrides).
func LoadConfigOverlay(path string, base Config) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := base
		return &c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("daemon: read config %s: %w", path, err)
	}
	// Deep-copy base via a JSON round-trip: a plain struct copy shares
	// base's maps/pointers (Sync.Agents, Web.Enabled, …), and the file
	// unmarshal below would write through them — mutating the CALLER's
	// baseline so the subsequent diff silently sees "no change".
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("daemon: snapshot base config: %w", err)
	}
	var next Config
	if err := json.Unmarshal(baseJSON, &next); err != nil {
		return nil, fmt.Errorf("daemon: snapshot base config: %w", err)
	}
	if err := json.Unmarshal(data, &next); err != nil {
		return nil, fmt.Errorf("daemon: parse config %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("daemon: parse config %s: %w", path, err)
	}
	if syncRaw, ok := raw["sync"]; ok {
		next.Sync = SyncConfig{}
		if err := json.Unmarshal(syncRaw, &next.Sync); err != nil {
			return nil, fmt.Errorf("daemon: parse config %s sync section: %w", path, err)
		}
	}
	return &next, nil
}

// syncConfigsEqual reports whether two SyncConfigs gate identically.
// A nil Agents map and an empty one are equivalent.
func syncConfigsEqual(a, b SyncConfig) bool {
	if a.All != b.All || len(a.Agents) != len(b.Agents) || len(a.ConvBackfill) != len(b.ConvBackfill) {
		return false
	}
	for name, v := range a.Agents {
		if bv, ok := b.Agents[name]; !ok || bv != v {
			return false
		}
	}
	for name, v := range a.ConvBackfill {
		if bv, ok := b.ConvBackfill[name]; !ok || bv != v {
			return false
		}
	}
	return true
}

// SyncEnablementExpanded reports whether next allows fan-out to any agent
// that prev denied. Used by the reload paths to decide whether a backfill
// (RefanOutAll) is needed: the gate alone only affects FUTURE events, so a
// newly-enabled agent needs existing artifacts re-fanned (mirrors the
// portal toggle's backfill-on-enable). The agent namespace is open-ended,
// so this checks the All flip plus every name mentioned in either map.
func SyncEnablementExpanded(prev, next SyncConfig) bool {
	if !prev.All && next.All {
		return true
	}
	pg := syncgate.New(syncgate.Config{All: prev.All, Agents: prev.Agents})
	ng := syncgate.New(syncgate.Config{All: next.All, Agents: next.Agents})
	for name := range prev.Agents {
		if ng.Enabled(name) && !pg.Enabled(name) {
			return true
		}
	}
	for name := range next.Agents {
		if ng.Enabled(name) && !pg.Enabled(name) {
			return true
		}
	}
	return false
}

// ParseLogLevel maps a config-file LogLevel string to slog.Level. Accepted
// values: "debug" | "info" | "warn" | "error". Unknown / empty values
// fall back to slog.LevelInfo (the daemon's default).
func ParseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
