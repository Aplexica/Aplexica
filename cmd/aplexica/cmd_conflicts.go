package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/spf13/cobra"
)

var conflictsRoot string

var conflictsCmd = &cobra.Command{
	Use:   "conflicts",
	Short: "List, inspect, and resolve daemon-detected conflicts",
}

var conflictsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List artifacts currently in conflict",
	RunE: func(cmd *cobra.Command, args []string) error {
		s := &conflicts.Store{Root: conflictsRoot}
		list, err := s.List()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no conflicts")
			return nil
		}
		for _, c := range list {
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %d divergent heads\n", c.ArtifactID, c.Kind, len(c.Heads))
		}
		return nil
	},
}

var conflictsShowCmd = &cobra.Command{
	Use:   "show <artifact-id>",
	Short: "Show the divergent heads for one conflict",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s := &conflicts.Store{Root: conflictsRoot}
		c, err := s.Get(args[0])
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(c, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	},
}

var (
	resolvePickIdx   int
	resolveStoreRoot string
)

var conflictsResolveCmd = &cobra.Command{
	Use:   "resolve <artifact-id>",
	Short: "Resolve a conflict by picking a head (--pick INDEX, 0 = first)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s := &conflicts.Store{Root: conflictsRoot}
		c, err := s.Get(args[0])
		if err != nil {
			return err
		}
		if resolvePickIdx < 0 || resolvePickIdx >= len(c.Heads) {
			return fmt.Errorf("--pick %d out of range [0, %d)", resolvePickIdx, len(c.Heads))
		}
		winner := c.Heads[resolvePickIdx]

		// Append a dedicated EventTypeResolution event to the artifact's
		// event log that re-asserts the winning payload's content hash as
		// the canonical head. This is the documented resolve flow
		// (ADR-0038). v0.28.0 used a plain EventTypeUpdate; v0.34.0
		// introduced EventTypeResolution so conflict resolution shows up
		// distinctly in the event log without grepping provenance. Replay
		// paths treat the type identically to create/update.
		store := &acf.Store{Root: resolveStoreRoot}
		events, err := store.ReadEvents(c.Kind, c.ArtifactID)
		if err != nil {
			return fmt.Errorf("read events: %w", err)
		}
		// Look up the winner's full payload by EventID.
		var winnerPayload json.RawMessage
		for _, e := range events {
			if e.EventID == winner.EventID {
				winnerPayload = e.Payload
				break
			}
		}
		// A remote inbound conflict head is recorded but never appended to any
		// local branch (B3), so its EventID is absent from the local log. Its
		// full content is preserved in the conflict sidecar's FullPayload — fall
		// back to it so the remote head can still win. The resolution below still
		// writes a real EventTypeResolution event (BRD-04 §6.3).
		if len(winnerPayload) == 0 && len(winner.FullPayload) > 0 {
			winnerPayload = winner.FullPayload
		}
		if len(winnerPayload) == 0 {
			return fmt.Errorf("winner event %s not found in artifact log", winner.EventID)
		}
		if err := appendConflictResolutionEvent(store, c.Kind, c.ArtifactID, winnerPayload, "aplexica:resolve", ""); err != nil {
			return err
		}
		if err := s.Clear(c.ArtifactID); err != nil {
			return err
		}
		// TODO(resolve-time re-propagation): handleEvent now WITHHOLDS
		// fan-out + remote forwarding for an artifact while a conflict is recorded
		// (gating in internal/sync/orchestrator.go). Clearing the conflict here
		// resumes propagation only on the NEXT native edit of the artifact. To
		// resume immediately, this CLI path would need to signal the running
		// daemon (e.g. a "refanout <artifactId>" control-socket command that calls
		// Orchestrator.fanOut/forwardCommitted for the resolved head). This command
		// talks directly to the file stores and holds no Orchestrator handle, so
		// wiring that cleanly is deferred — see openIssues.
		fmt.Fprintf(cmd.OutOrStdout(), "resolved %s (winner: head %d, source %s)\n",
			c.ArtifactID, resolvePickIdx, winner.SourceAgent)
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	defaultConflictsRoot := filepath.Join(home, ".aplexica", "conflicts")
	defaultStoreRoot := filepath.Join(home, ".aplexica", "store")

	for _, c := range []*cobra.Command{conflictsListCmd, conflictsShowCmd, conflictsResolveCmd} {
		c.Flags().StringVar(&conflictsRoot, "conflicts-root", defaultConflictsRoot, "Conflicts store root")
	}
	conflictsResolveCmd.Flags().IntVar(&resolvePickIdx, "pick", 0, "Index of the head to pick (0 = first)")
	conflictsResolveCmd.Flags().StringVar(&resolveStoreRoot, "store", defaultStoreRoot, "Canonical store root")

	conflictsCmd.AddCommand(conflictsListCmd)
	conflictsCmd.AddCommand(conflictsShowCmd)
	conflictsCmd.AddCommand(conflictsResolveCmd)
	rootCmd.AddCommand(conflictsCmd)
}
