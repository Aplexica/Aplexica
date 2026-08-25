package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/anonymize"
)

var (
	backupStoreRoot        string
	backupSecretsRoot      string
	backupIncludeSecrets   bool
	backupRespectSyncFlags bool
	backupSign             bool
	backupKeyPath          string
	backupEncrypt          bool
	backupRecipientPath    string
	backupPassphraseEnvVar string
	backupAnonymize        bool
	backupAnonymizeDryRun  bool
	backupJSON             bool

	// v0.63.0 BRD-02 §4.13 / FR-01.10 scope filters.
	backupScope                  string
	backupProjects               []string
	backupIncludePendingProjects bool
	backupStateDir               string // for resolving the pending-projects registry
)

// aplexicaCLIVersion is the version recorded in BundleMeta.AplexicaVersion.
// Bumped manually per release; could be wired to build flags later.
const aplexicaCLIVersion = "0.1.10"

var backupCmd = &cobra.Command{
	Use:   "backup <bundle-path>",
	Short: "Export the entire canonical store to a portable .tar.gz bundle",
	Long: `Write a .tar.gz bundle containing every artifact in the canonical store.
By default secrets are NOT included; pass --include-secrets to bundle
the secrets store too.

The bundle is suitable for backup, transfer between machines, or input to
'aplexica restore'.

Optional --encrypt wraps the bundle in age format (X25519 recipient or
scrypt passphrase). If both --encrypt and --sign are used the order is
encrypt-first-then-sign — the signature covers the encrypted bytes (which
are what gets transmitted). Restore order is verify-first-then-decrypt.`,
	// --anonymize-dry-run scans the store and prints a count without
	// writing a bundle, so the <bundle-path> arg is optional in that mode.
	// All other modes still require exactly one positional arg.
	Args: cobra.RangeArgs(0, 1),
	RunE: secureBackupRunE,
}

func init() {
	home, _ := os.UserHomeDir()
	backupCmd.Flags().StringVar(&backupStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	backupCmd.Flags().StringVar(&backupSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root (only walked if --include-secrets is set)")
	backupCmd.Flags().BoolVar(&backupRespectSyncFlags, "respect-sync-flags", true,
		"When --include-secrets is set, bundle only global-name "+
			"secrets whose .meta/<name>.json sidecar has syncEnabled=true. "+
			"Pass --respect-sync-flags=false to bundle every secret unconditionally.")
	backupCmd.Flags().BoolVar(&backupIncludeSecrets, "include-secrets", false,
		"Include the secrets store in the bundle (default: false)")
	backupCmd.Flags().BoolVar(&backupSign, "sign", false,
		"Write a detached <bundle>.sig signature next to the bundle")
	backupCmd.Flags().StringVar(&backupKeyPath, "key", "",
		"Ed25519 private key path (required with --sign)")
	backupCmd.Flags().BoolVar(&backupEncrypt, "encrypt", false,
		"Encrypt the bundle in place with age (X25519 recipient or scrypt passphrase)")
	backupCmd.Flags().StringVar(&backupRecipientPath, "recipient", "",
		"Path to age recipient file (mutually exclusive with --passphrase-from-env)")
	backupCmd.Flags().StringVar(&backupPassphraseEnvVar, "passphrase-from-env", "",
		"Read scrypt passphrase from this env var (mutually exclusive with --recipient)")
	backupCmd.Flags().StringVar(&backupScope, "scope", "",
		"Only include artifacts of this scope kind (global / project / namespace). Empty = no filter.")
	backupCmd.Flags().StringSliceVar(&backupProjects, "project", nil,
		"Only include artifacts whose Project.ID matches one of these (repeatable; comma-separated also accepted).")
	backupCmd.Flags().BoolVar(&backupIncludePendingProjects, "include-pending-projects", true,
		"Include project-scope artifacts whose project isn't currently linked on this device. Default true.")
	backupCmd.Flags().StringVar(&backupStateDir, "state-dir", "",
		"daemon state directory (for resolving the pending-projects registry when --include-pending-projects=false). Default: ~/.aplexica/state")
	backupCmd.Flags().BoolVar(&backupAnonymize, "anonymize", false,
		"Strip PII from bundle payloads: $HOME→~, emails, common secret patterns (mutually exclusive with --include-secrets)")
	backupCmd.Flags().BoolVar(&backupAnonymizeDryRun, "anonymize-dry-run", false,
		"Scan the store and print what --anonymize would scrub, but don't write a bundle")
	backupCmd.Flags().BoolVar(&backupJSON, "json", false,
		"Emit a machine-readable JSON result (suppresses the human note/wrote lines)")
	rootCmd.AddCommand(backupCmd)
}

// runAnonymizeDryRun walks the store, runs each artifact JSON + each event
// JSON through anonymize.Scrub, and prints a per-kind match summary. Does
// NOT write a bundle. Used by `aplexica backup --anonymize-dry-run`.
func runAnonymizeDryRun(cmd *cobra.Command, storeRoot string) error {
	store := &acf.Store{Root: storeRoot}
	if err := store.Init(); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	opts := anonymize.Options{HomeDir: home, RedactEmails: true, ScrubSecrets: true}
	totals := map[string]int{}
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			return fmt.Errorf("dry-run list %s: %w", k, err)
		}
		for _, a := range arts {
			artJSON, _ := json.Marshal(a)
			_, m1 := anonymize.Scrub(artJSON, opts)
			for _, m := range m1 {
				totals[m.Kind]++
			}
			events, _ := store.ReadEvents(k, a.ArtifactID)
			for _, e := range events {
				evJSON, _ := json.Marshal(e)
				_, m2 := anonymize.Scrub(evJSON, opts)
				for _, m := range m2 {
					totals[m.Kind]++
				}
			}
		}
	}
	fmt.Fprint(cmd.OutOrStdout(), "dry-run summary:")
	any := false
	for _, kind := range []string{"path", "email", "secret"} {
		if n, ok := totals[kind]; ok {
			fmt.Fprintf(cmd.OutOrStdout(), " %s=%d", kind, n)
			any = true
		}
	}
	if !any {
		fmt.Fprint(cmd.OutOrStdout(), " no matches")
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}
