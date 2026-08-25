package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	diffStoreRoot string
	diffBranchA   string
	diffEventA    string
	diffTo        string // shared "B side" target; meaning depends on mode
	diffJSON      bool
)

var diffCmd = &cobra.Command{
	Use:   "diff <artifact-id>",
	Short: "Diff two branches or two events of an artifact",
	Long: `Show divergent content between two refs on the same artifact.

Two modes:

  --branch <a> --to <b>   diff the head of branch <a> against the head
                          of branch <b> (event-by-event for
                          conversations; payload-shape for memories/
                          skills/tools)
  --event <e1> --to <e2>  diff two specific events by event ID or hash

For conversations, the diff walks both branches' event streams and
labels divergent segments (events present in A but not B, vice versa,
or differing payloads at the same logical position). For other kinds,
the diff compares the payload at each event.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if diffTo == "" {
			return fmt.Errorf("--to is required")
		}
		if diffBranchA == "" && diffEventA == "" {
			return fmt.Errorf("must provide --branch <a> or --event <e>")
		}
		if diffBranchA != "" && diffEventA != "" {
			return fmt.Errorf("--branch and --event are mutually exclusive")
		}
		store := &acf.Store{Root: diffStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, _, found, err := findArtifactByID(store, args[0])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("artifact %s not found in any kind", args[0])
		}
		if diffBranchA != "" {
			normA, errA := acf.NormalizeBranchName(diffBranchA)
			if errA != nil {
				return errA
			}
			normB, errB := acf.NormalizeBranchName(diffTo)
			if errB != nil {
				return errB
			}
			return diffBranches(cmd, store, kind, args[0], normA, normB)
		}
		all, err := store.ReadEvents(kind, args[0])
		if err != nil {
			return err
		}
		return diffEvents(cmd, all, diffEventA, diffTo)
	},
}

type diffEntry struct {
	Side    string    `json:"side"` // "A", "B", "both"
	EventID string    `json:"eventId,omitempty"`
	Hash    string    `json:"hash,omitempty"`
	Type    string    `json:"type,omitempty"`
	Time    time.Time `json:"timestamp,omitempty"`
	Summary string    `json:"summary,omitempty"`
}

func diffBranches(cmd *cobra.Command, store *acf.Store, kind acf.Kind, artifactID, a, b string) error {
	ea, err := store.ProjectEventsForBranch(kind, artifactID, a, acf.BranchProjectionOpts{})
	if err != nil {
		return err
	}
	eb, err := store.ProjectEventsForBranch(kind, artifactID, b, acf.BranchProjectionOpts{})
	if err != nil {
		return err
	}
	common := commonPrefix(ea, eb)
	out := []diffEntry{}
	for i := 0; i < common; i++ {
		out = append(out, diffEntry{
			Side:    "both",
			EventID: ea[i].EventID,
			Hash:    ea[i].Hash,
			Type:    string(ea[i].Type),
			Time:    ea[i].Timestamp,
			Summary: shortSummary(ea[i]),
		})
	}
	for _, e := range ea[common:] {
		out = append(out, diffEntry{
			Side:    "A",
			EventID: e.EventID,
			Hash:    e.Hash,
			Type:    string(e.Type),
			Time:    e.Timestamp,
			Summary: shortSummary(e),
		})
	}
	for _, e := range eb[common:] {
		out = append(out, diffEntry{
			Side:    "B",
			EventID: e.EventID,
			Hash:    e.Hash,
			Type:    string(e.Type),
			Time:    e.Timestamp,
			Summary: shortSummary(e),
		})
	}
	if diffJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Diff: branch %q (A) vs %q (B); %d events in common, %d only in A, %d only in B\n",
		a, b, common, len(ea)-common, len(eb)-common)
	for _, e := range out {
		mark := "="
		switch e.Side {
		case "A":
			mark = "<"
		case "B":
			mark = ">"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s [%s] %s\n",
			mark, shortHashOf(e.Hash), e.Time.Format(time.RFC3339), e.Type, e.Summary)
	}
	return nil
}

func diffEvents(cmd *cobra.Command, all []acf.Event, refA, refB string) error {
	var ea, eb *acf.Event
	for i := range all {
		if all[i].EventID == refA || all[i].Hash == refA {
			ea = &all[i]
		}
		if all[i].EventID == refB || all[i].Hash == refB {
			eb = &all[i]
		}
	}
	if ea == nil {
		return fmt.Errorf("event %q not found", refA)
	}
	if eb == nil {
		return fmt.Errorf("event %q not found", refB)
	}
	if diffJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"a": ea, "b": eb,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "--- A: %s [%s] %s\n+++ B: %s [%s] %s\n",
		shortHashOf(ea.Hash), ea.Type, ea.Timestamp.Format(time.RFC3339),
		shortHashOf(eb.Hash), eb.Type, eb.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(cmd.OutOrStdout(), "A.branch=%q  B.branch=%q\n", normOrMain(ea.Branch), normOrMain(eb.Branch))
	fmt.Fprintf(cmd.OutOrStdout(), "A.parent=%s  B.parent=%s\n", shortHashOf(ea.ParentHash), shortHashOf(eb.ParentHash))
	pa, _ := json.MarshalIndent(json.RawMessage(ea.Payload), "  ", "  ")
	pb, _ := json.MarshalIndent(json.RawMessage(eb.Payload), "  ", "  ")
	fmt.Fprintln(cmd.OutOrStdout(), "A.payload:")
	fmt.Fprintln(cmd.OutOrStdout(), "  "+string(pa))
	fmt.Fprintln(cmd.OutOrStdout(), "B.payload:")
	fmt.Fprintln(cmd.OutOrStdout(), "  "+string(pb))
	return nil
}

func commonPrefix(a, b []acf.Event) int {
	n := 0
	for n < len(a) && n < len(b) && a[n].Hash == b[n].Hash {
		n++
	}
	return n
}

func normOrMain(b string) string {
	if b == "" {
		return acf.MainBranch
	}
	return b
}

func shortSummary(e acf.Event) string {
	switch e.Type {
	case acf.EventTypeForkOuter:
		return fmt.Sprintf("fork from %s/%s", normOrMain(e.ForkSourceBranch), shortHashOf(e.ParentHash))
	case acf.EventTypeMergeOuter:
		return fmt.Sprintf("merge from %s/%s (%s)", e.MergeFromBranch, shortHashOf(e.MergeFromHash), e.MergeStrategy)
	default:
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if format, ok := p["format"].(string); ok {
			return fmt.Sprintf("format=%s", format)
		}
		return strings.TrimSpace(string(e.Payload))
	}
}

func init() {
	home, _ := os.UserHomeDir()
	diffCmd.Flags().StringVar(&diffStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"), "Canonical store root")
	diffCmd.Flags().StringVar(&diffBranchA, "branch", "", "First branch (A) for branch-mode diff")
	diffCmd.Flags().StringVar(&diffTo, "to", "", "Second branch or event (B)")
	diffCmd.Flags().StringVar(&diffEventA, "event", "", "First event (A) for event-mode diff")
	diffCmd.Flags().BoolVar(&diffJSON, "json", false, "Emit JSON instead of human-readable text")
	rootCmd.AddCommand(diffCmd)
}
