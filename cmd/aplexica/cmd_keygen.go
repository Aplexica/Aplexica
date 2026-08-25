package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	keygenOutPriv string
	keygenOutPub  string
)

var keygenCmd = &cobra.Command{
	Use:   "keygen",
	Short: "Generate an Ed25519 keypair for bundle signing",
	Long: `Generate a fresh Ed25519 keypair for signing backup bundles.

The private key is written with 0o600 permissions; the public key with 0o644.
Distribute the public key to anyone who needs to verify your bundles; keep the
private key secret. See 'aplexica backup --sign' and 'aplexica restore --verify'.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Ensure parent dirs exist (default location is ~/.aplexica/keys/).
		for _, p := range []string{keygenOutPriv, keygenOutPub} {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return fmt.Errorf("mkdir key dir: %w", err)
			}
		}
		if err := acf.GenerateKeyPairFiles(keygenOutPriv, keygenOutPub); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote priv: %s (0o600)\nwrote pub:  %s (0o644)\n", keygenOutPriv, keygenOutPub)
		fmt.Fprintf(cmd.ErrOrStderr(), "keep the priv key secret; distribute the pub key to verifiers.\n")
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	defaultPriv := filepath.Join(home, ".aplexica", "keys", "priv.key")
	defaultPub := filepath.Join(home, ".aplexica", "keys", "pub.key")
	keygenCmd.Flags().StringVar(&keygenOutPriv, "out-priv", defaultPriv, "Private key output path (0o600)")
	keygenCmd.Flags().StringVar(&keygenOutPub, "out-pub", defaultPub, "Public key output path (0o644)")
	rootCmd.AddCommand(keygenCmd)
}
