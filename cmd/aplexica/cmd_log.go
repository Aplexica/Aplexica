package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	logStoreRoot        string
	logIncludeCompacted bool
	logGraph            bool
	logBranchFilter     string
	logEventTagFilter   string
	logFormat           string // "json" (default) or "graph"
)

var logCmd = &cobra.Command{
	Use:   "log <artifact-id>",
	Short: "Print the event log for an artifact",
	Long: `Print the event log for an artifact. By default, one JSON event per
line in append order. Kind is auto-detected.

With --graph, render a git-log-style ASCII branch topology. With
--branch <name>, filter to events on that branch. With --event-tag
<tag>, filter to events bearing the named tag.

With --include-compacted, the output also includes events that have
been moved to <store>/events/.compacted/<kind>/<id>.jsonl.gz by
retention pruning (v0.29.0). Active + compacted layers are re-merged
in timestamp order. Use this for forensics across the retention
grace window — the default view shows only the post-snapshot tail
that lives in the active log.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: logStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, art, found, err := findArtifactByID(store, args[0])
		if err != nil {
			return err
		}
		if !found {
			// Fallback: resolve the arg as a native agent identifier (e.g. a
			// Claude Code session-id / .jsonl basename) rather than an ArtifactID.
			kind, art, found, err = store.FindByNativeID(args[0])
			if err != nil {
				return err
			}
		}
		if !found {
			return fmt.Errorf("artifact %s not found in any kind", args[0])
		}
		// Use the canonical ArtifactID for all store reads below — the user may
		// have passed a native session-id that findArtifactByID could not match.
		artID := art.ArtifactID
		var events []acf.Event
		var rerr error
		if logIncludeCompacted {
			events, rerr = store.ReadEventsIncludingCompacted(kind, artID)
		} else {
			events, rerr = store.ReadEvents(kind, artID)
		}
		if rerr != nil {
			return rerr
		}
		// Filter by branch first. Branch views show the projected history
		// (source ancestry through the fork point plus branch-local events),
		// not just events whose outer Branch equals the selected branch.
		if logBranchFilter != "" {
			normalized, nerr := acf.NormalizeBranchName(logBranchFilter)
			if nerr != nil {
				return nerr
			}
			opts := acf.BranchProjectionOpts{IncludeCompacted: logIncludeCompacted}
			projected, perr := store.ProjectEventsForBranch(kind, artID, normalized, opts)
			if perr != nil {
				return perr
			}
			events = projected
		}
		// Filter by event tag — union of write-time EventTags and the
		// per-artifact sidecar (v0.100.0 / FR-04.17).
		if logEventTagFilter != "" {
			sidecar, _ := store.LoadEventTagsFile(kind, artID)
			filtered := events[:0]
			for _, e := range events {
				if hasTag(e.EventTags, logEventTagFilter) ||
					hasTag(sidecar.ByHash[e.Hash], logEventTagFilter) {
					filtered = append(filtered, e)
				}
			}
			events = filtered
		}
		if logGraph || logFormat == "graph" {
			return renderGraph(cmd.OutOrStdout(), events)
		}
		for _, e := range events {
			b, merr := json.Marshal(e)
			if merr != nil {
				return fmt.Errorf("marshal event %s: %w", e.EventID, merr)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
		}
		return nil
	},
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// renderGraph emits a git-log-like ASCII rendering of the artifact's
// branch topology. Branch order is deterministic (main first, then by
// first-appearance), and each branch occupies one column. Fork edges
// draw a slash; merge edges draw a backslash.
//
// 100-column terminal target (FR-04.4). The renderer keeps columns
// fixed-width and truncates the message field.
func renderGraph(w io.Writer, events []acf.Event) error {
	// Discover branches in first-appearance order.
	var branchOrder []string
	branchIdx := map[string]int{}
	addBranch := func(b string) {
		if _, ok := branchIdx[b]; ok {
			return
		}
		branchIdx[b] = len(branchOrder)
		branchOrder = append(branchOrder, b)
	}
	addBranch(acf.MainBranch)
	for _, e := range events {
		b := e.Branch
		if b == "" {
			b = acf.MainBranch
		}
		addBranch(b)
	}
	// Build hash → branch map for cross-branch edges.
	hashBranch := map[string]string{}
	for _, e := range events {
		b := e.Branch
		if b == "" {
			b = acf.MainBranch
		}
		hashBranch[e.Hash] = b
	}
	const colWidth = 2 // 2 chars per column: "* " / "| " / "/ " / "\ "
	const msgMaxLen = 60
	for _, e := range events {
		b := e.Branch
		if b == "" {
			b = acf.MainBranch
		}
		col := branchIdx[b]
		var lane strings.Builder
		for i := range branchOrder {
			switch {
			case i == col:
				lane.WriteString("* ")
			case i < col:
				lane.WriteString("| ")
			default:
				lane.WriteString("  ")
			}
		}
		// Pad lane to total columns
		_ = colWidth
		shortHash := e.Hash
		if len(shortHash) > shortHashLen {
			shortHash = shortHash[:shortHashLen]
		}
		msg := fmt.Sprintf("[%s] %s", string(e.Type), e.EventID)
		switch e.Type {
		case acf.EventTypeForkOuter:
			msg = fmt.Sprintf("[fork] %s ← %s/%s", b, e.ForkSourceBranch, shortHashOf(e.ParentHash))
		case acf.EventTypeMergeOuter:
			from := e.MergeFromBranch
			if from == "" {
				from = "?"
			}
			msg = fmt.Sprintf("[merge] %s ← %s/%s (%s)", b, from, shortHashOf(e.MergeFromHash), e.MergeStrategy)
		}
		if len(msg) > msgMaxLen {
			msg = msg[:msgMaxLen-1] + "…"
		}
		ts := e.Timestamp.Format(time.RFC3339)
		fmt.Fprintf(w, "%s%s  %s  %s\n", lane.String(), shortHash, ts, msg)

		// Edge lines for fork/merge — connecting current column to the
		// source-branch column.
		var srcBranch string
		if e.Type == acf.EventTypeForkOuter {
			srcBranch = e.ForkSourceBranch
			if srcBranch == "" {
				srcBranch = acf.MainBranch
			}
		} else if e.Type == acf.EventTypeMergeOuter {
			srcBranch = e.MergeFromBranch
		}
		if srcBranch != "" {
			srcCol := branchIdx[srcBranch]
			if srcCol != col {
				var edge strings.Builder
				lo, hi := srcCol, col
				if lo > hi {
					lo, hi = hi, lo
				}
				for i := range branchOrder {
					switch {
					case i == srcCol:
						edge.WriteString("| ")
					case i == col:
						edge.WriteString("| ")
					case i > lo && i < hi:
						edge.WriteString("-")
						edge.WriteString("-")
					default:
						edge.WriteString("  ")
					}
				}
				fmt.Fprintln(w, edge.String())
			}
		}
	}
	// Legend
	fmt.Fprintln(w)
	fmt.Fprint(w, "Branches: ")
	fmt.Fprintln(w, strings.Join(branchOrder, ", "))
	return nil
}

func shortHashOf(h string) string {
	if len(h) > shortHashLen {
		return h[:shortHashLen]
	}
	return h
}

func init() {
	home, _ := os.UserHomeDir()
	logCmd.Flags().StringVar(&logStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"), "Canonical store root")
	logCmd.Flags().BoolVar(&logIncludeCompacted, "include-compacted", false,
		"Also include events that have been moved to .compacted/ by retention pruning (re-merged in timestamp order)")
	logCmd.Flags().BoolVar(&logGraph, "graph", false,
		"Render an ASCII branch-topology graph instead of raw JSON lines")
	logCmd.Flags().StringVar(&logBranchFilter, "branch", "",
		"Filter to events on the named branch (default: all branches)")
	logCmd.Flags().StringVar(&logEventTagFilter, "event-tag", "",
		"Filter to events bearing the named tag")
	logCmd.Flags().StringVar(&logFormat, "format", "",
		"Output format: empty (default — JSON), or \"graph\"")
	rootCmd.AddCommand(logCmd)

	// Suppress unused-import warning from defensive code path.
	_ = io.EOF
}
