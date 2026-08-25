package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/aplexica/aplexica/internal/acf"
)

var (
	restoreStoreRoot        string
	restoreSecretsRoot      string
	restorePeek             bool
	restoreDryRun           bool
	restoreVerify           bool
	restorePubKeyPath       string
	restoreExpectedKeyID    string
	restoreUnsignedOK       bool
	restoreDecrypt          bool
	restoreIdentityPath     string
	restorePassphraseEnvVar string
	restoreJSON             bool
)

var restoreCmd = &cobra.Command{
	Use:   "restore <bundle-path>",
	Short: "Restore artifacts from a .tar.gz bundle into the canonical store",
	Long: `Read a bundle produced by 'aplexica backup' and write its contents into the
canonical store (and the secrets store, if the bundle included secrets).
Fails if any artifact in the bundle already exists in the target store
(no merge/overwrite). Pass --peek to print bundle metadata without writing.

Optional --decrypt unwraps an age-encrypted bundle (X25519 identity or
scrypt passphrase). If both --verify and --decrypt are used, verification
runs FIRST over the bytes-as-on-disk (which are the encrypted bytes — the
signature was computed after encryption), then decryption runs.`,
	Args: cobra.ExactArgs(1),
	RunE: secureRestoreRunE,
}

func init() {
	home, _ := os.UserHomeDir()
	restoreCmd.Flags().StringVar(&restoreStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory (target of restore)")
	restoreCmd.Flags().StringVar(&restoreSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root (only used when bundle includes secrets)")
	restoreCmd.Flags().BoolVar(&restorePeek, "peek", false,
		"Print bundle metadata without writing anything")
	restoreCmd.Flags().BoolVar(&restoreDryRun, "dry-run", false,
		"Classify each bundled artifact as would-add or already-exists "+
			"against the target store and print the diff, without writing anything")
	restoreCmd.Flags().BoolVar(&restoreVerify, "verify", false,
		"Require a valid <bundle>.sig signature before restoring")
	restoreCmd.Flags().StringVar(&restorePubKeyPath, "pubkey", "",
		"Ed25519 public key path (required with --verify)")
	restoreCmd.Flags().StringVar(&restoreExpectedKeyID, "key-id", "",
		"Pinned full SHA-256 key ID (required for signed bundles)")
	restoreCmd.Flags().BoolVar(&restoreUnsignedOK, "unsigned-ok", false,
		"explicitly acknowledge an unsigned bundle")
	restoreCmd.Flags().BoolVar(&restoreDecrypt, "decrypt", false,
		"Decrypt the bundle with age before restoring (X25519 or scrypt)")
	restoreCmd.Flags().StringVar(&restoreIdentityPath, "identity", "",
		"Path to age identity (AGE-SECRET-KEY-... file)")
	restoreCmd.Flags().StringVar(&restorePassphraseEnvVar, "passphrase-from-env", "",
		"Read scrypt passphrase from this env var")
	restoreCmd.Flags().BoolVar(&restoreJSON, "json", false,
		"Emit a machine-readable JSON result (with --peek, emit the BundleMeta as JSON)")
	rootCmd.AddCommand(restoreCmd)
}

func secureRestoreRunE(cmd *cobra.Command, args []string) error {
	bundlePath := args[0]
	sigPath := bundlePath + ".sig"
	_, sigErr := os.Lstat(sigPath)
	hasSignature := sigErr == nil
	if sigErr != nil && !os.IsNotExist(sigErr) {
		return fmt.Errorf("inspect signature: %w", sigErr)
	}
	if restoreVerify && !hasSignature {
		return fmt.Errorf("--verify requires %s", sigPath)
	}
	opts := acf.RestoreOptions{UnsignedOK: restoreUnsignedOK}
	if hasSignature {
		if restorePubKeyPath == "" || restoreExpectedKeyID == "" {
			return fmt.Errorf("signed bundles require --pubkey PATH and --key-id SHA256")
		}
		rawID, err := hex.DecodeString(restoreExpectedKeyID)
		if err != nil || len(rawID) != 32 {
			return fmt.Errorf("--key-id must be exactly 64 hexadecimal characters")
		}
		copy(opts.ExpectedKeyID[:], rawID)
		pub, id, err := acf.LoadTrustedPublicKey(restorePubKeyPath, opts.ExpectedKeyID, acf.DefaultBundleLimits())
		if err != nil {
			return err
		}
		opts.TrustedPubKey, opts.ExpectedKeyID = pub, id
	} else if !restoreUnsignedOK {
		return fmt.Errorf("bundle is unsigned: pass --unsigned-ok to acknowledge")
	}
	if restoreDecrypt {
		if (restoreIdentityPath == "") == (restorePassphraseEnvVar == "") {
			return fmt.Errorf("--decrypt requires exactly one of --identity or --passphrase-from-env")
		}
		if restoreIdentityPath != "" {
			ids, err := acf.LoadAgeIdentitiesBounded(restoreIdentityPath, acf.DefaultBundleLimits())
			if err != nil {
				return err
			}
			opts.DecryptIdentities = ids
		} else {
			passphrase := os.Getenv(restorePassphraseEnvVar)
			if passphrase == "" {
				return fmt.Errorf("--passphrase-from-env %s is empty", restorePassphraseEnvVar)
			}
			id, err := age.NewScryptIdentity(passphrase)
			if err != nil {
				return err
			}
			opts.DecryptIdentities = []age.Identity{id}
		}
	}
	storeRoot := restoreStoreRoot
	if storeRoot == "" {
		if restorePeek {
			storeRoot = filepath.Join(filepath.Dir(bundlePath), ".aplexica-peek-target-unused")
		} else if home, err := os.UserHomeDir(); err == nil {
			storeRoot = filepath.Join(home, ".aplexica", "store")
		}
	}
	store := &acf.Store{Root: storeRoot}
	target := acf.BundleTarget{Store: store, SecretsRoot: restoreSecretsRoot}
	if !hasSignature {
		sigPath = ""
	}
	b, err := acf.OpenValidatedBundleFile(bundlePath, sigPath, target, opts)
	if err != nil {
		return err
	}
	defer b.Close()
	if restorePeek {
		if restoreJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(b.Meta)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Bundle metadata:")
		fmt.Fprintf(out, "  bundleVersion:   %s\n", b.Meta.BundleVersion)
		fmt.Fprintf(out, "  createdAt:       %s\n", b.Meta.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
		fmt.Fprintf(out, "  aplexicaVersion: %s\n", b.Meta.AplexicaVersion)
		if b.Meta.Hostname != "" {
			fmt.Fprintf(out, "  hostname:        %s\n", b.Meta.Hostname)
		}
		if b.Meta.TotalBytes > 0 {
			fmt.Fprintf(out, "  totalBytes:      %d\n", b.Meta.TotalBytes)
		}
		fmt.Fprintf(out, "  includesSecrets: %t\n", b.Meta.IncludesSecrets)
		return nil
	}
	if restoreDryRun {
		res, err := b.DryRun()
		if err != nil {
			return err
		}
		if restoreJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				DryRun          bool                         `json:"dryRun"`
				TotalAdds       int                          `json:"totalAdds"`
				TotalCollisions int                          `json:"totalCollisions"`
				ByKind          map[acf.Kind]*acf.KindDryRun `json:"byKind"`
			}{true, res.TotalAdds(), res.TotalCollisions(), res.ByKind})
		}
		out := cmd.OutOrStdout()
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			if kd := res.ByKind[k]; kd != nil {
				fmt.Fprintf(out, "%s: add=%d collision=%d\n", k, kd.Adds, len(kd.CollisionIDs))
				for _, id := range kd.CollisionIDs {
					fmt.Fprintf(out, "  collision: %s\n", id)
				}
			}
		}
		fmt.Fprintf(out, "dry-run totals: add=%d collision=%d (nothing written)\n", res.TotalAdds(), res.TotalCollisions())
		if res.TotalCollisions() > 0 {
			fmt.Fprintln(out, "note: a real restore would ABORT on the first collision.")
		}
		return nil
	}
	if err := b.VerifySemantic(); err != nil {
		return err
	}
	if err := b.Commit(); err != nil {
		return err
	}
	if restoreJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"bundlePath": bundlePath, "storeRoot": restoreStoreRoot, "restored": true})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "restored bundle %s into %s\n", bundlePath, restoreStoreRoot)
	return nil
}
