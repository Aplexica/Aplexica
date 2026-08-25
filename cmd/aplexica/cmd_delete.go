package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/spf13/cobra"
)

var (
	deleteStoreRoot   string
	deleteSecretsRoot string
	deleteYes         bool
)

var deleteCmd = &cobra.Command{
	Use:   "delete <artifact-id>",
	Short: "Delete an artifact from the canonical store (irreversible)",
	Long: `Delete an artifact and its event log from the canonical store, plus any
secrets associated with the artifact in the secrets store. This is
IRREVERSIBLE — the data cannot be recovered without a backup.

By default the command prompts for confirmation. Pass --yes to skip
the prompt (e.g. for use in scripts).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		store := &acf.Store{Root: deleteStoreRoot}

		// Find which kind this artifact is. Try each.
		var foundKind acf.Kind
		var found acf.Artifact
		ok := false
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			a, err := store.ReadArtifact(k, id)
			if err == nil {
				found = a
				foundKind = k
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("artifact %s not found in canonical store", id)
		}

		// Check whether this artifact has secrets (needs ss). Surface a
		// real list error rather than swallowing it: a swallowed error
		// would make secretKeys empty, skip the secret deletion below, and
		// orphan the secret material on disk after the artifact is gone.
		ss := &secrets.Store{Root: deleteSecretsRoot}
		secretKeys, err := ss.ListForArtifact(id)
		if err != nil {
			return fmt.Errorf("list secrets for %s: %w", id, err)
		}

		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "About to delete:")
		fmt.Fprintf(out, "  ID:        %s\n", found.ArtifactID)
		fmt.Fprintf(out, "  Kind:      %s\n", found.Kind)
		fmt.Fprintf(out, "  Name:      %s\n", found.Name)
		fmt.Fprintf(out, "  Scope:     %s\n", found.Scope)
		if len(secretKeys) > 0 {
			fmt.Fprintf(out, "  Secrets:   %d key(s) — %s\n", len(secretKeys), strings.Join(secretKeys, ", "))
		}
		fmt.Fprintln(out, "This is IRREVERSIBLE. To recover, you'll need a backup.")

		if !deleteYes {
			fmt.Fprint(out, "Type 'delete' to confirm: ")
			reader := bufio.NewReader(cmd.InOrStdin())
			line, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("read confirmation: %w", err)
			}
			if strings.TrimSpace(line) != "delete" {
				fmt.Fprintln(out, "cancelled")
				return nil
			}
		}

		if err := store.DeleteArtifact(foundKind, id); err != nil {
			return err
		}
		// Always remove secrets, even if the count above was zero:
		// DeleteForArtifact is idempotent for a missing dir, and an
		// unconditional call avoids leaving stranded secret material if the
		// listing under-reported.
		if err := ss.DeleteForArtifact(id); err != nil {
			return fmt.Errorf("delete secrets: %w", err)
		}
		fmt.Fprintf(out, "deleted %s artifact %s\n", foundKind, id)
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	deleteCmd.Flags().StringVar(&deleteStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	deleteCmd.Flags().StringVar(&deleteSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory")
	deleteCmd.Flags().BoolVar(&deleteYes, "yes", false,
		"Skip the confirmation prompt (e.g. for scripts)")
	rootCmd.AddCommand(deleteCmd)
}
