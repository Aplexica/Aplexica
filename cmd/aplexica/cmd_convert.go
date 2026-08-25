package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/spf13/cobra"
)

// Per FR-01.19 the CLI MUST support
//
//	aplexica convert <bundle> --to <agent> [--out <path>]
//
// to transcode an ACF bundle into the target adapter's native file tree
// WITHOUT importing it into the user's canonical store. This is a tooling
// primitive for "what would Codex see if I switched to it tomorrow?"
//
// Output layout (rooted at --out, defaults to <bundle-basename>-<agent>/):
//
//	<out>/global/         <-- global-scope artifacts: adapter's HomeDir is
//	                         pointed here, so claude-code writes
//	                         <out>/global/.claude/CLAUDE.md, codex writes
//	                         <out>/global/.codex/AGENTS.md, etc.
//	<out>/projects/<id>/  <-- project-scope artifacts: this directory is
//	                         the adapter's contextDir, so the layout
//	                         matches what would land at the user's
//	                         project root if they cloned <id> locally.
//	<out>/conversations/  <-- conversation artifacts, which NativePath
//	                         intentionally rejects for cross-adapter
//	                         fan-out (BRD-02 §4.6). Convert is single-
//	                         adapter, so we write them directly with a
//	                         per-adapter file extension.

var (
	convertTo         string
	convertOut        string
	convertVerbose    bool
	convertUnsignedOK bool
)

var convertCmd = &cobra.Command{
	Use:   "convert <bundle>",
	Short: "Transcode an ACF bundle into the target adapter's native file tree",
	Long: `Transcode a .tar.gz bundle into the named adapter's native file
layout WITHOUT importing the bundle into the user's canonical store.

The bundle is restored into a temporary directory; for each artifact,
the target adapter's NativePath + Export pipeline writes a native file
under --out. Conversation artifacts (which NativePath intentionally
rejects for cross-adapter fan-out) are written under
<out>/conversations/<artifactID>.<adapter-ext> so single-adapter convert
still round-trips them.

A fidelity report is printed at the end: exported / skipped counts and
the path of every skipped artifact with a one-line reason.

Examples:
  aplexica convert claude-bundle.tar.gz --to codex
  aplexica convert mybundle.tar.gz --to claude-code --out ./preview/`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConvert(cmd, args[0])
	},
}

func runConvert(cmd *cobra.Command, bundlePath string) error {
	if convertTo == "" {
		return fmt.Errorf("--to is required")
	}

	out := convertOut
	if out == "" {
		base := strings.TrimSuffix(filepath.Base(bundlePath), ".gz")
		base = strings.TrimSuffix(base, ".tar")
		out = base + "-" + convertTo
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("--out: %w", err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", out, err)
	}

	// Stage the bundle into a fresh temp store so we don't touch the
	// user's primary canonical store at all.
	stage, err := os.MkdirTemp("", "aplexica-convert-*")
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	defer os.RemoveAll(stage)

	stageStoreRoot := filepath.Join(stage, "store")
	stageSecretsRoot := filepath.Join(stage, "secrets")
	stageStore := &acf.Store{Root: stageStoreRoot}
	if err := stageStore.Init(); err != nil {
		return fmt.Errorf("init stage store: %w", err)
	}

	f, err := os.Open(bundlePath)
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	defer f.Close()
	if err := stageStore.RestoreWithOptions(f, stageSecretsRoot, acf.RestoreOptions{UnsignedOK: convertUnsignedOK}); err != nil {
		return fmt.Errorf("restore bundle: %w", err)
	}

	// Build the target adapter with HomeDir pointed inside <out>/global so
	// global-scope artifacts get rooted under the output tree (the adapter
	// writes <home>/.claude/... or <home>/.codex/... naturally — we just
	// move the home).
	globalRoot := filepath.Join(out, "global")
	if err := os.MkdirAll(globalRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", globalRoot, err)
	}
	a, err := buildAdapterForConvert(convertTo, stageSecretsRoot, globalRoot)
	if err != nil {
		return err
	}

	report := convertReport{out: out, adapter: convertTo}
	ctx := context.Background()

	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := stageStore.ListArtifacts(kind)
		if err != nil {
			return fmt.Errorf("list %s: %w", kind, err)
		}
		for _, art := range arts {
			if err := convertOne(ctx, a, stageStore, art, out, &report); err != nil {
				return err
			}
		}
	}

	report.print(cmd.OutOrStdout())
	return nil
}

func convertOne(
	ctx context.Context,
	a adapter.Adapter,
	store *acf.Store,
	art acf.Artifact,
	outRoot string,
	report *convertReport,
) error {
	// Conversations: NativePath rejects them for cross-adapter fan-out, but
	// single-adapter convert can still write them. Use a deterministic
	// per-artifact path under <out>/conversations/.
	if art.Kind == acf.KindConversation {
		convDir := filepath.Join(outRoot, "conversations")
		if err := os.MkdirAll(convDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", convDir, err)
		}
		dest := filepath.Join(convDir, art.ArtifactID+conversationExtFor(a.Name()))
		return runExport(ctx, a, store, art, dest, report)
	}

	// Memory / skill / tool: ask the adapter where it would write this.
	contextDir, err := contextDirFor(art, outRoot)
	if err != nil {
		report.skip(art, err.Error())
		return nil
	}
	dest, supports, err := a.NativePath(art, contextDir)
	if err != nil {
		report.skip(art, fmt.Sprintf("NativePath: %v", err))
		return nil
	}
	if !supports {
		report.skip(art, fmt.Sprintf("%s adapter does not support kind=%s", a.Name(), art.Kind))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
	}
	return runExport(ctx, a, store, art, dest, report)
}

func runExport(
	ctx context.Context,
	a adapter.Adapter,
	store *acf.Store,
	art acf.Artifact,
	dest string,
	report *convertReport,
) error {
	if err := a.Export(ctx, store, art.ArtifactID, dest); err != nil {
		if errors.Is(err, adapter.ErrArtifactTombstoned) {
			report.skip(art, "tombstoned (last event is a redaction)")
			return nil
		}
		return fmt.Errorf("export %s/%s: %w", art.Kind, art.ArtifactID, err)
	}
	report.exported(art, dest)
	return nil
}

// contextDirFor resolves where a project/namespace-scope artifact should
// land under outRoot. Global artifacts return "" (the adapter ignores
// contextDir for global). Project artifacts with a non-nil Project use
// the canonical project ID as a stable directory name; pre-v0.54.0
// project artifacts (Project == nil) land in <out>/projects/_unknown so
// they don't collide with each other.
func contextDirFor(art acf.Artifact, outRoot string) (string, error) {
	switch art.Scope {
	case acf.ScopeGlobal, "":
		return "", nil // adapter ignores contextDir for global
	case acf.ScopeProject:
		base := filepath.Join(outRoot, "projects")
		if art.Project != nil && art.Project.ID != "" {
			return filepath.Join(base, sanitizePathSegment(art.Project.ID)), nil
		}
		return filepath.Join(base, "_unknown"), nil
	case acf.ScopeNamespace:
		// Namespaces don't have a stable on-disk identifier in v0.1; group
		// them under _namespace/<artifact-id> so each artifact gets its
		// own slot.
		return filepath.Join(outRoot, "namespaces", art.ArtifactID), nil
	}
	return "", fmt.Errorf("unknown scope %q", art.Scope)
}

// sanitizePathSegment makes a project ID safe to use as a directory name.
// Project IDs are git/hg URLs (e.g. "https://github.com/foo/bar.git") and
// can contain characters that aren't filesystem-portable across all
// platforms. Replace the obvious unsafe ones with '_'. The result is not
// reversible — it doesn't need to be, it's a display-only output tree.
func sanitizePathSegment(s string) string {
	// Drop URL scheme and any leading slashes; collapse path separators.
	r := strings.NewReplacer(
		"://", "_",
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	out := r.Replace(s)
	if out == "" || out == "." || out == ".." {
		return "_"
	}
	return out
}

// conversationExtFor picks an output extension for conversation artifacts.
// Adapters produce per-agent session formats; ".jsonl" is the de facto
// shared extension across claude-code/codex/openclaw/hermes (the hermes
// adapter normalizes to JSONL on export) and ".session.json" for kilo.
func conversationExtFor(adapterName string) string {
	switch adapterName {
	case "kilo":
		return ".session.json"
	default:
		return ".jsonl"
	}
}

// buildAdapterForConvert is like buildAdapter() but overrides HomeDir on
// the concrete adapter type so global-scope artifacts route into
// <out>/global/ instead of the user's real home directory. The buildAdapter
// helper in adapters.go doesn't expose this knob because the regular CLI
// paths want the real home; convert is the only caller that needs an
// overridden home.
func buildAdapterForConvert(name, secretsRoot, globalRoot string) (adapter.Adapter, error) {
	a, err := buildAdapter(name, secretsRoot)
	if err != nil {
		return nil, err
	}
	switch t := a.(type) {
	case *claudecode.Adapter:
		t.HomeDir = globalRoot
	case *codex.Adapter:
		t.HomeDir = globalRoot
	case *hermes.Adapter:
		t.HomeDir = globalRoot
	case *openclaw.Adapter:
		t.HomeDir = globalRoot
	case *kilo.Adapter:
		t.HomeDir = globalRoot
	}
	return a, nil
}

// convertReport collects per-artifact outcomes for the trailing fidelity
// report.
type convertReport struct {
	out     string
	adapter string
	exp     []convertExportedRow
	skipped []convertSkippedRow
}

type convertExportedRow struct {
	Artifact acf.Artifact
	Dest     string
}
type convertSkippedRow struct {
	Artifact acf.Artifact
	Reason   string
}

func (r *convertReport) exported(art acf.Artifact, dest string) {
	r.exp = append(r.exp, convertExportedRow{Artifact: art, Dest: dest})
}
func (r *convertReport) skip(art acf.Artifact, reason string) {
	r.skipped = append(r.skipped, convertSkippedRow{Artifact: art, Reason: reason})
}

func (r *convertReport) print(out interface{ Write(p []byte) (int, error) }) {
	fmt.Fprintf(out, "convert: %s adapter → %s\n", r.adapter, r.out)
	fmt.Fprintf(out, "  exported: %d\n", len(r.exp))
	fmt.Fprintf(out, "  skipped:  %d\n", len(r.skipped))
	if convertVerbose {
		for _, row := range r.exp {
			fmt.Fprintf(out, "  + %s %s → %s\n", row.Artifact.Kind, row.Artifact.ArtifactID, row.Dest)
		}
	}
	for _, row := range r.skipped {
		fmt.Fprintf(out, "  - %s %s: %s\n", row.Artifact.Kind, row.Artifact.ArtifactID, row.Reason)
	}
}

func init() {
	convertCmd.Flags().StringVar(&convertTo, "to", "",
		"target adapter name (claude-code|codex|kilo|hermes|openclaw)")
	convertCmd.Flags().StringVar(&convertOut, "out", "",
		"output directory (default: <bundle-basename>-<adapter>/)")
	convertCmd.Flags().BoolVar(&convertVerbose, "verbose", false,
		"list every exported artifact in the fidelity report")
	convertCmd.Flags().BoolVar(&convertUnsignedOK, "unsigned-ok", false,
		"explicitly acknowledge an unsigned input bundle")
	_ = convertCmd.MarkFlagRequired("to")
	rootCmd.AddCommand(convertCmd)
}
