package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/nativebackup"
	"github.com/spf13/cobra"
)

var (
	restoreNativeAgent string
	restoreNativeFrom  string
	restoreNativeYes   bool
	restoreNativeJSON  bool

	backupsListJSON bool
)
var restoreNativeRootsForTest func() []nativebackup.AgentRoots

// restoreNativeCmd implements `aplexica restore-native` — roll the
// machine's native agent state back to a pre-Aplexica (or any prior)
// snapshot. DESTRUCTIVE: it overwrites the live native files. The
// underlying nativebackup.Restore is reversible (it snapshots the
// current state into a pre-restore-* directory first), but the user
// must still confirm with --yes or interactively.
var restoreNativeCmd = &cobra.Command{
	Use:   "restore-native",
	Short: "Restore agents' native on-disk state from a snapshot (DESTRUCTIVE)",
	Long: `Copy a native snapshot's files back over the live agent roots
(~/.claude, ~/.codex, ~/.hermes, …), rolling back to the captured
state. By default the latest first-run "pre-sync" snapshot is used.

This is DESTRUCTIVE — it overwrites the agents' current native files.
It is REVERSIBLE: before overwriting anything, the current native state
is snapshotted into ~/.aplexica/backups/pre-restore-<timestamp>/, so a
mistaken restore can itself be undone with another restore-native.

Examples:
  aplexica restore-native --yes
  aplexica restore-native --from pre-sync-2026-05-29T14-00-00Z --agent hermes --yes`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		backupsRoot := nativeBackupsRoot()

		from := restoreNativeFrom
		if from == "" {
			latest, err := latestPreSyncID(backupsRoot)
			if err != nil {
				return err
			}
			from = latest
		}
		dir, err := resolveBackupDir(backupsRoot, from)
		if err != nil {
			return err
		}

		// Confirmation gate (destructive). --yes bypasses; otherwise
		// prompt interactively and require an explicit "yes"/"y".
		if !restoreNativeYes {
			target := "ALL agents"
			if restoreNativeAgent != "" {
				target = "agent " + restoreNativeAgent
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"About to restore %s from snapshot %q, OVERWRITING current native files.\n",
				target, from)
			fmt.Fprint(cmd.OutOrStdout(), "Type 'yes' to proceed: ")
			reader := bufio.NewReader(cmd.InOrStdin())
			line, _ := reader.ReadString('\n')
			ans := strings.ToLower(strings.TrimSpace(line))
			if ans != "yes" && ans != "y" {
				return fmt.Errorf("aborted (no confirmation; pass --yes to skip the prompt)")
			}
		}

		var res nativebackup.RestoreResult
		sock, _ := daemonControlSocket()
		if resp, ctlErr := daemon.SendCommand(sock, daemon.Request{Command: "native-restore", BackupID: from, Agent: restoreNativeAgent}); ctlErr == nil {
			if !resp.OK {
				return fmt.Errorf("native restore: %s", resp.Error)
			}
			b, _ := json.Marshal(resp.Data)
			if err := json.Unmarshal(b, &res); err != nil {
				return err
			}
		} else {
			roots, err := discoverNativeRootsForRestore()
			if err != nil {
				return err
			}
			home, _ := os.UserHomeDir()
			res, err = nativebackup.RestoreWithOptions(cmd.Context(), dir, nativebackup.NativeRestoreOptions{
				Agent: restoreNativeAgent, CurrentAgentRoots: roots,
				ManifestKeyPath: filepath.Join(home, ".aplexica", "keys", "native-manifest-hmac-v2"),
				Coordinator:     nativebackup.LocalRestoreCoordinator{LockPath: filepath.Join(home, ".aplexica", "state", "native-restore.lock")},
				ExcludeTarget:   nativeBackupDynamicTargetExcluded,
			})
			if err != nil {
				return err
			}
		}

		if restoreNativeJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
		}
		ok, failed := 0, 0
		for _, fr := range res.Files {
			if fr.OK {
				ok++
			} else {
				failed++
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"restored %d file(s) from %q (%d ok, %d failed)\n",
			len(res.Files), from, ok, failed)
		fmt.Fprintf(cmd.OutOrStdout(),
			"reversible pre-restore snapshot of the prior state: %s\n", res.PreRestoreDir)
		for _, fr := range res.Files {
			if !fr.OK {
				fmt.Fprintf(cmd.ErrOrStderr(), "  FAILED %s: %s\n", fr.Path, fr.Err)
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d file(s) failed to restore", failed)
		}
		return nil
	},
}

func discoverNativeRootsForRestore() ([]nativebackup.AgentRoots, error) {
	if restoreNativeRootsForTest != nil {
		roots := restoreNativeRootsForTest()
		if len(roots) > 0 {
			return roots, nil
		}
	}
	all := []adapter.Adapter{claudecode.New(), codex.New(), kilo.New(), hermes.New(), openclaw.New()}
	discoveries := map[string]adapter.Discovery{}
	for _, ad := range all {
		d, err := ad.Discover()
		if err != nil {
			continue
		}
		discoveries[ad.Name()] = d
	}
	roots := agentRootsFromDiscoveries(discoveries)
	if len(roots) == 0 {
		return nil, fmt.Errorf("no currently installed agent roots found")
	}
	return withNativeBackupContentExclusions(roots), nil
}

// backupsCmd is the parent for native-snapshot management subcommands.
var backupsCmd = &cobra.Command{
	Use:   "backups",
	Short: "Manage native agent-state snapshots (first-run + reversible pre-restore)",
	Long: `Native snapshots capture each agent's own on-disk state (the files
agents like Claude, Codex, and Hermes write for themselves) so you can
roll back to your pre-Aplexica state. They live under
~/.aplexica/backups and are distinct from store bundles produced by
'aplexica backup'.`,
}

// backupsListCmd implements `aplexica backups list`.
var backupsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List native snapshots (newest first)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		backupsRoot := nativeBackupsRoot()
		infos, err := nativebackup.List(backupsRoot)
		if err != nil {
			return err
		}
		if backupsListJSON {
			if infos == nil {
				infos = []nativebackup.BackupInfo{}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(infos)
		}
		if len(infos) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "no native snapshots under %s\n", backupsRoot)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-40s  %-12s  %-20s  %8s  %s\n",
			"ID", "KIND", "CREATED", "FILES", "AGENTS")
		for _, bi := range infos {
			fmt.Fprintf(cmd.OutOrStdout(), "%-40s  %-12s  %-20s  %8d  %s\n",
				bi.ID, bi.Kind, bi.CreatedAt.Format("2006-01-02T15:04:05Z"),
				bi.FileCount, strings.Join(bi.Agents, ","))
		}
		return nil
	},
}

func init() {
	restoreNativeCmd.Flags().StringVar(&restoreNativeAgent, "agent", "",
		"Restore only this agent (default: every agent in the snapshot)")
	restoreNativeCmd.Flags().StringVar(&restoreNativeFrom, "from", "",
		"Snapshot ID to restore from (default: the latest pre-sync-* snapshot)")
	restoreNativeCmd.Flags().BoolVar(&restoreNativeYes, "yes", false,
		"Skip the interactive confirmation (the restore is destructive)")
	restoreNativeCmd.Flags().BoolVar(&restoreNativeJSON, "json", false,
		"Emit the per-file RestoreResult as JSON")
	rootCmd.AddCommand(restoreNativeCmd)

	backupsListCmd.Flags().BoolVar(&backupsListJSON, "json", false,
		"Emit the snapshot list as JSON")
	backupsCmd.AddCommand(backupsListCmd)
	rootCmd.AddCommand(backupsCmd)
}
