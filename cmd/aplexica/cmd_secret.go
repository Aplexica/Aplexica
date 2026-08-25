package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Per FR-02.18 the CLI surface is:
//
//   aplexica secret list
//   aplexica secret set <name>
//   aplexica secret get <name>
//   aplexica secret delete <name>
//   aplexica secret rotate <name>
//   aplexica secret sync-enable <name>
//   aplexica secret sync-disable <name>
//
// v0.64.0 shipped these against the per-artifact storage layout (taking
// `<artifact-id> <name>` as two positionals). v0.72.0 adds the BRD-02
// §4.4.1 global-name layout alongside, and switches the CLI primary
// form to single-arg `<name>`. The 2-arg form is retained as a
// power-user fallback for the per-artifact MCP storage (which is still
// where adapters externalize during inbound translation).
//
//   1 arg  →  global-name (BRD primary; touches ~/.aplexica/secrets/<name>
//             and ~/.aplexica/secrets/.meta/<name>.json)
//   2 args →  per-artifact (legacy v0.64.0 form; touches
//             ~/.aplexica/secrets/<artifact-id>/<name>)

var (
	secretRoot     string
	secretValue    string
	secretFromFile string
	secretReveal   bool
	secretJSON     bool
	secretFilter   string
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secret values referenced by tool artifacts",
	Long: `Manage the local secrets store under ~/.aplexica/secrets/.

Secret VALUES live on this device only and never enter the canonical
event chain.

Two layouts coexist:

  Global-name:
    ~/.aplexica/secrets/<name>
    ~/.aplexica/secrets/.meta/<name>.json    # createdAt, updatedAt,
                                              # usedByTools, syncEnabled

  Per-artifact (v0.64.0 legacy, used internally by the MCP adapter
  pipeline to keep tool-scoped values isolated):
    ~/.aplexica/secrets/<artifact-id>/<name>

Subcommands accept either form: a single positional names a global
secret; two positionals select the per-artifact path.

  list                          flat list of every secret on disk
  set <name>                    write a global-scope secret
  set <artifact-id> <name>      write a per-artifact-scoped secret
  get <name> [--reveal]
  delete <name>
  rotate <name>
  sync-enable <name>            opt this secret in to syncing
  sync-disable <name>           opt back out (the default)`,
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every secret on disk (global + per-artifact); never prints values",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ss := &secrets.Store{Root: secretRoot}

		// Per-artifact pairs.
		pairs, err := ss.ListAll()
		if err != nil {
			return err
		}

		// Global-name secrets + their sidecar sync flag.
		globals, err := ss.ListGlobal()
		if err != nil {
			return err
		}

		// Filter by artifact-id if requested (only affects per-artifact).
		if secretFilter != "" {
			filtered := pairs[:0]
			for _, p := range pairs {
				if p.ArtifactID == secretFilter {
					filtered = append(filtered, p)
				}
			}
			pairs = filtered
		}
		sort.Slice(pairs, func(i, j int) bool {
			if pairs[i].ArtifactID != pairs[j].ArtifactID {
				return pairs[i].ArtifactID < pairs[j].ArtifactID
			}
			return pairs[i].Key < pairs[j].Key
		})

		out := cmd.OutOrStdout()
		if secretJSON {
			payload := struct {
				Global      []secrets.Meta `json:"global"`
				PerArtifact []secrets.Pair `json:"perArtifact"`
			}{PerArtifact: pairs}
			for _, n := range globals {
				meta, err := ss.ReadMeta(n)
				if err == nil {
					payload.Global = append(payload.Global, meta)
				} else {
					payload.Global = append(payload.Global, secrets.Meta{Name: n})
				}
			}
			b, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(out, string(b))
			return nil
		}

		if len(globals) == 0 && len(pairs) == 0 {
			fmt.Fprintln(out, "(no secrets)")
			return nil
		}

		if len(globals) > 0 {
			fmt.Fprintln(out, "Global-name secrets:")
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "  NAME\tSYNC\tUSED BY (#)")
			for _, n := range globals {
				meta, err := ss.ReadMeta(n)
				flag := "off"
				users := 0
				if err == nil {
					if meta.SyncEnabled {
						flag = "on"
					}
					users = len(meta.UsedByTools)
				}
				fmt.Fprintf(w, "  %s\t%s\t%d\n", n, flag, users)
			}
			_ = w.Flush()
			fmt.Fprintln(out)
		}

		if len(pairs) > 0 {
			fmt.Fprintln(out, "Per-artifact secrets:")
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "  ARTIFACT-ID\tNAME")
			for _, p := range pairs {
				fmt.Fprintf(w, "  %s\t%s\n", p.ArtifactID, p.Key)
			}
			_ = w.Flush()
		}
		return nil
	},
}

var secretSetCmd = &cobra.Command{
	Use:   "set <name> | <artifact-id> <name>",
	Short: "Set a secret value (stdin by default; --value or --from-file)",
	Long: `Write a secret value.

With one positional, writes a global-name secret to
~/.aplexica/secrets/<name> and updates the sidecar at
~/.aplexica/secrets/.meta/<name>.json.

With two positionals, writes to the per-artifact location
~/.aplexica/secrets/<artifact-id>/<name>.

Value source precedence: --value > --from-file > stdin. Stdin is
TTY-hidden when interactive; piped input is consumed verbatim with
one trailing newline trimmed.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		value, err := resolveSecretValue(cmd)
		if err != nil {
			return err
		}
		ss := &secrets.Store{Root: secretRoot}
		if len(args) == 1 {
			if err := ss.PutGlobal(args[0], value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote global %s (%d bytes)\n", args[0], len(value))
			return nil
		}
		if err := ss.Put(args[0], args[1], value); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s/%s (%d bytes)\n", args[0], args[1], len(value))
		return nil
	},
}

var secretGetCmd = &cobra.Command{
	Use:   "get <name> | <artifact-id> <name>",
	Short: "Read a secret value (--reveal to print)",
	Long: "Read a secret. By default prints only \"(present; N bytes)\" so the\n" +
		"value doesn't leak into terminal scrollback. Pass --reveal to print\n" +
		"the raw value.\n\n" +
		"With one positional, reads from the global-name layout; with two,\n" +
		"from the per-artifact layout.",
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ss := &secrets.Store{Root: secretRoot}
		var (
			v   string
			err error
		)
		if len(args) == 1 {
			v, err = ss.GetGlobal(args[0])
		} else {
			v, err = ss.Get(args[0], args[1])
		}
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if secretReveal {
			fmt.Fprint(out, v)
			if !strings.HasSuffix(v, "\n") {
				fmt.Fprintln(out)
			}
			return nil
		}
		fmt.Fprintf(out, "(present; %d bytes — pass --reveal to print)\n", len(v))
		return nil
	},
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete <name> | <artifact-id> <name>",
	Short: "Remove one secret",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ss := &secrets.Store{Root: secretRoot}
		if len(args) == 1 {
			if err := ss.DeleteGlobal(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted global %s\n", args[0])
			return nil
		}
		if err := ss.Delete(args[0], args[1]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deleted %s/%s\n", args[0], args[1])
		return nil
	},
}

var secretRotateCmd = &cobra.Command{
	Use:   "rotate <name> | <artifact-id> <name>",
	Short: "Replace the value of an existing secret (refuses to create new)",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		value, err := resolveSecretValue(cmd)
		if err != nil {
			return err
		}
		ss := &secrets.Store{Root: secretRoot}
		if len(args) == 1 {
			if err := ss.RotateGlobal(args[0], value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rotated global %s (%d bytes)\n", args[0], len(value))
			return nil
		}
		// Per-artifact rotation — exists-check then Put.
		if _, err := ss.Get(args[0], args[1]); err != nil {
			return fmt.Errorf("rotate: secret %s/%s does not exist (use 'set' to create): %w",
				args[0], args[1], err)
		}
		if err := ss.Put(args[0], args[1], value); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "rotated %s/%s (%d bytes)\n", args[0], args[1], len(value))
		return nil
	},
}

var secretSyncEnableCmd = &cobra.Command{
	Use:   "sync-enable <name>",
	Short: "Mark a global-name secret as syncEnabled in its sidecar",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ss := &secrets.Store{Root: secretRoot}
		if err := ss.SetSyncEnabled(args[0], true); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "global %s: syncEnabled=true\n", args[0])
		return nil
	},
}

var secretSyncDisableCmd = &cobra.Command{
	Use:   "sync-disable <name>",
	Short: "Clear the syncEnabled flag on a global-name secret",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ss := &secrets.Store{Root: secretRoot}
		if err := ss.SetSyncEnabled(args[0], false); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "global %s: syncEnabled=false\n", args[0])
		return nil
	},
}

// resolveSecretValue picks the source of the secret value:
//  1. --value (literal; visible in process listing; suitable for CI
//     runners with redacted env)
//  2. --from-file <path> (verbatim file contents)
//  3. stdin (TTY-hidden if interactive; piped input consumed
//     verbatim minus a single trailing newline)
func resolveSecretValue(cmd *cobra.Command) (string, error) {
	if secretValue != "" {
		return secretValue, nil
	}
	if secretFromFile != "" {
		b, err := os.ReadFile(secretFromFile)
		if err != nil {
			return "", fmt.Errorf("--from-file: %w", err)
		}
		return string(b), nil
	}
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(cmd.ErrOrStderr(), "Secret value (input hidden, end with Enter): ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", fmt.Errorf("read tty: %w", err)
		}
		return string(b), nil
	}
	b, err := io.ReadAll(bufio.NewReader(in))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	s := string(b)
	if strings.HasSuffix(s, "\n") {
		s = strings.TrimSuffix(s, "\n")
	}
	if s == "" {
		return "", fmt.Errorf("read stdin: empty value")
	}
	return s, nil
}

func init() {
	home, _ := os.UserHomeDir()
	defaultSecretsRoot := filepath.Join(home, ".aplexica", "secrets")

	secretCmd.PersistentFlags().StringVar(&secretRoot, "secrets-root",
		defaultSecretsRoot,
		"Secrets store root directory")

	secretListCmd.Flags().BoolVar(&secretJSON, "json", false,
		"emit machine-readable JSON")
	secretListCmd.Flags().StringVar(&secretFilter, "artifact", "",
		"filter per-artifact entries to a single artifact-id")

	for _, c := range []*cobra.Command{secretSetCmd, secretRotateCmd} {
		c.Flags().StringVar(&secretValue, "value", "",
			"literal value (alternative to stdin; visible in process listing)")
		c.Flags().StringVar(&secretFromFile, "from-file", "",
			"load the value verbatim from a file")
	}

	secretGetCmd.Flags().BoolVar(&secretReveal, "reveal", false,
		"print the raw secret value (default prints '(present)' only)")

	secretCmd.AddCommand(
		secretListCmd, secretSetCmd, secretGetCmd,
		secretDeleteCmd, secretRotateCmd,
		secretSyncEnableCmd, secretSyncDisableCmd,
	)
	rootCmd.AddCommand(secretCmd)
}
