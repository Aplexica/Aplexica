package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "aplexica",
	Short:         "Cross-agent state portability for AI coding agents",
	Long:          "aplexica imports and exports agent state via the Aplexica Canonical Format (ACF).",
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return ensurePrivateAplexicaBase()
	},
}

var waitBeforeExit bool

func init() {
	rootCmd.PersistentFlags().BoolVar(&waitBeforeExit, "wait-before-exit", false, "wait for Enter before closing")
	if err := rootCmd.PersistentFlags().MarkHidden("wait-before-exit"); err != nil {
		panic(err)
	}
}

func main() {
	err := rootCmd.Execute()
	if err != nil {
		silent := false
		var value interface{ Silent() bool }
		if errors.As(err, &value) {
			silent = value.Silent()
		}
		if !silent {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
	if waitBeforeExit {
		waitForEnter(os.Stdin, os.Stderr)
	}
	if err != nil {
		code := 1
		var value interface{ ExitCode() int }
		if errors.As(err, &value) {
			code = value.ExitCode()
		}
		os.Exit(code)
	}
}

func waitForEnter(in io.Reader, out io.Writer) {
	_, _ = fmt.Fprint(out, "Press Enter to close…")
	_, _ = bufio.NewReader(in).ReadString('\n')
}

func ensurePrivateAplexicaBase() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	if err := privatefs.EnsureDir(filepath.Join(home, ".aplexica"), privatefs.DirPolicy{
		Access:        privatefs.AccessPrivate,
		RepairOwned:   true,
		AllowExisting: true,
	}); err != nil {
		return fmt.Errorf("secure Aplexica data directory: %w", err)
	}
	return nil
}
