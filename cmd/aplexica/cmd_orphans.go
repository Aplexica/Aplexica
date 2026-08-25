package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	orphansStoreRoot string
	orphansJSON      bool
)

var orphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "Inspect and clean orphaned artifacts",
}

var orphansListCmd = &cobra.Command{
	Use:   "list",
	Short: "List artifacts marked as orphaned in one or more agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: orphansStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		records, err := store.ListOrphans()
		if err != nil {
			return err
		}
		if orphansJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(records)
		}
		if len(records) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "(no orphans)")
			return nil
		}
		for _, r := range records {
			for agent, entry := range r.ByAgent {
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s  agent=%s  detected=%s\n",
					r.Kind, r.ArtifactID, agent, entry.DetectedAt.Format("2006-01-02T15:04:05Z"))
			}
		}
		return nil
	},
}

var orphansCleanCmd = &cobra.Command{
	Use:   "clean <agent>",
	Short: "Clear orphan markers for one agent (does NOT delete native files; user must remove them)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: orphansStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		records, err := store.ListOrphans()
		if err != nil {
			return err
		}
		agent := args[0]
		cleared := 0
		for _, r := range records {
			if _, ok := r.ByAgent[agent]; !ok {
				continue
			}
			if err := store.ClearOrphan(r.ArtifactID, agent); err != nil {
				return err
			}
			cleared++
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"cleared %d orphan marker(s) for agent %q (native files were NOT removed — see `aplexica orphans list` for the full path)\n",
			cleared, agent)
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")

	for _, c := range []*cobra.Command{orphansListCmd, orphansCleanCmd} {
		c.Flags().StringVar(&orphansStoreRoot, "store", defaultStore, "Canonical store root")
		c.Flags().BoolVar(&orphansJSON, "json", false, "Emit JSON instead of plain text")
	}
	orphansCmd.AddCommand(orphansListCmd, orphansCleanCmd)
	rootCmd.AddCommand(orphansCmd)
}
