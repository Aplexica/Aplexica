package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/spf13/cobra"
)

var (
	backfillAgents   []string
	backfillDepth    int
	backfillScope    string
	backfillApply    bool
	backfillStateDir string
)

// backfillControlTimeout bounds the control-socket exchange. Planning walks
// every conversation artifact and evaluates routing rules per target, which on
// a multi-thousand-conversation store takes longer than the default 5s
// control read deadline.
const backfillControlTimeout = 2 * time.Minute

// backfillFullHistoryDepth is the sentinel for "no cap".
const backfillFullHistoryDepth = -1

var backfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "Force a LOCAL conversation backfill beyond the recent-history cap",
	Long: `Materializes canonical conversation history into this device's enabled
agents past the ordinary backfill cap (sync.convBackfill /
route.historicalSyncDepth, default 10 most-recent per agent).

Local by design: the backfill itself writes native session files for agents
on THIS device and publishes nothing. Its side effects flow through ordinary
sync like any other local activity — if a freshly materialized copy reveals a
provable legacy corruption in a conversation's canonical head, the repair
event replicates to peers exactly as a hand-authored repair would. That
replicates corrections to conversations peers already hold; it never ships
history a peer lacks. Cross-device ("cloud") backfill is reserved for a
future release behind the sync.cloudBackfill configuration key; --scope cloud
is refused today.

Two standing rules still shape the plan. Routing rules apply — forcing the
depth changes how much history a permitted agent receives, never which
conversations an agent may see. And a conversation is never materialized into
the agent that authored it (including the same-named agent on another
device), so backfilling only an agent's own conversations plans nothing.

The printed counts are ELIGIBILITY counts: the planner does not check what is
already materialized, so a fully backfilled store re-plans the same numbers
and the apply pass simply finds each session already current.

By default this prints the plan without writing anything. Pass --apply to
start the backfill; it runs inside the daemon in the background, and every
native write goes through the same guards as ordinary sync (an agent's own
diverged session defers to the retry queue rather than being overwritten —
watch progress with "aplexica status" and "aplexica repair materialization").

A full backfill of a large store creates one native session per conversation
per agent. On a store with thousands of conversations expect thousands of new
session files per agent, plus the one-time import scan they trigger.

Requires the daemon to be running.`,
	Args: cobra.NoArgs,
	RunE: runBackfill,
}

func init() {
	home, _ := os.UserHomeDir()
	backfillCmd.Flags().StringArrayVar(&backfillAgents, "agent", nil,
		"Limit the backfill to this target agent (repeatable; default: every enabled agent)")
	backfillCmd.Flags().IntVar(&backfillDepth, "depth", backfillFullHistoryDepth,
		"Most-recent conversations to materialize per agent (-1 = full history)")
	backfillCmd.Flags().StringVar(&backfillScope, "scope", "local",
		"Backfill scope: local (this device's agents). \"cloud\" is reserved for a future release and refused")
	backfillCmd.Flags().BoolVar(&backfillApply, "apply", false,
		"Start the backfill instead of only printing the plan (default: dry run)")
	backfillCmd.Flags().StringVar(&backfillStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"Daemon state directory (contains aplexicad.sock)")
	rootCmd.AddCommand(backfillCmd)
}

func runBackfill(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	// The daemon re-validates (it owns the sync.cloudBackfill gate); rejecting
	// obvious typos client-side just gives a faster error.
	if backfillScope != "local" && backfillScope != "cloud" {
		return fmt.Errorf("unknown --scope %q (expected local or cloud)", backfillScope)
	}

	sockPath := filepath.Join(backfillStateDir, "aplexicad.sock")
	resp, err := daemon.SendCommandWithTimeout(sockPath, daemon.Request{
		Command: "backfill",
		Agents:  backfillAgents,
		Depth:   backfillDepth,
		Scope:   backfillScope,
		DryRun:  !backfillApply,
	}, backfillControlTimeout)
	if err != nil {
		return fmt.Errorf("backfill requires the running daemon (is it started?): %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("daemon backfill error: %s", resp.Error)
	}

	// Recover the typed plan from the generic Data payload. Apply wraps it as
	// {"started": true, "plan": {...}}; dry run returns the plan directly.
	raw, err := json.Marshal(resp.Data)
	if err != nil {
		return fmt.Errorf("re-marshal backfill response: %w", err)
	}
	var applied struct {
		Started bool                       `json:"started"`
		Plan    syncd.ForcedBackfillResult `json:"plan"`
	}
	var plan syncd.ForcedBackfillResult
	if backfillApply {
		if err := json.Unmarshal(raw, &applied); err != nil {
			return fmt.Errorf("decode backfill response: %w", err)
		}
		plan = applied.Plan
	} else if err := json.Unmarshal(raw, &plan); err != nil {
		return fmt.Errorf("decode backfill plan: %w", err)
	}

	depthLabel := fmt.Sprintf("most-recent %d", backfillDepth)
	if backfillDepth < 0 {
		depthLabel = "full history"
	}
	fmt.Fprintf(out, "Local backfill plan (%s per agent):\n", depthLabel)
	agents := make([]string, 0, len(plan.PerAgent))
	for agent := range plan.PerAgent {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	if len(agents) == 0 {
		fmt.Fprintln(out, "  (no eligible conversations — a conversation is never backfilled into its")
		fmt.Fprintln(out, "   authoring agent, and routing rules still apply)")
	}
	for _, agent := range agents {
		fmt.Fprintf(out, "  %-14s %d conversation%s\n", agent, plan.PerAgent[agent], plural(plan.PerAgent[agent], "", "s"))
	}
	fmt.Fprintf(out, "%d conversation%s across %d agent%s\n",
		plan.Conversations, plural(plan.Conversations, "", "s"),
		len(plan.Targets), plural(len(plan.Targets), "", "s"))

	if !backfillApply {
		fmt.Fprintln(out, "dry run only (default); rerun with --apply to start the backfill")
		return nil
	}
	fmt.Fprintln(out, "backfill started in the daemon; it materializes in the background")
	fmt.Fprintln(out, "watch progress with \"aplexica status\" and \"aplexica repair materialization\"")
	return nil
}
