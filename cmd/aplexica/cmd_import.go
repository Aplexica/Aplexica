package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/spf13/cobra"
)

var importStoreRoot string
var importSecretsRoot string
var importCanonical bool

var importCmd = &cobra.Command{
	Use:   "import <adapter> <path>",
	Short: "Import a native agent file or DB into the canonical store",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: importStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		a, err := buildAdapter(args[0], importSecretsRoot)
		if err != nil {
			return err
		}
		// --canonical flips the canonical-conversation flag on the adapters
		// that support it (claudecode since v0.15.0, codex since v0.16.0,
		// hermes since v0.17.0, openclaw since v0.24.1). Kilo DB imports
		// already emit acf.conversation.v1. Silently ignored for adapters
		// that don't recognize it.
		if cc, ok := a.(*claudecode.Adapter); ok && importCanonical {
			cc.CanonicalConversations = true
		}
		if cx, ok := a.(*codex.Adapter); ok && importCanonical {
			cx.CanonicalConversations = true
		}
		if hr, ok := a.(*hermes.Adapter); ok && importCanonical {
			hr.CanonicalConversations = true
		}
		if oc, ok := a.(*openclaw.Adapter); ok && importCanonical {
			oc.CanonicalConversations = true
		}
		ids, err := a.Import(context.Background(), store, args[1])
		if err != nil {
			return err
		}
		for _, id := range ids {
			fmt.Fprintln(cmd.OutOrStdout(), id)
		}
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	importCmd.Flags().StringVar(&importStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	importCmd.Flags().StringVar(&importSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory")
	importCmd.Flags().BoolVar(&importCanonical, "canonical", false,
		"For conversation imports: encode as acf.conversation.v1 (structured "+
			"event log) instead of the legacy opaque format. Supported by the "+
			"claude-code (v0.15.0+), codex (v0.16.0+), hermes (v0.17.0+), and "+
			"openclaw (v0.24.1+) adapters; Kilo DB imports are always canonical.")
	rootCmd.AddCommand(importCmd)
}
