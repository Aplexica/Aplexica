package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/syncstate"
	"github.com/spf13/cobra"
)

// Per FR-02.18 the CLI MUST provide:
//
//   aplexica tool list
//   aplexica tool show <id>
//   aplexica tool sync-secrets <id> --enable|--disable
//   aplexica tool capabilities <id>
//
// The syncSecrets flag is persisted in <state-dir>/tool-sync-secrets.json
// (see internal/syncstate) rather than as an artifact field, because the
// v1 schema doesn't yet model that field. A future schema bump can fold
// the sidecar into the artifact metadata.

var (
	toolStoreRoot   string
	toolStateDir    string
	toolSyncEnable  bool
	toolSyncDisable bool
)

var toolCmd = &cobra.Command{
	Use:   "tool",
	Short: "Inspect tool artifacts and toggle per-tool secret syncing",
	Long: `Manage tool artifacts (MCP server configs, subagents, hooks, slash
commands, plugins). Tool artifacts carry redacted native config bodies
with ${secret:<name>} placeholders; the raw values live in the local
secrets store (see 'aplexica secret').

Subcommands:
  list                          tabular index of every tool artifact
  show <id>                     metadata + extracted secret names
  sync-secrets <id> --enable    opt this tool's secrets into syncing
  sync-secrets <id> --disable   opt back out
  capabilities <id>             report adapter / format / secret count`,
}

var toolListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every tool artifact in the canonical store",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: toolStoreRoot}
		arts, err := store.ListArtifacts(acf.KindTool)
		if err != nil {
			return err
		}
		ss := &syncstate.Store{Path: syncstate.DefaultPath(toolStateDir)}

		out := cmd.OutOrStdout()
		if len(arts) == 0 {
			fmt.Fprintln(out, "(no tool artifacts)")
			return nil
		}
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSCOPE\tSECRETS\tSYNC\tSOURCE")
		for _, art := range arts {
			source := "?"
			events, err := store.ReadEvents(art.Kind, art.ArtifactID)
			if err == nil && len(events) > 0 {
				source = events[0].Provenance.SourceAgent
			}
			secrets := extractSecretNames(store, art)
			sync, _ := ss.Get(art.ArtifactID)
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
				art.ArtifactID, art.Name, art.Scope, len(secrets),
				syncFlag(sync), source)
		}
		return w.Flush()
	},
}

var toolShowCmd = &cobra.Command{
	Use:   "show <artifact-id>",
	Short: "Show tool metadata, extracted secret refs, and a payload preview",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: toolStoreRoot}
		art, err := store.ReadArtifact(acf.KindTool, args[0])
		if err != nil {
			return err
		}
		ss := &syncstate.Store{Path: syncstate.DefaultPath(toolStateDir)}
		sync, _ := ss.Get(art.ArtifactID)

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Tool artifact %s\n", art.ArtifactID)
		fmt.Fprintf(out, "  name:        %s\n", art.Name)
		fmt.Fprintf(out, "  scope:       %s\n", art.Scope)
		fmt.Fprintf(out, "  created:     %s\n", art.CreatedAt.Format("2006-01-02 15:04:05Z07:00"))
		fmt.Fprintf(out, "  updated:     %s\n", art.UpdatedAt.Format("2006-01-02 15:04:05Z07:00"))
		fmt.Fprintf(out, "  tombstoned:  %v\n", art.Tombstoned)
		fmt.Fprintf(out, "  syncSecrets: %v (default: false)\n", sync)

		events, err := store.ReadEvents(art.Kind, art.ArtifactID)
		if err != nil {
			return err
		}
		if len(events) > 0 {
			fmt.Fprintf(out, "  events:      %d (head=%s)\n", len(events), shortHash(art.HeadEventHash))
			fmt.Fprintf(out, "  sourceAgent: %s\n", events[0].Provenance.SourceAgent)
			fmt.Fprintf(out, "  format:      %s\n", currentFormat(events))
		}

		secrets := extractSecretNames(store, art)
		fmt.Fprintf(out, "  secretsRefs: %d\n", len(secrets))
		for _, name := range secrets {
			fmt.Fprintf(out, "    - %s\n", name)
		}
		return nil
	},
}

var toolSyncSecretsCmd = &cobra.Command{
	Use:   "sync-secrets <artifact-id> --enable|--disable",
	Short: "Toggle the per-tool syncSecrets opt-in",
	Long: `Toggle whether secrets referenced by this tool sync alongside
the config. The default for every newly-imported tool is --disable:
configs sync everywhere, secrets stay local. Opting in for a tool
implies opting in for every secret in that tool's secretsRefs.

The flag is persisted in <state-dir>/tool-sync-secrets.json. The daemon
(and any future relay) reads this sidecar before forwarding tool
events; v0.68.0 establishes the surface without wiring the daemon's
inbound/outbound paths to consult it yet.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if toolSyncEnable == toolSyncDisable {
			return fmt.Errorf("sync-secrets: pass exactly one of --enable or --disable")
		}
		// Confirm the artifact exists (so a typo'd ID errors clearly).
		store := &acf.Store{Root: toolStoreRoot}
		if _, err := store.ReadArtifact(acf.KindTool, args[0]); err != nil {
			return fmt.Errorf("sync-secrets: %w", err)
		}
		ss := &syncstate.Store{Path: syncstate.DefaultPath(toolStateDir)}
		if err := ss.Set(args[0], toolSyncEnable); err != nil {
			return err
		}
		state := "disabled"
		if toolSyncEnable {
			state = "enabled"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "tool %s: syncSecrets %s\n", args[0], state)
		return nil
	},
}

var toolCapabilitiesCmd = &cobra.Command{
	Use:   "capabilities <artifact-id>",
	Short: "Report adapter / format / cross-adapter support for this tool",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: toolStoreRoot}
		art, err := store.ReadArtifact(acf.KindTool, args[0])
		if err != nil {
			return err
		}
		events, err := store.ReadEvents(art.Kind, art.ArtifactID)
		if err != nil {
			return err
		}
		source := "(unknown)"
		format := "(unknown)"
		if len(events) > 0 {
			source = events[0].Provenance.SourceAgent
			format = currentFormat(events)
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Tool capabilities for %s\n", art.ArtifactID)
		fmt.Fprintf(out, "  source adapter: %s\n", source)
		fmt.Fprintf(out, "  payload format: %s\n", format)

		// Cross-adapter support. Per BRD-02 §4.4, MCP-server configs are
		// universally supported across all five V1 agents. Other tool
		// kinds vary; we don't yet have a per-kind tag in the artifact
		// so we report the universal-MCP fact as the v0.68.0 baseline.
		fmt.Fprintln(out, "  cross-adapter support:")
		for _, a := range []string{"claude-code", "codex", "kilo", "hermes", "openclaw"} {
			support := "native (MCP)"
			if format != "" && !isMCPFormat(format) {
				support = "best-effort (format not universally supported)"
			}
			fmt.Fprintf(out, "    %-13s %s\n", a, support)
		}

		secrets := extractSecretNames(store, art)
		fmt.Fprintf(out, "  secretsRefs:   %d\n", len(secrets))
		return nil
	},
}

// extractSecretNames walks the tool artifact's current event payload and
// extracts every ${secret:<name>} placeholder. Returns a sorted, deduped
// list — readable in `tool show` output and stable for tests.
func extractSecretNames(store *acf.Store, art acf.Artifact) []string {
	events, err := store.ReadEvents(art.Kind, art.ArtifactID)
	if err != nil || len(events) == 0 {
		return nil
	}
	body := currentPayloadBody(events)
	if body == "" {
		return nil
	}
	matches := secretPlaceholderPattern.FindAllStringSubmatch(body, -1)
	seen := map[string]struct{}{}
	for _, m := range matches {
		seen[m[1]] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// secretPlaceholderPattern matches ${secret:<name>} occurrences in the
// payload body. Same syntax as internal/mcp/secrets.go's secretRef.
var secretPlaceholderPattern = regexp.MustCompile(`\$\{secret:([^}]+)\}`)

// currentPayloadBody returns the content of the most-recent event's
// ToolPayload. Falls back to "" on any decode error so the caller's
// regex scan simply finds nothing.
func currentPayloadBody(events []acf.Event) string {
	if len(events) == 0 {
		return ""
	}
	p, err := acf.DecodeToolPayload(events[len(events)-1])
	if err != nil {
		return ""
	}
	return p.Content
}

// currentFormat returns the most-recent event's payload format string,
// or "(unknown)" on decode failure.
func currentFormat(events []acf.Event) string {
	if len(events) == 0 {
		return "(unknown)"
	}
	p, err := acf.DecodeToolPayload(events[len(events)-1])
	if err != nil || p.Format == "" {
		return "(unknown)"
	}
	return p.Format
}

func isMCPFormat(format string) bool {
	if format == "acf.mcp.v1" {
		return true
	}
	// Per-adapter native variants documented across the codebase.
	return strings.HasSuffix(format, ".mcp.json")
}

func syncFlag(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func shortHash(h string) string {
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

func init() {
	home, _ := os.UserHomeDir()
	toolCmd.PersistentFlags().StringVar(&toolStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"Canonical store root directory")
	toolCmd.PersistentFlags().StringVar(&toolStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"State directory (holds tool-sync-secrets.json)")

	toolSyncSecretsCmd.Flags().BoolVar(&toolSyncEnable, "enable", false,
		"opt this tool's secrets in to syncing")
	toolSyncSecretsCmd.Flags().BoolVar(&toolSyncDisable, "disable", false,
		"opt this tool's secrets out of syncing (the default)")
	// Mutual exclusion is enforced inside RunE (`toolSyncEnable ==
	// toolSyncDisable` → error). We don't use MarkFlagsMutuallyExclusive
	// because cobra retains "flag was set" state across calls to
	// rootCmd.Execute() in the test harness; the runtime check survives
	// that just fine.

	toolCmd.AddCommand(toolListCmd, toolShowCmd, toolSyncSecretsCmd, toolCapabilitiesCmd)
	rootCmd.AddCommand(toolCmd)
}
