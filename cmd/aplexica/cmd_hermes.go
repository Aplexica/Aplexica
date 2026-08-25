package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/spf13/cobra"
)

var (
	hermesStoreRoot    string
	hermesSecretsRoot  string
	hermesDBPath       string
	hermesSinceSeconds float64
	hermesSessionID    string
	hermesArtifactID   string
	hermesCanonical    bool
)

var hermesCmd = &cobra.Command{
	Use:   "hermes",
	Short: "Hermes-specific subcommands (SQLite-backed conversation kind)",
}

var hermesExportSessionsCmd = &cobra.Command{
	Use:   "export-sessions",
	Short: "Read Hermes sessions from state.db and write ACF conversation artifacts",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, a, err := setupHermes()
		if err != nil {
			return err
		}
		ids, err := a.ImportConversationsFromDB(context.Background(), store, hermesDBPath, hermesSinceSeconds)
		if err != nil {
			return err
		}
		for _, id := range ids {
			fmt.Fprintln(cmd.OutOrStdout(), id)
		}
		if len(ids) == 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "no sessions matched")
		}
		return nil
	},
}

var hermesImportSessionsCmd = &cobra.Command{
	Use:   "import-sessions",
	Short: "Read ACF conversation artifacts and INSERT them into Hermes state.db",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, a, err := setupHermes()
		if err != nil {
			return err
		}
		var ids []string
		if hermesArtifactID != "" {
			ids = []string{hermesArtifactID}
		} else {
			arts, err := store.ListArtifacts(acf.KindConversation)
			if err != nil {
				return fmt.Errorf("list conversation artifacts: %w", err)
			}
			for _, art := range arts {
				ids = append(ids, art.ArtifactID)
			}
		}
		if len(ids) == 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "no conversation artifacts in store")
			return nil
		}
		for _, id := range ids {
			if err := a.ExportConversationsToDB(context.Background(), store, id, hermesDBPath); err != nil {
				return fmt.Errorf("export %s: %w", id, err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
		}
		return nil
	},
}

func setupHermes() (*acf.Store, *hermes.Adapter, error) {
	store := &acf.Store{Root: hermesStoreRoot}
	if err := store.Init(); err != nil {
		return nil, nil, err
	}
	ss := &secrets.Store{Root: hermesSecretsRoot}
	if err := ss.Init(); err != nil {
		return nil, nil, err
	}
	a := hermes.New()
	a.SecretsStore = ss
	a.CanonicalConversations = hermesCanonical
	// Stamp the daemon's cloud identity so hermes-authored events are
	// publishable by the outbound sweep (see cliCloudDeviceID).
	if deviceID := cliCloudDeviceID(); deviceID != "" {
		a.SetDeviceID(deviceID)
	}
	return store, a, nil
}

func init() {
	home, _ := os.UserHomeDir()
	defaultDB := filepath.Join(home, ".hermes", "state.db")
	defaultStore := filepath.Join(home, ".aplexica", "store")
	defaultSecrets := filepath.Join(home, ".aplexica", "secrets")

	for _, c := range []*cobra.Command{hermesExportSessionsCmd, hermesImportSessionsCmd} {
		c.Flags().StringVar(&hermesStoreRoot, "store", defaultStore, "Canonical store root")
		c.Flags().StringVar(&hermesSecretsRoot, "secrets-root", defaultSecrets, "Secrets store root")
		c.Flags().StringVar(&hermesDBPath, "db", defaultDB, "Hermes state.db path")
		c.Flags().BoolVar(&hermesCanonical, "canonical", false,
			"Encode/decode conversations as acf.conversation.v1 (default: legacy acf.hermes.session.v1)")
	}
	hermesExportSessionsCmd.Flags().Float64Var(&hermesSinceSeconds, "since", 0, "Only export sessions with started_at > this unix timestamp")
	hermesExportSessionsCmd.Flags().StringVar(&hermesSessionID, "session", "", "(reserved for future use; filters not yet wired)")
	hermesImportSessionsCmd.Flags().StringVar(&hermesArtifactID, "artifact", "", "Only import this single conversation artifact ID")

	hermesCmd.AddCommand(hermesExportSessionsCmd)
	hermesCmd.AddCommand(hermesImportSessionsCmd)
	rootCmd.AddCommand(hermesCmd)
}
