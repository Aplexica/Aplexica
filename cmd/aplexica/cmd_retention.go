package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/config"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/spf13/cobra"
)

var (
	retentionStoreRoot       string
	retentionGrace           time.Duration
	retentionRestoreArtifact string
	retentionRestoreOut      string
)

// retentionKeyPrefix scopes `retention show` / `retention set` to the
// retention.* configuration namespace.
const retentionKeyPrefix = "retention."

var retentionCmd = &cobra.Command{
	Use:   "retention",
	Short: "Retention engine: snapshot + prune",
}

// resolveRetentionConfig resolves the effective layered config and maps the
// retention.* keys onto a typed retention.Config via the shared loader. It honors
// any --user-path / --system-path / --project-path overrides (shared with
// `aplexica config`).
func resolveRetentionConfig() (retention.Config, error) {
	eff, err := config.Load(currentLoadOpts())
	if err != nil {
		return retention.Config{}, err
	}
	return retention.Load(eff)
}

// retentionShowCmd prints the effective retention.* config with the layer that
// set each value (provenance), mirroring `aplexica config show`.
var retentionShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the effective retention config with provenance per key",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		eff, err := config.Load(currentLoadOpts())
		if err != nil {
			return err
		}
		// Surface the typed Config too, so a cross-field invariant violation
		// (which the per-key schema can't express) is caught early.
		if cfg, lerr := retention.Load(eff); lerr == nil {
			if verr := cfg.Validate(); verr != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "WARN:", verr)
			}
		}
		out := cmd.OutOrStdout()
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "KEY\tVALUE\tFROM")
		for _, k := range eff.Keys() {
			if !strings.HasPrefix(k, retentionKeyPrefix) {
				continue
			}
			v, layer, _ := eff.Get(k)
			fmt.Fprintf(w, "%s\t%s\t%s\n", k, v, layer)
		}
		return w.Flush()
	},
}

// retentionSetCmd validates a retention.* key+value against the embedded
// schema, then persists it to the USER layer (reusing config.SetKey, the same
// writer `aplexica config set` uses).
var retentionSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Validate and persist a retention.* config key to the user layer",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]
		if !strings.HasPrefix(key, retentionKeyPrefix) {
			return fmt.Errorf("retention set: key must be a retention.* key, got %q", key)
		}
		// Validate the key+value via the embedded schema (FR-10.9) before any
		// write: build a synthetic single-key Effective and range-check it.
		eff := &config.Effective{
			Values:     map[string]string{key: value},
			Provenance: map[string]config.Layer{key: config.LayerUser},
		}
		errs, warns := config.SchemaValidate(eff)
		for _, ws := range warns {
			fmt.Fprintln(cmd.ErrOrStderr(), "WARN:", ws)
		}
		if len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "ERR :", e)
			}
			return fmt.Errorf("retention set: %d schema violation(s) for %s", len(errs), key)
		}
		path, err := resolveLayerPath("user")
		if err != nil {
			return err
		}
		if err := config.SetKey(path, key, value); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s = %s   (in user layer at %s)\n", key, value, path)
		return nil
	},
}

// retentionRestoreCmd is the §4.8.5 un-compact surface — EXTRACT-ONLY.
// Re-inserting a compacted event into the append-only active chain is
// structurally impossible (AppendEvent enforces ParentHash == head), so this
// command DECODES the requested event from the .compacted layer and writes it
// to stdout (or --out) as JSON. It never re-chains.
var retentionRestoreCmd = &cobra.Command{
	Use:   "restore <eventId>",
	Short: "Extract a compacted event as JSON for inspection/recovery",
	Long: `Decode a single event from the .compacted layer and write it to stdout (or
--out <file>) as JSON.

This is EXTRACTION for inspection/recovery, NOT reinsertion. A compacted event
cannot be re-chained into the append-only active log (AppendEvent enforces
ParentHash == branch head), so restore never mutates any event log.

The event is located by scanning compacted logs (merged active+compacted) for
the EventID. Pass --artifact <id> to scope the search to a single artifact
(faster, and required when the same EventID could appear under multiple
artifacts).

Recovery is bounded by the compacted file's existence: once 'retention prune'
sweeps a past-grace .compacted file the event becomes unrecoverable (no explicit
grace check is applied while the file survives).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: retentionStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		evt, found, err := findCompactedEvent(store, args[0], retentionRestoreArtifact)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("retention restore: event %s not found in any compacted log", args[0])
		}
		body, err := json.MarshalIndent(evt, "", "  ")
		if err != nil {
			return fmt.Errorf("retention restore: marshal event: %w", err)
		}
		if retentionRestoreOut != "" {
			if werr := os.WriteFile(retentionRestoreOut, append(body, '\n'), 0o644); werr != nil {
				return fmt.Errorf("retention restore: write %s: %w", retentionRestoreOut, werr)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "extracted event %s to %s (inspection only; not re-chained)\n",
				evt.EventID, retentionRestoreOut)
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(body))
		return nil
	},
}

// findCompactedEvent locates a single event by EventID in the merged
// active+compacted logs. When artifactID is non-empty the search is scoped to
// that artifact (across all kinds); otherwise every artifact of every kind is
// scanned. Only events that live in the COMPACTED layer (i.e. not in the
// active log) are eligible — restore extracts pruned history, not live events.
func findCompactedEvent(store *acf.Store, eventID, artifactID string) (acf.Event, bool, error) {
	kinds := []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation}
	for _, k := range kinds {
		var ids []string
		if artifactID != "" {
			ids = []string{artifactID}
		} else {
			arts, err := store.ListArtifacts(k)
			if err != nil {
				return acf.Event{}, false, err
			}
			for _, a := range arts {
				ids = append(ids, a.ArtifactID)
			}
		}
		for _, id := range ids {
			merged, err := store.ReadEventsIncludingCompacted(k, id)
			if err != nil {
				// A scoped --artifact under the wrong kind simply has no log.
				if artifactID != "" {
					continue
				}
				return acf.Event{}, false, err
			}
			active, err := store.ReadEvents(k, id)
			if err != nil {
				return acf.Event{}, false, err
			}
			activeIDs := make(map[string]struct{}, len(active))
			for _, e := range active {
				activeIDs[e.EventID] = struct{}{}
			}
			for _, e := range merged {
				if e.EventID != eventID {
					continue
				}
				if _, live := activeIDs[e.EventID]; live {
					// Event is still in the active log; restore only extracts
					// compacted (pruned) history.
					continue
				}
				return e, true, nil
			}
		}
	}
	return acf.Event{}, false, nil
}

var retentionPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Move pre-snapshot events to .compacted; delete compacted files older than --grace",
	Long: `Iterate every artifact in the canonical store and, for each one with a
snapshot event in its log, move all pre-snapshot events into
<store>/events/.compacted/<kind>/<id>.jsonl.gz (gzipped). Artifacts with
` + "`Tags`" + ` containing "pinned" or "keep-forever" are exempt.

After moving, this command checks the compacted file's mtime against the
grace deadline (now - --grace). Compacted files older than the deadline
are deleted outright.

The grace window default is 7 days.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: retentionStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		graceDeadline := time.Now().Add(-retentionGrace)
		var totalMoved, totalDeleted int
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			arts, err := store.ListArtifacts(k)
			if err != nil {
				return err
			}
			for _, a := range arts {
				moved, deleted, err := retention.PruneArtifact(context.Background(), store, k, a.ArtifactID, graceDeadline)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "prune %s: %v\n", a.ArtifactID, err)
					continue
				}
				totalMoved += moved
				totalDeleted += deleted
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "pruned: %d events moved to .compacted, %d compacted files deleted past grace\n",
			totalMoved, totalDeleted)
		return nil
	},
}

// retentionPreviewCmd is `gc --dry-run` under the hood (FR-03.22/23): it runs
// the manual retention pass in plan-only mode and prints the GCReport, so an
// operator can preview exactly what a real gc would evict, snapshot, and prune
// — including which history-compaction prunes are blocked without
// --force-local-only — without writing anything.
var retentionPreviewCmd = &cobra.Command{
	Use:   "preview",
	Short: "Preview a manual retention pass without writing (alias for gc --dry-run)",
	Long: `Run the manual retention garbage-collection pass in DRY-RUN mode and print
the report. No file is written. This is exactly ` + "`aplexica gc --dry-run`" + `,
surfaced under retention for discoverability. History-compaction prunes are
reported as blocked (they require --force-local-only on a real gc run).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Route through the shared gc body in dry-run mode, reusing the
		// retention --store root. Markdown report, never force, never mutate.
		gcStoreRoot = retentionStoreRoot
		return runGC(cmd, true, false, false)
	},
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")
	retentionPruneCmd.Flags().StringVar(&retentionStoreRoot, "store",
		defaultStore, "Canonical store root")
	retentionPruneCmd.Flags().DurationVar(&retentionGrace, "grace", 7*24*time.Hour,
		"Grace period before .compacted files are deleted")
	retentionPreviewCmd.Flags().StringVar(&retentionStoreRoot, "store",
		defaultStore, "Canonical store root")

	// show/set read & write the layered config; expose the same path
	// overrides as `aplexica config` so tests (and operators) can pin layers.
	retentionCmd.PersistentFlags().StringVar(&configSystemPath, "system-path", "",
		"override the system-layer config path (default: platform-specific)")
	retentionCmd.PersistentFlags().StringVar(&configUserPath, "user-path", "",
		"override the user-layer config path (default: ~/.aplexica/config.toml)")
	retentionCmd.PersistentFlags().StringVar(&configProjectPath, "project-path", "",
		"project-layer config path (no default — supply when in a project context)")

	retentionRestoreCmd.Flags().StringVar(&retentionStoreRoot, "store",
		defaultStore, "Canonical store root")
	retentionRestoreCmd.Flags().StringVar(&retentionRestoreArtifact, "artifact", "",
		"scope the compacted-event search to a single artifact id")
	retentionRestoreCmd.Flags().StringVar(&retentionRestoreOut, "out", "",
		"write the extracted event JSON to this file instead of stdout")

	retentionCmd.AddCommand(retentionPruneCmd)
	retentionCmd.AddCommand(retentionPreviewCmd)
	retentionCmd.AddCommand(retentionShowCmd)
	retentionCmd.AddCommand(retentionSetCmd)
	retentionCmd.AddCommand(retentionRestoreCmd)
	rootCmd.AddCommand(retentionCmd)
}
