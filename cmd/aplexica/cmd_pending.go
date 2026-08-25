package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/spf13/cobra"
)

var (
	pendingStateDir  string
	pendingStoreRoot string
	pendingJSON      bool
)

var pendingCmd = &cobra.Command{
	Use:   "pending",
	Short: "Manage project-scope artifacts whose project isn't linked locally",
	Long: `aplexica pending list enumerates project-scope artifacts whose canonical
project ID has no entry in the local projects registry. This state is
computed from the canonical store and local registry; there is no
separate staging directory.

Artifacts land here when the daemon imports a project-scope file from
a directory whose project (git remote URL or path-derived ID) hasn't
been linked via "aplexica project init" / "aplexica project link".
Once you link the project, the daemon's next fan-out pass re-exports
the pending artifacts to the linked path.`,
}

var pendingListCmd = &cobra.Command{
	Use:   "list",
	Short: "List pending projects (project ID + artifact count + sample source path)",
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: pendingStoreRoot}
		if err := store.Init(); err != nil {
			return fmt.Errorf("pending list: init store: %w", err)
		}
		reg, err := project.NewRegistry(filepath.Join(pendingStateDir, "projects.json"))
		if err != nil {
			return fmt.Errorf("pending list: open registry: %w", err)
		}
		entries, err := pending.List(store, reg)
		if err != nil {
			return err
		}
		if pendingJSON {
			b, err := json.MarshalIndent(entries, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}
		if len(entries) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "(no pending projects)")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-45s  %5s  %s\n", "PROJECT ID", "COUNT", "SAMPLE PATH")
		for _, e := range entries {
			fmt.Fprintf(cmd.OutOrStdout(), "%-45s  %5d  %s\n", e.ID, e.ArtifactCount, e.SamplePath)
		}
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(),
			"Link a project locally to materialize its pending artifacts:")
		fmt.Fprintln(cmd.OutOrStdout(),
			"  aplexica project link <project-id> <local-path>")
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	pendingCmd.PersistentFlags().StringVar(&pendingStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"daemon state directory (contains projects.json)")
	pendingCmd.PersistentFlags().StringVar(&pendingStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"canonical store root directory")
	pendingListCmd.Flags().BoolVar(&pendingJSON, "json", false, "emit machine-readable JSON")

	pendingCmd.AddCommand(pendingListCmd)
	rootCmd.AddCommand(pendingCmd)
}
