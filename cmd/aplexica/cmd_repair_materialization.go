package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/daemon"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/spf13/cobra"
)

var (
	repairMaterializationStateDir string
	repairMaterializationStore    string
	repairMaterializationAgent    string
	repairMaterializationArtifact string
	repairMaterializationDrop     bool
	repairMaterializationJSON     bool
)

var repairMaterializationCmd = &cobra.Command{
	Use:   "materialization",
	Short: "Inspect or drain the native-materialization retry queue",
	Long: `Lists the native session writes the daemon could not complete.

Each entry is one artifact the daemon still owes to one agent. The daemon
retries an entry with exponential backoff; an entry that never succeeds is
eventually abandoned, which leaves an "abandoned" row here and a warning in
the daemon log instead of retrying forever.

The usual cause of a permanently stuck entry is a canonical conversation head
whose turns duplicated while its native session did not. Those never converge
on their own — repair the head first (see "aplexica repair conversation"),
then drop the stale entry:

  aplexica repair materialization
  aplexica repair materialization --agent claude-code
  aplexica repair materialization --drop --artifact 019f8c5e-...
  aplexica repair materialization --drop --agent codex

Dropping a pending entry forfeits that write: nothing rematerializes the
artifact until it is deferred again by a new canonical commit, a daemon
restart reconciliation, or an explicit "aplexica materialize". Dropping an
abandoned entry only clears the diagnostic.

Reads live daemon state over the control socket when the daemon is running,
and falls back to the on-disk journal when it is not.`,
	Args: cobra.NoArgs,
	RunE: runRepairMaterialization,
}

func init() {
	home, _ := os.UserHomeDir()
	repairMaterializationCmd.Flags().StringVar(&repairMaterializationStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"Daemon state directory (contains aplexicad.sock)")
	repairMaterializationCmd.Flags().StringVar(&repairMaterializationStore, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root (used when the daemon is not running)")
	repairMaterializationCmd.Flags().StringVar(&repairMaterializationAgent, "agent", "",
		"Limit to one target agent (default: all agents)")
	repairMaterializationCmd.Flags().StringVar(&repairMaterializationArtifact, "artifact", "",
		"Limit to one artifact ID (default: all artifacts)")
	repairMaterializationCmd.Flags().BoolVar(&repairMaterializationDrop, "drop", false,
		"Remove the selected entries instead of only listing them")
	repairMaterializationCmd.Flags().BoolVar(&repairMaterializationJSON, "json", false,
		"Emit machine-readable JSON")
	repairCmd.AddCommand(repairMaterializationCmd)
}

func runRepairMaterialization(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	sockPath := filepath.Join(repairMaterializationStateDir, "aplexicad.sock")
	daemonUp := daemonAcceptsControl(sockPath)

	if repairMaterializationDrop {
		return dropDeferredMaterializations(out, sockPath, daemonUp)
	}
	rows, err := listDeferredMaterializations(sockPath, daemonUp)
	if err != nil {
		return err
	}
	rows = filterDeferredMaterializations(rows, repairMaterializationAgent, repairMaterializationArtifact)
	if repairMaterializationJSON {
		encoded, mErr := json.Marshal(rows)
		if mErr != nil {
			return mErr
		}
		fmt.Fprintln(out, string(encoded))
		return nil
	}
	renderDeferredMaterializationRows(out, rows, daemonUp)
	return nil
}

func daemonAcceptsControl(sockPath string) bool {
	resp, err := daemon.SendCommand(sockPath, daemon.Request{Command: "status"})
	return err == nil && resp.OK
}

func listDeferredMaterializations(sockPath string, daemonUp bool) ([]map[string]any, error) {
	if !daemonUp {
		return syncd.LoadDeferredMaterializationJournal(repairMaterializationStore)
	}
	info, err := queryDaemonStatus(sockPath, false)
	if err != nil {
		return nil, err
	}
	return info.DeferredMaterializations, nil
}

func dropDeferredMaterializations(out io.Writer, sockPath string, daemonUp bool) error {
	if !daemonUp {
		// A failed control probe is not proof the daemon is gone — a wrong
		// --state-dir, a timeout, or the startup window before the control
		// server binds all look identical from here, and in that window the
		// daemon already owns the in-memory queue and would overwrite this
		// edit on its next persist. Direct mutation is therefore allowed only
		// while this process holds the daemon instance lock, which a live
		// daemon keeps for its whole lifetime.
		instanceLock, lockErr := daemon.Acquire(
			filepath.Join(repairMaterializationStateDir, "aplexicad.lock"))
		if lockErr != nil {
			return fmt.Errorf(
				"daemon control unavailable and running-daemon exclusion failed "+
					"(is the daemon running with a different --state-dir?): %w", lockErr)
		}
		defer func() { _ = instanceLock.Release() }()
		// With the daemon stopped the journal IS the queue, so editing it in
		// place is safe. With it running the in-memory queue wins, so the drop
		// has to go through the control socket or it would be silently undone.
		dropped, err := syncd.DropDeferredMaterializationJournal(
			repairMaterializationStore, repairMaterializationAgent, repairMaterializationArtifact)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Dropped %d deferred materialization entr%s from the store journal.\n",
			dropped, plural(dropped, "y", "ies"))
		return nil
	}
	resp, err := daemon.SendCommand(sockPath, daemon.Request{
		Command:    "deferred-drop",
		Agent:      repairMaterializationAgent,
		ArtifactID: repairMaterializationArtifact,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("daemon deferred-drop error: %s", resp.Error)
	}
	dropped := 0
	if data, ok := resp.Data.(map[string]any); ok {
		if n, ok := data["dropped"].(float64); ok {
			dropped = int(n)
		}
	}
	fmt.Fprintf(out, "Dropped %d deferred materialization entr%s.\n", dropped, plural(dropped, "y", "ies"))
	return nil
}

func filterDeferredMaterializations(rows []map[string]any, agent, artifactID string) []map[string]any {
	if agent == "" && artifactID == "" {
		return rows
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if agent != "" {
			if name, _ := row["agent"].(string); name != agent {
				continue
			}
		}
		if artifactID != "" {
			if id, _ := row["artifactId"].(string); id != artifactID {
				continue
			}
		}
		out = append(out, row)
	}
	return out
}

func renderDeferredMaterializationRows(out io.Writer, rows []map[string]any, daemonUp bool) {
	source := "store journal (daemon not running)"
	if daemonUp {
		source = "live daemon"
	}
	if len(rows) == 0 {
		fmt.Fprintf(out, "No deferred native materializations — %s.\n", source)
		return
	}
	fmt.Fprintf(out, "Deferred native materializations (%s):\n", source)
	for _, row := range rows {
		agent, _ := row["agent"].(string)
		artifactID, _ := row["artifactId"].(string)
		if artifactID == "" {
			artifactID = "(whole target)"
		}
		state, _ := row["state"].(string)
		if deferred, _ := row["escalationDeferred"].(bool); deferred {
			state += "*"
		}
		fmt.Fprintf(out, "  %-12s %-38s %-9s attempts=%d\n",
			agent, artifactID, state, deferredMaterializationRowInt(row, "attempts"))
		for _, key := range []string{
			"originAgent", "reason", "retryClass",
			"firstDeferredAt", "nextAttemptAt", "abandonedAt", "lastError",
		} {
			if value, _ := row[key].(string); value != "" {
				fmt.Fprintf(out, "      %-16s %s\n", key, value)
			}
		}
		// explain and remedy are per class, so they belong beside the entry
		// rather than in one trailing sentence that has to fit every row.
		if explain, _ := row["explain"].(string); explain != "" {
			fmt.Fprintf(out, "      %-16s %s\n", "explain", explain)
		}
		if remedy, _ := row["remedy"].(string); remedy != "" {
			fmt.Fprintf(out, "      %-16s %s\n", "fix with", remedy)
		}
	}
	attention, held := 0, 0
	for _, row := range rows {
		if state, _ := row["state"].(string); state == "needs_attention" || state == "abandoned" {
			attention++
		}
		if deferred, _ := row["escalationDeferred"].(bool); deferred {
			held++
		}
	}
	if attention > 0 {
		fmt.Fprintf(out,
			"\n%d entr%s stopped being retried. Each row above names what to run, or says that nothing repairs it.\n",
			attention, plural(attention, "y", "ies"))
	}
	if held > 0 {
		fmt.Fprintf(out,
			"%d entr%s marked * qualified to be raised and are waiting on the %d-per-day limit.\n",
			held, plural(held, "y", "ies"), syncd.EscalationsPerDay)
	}
}

// deferredMaterializationRowInt reads a numeric row field, which arrives as an
// int in-process and as a float64 after a JSON round-trip over the control
// socket.
func deferredMaterializationRowInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}
