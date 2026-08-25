package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/spf13/cobra"
)

var exportStoreRoot string
var exportSecretsRoot string

var exportCmd = &cobra.Command{
	Use:   "export <adapter> <artifact-id> <dest-path>",
	Short: "Export a Claude Code or Codex artifact (memory, skill, conversation, or tool) to a native file",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: exportStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		a, err := buildAdapter(args[0], exportSecretsRoot)
		if err != nil {
			return err
		}
		if err := a.Export(context.Background(), store, args[1], args[2]); err != nil {
			if errors.Is(err, adapter.ErrArtifactTombstoned) {
				fmt.Fprintf(cmd.ErrOrStderr(), "skipped %s (tombstoned/redacted)\n", args[1])
				return nil
			}
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "wrote:", args[2])
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	exportCmd.Flags().StringVar(&exportStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	exportCmd.Flags().StringVar(&exportSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory")
	rootCmd.AddCommand(exportCmd)
}
