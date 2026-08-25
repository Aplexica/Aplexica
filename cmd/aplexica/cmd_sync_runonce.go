package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/spf13/cobra"
)

// FR-03.8: aplexica sync run-once --from <a> --to <b>
//
// One-shot reconciliation between two adapters. For each artifact in
// the canonical store that originated from <from> (i.e., the first
// event's provenance.sourceAgent matches), exports it through <to>'s
// native form.
//
// Useful for:
//   - Recovering after a pause+resume gap.
//   - Forcing a re-sync after manual edits on one side.
//   - Bootstrapping a new adapter on an already-populated store.

var (
	syncRunOnceFrom        string
	syncRunOnceTo          string
	syncRunOnceContextDir  string
	syncRunOnceStoreRoot   string
	syncRunOnceSecretsRoot string
	syncRunOnceVerbose     bool
)

var syncRunOnceCmd = &cobra.Command{
	Use:   "run-once",
	Short: "One-shot reconciliation between two adapters",
	Long: `Re-export every artifact whose first-event source-agent matches
--from through the --to adapter's native form. The canonical store
isn't modified; only the --to adapter's native files are written.

By default, --context-dir is the current working directory (used as
the project root for non-global artifacts). Global artifacts route
through the destination adapter's HomeDir per its standard
NativePath logic.

Reports per-artifact outcomes: ok / unsupported / tombstoned /
error. Exit code is non-zero on any error.

Examples:

  aplexica sync run-once --from claude-code --to codex
  aplexica sync run-once --from codex --to hermes --context-dir ./proj`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if syncRunOnceFrom == "" || syncRunOnceTo == "" {
			return fmt.Errorf("--from and --to are required")
		}
		if syncRunOnceFrom == syncRunOnceTo {
			return fmt.Errorf("--from and --to must be different adapters")
		}

		// Build the destination adapter (we don't need the source —
		// we just match on provenance.sourceAgent).
		toAd, err := buildAdapter(syncRunOnceTo, syncRunOnceSecretsRoot)
		if err != nil {
			return fmt.Errorf("build --to adapter %q: %w", syncRunOnceTo, err)
		}

		store := &acf.Store{Root: syncRunOnceStoreRoot}
		if err := store.Init(); err != nil {
			return fmt.Errorf("init store: %w", err)
		}

		// Resolve contextDir for project-scope artifacts.
		contextDir := syncRunOnceContextDir
		if contextDir == "" {
			contextDir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("resolve cwd: %w", err)
			}
		}
		contextDir, err = filepath.Abs(contextDir)
		if err != nil {
			return err
		}

		ctx := context.Background()
		report := runOnceReport{from: syncRunOnceFrom, to: syncRunOnceTo}

		for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			arts, err := store.ListArtifacts(kind)
			if err != nil {
				return fmt.Errorf("list %s: %w", kind, err)
			}
			for _, art := range arts {
				if err := runOnceProcess(ctx, store, art, toAd, contextDir, &report); err != nil {
					return err
				}
			}
		}

		report.print(cmd.OutOrStdout(), syncRunOnceVerbose)
		if report.errors > 0 {
			return fmt.Errorf("sync run-once: %d error(s) (see report above)", report.errors)
		}
		return nil
	},
}

func runOnceProcess(
	ctx context.Context,
	store *acf.Store,
	art acf.Artifact,
	to adapter.Adapter,
	contextDir string,
	report *runOnceReport,
) error {
	// Filter to artifacts whose FIRST event's sourceAgent matches --from.
	events, err := store.ReadEvents(art.Kind, art.ArtifactID)
	if err != nil {
		return fmt.Errorf("read events %s/%s: %w", art.Kind, art.ArtifactID, err)
	}
	if len(events) == 0 {
		return nil
	}
	if events[0].Provenance.SourceAgent != syncRunOnceFrom {
		return nil
	}

	dest, supports, err := to.NativePath(art, contextDir)
	if err != nil {
		report.skipped++
		report.notes = append(report.notes, runOnceNote{art, "NativePath: " + err.Error()})
		return nil
	}
	if !supports {
		report.skipped++
		report.notes = append(report.notes, runOnceNote{art,
			to.Name() + " does not support kind=" + string(art.Kind)})
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		report.errors++
		report.notes = append(report.notes, runOnceNote{art, "mkdir: " + err.Error()})
		return nil
	}
	if err := to.Export(ctx, store, art.ArtifactID, dest); err != nil {
		if errors.Is(err, adapter.ErrArtifactTombstoned) {
			report.tombstoned++
			return nil
		}
		report.errors++
		report.notes = append(report.notes, runOnceNote{art, "Export: " + err.Error()})
		return nil
	}
	report.exported++
	if syncRunOnceVerbose {
		report.notes = append(report.notes, runOnceNote{art, "ok → " + dest})
	}
	return nil
}

type runOnceReport struct {
	from       string
	to         string
	exported   int
	skipped    int
	tombstoned int
	errors     int
	notes      []runOnceNote
}

type runOnceNote struct {
	art    acf.Artifact
	detail string
}

func (r *runOnceReport) print(w interface{ Write(p []byte) (int, error) }, verbose bool) {
	fmt.Fprintf(w, "sync run-once: %s → %s\n", r.from, r.to)
	fmt.Fprintf(w, "  exported:   %d\n", r.exported)
	fmt.Fprintf(w, "  skipped:    %d\n", r.skipped)
	fmt.Fprintf(w, "  tombstoned: %d\n", r.tombstoned)
	fmt.Fprintf(w, "  errors:     %d\n", r.errors)
	for _, n := range r.notes {
		fmt.Fprintf(w, "  - %s %s: %s\n", n.art.Kind, n.art.ArtifactID, n.detail)
	}
}

func init() {
	home, _ := os.UserHomeDir()
	syncRunOnceCmd.Flags().StringVar(&syncRunOnceFrom, "from", "",
		"source adapter name (artifacts whose first event came from this adapter)")
	syncRunOnceCmd.Flags().StringVar(&syncRunOnceTo, "to", "",
		"destination adapter name (Export every matching artifact through this adapter)")
	syncRunOnceCmd.Flags().StringVar(&syncRunOnceContextDir, "context-dir", "",
		"context directory for project-scope artifacts (default: cwd)")
	syncRunOnceCmd.Flags().StringVar(&syncRunOnceStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"),
		"canonical store root")
	syncRunOnceCmd.Flags().StringVar(&syncRunOnceSecretsRoot, "secrets-root",
		filepath.Join(home, ".aplexica", "secrets"),
		"secrets store root")
	syncRunOnceCmd.Flags().BoolVar(&syncRunOnceVerbose, "verbose", false,
		"list every exported artifact with its destination path")
	_ = syncRunOnceCmd.MarkFlagRequired("from")
	_ = syncRunOnceCmd.MarkFlagRequired("to")
	syncCmd.AddCommand(syncRunOnceCmd)
}
