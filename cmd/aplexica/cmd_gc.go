package main

import (
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
	"github.com/aplexica/aplexica/internal/config"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/spf13/cobra"
)

var (
	gcStoreRoot      string
	gcDryRun         bool
	gcForceLocalOnly bool
	gcJSON           bool
)

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Manual retention pass: evict attachments, GC blobs, snapshot, prune",
	Long: `Run a manual, full retention garbage-collection pass over the canonical
store. Unlike the daemon's disk-pressure sweep, gc is NOT
watermark-gated — it always runs the full ordered pass:

  1. evict old attachment bytes, then GC unreferenced blobs (OSS default);
  2. snapshot artifacts whose following prune is policy- and peer-authorized;
  3. prune (compact pre-snapshot history) each artifact.

History compaction (step 3) is destructive for peers that have not yet
replicated the pruned events. Because there is no per-device ACK-cursor API
yet, a prune that would move events past a snapshot is SKIPPED unless
--force-local-only is passed. Its preparatory snapshot is skipped too. Both
are also skipped when keep_last_n_snapshots is "all", because a full-state
checkpoint with no consuming prune only grows the store. Skipped prunes are
reported so an operator can see what --force-local-only would compact.

Use --dry-run to see exactly what gc would do without writing anything.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGC(cmd, gcDryRun, gcForceLocalOnly, gcJSON)
	},
}

// runGC is the shared body behind `aplexica gc` and `aplexica retention
// preview` (the latter forces dryRun). It builds the store + blobstore, loads
// the retention config, runs retention.RunGC, and prints the report as Markdown
// (default) or JSON (--json).
func runGC(cmd *cobra.Command, dryRun, forceLocalOnly, asJSON bool) error {
	store := &acf.Store{Root: gcStoreRoot}
	if err := store.Init(); err != nil {
		return err
	}
	blobs := &blobstore.Store{Root: store.BlobsDir()}

	cfg, err := gcRetentionConfig()
	if err != nil {
		return err
	}

	report, err := retention.RunGC(cmd.Context(), store, blobs, cfg, retention.GCOptions{
		DryRun:         dryRun,
		ForceLocalOnly: forceLocalOnly,
		AckGate:        retention.NoPeerAck{},
	})
	if err != nil {
		return err
	}

	if asJSON {
		out, merr := report.MarshalJSON()
		if merr != nil {
			return merr
		}
		_, werr := cmd.OutOrStdout().Write(append(out, '\n'))
		return werr
	}
	_, werr := cmd.OutOrStdout().Write(report.MarshalMarkdown())
	return werr
}

// gcRetentionConfig resolves the typed retention.Config from the layered
// config (shipped → system → user → env). It reuses retention.Load — the same
// loader the daemon's loadRetentionConfig uses — so the manual pass honors the
// operator's retention.* settings (keep_last_n_snapshots, attachment_min_age,
// attachments_only, pin tags) exactly as the daemon does.
func gcRetentionConfig() (retention.Config, error) {
	sys, usr, _ := config.DefaultSources()
	eff, err := config.Load(config.LoadOptions{
		SystemPath: sys,
		UserPath:   usr,
		Env:        os.Environ(),
	})
	if err != nil {
		return retention.Config{}, err
	}
	return retention.Load(eff)
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")
	gcCmd.Flags().StringVar(&gcStoreRoot, "store", defaultStore, "Canonical store root")
	gcCmd.Flags().BoolVar(&gcDryRun, "dry-run", false, "Report what gc would do without writing anything")
	gcCmd.Flags().BoolVar(&gcForceLocalOnly, "force-local-only", false,
		"Authorize history-compaction prunes without per-device ACK (single-device stores only)")
	gcCmd.Flags().BoolVar(&gcJSON, "json", false, "Emit the GCReport as JSON instead of Markdown")
	rootCmd.AddCommand(gcCmd)
}
