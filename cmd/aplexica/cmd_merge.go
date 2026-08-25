package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	mergeStoreRoot string
	mergeFrom      string
	mergeInto      string
	mergeStrategy  string
	mergeNotes     string
	mergeAccept    string // "from" | "into" | "" for non-TTY tests
	mergeJSON      bool
	mergeNonInter  bool
)

const (
	strategyFastForward = "fast-forward"
	strategyManual      = "manual"
	strategyOurs        = "ours"
	strategyTheirs      = "theirs"
)

var mergeCmd = &cobra.Command{
	Use:   "merge <artifact-id>",
	Short: "Merge one branch into another",
	Long: `Combine two branches of an artifact. Supported strategies:

  fast-forward (default)  --into MUST be a strict prefix of --from.
                          The --into branch's head advances to --from's
                          head; no merge-event payload change.

  manual                  Interactive resolver: the user is shown the
                          divergent events side-by-side and picks
                          which payload becomes the merge result.

  ours                    Keep --into's head payload; record a merge
                          event referencing --from.

  theirs                  Adopt --from's head payload onto --into.

When three or more branches diverge, running ` + "`aplexica merge <id>`" + `
without --from/--into prompts for a destination and performs N-1
pairwise merges sequentially.`,
	Args: cobra.ExactArgs(1),
	RunE: runMerge,
}

func runMerge(cmd *cobra.Command, args []string) error {
	artifactID := args[0]
	store := &acf.Store{Root: mergeStoreRoot}
	if err := store.Init(); err != nil {
		return err
	}
	kind, _, found, err := findArtifactByID(store, artifactID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("artifact %s not found in any kind", artifactID)
	}

	// N-way mode: no --from/--into → prompt or auto-pick when there are
	// 3+ unmerged branches (§5.4.1).
	if mergeFrom == "" && mergeInto == "" {
		return runNWayMerge(cmd, store, kind, artifactID)
	}
	if mergeFrom == "" || mergeInto == "" {
		return fmt.Errorf("--from and --into are both required (or omit both for N-way mode)")
	}
	normFrom, err := acf.NormalizeBranchName(mergeFrom)
	if err != nil {
		return err
	}
	normInto, err := acf.NormalizeBranchName(mergeInto)
	if err != nil {
		return err
	}
	return doMerge(cmd, store, kind, artifactID, normFrom, normInto, mergeStrategy)
}

func runNWayMerge(cmd *cobra.Command, store *acf.Store, kind acf.Kind, artifactID string) error {
	branches, err := store.ListBranches(kind, artifactID, false)
	if err != nil {
		return err
	}
	// Keep only unmerged, non-archived branches.
	var open []acf.BranchInfo
	for _, b := range branches {
		if b.Archived || b.MergedInto != "" {
			continue
		}
		open = append(open, b)
	}
	if len(open) < 2 {
		return fmt.Errorf("fewer than 2 active branches; nothing to merge")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Detected %d diverging branches:\n", len(open))
	for _, b := range open {
		fmt.Fprintf(cmd.OutOrStdout(), "  %-32s  (head: %s..., last event %s, %d events)\n",
			b.Name, shortHashOf(b.Head), humanDelta(b.LastEventAt), b.EventCount)
	}
	dest, err := promptForBranch(cmd, open)
	if err != nil {
		return err
	}
	// Confirm.
	others := make([]string, 0, len(open)-1)
	for _, b := range open {
		if b.Name != dest {
			others = append(others, b.Name)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Will merge %s into %q in that order.\n", strings.Join(others, ", "), dest)
	if !mergeNonInter {
		fmt.Fprint(cmd.OutOrStdout(), "Continue? [y/N] ")
		if !readYes(cmd.InOrStdin()) {
			return fmt.Errorf("aborted")
		}
	}
	strategy := mergeStrategy
	if strategy == "" {
		strategy = strategyManual
	}
	for _, src := range others {
		if err := doMerge(cmd, store, kind, artifactID, src, dest, strategy); err != nil {
			return fmt.Errorf("merge %s → %s: %w", src, dest, err)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "merged %d branch(es) into %q\n", len(others), dest)
	return nil
}

func promptForBranch(cmd *cobra.Command, open []acf.BranchInfo) (string, error) {
	if mergeInto != "" {
		// --into provided as override for N-way mode.
		return acf.NormalizeBranchName(mergeInto)
	}
	if mergeNonInter {
		// Non-interactive: pick main if present, else first.
		for _, b := range open {
			if b.Name == acf.MainBranch {
				return acf.MainBranch, nil
			}
		}
		return open[0].Name, nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Which branch should become the destination?")
	for i, b := range open {
		marker := "  "
		if b.Name == acf.MainBranch {
			marker = "> "
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s%d) %s\n", marker, i+1, b.Name)
	}
	fmt.Fprint(cmd.OutOrStdout(), "Pick [1]: ")
	sc := bufio.NewScanner(cmd.InOrStdin())
	if !sc.Scan() {
		return open[0].Name, nil
	}
	line := strings.TrimSpace(sc.Text())
	if line == "" {
		return open[0].Name, nil
	}
	for _, b := range open {
		if b.Name == line {
			return b.Name, nil
		}
	}
	return "", fmt.Errorf("invalid selection %q", line)
}

func readYes(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(sc.Text()))
	return ans == "y" || ans == "yes"
}

// hoursPerDay is a calendar constant, not a tunable.
const hoursPerDay = 24

func humanDelta(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < hoursPerDay*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/hoursPerDay))
	}
}

func doMerge(cmd *cobra.Command, store *acf.Store, kind acf.Kind, artifactID, from, into, strategy string) error {
	if strategy == "" {
		strategy = strategyFastForward
	}
	switch strategy {
	case strategyFastForward, strategyManual, strategyOurs, strategyTheirs:
	default:
		return fmt.Errorf("unknown strategy %q (allowed: fast-forward, manual, ours, theirs)", strategy)
	}
	fromHead, err := store.HeadHashByBranch(kind, artifactID, from)
	if err != nil {
		return err
	}
	if fromHead == "" {
		return fmt.Errorf("source branch %q has no events", from)
	}
	intoHead, err := store.HeadHashByBranch(kind, artifactID, into)
	if err != nil {
		return err
	}
	// Mirror the source guard: merge combines two EXISTING branches (BRD-04
	// §5.4). A nonexistent destination would otherwise be conjured into a
	// ghost branch rooted at an orphan merge event with an empty parent hash.
	// main always exists.
	if into != acf.MainBranch && intoHead == "" {
		return fmt.Errorf("destination branch %q does not exist on artifact %s", into, artifactID)
	}
	events, err := store.ReadEvents(kind, artifactID)
	if err != nil {
		return err
	}
	var (
		fromEv *acf.Event
	)
	for i := range events {
		if events[i].Hash == fromHead {
			fromEv = &events[i]
		}
	}
	if fromEv == nil {
		return fmt.Errorf("source head event %s not found in log", fromHead)
	}

	if strategy == strategyFastForward {
		// Verify --into is a strict prefix of --from.
		if !isStrictPrefix(events, into, from) {
			return fmt.Errorf("fast-forward merge refused: branch %q is not a strict prefix of %q (use --strategy manual or theirs)",
				into, from)
		}
	}

	// Resolve the merge payload.
	var resolvedPayload json.RawMessage
	resolvedFormat := ""
	notes := mergeNotes
	switch strategy {
	case strategyFastForward, strategyTheirs:
		resolvedPayload = fromEv.Payload
	case strategyOurs:
		intoEv := findEventByHash(events, intoHead)
		if intoEv == nil {
			return fmt.Errorf("destination head event %s not found in log", intoHead)
		}
		resolvedPayload = intoEv.Payload
	case strategyManual:
		picked, msg, err := manualResolve(cmd, events, from, into)
		if err != nil {
			return err
		}
		resolvedPayload = picked.Payload
		if notes == "" {
			notes = msg
		}
	}
	_ = resolvedFormat

	merge := acf.Event{
		EventID:              uuid.NewString(),
		ArtifactID:           artifactID,
		Type:                 acf.EventTypeMergeOuter,
		Timestamp:            time.Now().UTC(),
		ParentHash:           intoHead,
		Branch:               into,
		MergeFromBranch:      from,
		MergeFromHash:        fromHead,
		MergeStrategy:        strategy,
		MergeResolutionNotes: notes,
		Payload:              resolvedPayload,
		Provenance: acf.Provenance{
			SourceAgent:    "aplexica-cli",
			AdapterVersion: "aplexica-cli",
		},
	}
	if err := store.AppendEvent(kind, merge); err != nil {
		return err
	}
	if _, err := store.RefreshBranchIndex(kind, artifactID); err != nil {
		return err
	}
	_ = journalBranchOp(mergeStoreRoot, "merge", map[string]any{
		"artifactId": artifactID,
		"from":       from,
		"into":       into,
		"strategy":   strategy,
		"fromHead":   fromHead,
		"intoHead":   intoHead,
	})
	if mergeJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"artifactId": artifactID,
			"from":       from,
			"into":       into,
			"strategy":   strategy,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "merged %s → %s on %s (strategy: %s)\n",
		from, into, artifactID, strategy)
	return nil
}

func findEventByHash(events []acf.Event, hash string) *acf.Event {
	for i := range events {
		if events[i].Hash == hash {
			return &events[i]
		}
	}
	return nil
}

// isStrictPrefix returns true when every event on `into` also appears
// on `from` in the same prefix order (i.e., `into` is git-style fast-
// forwardable into `from`).
func isStrictPrefix(events []acf.Event, into, from string) bool {
	var intoEv, fromEv []acf.Event
	for _, e := range events {
		b := e.Branch
		if b == "" {
			b = acf.MainBranch
		}
		switch b {
		case into:
			intoEv = append(intoEv, e)
		case from:
			fromEv = append(fromEv, e)
		}
	}
	if len(intoEv) > len(fromEv) {
		return false
	}
	for i := range intoEv {
		if intoEv[i].Hash != fromEv[i].Hash {
			return false
		}
	}
	return true
}

// manualResolve runs the interactive picker (or honours --accept-from /
// --accept-into for tests). Returns the picked event and a short notes
// string describing the resolution.
func manualResolve(cmd *cobra.Command, events []acf.Event, from, into string) (*acf.Event, string, error) {
	fromEvs := branchEvents(events, from)
	intoEvs := branchEvents(events, into)
	if len(fromEvs) == 0 {
		return nil, "", fmt.Errorf("branch %q empty", from)
	}
	if len(intoEvs) == 0 {
		return nil, "", fmt.Errorf("branch %q empty", into)
	}
	fromHead := &fromEvs[len(fromEvs)-1]
	intoHead := &intoEvs[len(intoEvs)-1]
	if mergeAccept != "" {
		switch mergeAccept {
		case "from":
			return fromHead, "accepted from-side via --accept", nil
		case "into":
			return intoHead, "accepted into-side via --accept", nil
		default:
			return nil, "", fmt.Errorf("--accept must be \"from\" or \"into\"")
		}
	}
	// Interactive prompt:
	fmt.Fprintln(cmd.OutOrStdout(), "Manual merge resolver — divergent heads:")
	fmt.Fprintf(cmd.OutOrStdout(), "  A) %s (from %s): %s\n", shortHashOf(fromHead.Hash), from,
		strings.TrimSpace(string(fromHead.Payload)))
	fmt.Fprintf(cmd.OutOrStdout(), "  B) %s (into %s): %s\n", shortHashOf(intoHead.Hash), into,
		strings.TrimSpace(string(intoHead.Payload)))
	fmt.Fprint(cmd.OutOrStdout(), "Pick [A/B]: ")
	sc := bufio.NewScanner(cmd.InOrStdin())
	pick := "A"
	if sc.Scan() {
		s := strings.ToUpper(strings.TrimSpace(sc.Text()))
		if s != "" {
			pick = s
		}
	}
	switch pick {
	case "A":
		return fromHead, "picked A (from-side) interactively", nil
	case "B":
		return intoHead, "picked B (into-side) interactively", nil
	default:
		return nil, "", fmt.Errorf("invalid pick %q", pick)
	}
}

func branchEvents(events []acf.Event, branch string) []acf.Event {
	var out []acf.Event
	for _, e := range events {
		b := e.Branch
		if b == "" {
			b = acf.MainBranch
		}
		if b == branch {
			out = append(out, e)
		}
	}
	return out
}

// resolveCmd is the top-level BRD-04 §5.6 alias: `aplexica resolve
// <artifact-id>` opens an interactive resolver for an artifact in
// conflict. Defers to `aplexica conflicts resolve` for the storage
// machinery so the two surfaces produce the same on-disk event.
var resolveTopStoreRoot string
var resolveTopConflictsRoot string
var resolveTopPick int

var resolveCmd = &cobra.Command{
	Use:   "resolve <artifact-id>",
	Short: "Interactive resolver for an artifact in conflict",
	Long: `Open the interactive resolver for an artifact currently marked in
conflict. Lists the divergent heads with previews; the user picks
which side becomes canonical. Records the resolution as an
EventTypeResolution event in the artifact's log.

This is an alias for ` + "`aplexica conflicts resolve --pick <index>`" + ` that
adds an interactive picker for the index when --pick is omitted.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Delegate to conflicts resolve flow with our flags forwarded.
		conflictsRoot = resolveTopConflictsRoot
		resolveStoreRoot = resolveTopStoreRoot
		resolvePickIdx = resolveTopPick
		// Pre-show the conflict so the user sees their choices.
		showCmd := conflictsShowCmd
		_ = showCmd.RunE(cmd, args)

		// BRD-04 §5.6: when --pick is omitted and stdin is an interactive
		// terminal, prompt the user to choose which head wins rather than
		// silently defaulting to head 0. Non-interactive/CI (piped stdin,
		// no TTY) keeps the index-0 / explicit --pick default.
		if !cmd.Flags().Changed("pick") {
			if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
				cs := &conflicts.Store{Root: resolveTopConflictsRoot}
				if c, err := cs.Get(args[0]); err == nil && len(c.Heads) > 1 {
					idx, perr := promptForHeadIndex(cmd, c.Heads)
					if perr != nil {
						return perr
					}
					resolvePickIdx = idx
				}
			}
		}
		return conflictsResolveCmd.RunE(cmd, args)
	},
}

// promptForHeadIndex lists the divergent heads and reads a 1-based
// selection from stdin, returning the chosen 0-based index. An empty
// line (or closed stdin) defaults to head 0. Used by `aplexica resolve`
// when --pick is omitted on an interactive terminal.
func promptForHeadIndex(cmd *cobra.Command, heads []conflicts.Head) (int, error) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Which head should become canonical?")
	for i, h := range heads {
		marker := "  "
		if i == 0 {
			marker = "> "
		}
		preview := h.PayloadPreview
		if preview == "" {
			preview = h.ContentSHA256
		}
		fmt.Fprintf(out, "%s%d) %s  %s\n", marker, i+1, h.SourceAgent, preview)
	}
	fmt.Fprint(out, "Pick [1]: ")
	sc := bufio.NewScanner(cmd.InOrStdin())
	if !sc.Scan() {
		return 0, nil
	}
	line := strings.TrimSpace(sc.Text())
	if line == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(line)
	if err != nil {
		return 0, fmt.Errorf("invalid selection %q", line)
	}
	if n < 1 || n > len(heads) {
		return 0, fmt.Errorf("selection %d out of range [1, %d]", n, len(heads))
	}
	return n - 1, nil
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")
	defaultConflicts := filepath.Join(home, ".aplexica", "conflicts")

	mergeCmd.Flags().StringVar(&mergeStoreRoot, "store", defaultStore, "Canonical store root")
	mergeCmd.Flags().StringVar(&mergeFrom, "from", "", "Source branch (events come from here)")
	mergeCmd.Flags().StringVar(&mergeInto, "into", "", "Destination branch (the merge event lives here)")
	mergeCmd.Flags().StringVar(&mergeStrategy, "strategy", strategyFastForward,
		"Merge strategy: fast-forward (default), manual, ours, theirs")
	mergeCmd.Flags().StringVar(&mergeNotes, "notes", "",
		"Free-text resolution notes recorded on the merge event")
	mergeCmd.Flags().StringVar(&mergeAccept, "accept", "",
		"Non-interactive resolver hint: \"from\" or \"into\" (testing / CI)")
	mergeCmd.Flags().BoolVar(&mergeJSON, "json", false, "Emit JSON instead of plain text")
	mergeCmd.Flags().BoolVar(&mergeNonInter, "non-interactive", false,
		"Disable confirmation/destination prompts (uses safe defaults)")
	rootCmd.AddCommand(mergeCmd)

	resolveCmd.Flags().StringVar(&resolveTopStoreRoot, "store", defaultStore, "Canonical store root")
	resolveCmd.Flags().StringVar(&resolveTopConflictsRoot, "conflicts-root", defaultConflicts,
		"Conflicts state root")
	resolveCmd.Flags().IntVar(&resolveTopPick, "pick", 0, "Index of the head to pick (0 = first)")
	rootCmd.AddCommand(resolveCmd)

	// silence unused
	sort.Strings(nil)
}
