package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/syncgate"
)

// Config is the JSON representation of the daemon's runtime configuration.
// Empty/zero fields mean "use the CLI flag value or the compiled-in default."
//
// File path convention: <state-dir>/config.json. Atomic via atomicfile.
//
// Precedence (lowest -> highest): compiled-in defaults, file values, CLI
// flags explicitly set by the user. The daemon's serve command applies
// this precedence at startup; SIGHUP re-reads the file and re-applies the
// hot-reloadable subset.
type Config struct {
	Dir                 string        `json:"dir,omitempty"`
	StateDir            string        `json:"stateDir,omitempty"`
	LogDir              string        `json:"logDir,omitempty"`
	StoreRoot           string        `json:"storeRoot,omitempty"`
	SecretsRoot         string        `json:"secretsRoot,omitempty"`
	Quiet               time.Duration `json:"quiet,omitempty"`
	GuardWindow         time.Duration `json:"guardWindow,omitempty"`
	Recursive           bool          `json:"recursive,omitempty"`
	HermesWatch         bool          `json:"hermesWatch,omitempty"`
	HermesWatchInterval time.Duration `json:"hermesWatchInterval,omitempty"`
	HermesDB            string        `json:"hermesDB,omitempty"`
	LogLevel            string        `json:"logLevel,omitempty"` // "debug" | "info" | "warn" | "error"; empty = "info"

	// SnapshotCadenceConversation / Memory / Skill / Tool — per-kind
	// event-count snapshot thresholds. After each primary import, when an
	// artifact's total event count crosses its kind's threshold
	// (count % threshold == 0), the orchestrator calls
	// retention.CreateSnapshot. 0 = disabled for that kind. threshold=1
	// snapshots after EVERY event for that kind (semantically valid, just
	// inefficient).
	//
	// BRD-03 §4.8.1 defaults applied by the daemon CLI wiring (not by this
	// package): conversation=100, memory=50, skill=50, tool=50.
	//
	// v0.29.2 replaced v0.29.1's single SnapshotCadenceEvents with four
	// per-kind fields; the orchestrator's SnapshotCadence map is built
	// from these in the daemon serve startup path.
	SnapshotCadenceConversation int `json:"snapshotCadenceConversation,omitempty"`
	SnapshotCadenceMemory       int `json:"snapshotCadenceMemory,omitempty"`
	SnapshotCadenceSkill        int `json:"snapshotCadenceSkill,omitempty"`
	SnapshotCadenceTool         int `json:"snapshotCadenceTool,omitempty"`

	// SnapshotMaxAgeConversation / Memory / Skill / Tool — per-kind
	// time-based snapshot trigger thresholds. A background ticker in
	// daemon serve calls retention.TickTimeBasedSnapshots(); for each
	// artifact whose most-recent event is older than its kind's threshold
	// AND whose last event is NOT itself a snapshot, a fresh snapshot is
	// appended. 0 = disabled for that kind.
	//
	// BRD-03 §4.8.1 defaults applied by the daemon CLI wiring (not by
	// this package): conversation=24h, memory=7d, skill=7d, tool=7d.
	SnapshotMaxAgeConversation time.Duration `json:"snapshotMaxAgeConversation,omitempty"`
	SnapshotMaxAgeMemory       time.Duration `json:"snapshotMaxAgeMemory,omitempty"`
	SnapshotMaxAgeSkill        time.Duration `json:"snapshotMaxAgeSkill,omitempty"`
	SnapshotMaxAgeTool         time.Duration `json:"snapshotMaxAgeTool,omitempty"`

	// StoreHighWatermarkGB is the disk-pressure emergency-mode trigger
	// per BRD-03 §4.8.2. When the canonical store's footprint crosses
	// this size in gigabytes, the daemon force-snapshots every artifact
	// (across all four kinds) so a subsequent prune pass can free space.
	// 0 = disabled. Daemon checks every 5 minutes via the goroutine
	// wired in cmd_daemon.go (v0.34.0).
	StoreHighWatermarkGB float64 `json:"storeHighWatermarkGB,omitempty"`

	// Tray is the user-facing tray-indicator configuration (BRD-03
	// §4.9.4; FR-03.30). v0.50.0+.
	Tray TrayConfig `json:"tray,omitempty"`

	// Web is the loopback HTTP listener configuration for the local web
	// UI.
	//
	// Zero value (omitted from the on-disk config) means "use defaults":
	// Enabled=true, Bind=127.0.0.1, Port=0 (random ephemeral). The
	// upgrade-detection path in cmd_daemon.go's loader treats a config
	// file with no "web" key as a first-run-with-web event and emits a
	// one-time stderr notice (FR-W2.8).
	Web WebConfig `json:"web,omitempty"`

	// Remote is the optional remote-plugin configuration. When set,
	// the daemon spawns the named executable as a kind="remote"
	// plugin at startup and routes outbound canonical-store events
	// through it (Aplexica Cloud, a self-hosted relay, or any other
	// implementation of the remote-plugin ABI).
	//
	// Zero value (no remote section) = local-only daemon, no sync.
	// Available when a remote-transport plugin is configured.
	Remote RemoteConfig `json:"remote,omitempty"`

	// Sync gates cross-agent fan-out per the FR-03.3 "discover + show,
	// await config" default: discovered agents import into the canonical
	// store and are shown, but the daemon does NOT export to a target agent
	// until the user enables it (via `aplexica sync enable`). Zero value =
	// nothing enabled (await config).
	Sync SyncConfig `json:"sync,omitempty"`
}

// SyncConfig persists per-agent (and global) fan-out enablement. It bridges
// to syncgate.Config, which the orchestrator consults in fanOut.
type SyncConfig struct {
	// All enables fan-out to every installed agent.
	All bool `json:"all,omitempty"`
	// Agents holds per-agent overrides (true = enabled, false = excluded).
	// A present key wins over All.
	Agents map[string]bool `json:"agents,omitempty"`
	// ConvBackfill caps, per agent, how many of the most-recent conversations
	// are materialized into that agent the first time it's enabled for sync.
	// A missing agent uses the default (syncd.DefaultConvBackfill = 10); a
	// negative value means unlimited ("all"). Bounds the otherwise-unbounded
	// "enable an agent → replicate every other agent's entire conversation
	// history into it" backfill. Only the backfill is capped; live (going-
	// forward) conversations always fan out.
	ConvBackfill map[string]int `json:"convBackfill,omitempty"`
	// CloudBackfill is RESERVED and currently has no effect: `aplexica
	// backfill --scope cloud` is refused whether or not this is set, and the
	// error message differs only in which gate it names. It exists so the
	// future cross-device backfill ships behind a config key that already has
	// a stable name, instead of inventing one (and a migration) later.
	// `aplexica backfill` itself is strictly local: it materializes canonical
	// history into this device's agents and never publishes to the relay.
	// Read at daemon startup; changing it requires a restart.
	CloudBackfill bool `json:"cloudBackfill,omitempty"`

	// RepairForkedMirrors authorizes the daemon to REBUILD a synthetic Claude
	// Code conversation mirror whose parentUuid graph has forked — the state
	// where Claude Code appended its own child of a node Aplexica had already
	// extended, stranding Aplexica's rows on a dead sibling branch. That state
	// is permanent on disk and fails closed at every append and match door, so
	// the artifact re-enters the materialization queue on every inbound event
	// and its transcript freezes where it forked.
	//
	// DEFAULT FALSE. With it unset the daemon behaves exactly as it did before
	// the repair existed: the divergence is reported honestly in `aplexica
	// status` and left alone. It is opt-in because it authorizes a whole-file
	// rewrite of a file Claude Code co-owns; the rewrite itself is
	// inode-preserving, keeps a copy of the pre-repair bytes under
	// ~/.aplexica/quarantine/claude-conversations/, and runs only when EVERY
	// conversational row is provably reproducible from the canonical thread.
	// It never modifies the canonical store and never creates a second session.
	//
	// As of v1.0.65 this also covers a NATIVE-ORIGIN transcript — the user's own
	// Claude session that diverged from canonical after being continued in
	// another agent without resuming. That is the common shape, and refusing it
	// left the transcript frozen forever. The same row-level containment proof
	// gates both; the proof, not the file's provenance, is the safety argument.
	//
	// Read at daemon startup; changing it requires a restart.
	RepairForkedMirrors bool `json:"repairForkedMirrors,omitempty"`
}

// SyncGateConfig projects the persisted SyncConfig onto the syncgate.Config
// the orchestrator's fan-out gate consumes.
func SyncGateConfig(c Config) syncgate.Config {
	return syncgate.Config{All: c.Sync.All, Agents: c.Sync.Agents}
}

// RemoteConfig points at an installed remote-transport plugin and
// captures the user's sync-mode preference. The daemon's startup path
// reads this; the configured executable is spawned with stdin/stdout
// piped to the daemon, then the daemon calls plugin/proxy.OpenRemote
// against the resulting pipe.
//
// Honored by:
//   - cmd/aplexica/cmd_daemon.go's startup wiring (spawns the plugin
//     when RemoteEnabled(cfg) returns true)
//   - cmd/aplexica/cmd_remote.go's install/uninstall/status subcommands
//   - the daemon's sync orchestrator (publishes events through the
//     RemoteProxy on each successful import + fan-out cycle)
type RemoteConfig struct {
	// Enabled tri-states the user's preference. Setting Executable
	// without Enabled=true means "configured but disabled" — useful
	// for trial accounts that want to keep their pairing state but
	// pause sync.
	Enabled *bool `json:"enabled,omitempty"`

	// Executable is the absolute path to the remote-plugin binary
	// (typically aplexica-cloud-plugin from the Cloud subscription
	// install). Empty = no remote configured.
	Executable string `json:"executable,omitempty"`

	// SyncMode selects the cadence: "manual" | "scheduled" |
	// "realtime". Personal tier supports manual + scheduled; realtime
	// requires Pro entitlement. Empty = "scheduled".
	SyncMode string `json:"syncMode,omitempty"`

	// ScheduledInterval is the cadence for SyncMode="scheduled".
	// Defaults to 15 minutes. Zero = use default.
	ScheduledInterval time.Duration `json:"scheduledInterval,omitempty"`
}

// RemoteEnabled returns the effective remote-plugin enabled state.
// Returns false when the config is nil, when Executable is empty
// (you can't enable a plugin you haven't pointed at), or when the
// tri-state Enabled is explicitly false.
//
// Defaults to true when Enabled is unset AND Executable is non-empty —
// configuring an executable without explicitly disabling it means
// "use it."
func RemoteEnabled(cfg *Config) bool {
	if cfg == nil || cfg.Remote.Executable == "" {
		return false
	}
	if cfg.Remote.Enabled == nil {
		return true
	}
	return *cfg.Remote.Enabled
}

// RemoteSyncMode returns the effective sync mode for cfg, defaulting
// to "scheduled" when unset.
func RemoteSyncMode(cfg *Config) string {
	if cfg == nil || cfg.Remote.SyncMode == "" {
		return "scheduled"
	}
	return cfg.Remote.SyncMode
}

// RemoteScheduledIntervalDefault is the default cadence
// for SyncMode="scheduled" — fifteen minutes.
const RemoteScheduledIntervalDefault = 15 * time.Minute

// RemoteScheduledInterval returns the effective scheduled-sync
// interval, defaulting to RemoteScheduledIntervalDefault when unset
// or zero.
func RemoteScheduledInterval(cfg *Config) time.Duration {
	if cfg == nil || cfg.Remote.ScheduledInterval <= 0 {
		return RemoteScheduledIntervalDefault
	}
	return cfg.Remote.ScheduledInterval
}

// TrayConfig is the persisted tray-indicator configuration. The only
// field today is Enabled — a tri-state pointer so the daemon and tray
// binary can distinguish "user explicitly set this" from "use platform
// default" (see TrayEnabledDefault).
//
// Default policy: the tray ships ENABLED on every OS (opt-out). This
// overrides BRD-03 §4.9.4's original opt-in-on-macOS/Linux stance — a
// per-platform default meant a stock macOS/Linux install came up with no
// tray, which read as a missing component. Every install now has the tray
// unless the user explicitly opts out (tray.enabled=false). NOTE: a headless
// host (no display server) should set tray.enabled=false; the tray binary
// needs a desktop session.
//
// Honored by:
//   - cmd/aplexica/cmd_tray.go's install/uninstall verbs
//   - cmd/aplexicatray/main.go's startup gate (exits cleanly when disabled)
//   - cmd/aplexica/cmd_daemon.go's install verb (with --tray flag)
type TrayConfig struct {
	// Enabled tri-states the user's preference:
	//   nil  → not set; use TrayEnabledDefault()
	//   true → user opted in; tray binary runs
	//   false → user opted out; tray binary exits cleanly with a
	//           one-line message and the daemon installer skips
	//           the autostart-entry creation
	Enabled *bool `json:"enabled,omitempty"`
}

// TrayEnabledDefault returns the default tray.enabled value used when
// TrayConfig.Enabled is nil. The tray ships ENABLED on every OS (opt-out)
// so a stock install on any platform comes up with all components present;
// this supersedes BRD-03 §4.9.4's original macOS/Linux opt-in. A user opts
// out per host with tray.enabled=false (e.g. a headless Linux box with no
// display server).
func TrayEnabledDefault() bool {
	return true
}

// TrayEnabled returns the effective tray-enabled state for cfg,
// applying the per-platform default from TrayEnabledDefault when
// cfg.Tray.Enabled is unset. Safe to call with a nil cfg (treated as
// all-defaults).
func TrayEnabled(cfg *Config) bool {
	if cfg == nil || cfg.Tray.Enabled == nil {
		return TrayEnabledDefault()
	}
	return *cfg.Tray.Enabled
}

// WebConfig is the persisted local-web-listener configuration. The listener
// serves the embedded portal SPA on loopback HTTP and
// powers the local REST + SSE API documented in the design spec.
//
// Tri-state Enabled mirrors TrayConfig's pattern so the loader can
// distinguish "user opted out" (false) from "missing; honor default"
// (nil). The default is opt-out: V1 ships with the web UI on by default
// for all OSS users, consistent with the broadened-positioning audience
// per MP-DEC-009.
//
// Honored by:
//   - cmd/aplexica/cmd_daemon.go's startup wiring (starts the listener
//     in a goroutine when WebEnabled(cfg) is true)
//   - cmd/aplexica/cmd_web_*.go's enable/disable CLI subcommands (write
//     to this struct via WriteConfig)
//   - cmd/aplexicatray/openweb.go's "Open Aplexica" menu item, which
//     spawns `aplexica web issue-token` only when the listener is up
type WebConfig struct {
	// Enabled tri-states the user's preference:
	//   nil   → not set; use WebEnabledDefault() (true)
	//   true  → user opted in (or unchanged from default)
	//   false → user opted out; daemon skips starting the listener
	Enabled *bool `json:"enabled,omitempty"`

	// Bind is the loopback address to listen on. Only "127.0.0.1" and
	// "::1" are valid in V1; the listener constructor refuses LAN binds
	// per the V1 scope decision (LAN access deferred to V2 with mDNS +
	// passkey + cert provisioning). Empty string = WebBindDefault.
	Bind string `json:"bind,omitempty"`

	// Port is the TCP port to listen on. 0 means pick a random
	// ephemeral port (49152-65535). The chosen port is written to
	// <state-dir>/portinfo.json (mode 0600) at startup so tooling (the
	// tray, `aplexica web port`, etc.) can discover it without polling
	// daemon stdout.
	Port int `json:"port,omitempty"`
}

// WebEnabledDefault returns the V1 platform-independent default
// (true). Local web UI is opt-out, not opt-in — matches the broadened
// positioning that targets non-CLI users.
func WebEnabledDefault() bool { return true }

// WebEnabled returns the effective web-enabled state for cfg, applying
// the WebEnabledDefault when cfg.Web.Enabled is unset. Safe to call with
// a nil cfg (treated as all-defaults).
func WebEnabled(cfg *Config) bool {
	if cfg == nil || cfg.Web.Enabled == nil {
		return WebEnabledDefault()
	}
	return *cfg.Web.Enabled
}

// WebBindDefault is the V1 fallback for an empty Web.Bind: 127.0.0.1.
// ::1 support exists in the listener constructor for clients that
// resolve "localhost" to IPv6 first, but is not the documented default.
const WebBindDefault = "127.0.0.1"

// WebBind returns the effective bind address for cfg. Empty string in
// cfg.Web.Bind falls back to WebBindDefault. Safe to call with a nil
// cfg.
func WebBind(cfg *Config) string {
	if cfg == nil || cfg.Web.Bind == "" {
		return WebBindDefault
	}
	return cfg.Web.Bind
}

// LoadConfig reads a Config from path. A missing file returns a zero-value
// Config + nil error (treated as "all defaults"). Parse errors propagate.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("daemon: read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("daemon: parse config %s: %w", path, err)
	}
	return &c, nil
}

// WriteConfig serializes c and atomically writes it to path.
func WriteConfig(path string, c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("daemon: marshal config: %w", err)
	}
	return atomicfile.WriteFile(path, data, 0o600)
}

// firstRunWebNotice is the one-time stderr message printed by
// EmitFirstRunWebNotice when an existing daemon installation is
// upgraded to a version that ships the local web UI. The text is a
// const so behavior is stable across daemon versions and so tests can
// assert on substrings.
const firstRunWebNotice = "aplexica: now ships with a local web UI. " +
	"Click the tray icon → Open Aplexica, or run `aplexica web open`. " +
	"Disable with `aplexica web disable`."

// ConfigHasWebSection returns true if the config file at path exists
// AND contains a top-level "web" key. Used by EmitFirstRunWebNotice
// to detect "user is upgrading from a pre-W2 daemon" — that signal
// can't reliably come from the parsed *Config (a zero WebConfig is
// indistinguishable from a missing one once it's been Unmarshal'd
// because Go's encoding/json fills in zero values silently).
//
// Returns (false, nil) when the file doesn't exist (brand-new
// install). Returns (false, err) when the file exists but isn't valid
// JSON; callers may surface that error or skip the notice — the
// existing LoadConfig path will report the same parse error
// downstream.
func ConfigHasWebSection(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("daemon: read config %s: %w", path, err)
	}
	// Decode into a top-level map[string]json.RawMessage so we can
	// inspect key presence without parsing the value. This is
	// equivalent to "does the on-disk JSON have a 'web' field" — a
	// stronger signal than what Unmarshal-into-Config would give us
	// (which loses the difference between "key absent" and "key set
	// to zero value").
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, fmt.Errorf("daemon: parse config %s: %w", path, err)
	}
	_, ok := raw["web"]
	return ok, nil
}

// EmitFirstRunWebNotice detects + handles the upgrade-from-pre-W2
// scenario: a config file exists at path AND has no top-level "web"
// key. On detection, it writes the one-time notice to w (callers
// typically pass os.Stderr) and persists the loaded config back to
// disk with default WebConfig zero values so subsequent starts find a
// "web" key and skip the notice.
//
// Returns (true, nil) when the notice fired. Returns (false, nil) on:
//   - brand-new install (config file missing — fresh users discover the
//     web UI via `aplexica setup` or the tray, not via stderr)
//   - config already has a "web" key (prior start already wrote
//     defaults, OR user has customized web settings)
//
// w may be io.Discard if the caller wants the persistence-only side
// effect (e.g., when the daemon runs without a tty).
func EmitFirstRunWebNotice(path string, w io.Writer) (bool, error) {
	has, err := ConfigHasWebSection(path)
	if err != nil {
		return false, err
	}
	// Brand-new install: nothing to upgrade, don't notice.
	// Already-upgraded: nothing to notice.
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if has {
		return false, nil
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		return false, err
	}
	// Forcing the Web field to its zero value is a no-op semantically
	// but causes the JSON to serialize a "web":{} key — exactly the
	// signal ConfigHasWebSection will read on the next start to
	// suppress the notice.
	cfg.Web = WebConfig{}
	if err := WriteConfig(path, cfg); err != nil {
		return false, err
	}

	// Print after persisting so a write failure doesn't leave the
	// user with a notice that won't go away.
	_, _ = fmt.Fprintln(w, firstRunWebNotice)
	return true, nil
}
