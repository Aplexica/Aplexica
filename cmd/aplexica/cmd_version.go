package main

import (
	"fmt"

	"github.com/aplexica/aplexica/internal/version"
	"github.com/spf13/cobra"
)

// versionCmd implements `aplexica version` and `aplexica --version` so the
// exact release version is always printable without poking at internal daemon
// state. Build provenance is carried by signed release evidence rather than
// appended to the user-visible numeric version.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the exact aplexica version",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		fmt.Println(version.String())
		return nil
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	// Also wire --version on the root so `aplexica --version` works the
	// same way as the README + install docs claim. cobra's default
	// Version+SetVersionTemplate path keeps it identical to the
	// subcommand output.
	rootCmd.Version = version.String()
	rootCmd.SetVersionTemplate("{{.Version}}\n")
}
