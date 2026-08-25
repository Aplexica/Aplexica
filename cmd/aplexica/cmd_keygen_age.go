package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/aplexica/aplexica/internal/acf"
)

var (
	keygenAgeOutPriv string
	keygenAgeOutPub  string
)

var keygenAgeCmd = &cobra.Command{
	Use:   "keygen-age",
	Short: "Generate an X25519 age keypair for bundle encryption",
	Long: `Generates an age X25519 identity. The private key (AGE-SECRET-KEY-...) is
written to --out-priv with 0o600 perms. The public recipient (age1...) is
written to --out-pub with 0o644 perms. Share the recipient with anyone who
needs to encrypt bundles TO you; never share the identity.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := acf.GenerateAgeIdentity()
		if err != nil {
			return err
		}
		// Ensure parent dirs exist for the default ~/.aplexica/keys/ location.
		if dir := filepath.Dir(keygenAgeOutPriv); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}
		if dir := filepath.Dir(keygenAgeOutPub); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}
		if err := acf.SaveAgeIdentity(keygenAgeOutPriv, id); err != nil {
			return err
		}
		if err := acf.SaveAgeRecipient(keygenAgeOutPub, id); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote identity %s (0o600)\nwrote recipient %s (0o644)\nrecipient: %s\n",
			keygenAgeOutPriv, keygenAgeOutPub, id.Recipient().String())
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	keygenAgeCmd.Flags().StringVar(&keygenAgeOutPriv, "out-priv",
		filepath.Join(home, ".aplexica", "keys", "age.key"), "Age private key output path (0o600)")
	keygenAgeCmd.Flags().StringVar(&keygenAgeOutPub, "out-pub",
		filepath.Join(home, ".aplexica", "keys", "age.pub"), "Age recipient output path (0o644)")
	rootCmd.AddCommand(keygenAgeCmd)
}
