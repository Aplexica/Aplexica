package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/daemon"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/spf13/cobra"
)

var (
	statusConflictsRoot string
	statusStateDir      string
	statusStoreRoot     string
	statusWatch         bool
	statusInterval      time.Duration
	statusJSON          bool
	statusSyncEvidence  bool
)

// statusAgentRefreshInterval bounds the expensive canonical-store walk used
// to populate per-agent artifact counts in watch mode. The tray asks for a
// status snapshot every five seconds, but those counts do not need five-second
// precision. Rewalking a multi-gigabyte store on every tick caused the status
// helper to consume a substantial fraction of a CPU on all platforms.
const statusAgentRefreshInterval = 5 * time.Minute

type statusAgentCache struct {
	agents      []AgentStatus
	refreshedAt time.Time
	load        func(string) []AgentStatus
}

func (c *statusAgentCache) snapshot(now time.Time, storeRoot string) []AgentStatus {
	if c.load == nil {
		c.load = collectAgents
	}
	if c.agents == nil || c.refreshedAt.IsZero() || now.Sub(c.refreshedAt) >= statusAgentRefreshInterval {
		c.agents = c.load(storeRoot)
		c.refreshedAt = now
	}
	return c.agents
}

// StatusSnapshot is one tick of status. JSON-emitted under --json.
//
// Stable wire contract for third-party tray apps and monitoring integrations
// (Prometheus textfile collector, log shippers, future GUI tray binary). New
// fields may be added; existing ones won't change shape without a major bump.
type StatusSnapshot struct {
	Timestamp       time.Time            `json:"timestamp"`
	DaemonAvailable bool                 `json:"daemonAvailable"`
	DaemonInfo      *daemon.StatusInfo   `json:"daemonInfo,omitempty"`
	Conflicts       []conflicts.Conflict `json:"conflicts"`
	ConflictCount   int                  `json:"conflictCount"`
	// Agents is the per-agent discovery + artifact-count view (BRD-03
	// FR-03.3 detection, BRD-01 FR-01.28 per-agent counts/timestamps).
	// Computed locally (cheap Discover() stat + a single store walk) so it
	// works whether or not the daemon is running and reflects the invoking
	// user's home directory.
	Agents []AgentStatus `json:"agents"`
}

// AgentStatus is one row of the status agents table.
type AgentStatus struct {
	Name          string    `json:"name"`
	Installed     bool      `json:"installed"`
	ArtifactCount int       `json:"artifactCount"`
	LastActivity  time.Time `json:"lastActivity,omitzero"`
	Detail        string    `json:"detail,omitempty"`
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon + conflict status (--watch for continuous output, --json for machine-readable)",
	Long: `Show a snapshot of the daemon's liveness and any unresolved conflicts.

One-shot mode (default): emit one snapshot and exit.

  aplexica status
  aplexica status --json

Acceptance diagnostics may opt in to one bounded remote sync-evidence query:

  aplexica status --json --sync-evidence

Watch mode: poll every --interval (default 5s) and emit one snapshot per tick.
Use Ctrl-C (SIGINT) or SIGTERM to stop.

  aplexica status --watch
  aplexica status --watch --interval 2s --json

The --json output is a stable single-line contract per tick — one JSON object
per line in watch mode. Intended for third-party tray apps, Prometheus
textfile collectors, log shippers, and a future cross-platform tray binary.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		if !statusWatch {
			snap := collectStatus()
			return emitStatus(cmd, snap, statusJSON)
		}

		t := time.NewTicker(statusInterval)
		defer t.Stop()
		agentCache := statusAgentCache{}
		if err := emitStatus(cmd, collectStatusWithAgents(agentCache.snapshot(time.Now(), statusStoreRoot)), statusJSON); err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				if err := emitStatus(cmd, collectStatusWithAgents(agentCache.snapshot(time.Now(), statusStoreRoot)), statusJSON); err != nil {
					return err
				}
			}
		}
	},
}

func collectStatus() StatusSnapshot {
	return collectStatusWithAgents(collectAgents(statusStoreRoot))
}

func collectStatusWithAgents(agents []AgentStatus) StatusSnapshot {
	snap := StatusSnapshot{Timestamp: time.Now().UTC()}

	// Conflicts.
	confStore := &conflicts.Store{Root: statusConflictsRoot}
	if list, err := confStore.List(); err == nil {
		snap.Conflicts = statusConflictSummaries(list)
		snap.ConflictCount = len(list)
	}

	// Daemon liveness via control socket.
	sockPath := filepath.Join(statusStateDir, "aplexicad.sock")
	if info, err := queryDaemonStatus(sockPath, statusSyncEvidence); err == nil {
		snap.DaemonAvailable = true
		snap.DaemonInfo = info
	}

	// Per-agent discovery + counts (FR-03.3 / FR-01.28). One-shot callers
	// collect these immediately; watch-mode callers reuse a bounded cache so
	// the tray's five-second heartbeat stays cheap.
	snap.Agents = agents

	return snap
}

func statusConflictSummaries(list []conflicts.Conflict) []conflicts.Conflict {
	if len(list) == 0 {
		return nil
	}
	out := make([]conflicts.Conflict, len(list))
	for i, c := range list {
		out[i] = c
		if len(c.Heads) == 0 {
			continue
		}
		out[i].Heads = make([]conflicts.Head, len(c.Heads))
		copy(out[i].Heads, c.Heads)
		for j := range out[i].Heads {
			out[i].Heads[j].FullPayload = nil
		}
	}
	return out
}

// collectAgents probes each built-in adapter's Discover() (cheap stat) for
// installed/not-installed presence and counts the canonical-store artifacts
// attributed to it (synced to it, or originating from its native global
// storage). Daemon-independent: reflects the invoking user's home + the store
// on disk. A missing/empty store yields zero counts, not an error.
func collectAgents(storeRoot string) []AgentStatus {
	probes := []adapter.Adapter{
		claudecode.New(), codex.New(), kilo.New(), hermes.New(), openclaw.New(),
	}
	st := &acf.Store{Root: storeRoot}
	var arts []acf.Artifact
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		if list, err := st.ListArtifacts(kind); err == nil {
			arts = append(arts, list...)
		}
	}
	out := make([]AgentStatus, 0, len(probes))
	for _, ad := range probes {
		d, _ := ad.Discover()
		row := AgentStatus{Name: ad.Name(), Installed: d.Installed, Detail: d.Detail}
		for _, art := range arts {
			if agentAttributed(art, ad.Name(), d.GlobalRoots) {
				row.ArtifactCount++
				if art.UpdatedAt.After(row.LastActivity) {
					row.LastActivity = art.UpdatedAt
				}
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// queryDaemonStatus sends the v0.8.0 control-server "status" command over
// the Unix-domain control socket and returns the unmarshaled StatusInfo.
//
// DEVIATION FROM PLAN: the v0.33.0 plan guessed `daemon.QueryStatus(sockPath)`
// but the v0.8.0 daemon package exposes only the generic
// `daemon.SendCommand(sockPath, daemon.Request)` -> daemon.Response wrapper,
// where Response.Data is typed `any` (a `map[string]interface{}` after JSON
// round-trip). This helper does the second JSON marshal+unmarshal hop to
// recover a typed *StatusInfo. Kept local to cmd_status.go so the wire-shape
// dependency lives next to its consumer.
func queryDaemonStatus(sockPath string, includeSyncEvidence bool) (*daemon.StatusInfo, error) {
	resp, err := daemon.SendCommand(sockPath, daemon.Request{
		Command:             "status",
		IncludeSyncEvidence: includeSyncEvidence,
	})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("daemon status error: %s", resp.Error)
	}
	// resp.Data is `any` and lands as map[string]interface{} after JSON
	// round-trip on the wire; re-marshal then unmarshal into StatusInfo.
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		return nil, fmt.Errorf("re-marshal status data: %w", err)
	}
	var info daemon.StatusInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("decode status data: %w", err)
	}
	return &info, nil
}

func emitStatus(cmd *cobra.Command, snap StatusSnapshot, asJSON bool) error {
	if asJSON {
		b, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "[%s]\n", snap.Timestamp.Format(time.RFC3339))
	if snap.DaemonAvailable && snap.DaemonInfo != nil {
		fmt.Fprintf(out, "  Daemon: running (pid %d, watching %s, started %s)\n",
			snap.DaemonInfo.PID, snap.DaemonInfo.WatchedDir,
			snap.DaemonInfo.StartedAt.Format(time.RFC3339))
		renderStorePressure(out, snap.DaemonInfo)
		renderDeferredMaterializations(out, snap.DaemonInfo)
		renderSyncSuppressions(out, snap.DaemonInfo)
	} else {
		fmt.Fprintln(out, "  Daemon: not running (no socket)")
	}
	if len(snap.Agents) > 0 {
		fmt.Fprintln(out, "  Agents:")
		for _, ag := range snap.Agents {
			if !ag.Installed {
				fmt.Fprintf(out, "    %-12s not installed\n", ag.Name)
				continue
			}
			last := "never"
			if !ag.LastActivity.IsZero() {
				last = ag.LastActivity.Format(time.RFC3339)
			}
			fmt.Fprintf(out, "    %-12s installed — %d artifacts, last activity %s\n",
				ag.Name, ag.ArtifactCount, last)
		}
	}
	if snap.ConflictCount == 0 {
		fmt.Fprintln(out, "  Conflicts: none")
	} else {
		fmt.Fprintf(out, "  Conflicts: %d unresolved\n", snap.ConflictCount)
		for _, c := range snap.Conflicts {
			fmt.Fprintf(out, "    %s  %s  %d heads\n", c.ArtifactID, c.Kind, len(c.Heads))
		}
	}
	return nil
}

// statusBytesPerGB renders store byte counts as gigabytes for the human
// status line. Matches the daemon's GB-based cap (retention.StoreMaxGB).
const statusBytesPerGB = 1024 * 1024 * 1024

// renderStorePressure prints the store-disk-pressure state (FR-03.21) under
// the Daemon line. It is a no-op when the cap is disabled (StoreMaxBytes==0,
// e.g. store_max_gb=0). When the store is over the emergency ceiling it prints
// a strong warning that ingestion is being refused; over the high watermark
// (but under the ceiling) it prints a softer warning; otherwise a quiet OK
// usage line. info is non-nil (the caller guards on DaemonInfo != nil).
//
// Below the usage line it renders the honest split — bytes retention
// could actually reclaim vs bytes it cannot legally touch — and, when the
// pinned bytes alone meet the high watermark, says so explicitly instead of
// letting the over-watermark warning imply that retention merely hasn't
// caught up yet. A daemon predating the split omits the fields (all zero)
// and only the original lines render.
func renderStorePressure(out io.Writer, info *daemon.StatusInfo) {
	if info.StoreMaxBytes <= 0 {
		// Disk-pressure tracking disabled (store_max_gb=0) — stay quiet.
		return
	}
	usedGB := float64(info.StoreBytes) / statusBytesPerGB
	maxGB := float64(info.StoreMaxBytes) / statusBytesPerGB
	switch {
	case info.OverEmergency:
		fmt.Fprintf(out, "  Store: WARNING — over emergency quota (%.1f / %.1f GB); NEW INGESTION IS BEING REFUSED\n",
			usedGB, maxGB)
	case info.OverHighWatermark:
		fmt.Fprintf(out, "  Store: WARNING — over high watermark (%.1f / %.1f GB); emergency pruning active\n",
			usedGB, maxGB)
	default:
		fmt.Fprintf(out, "  Store: %.1f / %.1f GB\n", usedGB, maxGB)
	}
	if info.StorePinnedBytes <= 0 && info.StoreReclaimableBytes <= 0 {
		return
	}
	pinnedGB := float64(info.StorePinnedBytes) / statusBytesPerGB
	fmt.Fprintf(out, "    Pinned: %.1f GB (append-only event logs %.1f GB) · reclaimable by retention: %.1f GB\n",
		pinnedGB,
		float64(info.StoreEventLogBytes)/statusBytesPerGB,
		float64(info.StoreReclaimableBytes)/statusBytesPerGB)
	if info.StoreWatermarkUnreachable && info.StoreHighWatermarkBytes > 0 {
		fmt.Fprintf(out, "    Watermark unreachable: %.1f GB pinned meets the %.1f GB high watermark — retention cannot get under it\n",
			pinnedGB, float64(info.StoreHighWatermarkBytes)/statusBytesPerGB)
	}
}

// renderDeferredMaterializations summarizes the native-materialization retry
// queue under the Daemon line. Pending entries are normal and transient in
// small numbers; abandoned ones spent their whole retry budget and need an
// operator, so they are always named. Stays silent when the queue is empty.
func renderDeferredMaterializations(out io.Writer, info *daemon.StatusInfo) {
	if len(info.DeferredMaterializations) == 0 {
		return
	}
	pending, attention, held := 0, 0, 0
	for _, row := range info.DeferredMaterializations {
		if deferredNeedsAttention(row) {
			attention++
			continue
		}
		if deferred, _ := row["escalationDeferred"].(bool); deferred {
			held++
		}
		pending++
	}
	if pending > 0 {
		fmt.Fprintf(out, "  Materialization: %d pending retr%s\n", pending, plural(pending, "y", "ies"))
	}
	if held > 0 {
		// Report the cap's backlog rather than truncating it silently. The cap
		// can hold a substantial backlog, so the operator must be able to see it.
		fmt.Fprintf(out,
			"  Materialization: %d more write%s waiting to be raised (max %d new per day)\n",
			held, plural(held, "", "s"), syncd.EscalationsPerDay)
	}
	if attention == 0 {
		return
	}
	// Name the action, not just the count. Reporting only "abandoned" leaves
	// the operator with nowhere to go and allows unresolved writes to accumulate.
	fmt.Fprintf(out, "  Materialization: WARNING — %d write%s attention (retries exhausted)\n",
		attention, plural(attention, " needs", "s need"))
	shown := 0
	for _, row := range info.DeferredMaterializations {
		if !deferredNeedsAttention(row) {
			continue
		}
		if shown == deferredAttentionListLimit {
			fmt.Fprintf(out, "    ... and %d more\n", attention-shown)
			break
		}
		agent, _ := row["agent"].(string)
		artifactID, _ := row["artifactId"].(string)
		fmt.Fprintf(out, "    %s  %s\n", agent, artifactID)
		// The per-class remedy, when one exists. A class no shipped command can
		// resolve prints nothing here rather than a command that repairs
		// nothing — which is what every one of these rows used to offer.
		if remedy, _ := row["remedy"].(string); remedy != "" {
			fmt.Fprintf(out, "      Fix with: %s\n", remedy)
		}
		shown++
	}
	fmt.Fprintln(out, "    These resume automatically if the artifact changes again.")
	fmt.Fprintln(out, "    See why with: aplexica repair materialization")
}

// deferredAttentionListLimit bounds how many stuck writes plain `status`
// enumerates. A device with hundreds must not push everything else off the
// screen; `--json` and `aplexica repair materialization` carry the full list.
const deferredAttentionListLimit = 10

// deferredNeedsAttention accepts both the current "needs_attention" state and
// the historical "abandoned" spelling, so a newer CLI reading an older
// daemon's status (or a rolling upgrade across a fleet) does not silently
// count stuck writes as healthy pending retries.
func deferredNeedsAttention(row map[string]any) bool {
	state, _ := row["state"].(string)
	return state == "needs_attention" || state == "abandoned"
}

// renderSyncSuppressions answers "why is nothing syncing?" directly.
//
// It leads with the device-wide verdict when sync is structurally disabled.
// Without that verdict, a missing rules.toml can deny every write while the
// remaining status indicators look healthy and only a log line names the cause.
//
// Defect suppressions are named individually (they are faults). Policy
// suppressions are summarized (they are the user's own configuration working
// as asked) — loud enough to explain a missing file, quiet enough not to nag.
func renderSyncSuppressions(out io.Writer, info *daemon.StatusInfo) {
	if info.SyncDisabledReason != "" {
		fmt.Fprintf(out, "  Sync: DISABLED — %s\n", info.SyncDisabledReason)
		fmt.Fprintln(out, "    Fix with: aplexica rules add <file.toml>")
	}
	if len(info.SyncSuppressions) == 0 {
		return
	}
	var defects, policies []map[string]any
	for _, row := range info.SyncSuppressions {
		// A stale row's condition is no longer true. It is kept in --json as
		// history but must never be printed as a problem the operator still
		// has, or a resolved condition becomes a standing false alarm.
		if stale, _ := row["stale"].(bool); stale {
			continue
		}
		switch class, _ := row["class"].(string); class {
		case "defect":
			defects = append(defects, row)
		case "policy":
			policies = append(policies, row)
		}
		// Capability rows ("this agent cannot hold this kind of artifact")
		// are permanent facts about the adapter, not something the operator
		// can act on, so they stay in --json only.
	}
	for _, row := range defects {
		agent, _ := row["agent"].(string)
		explain, _ := row["explain"].(string)
		fmt.Fprintf(out, "  Sync: %s — %s\n", agent, explain)
		if remedy, _ := row["remedy"].(string); remedy != "" {
			fmt.Fprintf(out, "    Fix with: %s\n", remedy)
		}
	}
	if len(policies) > 0 && info.SyncDisabledReason == "" {
		total := uint64(0)
		for _, row := range policies {
			switch c := row["count"].(type) {
			case float64:
				total += uint64(c)
			case uint64:
				total += c
			case int:
				total += uint64(c)
			}
		}
		fmt.Fprintf(out, "  Sync: %d write%s not copied by your own rules (aplexica status --json for detail)\n",
			total, plural(int(total), "", "s"))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func init() {
	home, _ := os.UserHomeDir()
	statusCmd.Flags().StringVar(&statusConflictsRoot, "conflicts-root",
		filepath.Join(home, ".aplexica", "state", "conflicts"),
		"Conflicts store root (typically <state-dir>/conflicts/)")
	statusCmd.Flags().StringVar(&statusStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"Daemon state directory (contains aplexicad.sock)")
	statusCmd.Flags().StringVar(&statusStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root (for per-agent artifact counts)")
	statusCmd.Flags().BoolVar(&statusWatch, "watch", false,
		"Poll forever; emit one snapshot per --interval. Stop with Ctrl-C.")
	statusCmd.Flags().DurationVar(&statusInterval, "interval", 5*time.Second,
		"Poll interval when --watch is set")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false,
		"Emit machine-readable JSON snapshots (one per line in watch mode)")
	statusCmd.Flags().BoolVar(&statusSyncEvidence, "sync-evidence", false,
		"Include one bounded remote sync-evidence snapshot (acceptance diagnostics)")
	rootCmd.AddCommand(statusCmd)
}
