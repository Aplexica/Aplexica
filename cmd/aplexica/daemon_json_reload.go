package main

import (
	"context"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/hermeswatch"
	"github.com/aplexica/aplexica/internal/retention"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/aplexica/aplexica/internal/syncgate"
)

// jsonReloadMu serializes config.json reload passes. SIGHUP (Unix) and the
// control-socket "reload" command can fire concurrently; both mutate
// *currentCfg through applyJSONConfigReload.
var jsonReloadMu sync.Mutex

// applyJSONConfigReload re-reads <state-dir>/config.json, diffs it against
// *currentCfg via daemon.ApplyReload, applies every hot-reloadable field to
// the live daemon references, and advances *currentCfg to the new baseline.
// Returns the list of hot fields applied (nil when nothing changed).
//
// Shared by the SIGHUP handler (Unix) and the control-socket "reload"
// command (cross-platform) so `aplexica daemon reload` and `kill -HUP`
// apply the SAME field set — including the FR-03.3 await-config sync gate,
// which previously only the portal toggle applied without a restart even
// though `aplexica sync enable` told users a reload would suffice.
func applyJSONConfigReload(
	lg *daemon.RotatingLogger,
	configPath string,
	currentCfg *daemon.Config,
	orch *syncd.Orchestrator,
	hw *hermeswatch.Watcher,
	snapRunner *retention.Runner,
) ([]string, error) {
	jsonReloadMu.Lock()
	defer jsonReloadMu.Unlock()

	// Overlay, not bare-load: config.json is sparse (setup/CLI persist only
	// the fields they own), while *currentCfg carries flag-derived runtime
	// tunables. A bare LoadConfig made every absent field diff as "changed
	// to zero" — a reload would hot-apply quiet=0/guardWindow=0/cadence=0
	// and spam phantom restart-required logs for dir/store/hermesDB.
	nextCfg, err := daemon.LoadConfigOverlay(configPath, *currentCfg)
	if err != nil {
		return nil, err
	}
	diff := daemon.ApplyReload(currentCfg, nextCfg, lg.Logger)
	var applied []string

	if diff.LogLevelChanged {
		newLevel := daemon.ParseLogLevel(nextCfg.LogLevel)
		lg.SetLevel(newLevel)
		lg.Info("log level updated", "level", newLevel.String())
		applied = append(applied, "logLevel")
	}
	if diff.QuietChanged {
		if orch != nil && orch.Debouncer() != nil {
			orch.Debouncer().SetQuietPeriod(nextCfg.Quiet)
			lg.Info("quiet hot-reload applied",
				"old", currentCfg.Quiet, "new", nextCfg.Quiet)
			applied = append(applied, "quiet")
		} else {
			lg.Info("quiet hot-reload skipped — no debouncer available",
				"old", currentCfg.Quiet, "new", nextCfg.Quiet)
		}
	}
	if diff.GuardWindowChanged {
		if orch != nil && orch.Guard() != nil {
			orch.Guard().SetWindow(nextCfg.GuardWindow)
			lg.Info("guardWindow hot-reload applied",
				"old", currentCfg.GuardWindow, "new", nextCfg.GuardWindow)
			applied = append(applied, "guardWindow")
		} else {
			lg.Info("guardWindow hot-reload skipped — no guard available",
				"old", currentCfg.GuardWindow, "new", nextCfg.GuardWindow)
		}
	}
	if diff.HermesWatchIntervalChanged {
		if hw != nil {
			hw.SetInterval(nextCfg.HermesWatchInterval)
			lg.Info("hermesWatchInterval hot-reload applied",
				"old", currentCfg.HermesWatchInterval,
				"new", nextCfg.HermesWatchInterval)
			applied = append(applied, "hermesWatchInterval")
		} else {
			lg.Info("hermesWatchInterval hot-reload skipped — hermeswatch not running (disabled or state.db missing at startup)",
				"old", currentCfg.HermesWatchInterval,
				"new", nextCfg.HermesWatchInterval)
		}
	}
	if diff.SnapshotCadenceChanged {
		if orch != nil {
			next := map[acf.Kind]int{
				acf.KindConversation: nextCfg.SnapshotCadenceConversation,
				acf.KindMemory:       nextCfg.SnapshotCadenceMemory,
				acf.KindSkill:        nextCfg.SnapshotCadenceSkill,
				acf.KindTool:         nextCfg.SnapshotCadenceTool,
			}
			orch.SetSnapshotCadence(next)
			lg.Info("snapshotCadence hot-reload applied",
				"conv_old", currentCfg.SnapshotCadenceConversation,
				"conv_new", nextCfg.SnapshotCadenceConversation,
				"mem_old", currentCfg.SnapshotCadenceMemory,
				"mem_new", nextCfg.SnapshotCadenceMemory,
				"skill_old", currentCfg.SnapshotCadenceSkill,
				"skill_new", nextCfg.SnapshotCadenceSkill,
				"tool_old", currentCfg.SnapshotCadenceTool,
				"tool_new", nextCfg.SnapshotCadenceTool)
			applied = append(applied, "snapshotCadence")
		} else {
			lg.Info("snapshotCadence hot-reload skipped — no orchestrator available")
		}
	}
	if diff.SnapshotMaxAgeChanged {
		// v0.34.0: hot-apply SnapshotMaxAge* via the Runner's
		// thread-safe SetMaxAge. snapRunner is always non-nil
		// (constructed unconditionally in cmd_daemon.go), but the
		// goroutine may not be .Run()-started if every per-kind
		// threshold was 0 at boot — in that case the new values are
		// still captured for any future tick triggered by another
		// reload that re-enables the snapshotter.
		if snapRunner != nil {
			next := map[acf.Kind]time.Duration{
				acf.KindConversation: nextCfg.SnapshotMaxAgeConversation,
				acf.KindMemory:       nextCfg.SnapshotMaxAgeMemory,
				acf.KindSkill:        nextCfg.SnapshotMaxAgeSkill,
				acf.KindTool:         nextCfg.SnapshotMaxAgeTool,
			}
			snapRunner.SetMaxAge(next)
			lg.Info("snapshotMaxAge hot-reload applied",
				"conv_old", currentCfg.SnapshotMaxAgeConversation,
				"conv_new", nextCfg.SnapshotMaxAgeConversation,
				"mem_old", currentCfg.SnapshotMaxAgeMemory,
				"mem_new", nextCfg.SnapshotMaxAgeMemory,
				"skill_old", currentCfg.SnapshotMaxAgeSkill,
				"skill_new", nextCfg.SnapshotMaxAgeSkill,
				"tool_old", currentCfg.SnapshotMaxAgeTool,
				"tool_new", nextCfg.SnapshotMaxAgeTool)
			applied = append(applied, "snapshotMaxAge")
		} else {
			lg.Info("snapshotMaxAge hot-reload skipped — no runner available")
		}
	}
	if diff.SyncChanged {
		if orch != nil {
			orch.SetSyncGate(syncgate.New(daemon.SyncGateConfig(*nextCfg)))
			// Apply per-agent conversation-backfill caps BEFORE the backfill
			// below runs, so enabling an agent backfills only its most-recent-N
			// conversations instead of every other agent's entire history.
			orch.SetConvBackfill(nextCfg.Sync.ConvBackfill)
			lg.Info("sync gate hot-reload applied",
				"all_old", currentCfg.Sync.All, "all_new", nextCfg.Sync.All,
				"agents_old", currentCfg.Sync.Agents, "agents_new", nextCfg.Sync.Agents)
			applied = append(applied, "syncGate")
			// A newly-enabled agent needs existing artifacts re-fanned —
			// the gate alone only affects FUTURE events. Mirrors the
			// portal toggle's backfill-on-enable; async so the reload
			// response returns immediately. fanOut honors the
			// (just-swapped) gate, so only enabled targets receive.
			if daemon.SyncEnablementExpanded(currentCfg.Sync, nextCfg.Sync) {
				go func() {
					n, err := orch.RefanOutAll(context.Background())
					if err != nil {
						lg.Warn("fan-out backfill failed (gate enabled via reload)", "err", err)
						return
					}
					lg.Info("fan-out backfill complete (gate enabled via reload)", "artifacts_refanned", n)
				}()
			}
		} else {
			lg.Info("sync gate hot-reload skipped — no orchestrator available")
		}
	}

	*currentCfg = *nextCfg
	return applied, nil
}
