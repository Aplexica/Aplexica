package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/spf13/cobra"
)

var snapshotStoreRoot string

var snapshotCmd = &cobra.Command{
	Use:   "snapshot <artifact-id>",
	Short: "Create a snapshot event on demand (bounds replay cost)",
	Long: `Create an EventTypeSnapshot event on an artifact's event log. The event
carries a SHA-256 of the latest materialized payload in its SnapshotState
field. Snapshots bound replay cost — readers can replay forward from the
most recent snapshot rather than from the genesis event.

Snapshots are a precondition for retention pruning (see ` + "`aplexica retention prune`" + `):
PruneArtifact moves all events strictly before the most recent snapshot
into <store>/events/.compacted/<kind>/<id>.jsonl.gz.

The kind is auto-detected by probing each kind in turn for the given ID.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: snapshotStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			if _, err := store.ReadArtifact(k, args[0]); err == nil {
				snap, serr := retention.CreateSnapshot(context.Background(), store, k, args[0])
				if serr != nil {
					return serr
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created snapshot %s (state %s)\n", snap.EventID, snap.SnapshotState)
				return nil
			}
		}
		return fmt.Errorf("artifact %s not found in any kind", args[0])
	},
}

// snapshotListCmd lists an artifact's EventTypeSnapshot events. It is a
// subcommand of snapshotCmd; the bare `snapshot <id>` create form is preserved
// (cobra dispatches to a subcommand only when args[0] matches its name).
var snapshotListCmd = &cobra.Command{
	Use:   "list <artifact-id>",
	Short: "List the snapshot events on an artifact's log",
	Long: `Print every EventTypeSnapshot event in an artifact's log — including any in
the .compacted layer — with its EventID, timestamp, and snapshot state hash.

The kind is auto-detected by probing each kind in turn for the given ID.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: snapshotStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
			if _, err := store.ReadArtifact(k, args[0]); err != nil {
				continue
			}
			events, err := store.ReadEventsIncludingCompacted(k, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var snaps []acf.Event
			for _, e := range events {
				if e.Type == acf.EventTypeSnapshot {
					snaps = append(snaps, e)
				}
			}
			if len(snaps) == 0 {
				fmt.Fprintf(out, "no snapshot events on %s %s\n", k, args[0])
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "EVENT\tTIMESTAMP\tSTATE")
			for _, e := range snaps {
				fmt.Fprintf(w, "%s\t%s\t%s\n",
					e.EventID, e.Timestamp.UTC().Format("2006-01-02T15:04:05Z"), e.SnapshotState)
			}
			return w.Flush()
		}
		return fmt.Errorf("artifact %s not found in any kind", args[0])
	},
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")
	snapshotCmd.Flags().StringVar(&snapshotStoreRoot, "store",
		defaultStore, "Canonical store root")
	snapshotListCmd.Flags().StringVar(&snapshotStoreRoot, "store",
		defaultStore, "Canonical store root")
	snapshotCmd.AddCommand(snapshotListCmd)
	rootCmd.AddCommand(snapshotCmd)
}
