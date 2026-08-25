package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/spf13/cobra"
)

var listSecretsRoot string

var listSecretsCmd = &cobra.Command{
	Use:   "list-secrets <artifact-id>",
	Short: "List secret key names for an artifact (never prints values)",
	Long: `List secret key names stored for a tool artifact. Secret VALUES
are never printed by this command — only the names.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		ss := &secrets.Store{Root: listSecretsRoot}

		keys, err := ss.ListForArtifact(id)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(keys) == 0 {
			fmt.Fprintln(out, "(no secrets for this artifact)")
			return nil
		}
		fmt.Fprintf(out, "Secret key names for artifact %s (%d):\n", id, len(keys))
		for _, k := range keys {
			fmt.Fprintf(out, "  %s\n", k)
		}
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	listSecretsCmd.Flags().StringVar(&listSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory")
	rootCmd.AddCommand(listSecretsCmd)
}
