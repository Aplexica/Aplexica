package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapterstate"
	"github.com/spf13/cobra"
)

// Per FR-02.6 the CLI MUST provide:
//
//   aplexica adapters check <name>
//
// which "runs the conformance suite against a local installation."
// v0.84.0 implements an in-process subset of the conformance harness
// that doesn't require Go testing.T — round-trip, idempotency, and
// capability declaration. Watch correctness / cross-conversion /
// performance / recursion guard are full-blown test-package
// concerns; they remain reachable via `go test ./internal/adapter/...
// -run TestConformance` from a source checkout.

var (
	adaptersCheckSecretsRoot string
	adaptersStateDir         string
)

var adaptersCmd = &cobra.Command{
	Use:   "adapters",
	Short: "Inspect and check the installed adapters",
}

var adaptersListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every installed adapter with its capability declaration",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		ass := &adapterstate.Store{Path: adapterstate.DefaultPath(adaptersStateDir)}
		disabled := ass.DisabledSet()

		fmt.Fprintln(out, "Installed adapters:")
		for _, name := range []string{"claude-code", "codex", "hermes", "openclaw", "kilo"} {
			ad, err := buildAdapter(name, adaptersCheckSecretsRoot)
			if err != nil {
				fmt.Fprintf(out, "  %s: build failed: %v\n", name, err)
				continue
			}
			caps := ad.Capabilities()
			discovery, discoveryErr := ad.Discover()
			state := "enabled"
			if _, off := disabled[name]; off {
				state = "DISABLED"
			}
			fmt.Fprintf(out, "  %s [%s]\n", caps.Name, state)
			fmt.Fprintf(out, "    supported surfaces: %s\n", joinSurfaces(caps.Surfaces))
			if discoveryErr != nil {
				fmt.Fprintf(out, "    detected surfaces:  probe failed: %v\n", discoveryErr)
			} else {
				fmt.Fprintf(out, "    detected surfaces:  %s\n", joinDetectedSurfaces(discovery.ActiveSurfaces))
			}
			fmt.Fprintf(out, "    artifacts:   memory=%v skill=%v tool=%v conversation=%v\n",
				caps.Artifacts.Memory, caps.Artifacts.Skill,
				caps.Artifacts.Tool, caps.Artifacts.Conversation)
			fmt.Fprintf(out, "    tool kinds:  %s\n", joinToolKinds(caps.Tools))
			fmt.Fprintf(out, "    basenames:   %s\n", strings.Join(caps.NativeBasenames, ", "))
		}
		return nil
	},
}

// validAdapterNames is the V1 set; adapters enable/disable rejects
// unknown names so a typo doesn't silently leave the user thinking
// they disabled something.
var validAdapterNames = map[string]struct{}{
	"claude-code": {},
	"codex":       {},
	"hermes":      {},
	"openclaw":    {},
	"kilo":        {},
}

var adaptersEnableCmd = &cobra.Command{
	Use:   "enable <name>",
	Short: "Enable a previously-disabled adapter",
	Long: `Removes the named adapter from <state-dir>/adapters.json's
disabled list. Idempotent — enabling an already-enabled adapter is a
no-op. Takes effect on the next daemon start; running daemons must
be restarted via ` + "`aplexica daemon restart`" + ` to pick up the change.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, ok := validAdapterNames[args[0]]; !ok {
			return fmt.Errorf("unknown adapter %q (expected one of: claude-code, codex, hermes, openclaw, kilo)", args[0])
		}
		s := &adapterstate.Store{Path: adapterstate.DefaultPath(adaptersStateDir)}
		if err := s.Enable(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "enabled %s\n", args[0])
		return nil
	},
}

var adaptersDisableCmd = &cobra.Command{
	Use:   "disable <name>",
	Short: "Disable an adapter wholesale",
	Long: `Adds the named adapter to <state-dir>/adapters.json's disabled
list. Disabled adapters are NOT loaded by the daemon — they don't
watch, don't Import, don't Export. Different from
` + "`aplexica sync pause --agent <name>`" + ` which is timed and
outbound-only. Idempotent. Takes effect on the next daemon start.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, ok := validAdapterNames[args[0]]; !ok {
			return fmt.Errorf("unknown adapter %q (expected one of: claude-code, codex, hermes, openclaw, kilo)", args[0])
		}
		s := &adapterstate.Store{Path: adapterstate.DefaultPath(adaptersStateDir)}
		if err := s.Disable(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "disabled %s (takes effect on next daemon start)\n", args[0])
		return nil
	},
}

var adaptersCheckCmd = &cobra.Command{
	Use:   "check <name>",
	Short: "Run local checks against an adapter installation",
	Long: `Runs the adapter checks that can execute in-process against the
named adapter:

  - Capability declaration: every NativeBasename the adapter
    advertises must be Importable.
  - Round-trip: for each advertised basename whose minimal body is
    the canonical native form (the Markdown memory/skill files), a
    native->ACF->native round trip must reproduce it byte-for-byte.
    Config formats (MCP JSON/YAML) are legitimately reformatted on
    export, so they are checked for idempotency only, not byte
    equality.
  - Idempotency: for every advertised basename, a second Export
    produces identical bytes.

Reports pass/fail per check; exit code is non-zero on any failure.

Use the unrestricted-runner test suite for the heavier checks
(watch correctness, cross-conversion, recursion guard, performance):
  go test ./internal/adapter/<name> -run TestConformance`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ad, err := buildAdapter(name, adaptersCheckSecretsRoot)
		if err != nil {
			return err
		}
		caps := ad.Capabilities()

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "aplexica adapters check %s\n", caps.Name)
		fmt.Fprintln(out)

		failures := 0
		ctx := context.Background()

		// Per-basename round-trip + idempotency.
		basenames := append([]string{}, caps.NativeBasenames...)
		sort.Strings(basenames)
		for _, bn := range basenames {
			if minimalBodyForBasename(bn) == "" {
				fmt.Fprintf(out, "  [skip] %s — no shared minimal body\n", bn)
				continue
			}
			ok, msg := checkAdapterBasename(ctx, ad, bn)
			if !ok {
				fmt.Fprintf(out, "  [FAIL] %s — %s\n", bn, msg)
				failures++
				continue
			}
			fmt.Fprintf(out, "  [ok]   %s\n", bn)
		}

		fmt.Fprintln(out)
		if failures > 0 {
			return fmt.Errorf("%s: %d conformance check(s) failed", caps.Name, failures)
		}
		fmt.Fprintf(out, "ok: %s passes the in-process conformance subset.\n", caps.Name)
		return nil
	},
}

// minimalBodyForBasename mirrors the conformance package helper so
// `adapters check` can run without depending on test-only code. Keep
// in sync with internal/conformance's version.
func minimalBodyForBasename(bn string) string {
	switch bn {
	case "AGENTS.md", "AGENT.md", "CLAUDE.md", "MEMORY.md", "USER.md", "DREAMS.md":
		return "# memory\n"
	case "SKILL.md":
		return "---\nname: probe\ndescription: capability probe.\n---\n\n# probe\n"
	case ".mcp.json", "openclaw.json", "openclaw.jsonc", "openclaw.json5",
		"kilo.jsonc":
		return `{"mcpServers":{}}`
	case "config.yaml", "hermes.yaml", "hermes.yml":
		return "mcpServers: {}\n"
	}
	return ""
}

// bodyIsCanonical reports whether minimalBodyForBasename(bn) is the
// adapter's CANONICAL native form — i.e. a native->ACF->native round
// trip is expected to reproduce it byte-for-byte. This holds for the
// Markdown memory/skill basenames (plain text), but NOT for the MCP
// config formats: an adapter legitimately reformats the compact probe
// JSON/YAML on export (e.g. hermes emits `mcp_servers:` YAML, openclaw a
// pretty-printed `mcp.servers` tree), so the shared probe body is not
// byte-canonical for those. We therefore enforce byte-equal round-trip
// only where it is meaningful and skip it (idempotency only) elsewhere.
func bodyIsCanonical(bn string) bool {
	switch bn {
	case "AGENTS.md", "AGENT.md", "CLAUDE.md", "MEMORY.md", "USER.md", "DREAMS.md", "SKILL.md":
		return true
	}
	return false
}

// checkAdapterBasename runs the in-process conformance subset for one
// native basename against ad: import a minimal-valid body, export it, and
// (for canonical basenames) assert the export reproduces the input
// byte-for-byte — the native->ACF->native round trip the real harness
// checks via fidelityEqual. It also asserts Export is idempotent for
// every basename. Returns (false, reason) on the first failure.
//
// The round-trip assertion is what catches a deterministically-wrong
// exporter (e.g. one that drops SKILL.md frontmatter); idempotency alone
// would pass such an adapter.
func checkAdapterBasename(ctx context.Context, ad adapter.Adapter, bn string) (bool, string) {
	body := minimalBodyForBasename(bn)
	if body == "" {
		return true, ""
	}
	dir, err := os.MkdirTemp("", "aplexica-adapters-check-*")
	if err != nil {
		return false, fmt.Sprintf("tempdir: %v", err)
	}
	defer os.RemoveAll(dir)

	store := &acf.Store{Root: filepath.Join(dir, "store")}
	if err := store.Init(); err != nil {
		return false, fmt.Sprintf("store init: %v", err)
	}
	nativePath := filepath.Join(dir, bn)
	if err := os.WriteFile(nativePath, []byte(body), 0o644); err != nil {
		return false, fmt.Sprintf("write native: %v", err)
	}

	ids, err := ad.Import(ctx, store, nativePath)
	if err != nil {
		return false, fmt.Sprintf("Import: %v", err)
	}
	if len(ids) == 0 {
		return false, "Import returned 0 artifact IDs"
	}
	id := ids[0]

	outPath := filepath.Join(dir, "out-"+bn)
	if err := ad.Export(ctx, store, id, outPath); err != nil {
		return false, fmt.Sprintf("Export: %v", err)
	}
	gotA, err := os.ReadFile(outPath)
	if err != nil {
		return false, fmt.Sprintf("read export: %v", err)
	}

	// Round-trip: for canonical basenames the export must reproduce the
	// input verbatim. This is the check that catches a stable-but-wrong
	// exporter (dropped field, mangled frontmatter).
	if bodyIsCanonical(bn) && string(gotA) != body {
		return false, fmt.Sprintf("round-trip mismatch: imported %q but exported %q", body, string(gotA))
	}

	// Idempotency: a second Export onto the same target must match.
	if err := ad.Export(ctx, store, id, outPath); err != nil {
		return false, fmt.Sprintf("second Export: %v", err)
	}
	gotB, err := os.ReadFile(outPath)
	if err != nil {
		return false, fmt.Sprintf("read second export: %v", err)
	}
	if string(gotA) != string(gotB) {
		return false, "idempotency violation"
	}
	return true, ""
}

func joinToolKinds(tks []adapter.ToolKind) string {
	parts := make([]string, 0, len(tks))
	for _, t := range tks {
		parts = append(parts, string(t))
	}
	if len(parts) == 0 {
		return "(none declared)"
	}
	return strings.Join(parts, ", ")
}

func joinSurfaces(surfaces []adapter.Surface) string {
	parts := make([]string, 0, len(surfaces))
	for _, surface := range surfaces {
		parts = append(parts, string(surface))
	}
	if len(parts) == 0 {
		return "(not declared)"
	}
	return strings.Join(parts, ", ")
}

func joinDetectedSurfaces(surfaces []adapter.Surface) string {
	if len(surfaces) == 0 {
		return "(none detected)"
	}
	return joinSurfaces(surfaces)
}

func init() {
	home, _ := os.UserHomeDir()
	adaptersCmd.PersistentFlags().StringVar(&adaptersCheckSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"Secrets store root directory (used to construct adapters)")
	adaptersCmd.PersistentFlags().StringVar(&adaptersStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"Daemon state directory (holds adapters.json)")
	adaptersCmd.AddCommand(adaptersListCmd, adaptersCheckCmd, adaptersEnableCmd, adaptersDisableCmd)
	rootCmd.AddCommand(adaptersCmd)
}
