package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/aplexica/aplexica/internal/pausestate"
	"github.com/spf13/cobra"
)

// FR-03.8: aplexica sync pause [--agent <name>] [--for <duration>]
// FR-03.8: aplexica sync resume [--agent <name>]
//
// Persists state via internal/pausestate so the daemon and tray both
// see the same picture. The daemon's orchestrator consults
// pausestate.Store on every fan-out decision (v0.88.0 wiring in
// internal/sync/orchestrator.go).

var (
	syncPauseStateDir string
	syncPauseAgent    string
	syncPauseFor      time.Duration
	syncResumeAgent   string
)

var syncPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Pause sync globally or for a single agent",
	Long: `Pause fan-out activity. The daemon's orchestrator consults
the persisted pause state on every outbound write decision and skips
paused targets.

Examples:

  aplexica sync pause                          # pause all adapters
  aplexica sync pause --agent codex            # pause just codex
  aplexica sync pause --for 1h                 # global; auto-resume after 1h
  aplexica sync pause --agent codex --for 30m  # codex; auto-resume after 30m

Without --for, the pause lasts until an explicit ` + "`aplexica sync resume`" + `.

State is persisted in <state-dir>/sync-pause.json; the running daemon
and the tray indicator both observe changes within their next poll
cycle.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &pausestate.Store{Path: pausestate.DefaultPath(syncPauseStateDir)}
		var err error
		if syncPauseAgent != "" {
			err = store.PauseAdapter(syncPauseAgent, syncPauseFor)
		} else {
			err = store.PauseGlobal(syncPauseFor)
		}
		if err != nil {
			return err
		}

		scope := "global"
		if syncPauseAgent != "" {
			scope = "adapter:" + syncPauseAgent
		}
		until := "until explicit resume"
		if syncPauseFor > 0 {
			until = "until " + time.Now().UTC().Add(syncPauseFor).Format(time.RFC3339)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "paused %s (%s)\n", scope, until)
		return nil
	},
}

var syncResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume paused sync",
	Long: `Resume fan-out activity. Without --agent, resumes the global
pause. With --agent <name>, resumes only that adapter (the global
pause, if any, stays in effect).

Examples:

  aplexica sync resume                # clear global pause
  aplexica sync resume --agent codex  # clear codex pause only`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &pausestate.Store{Path: pausestate.DefaultPath(syncPauseStateDir)}
		var err error
		scope := "global"
		if syncResumeAgent != "" {
			err = store.ResumeAdapter(syncResumeAgent)
			scope = "adapter:" + syncResumeAgent
		} else {
			err = store.ResumeGlobal()
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "resumed %s\n", scope)
		return nil
	},
}

var syncPauseStatusCmd = &cobra.Command{
	Use:   "pause-status",
	Short: "Show current pause state (read-only)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &pausestate.Store{Path: pausestate.DefaultPath(syncPauseStateDir)}
		st, err := store.Load()
		if err != nil {
			return err
		}
		return renderPauseStatus(st, time.Now().UTC(), cmd.OutOrStdout())
	},
}

// pauseEntryActive reports whether a pause entry is still in effect at
// `now`: it must be flagged paused AND either have no expiry (zero
// Until) or an expiry still in the future. An expired entry (non-zero
// Until <= now) is already treated as resumed by the orchestrator and
// pausestate.IsPaused; this keeps the read-only displays consistent
// with that between the daemon's periodic CleanExpired sweeps.
func pauseEntryActive(paused bool, until time.Time, now time.Time) bool {
	if !paused {
		return false
	}
	return until.IsZero() || until.After(now)
}

// renderPauseStatus writes the human-readable pause table to w, judging
// expiry against `now` so a lapsed `--for` pause shows as resumed even
// before the daemon's periodic cleanup removes it from disk.
func renderPauseStatus(st pausestate.State, now time.Time, out io.Writer) error {
	globalActive := pauseEntryActive(st.Global.Paused, st.Global.Until, now)

	activeAdapters := make([]string, 0, len(st.Adapters))
	for n, as := range st.Adapters {
		if pauseEntryActive(as.Paused, as.Until, now) {
			activeAdapters = append(activeAdapters, n)
		}
	}
	sort.Strings(activeAdapters)

	if !globalActive && len(activeAdapters) == 0 {
		fmt.Fprintln(out, "sync: not paused")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SCOPE\tSTATE\tEXPIRES")
	if globalActive {
		expires := "(no expiry)"
		if !st.Global.Until.IsZero() {
			expires = st.Global.Until.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "global\tpaused\t%s\n", expires)
	}
	for _, n := range activeAdapters {
		as := st.Adapters[n]
		expires := "(no expiry)"
		if !as.Until.IsZero() {
			expires = as.Until.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "adapter:%s\tpaused\t%s\n", n, expires)
	}
	return w.Flush()
}

var syncPauseStatusJSON = false

var syncPauseStatusJSONCmd = &cobra.Command{
	Use:    "pause-status-json",
	Short:  "Internal: emit pause state as JSON (used by the tray)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &pausestate.Store{Path: pausestate.DefaultPath(syncPauseStateDir)}
		st, err := store.Load()
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStateDir := filepath.Join(home, ".aplexica", "state")

	for _, c := range []*cobra.Command{syncPauseCmd, syncResumeCmd, syncPauseStatusCmd, syncPauseStatusJSONCmd} {
		c.Flags().StringVar(&syncPauseStateDir, "state-dir", defaultStateDir,
			"daemon state directory (holds sync-pause.json)")
	}
	syncPauseCmd.Flags().StringVar(&syncPauseAgent, "agent", "",
		"pause only the named adapter (default: pause globally)")
	syncPauseCmd.Flags().DurationVar(&syncPauseFor, "for", 0,
		"auto-resume after this duration (default: pause until explicit resume)")
	syncResumeCmd.Flags().StringVar(&syncResumeAgent, "agent", "",
		"resume only the named adapter (default: resume global pause)")
	_ = syncPauseStatusJSON

	syncCmd.AddCommand(syncPauseCmd, syncResumeCmd, syncPauseStatusCmd, syncPauseStatusJSONCmd)
}
