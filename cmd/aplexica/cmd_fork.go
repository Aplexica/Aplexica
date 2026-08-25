package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	forkStoreRoot     string
	forkFromEvent     string
	forkToAgent       string
	forkBranch        string
	forkRationale     string
	forkNoMaterialize bool
)

var forkCmd = &cobra.Command{
	Use:   "fork <artifact-id>",
	Short: "Fork an artifact at a chosen event into a new branch",
	Long: `Create a new branch starting at the chosen event. Records a fork event
in the new branch's log referencing the parent event and the originating
agent. Updates the target agent's materialization pointer to the new
branch; the source agent's pointer is unchanged.

If --branch is omitted, a name is derived from the parent event ID
suffix and the target agent name.`,
	Args: cobra.ExactArgs(1),
	RunE: runFork,
}

func runFork(cmd *cobra.Command, args []string) error {
	artifactID := args[0]
	if forkFromEvent == "" {
		return fmt.Errorf("--from <eventId|hash> is required")
	}
	if forkToAgent == "" {
		return fmt.Errorf("--to-agent <name> is required")
	}
	store := &acf.Store{Root: forkStoreRoot}
	if err := store.Init(); err != nil {
		return err
	}
	kind, artifact, found, err := findArtifactByID(store, artifactID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("artifact %s not found in any kind", artifactID)
	}
	events, err := store.ReadEvents(kind, artifactID)
	if err != nil {
		return err
	}
	var parent *acf.Event
	for i := range events {
		if events[i].EventID == forkFromEvent || events[i].Hash == forkFromEvent {
			parent = &events[i]
			break
		}
	}
	if parent == nil {
		return fmt.Errorf("event %q not found in artifact %s", forkFromEvent, artifactID)
	}
	branchName := forkBranch
	if branchName == "" {
		shortHash := parent.Hash
		if len(shortHash) > shortHashLen {
			shortHash = shortHash[:shortHashLen]
		}
		branchName = fmt.Sprintf("%s-%s", shortHash, forkToAgent)
	}
	normalized, err := acf.NormalizeBranchName(branchName)
	if err != nil {
		return err
	}
	srcBranch := parent.Branch
	if srcBranch == "" {
		srcBranch = acf.MainBranch
	}
	originAgent := parent.Provenance.SourceAgent
	if originAgent == "" {
		originAgent = "aplexica-cli"
	}
	event := acf.Event{
		EventID:          uuid.NewString(),
		ArtifactID:       artifactID,
		Type:             acf.EventTypeForkOuter,
		Timestamp:        time.Now().UTC(),
		ParentHash:       parent.Hash,
		Branch:           normalized,
		ForkSourceBranch: srcBranch,
		ForkFromEventID:  parent.EventID,
		ForkOriginAgent:  originAgent,
		ForkRationale:    forkRationale,
		Provenance: acf.Provenance{
			SourceAgent:    "aplexica-cli",
			AdapterVersion: "aplexica-cli",
		},
	}
	if err := store.AppendEvent(kind, event); err != nil {
		return err
	}

	// Update the per-agent materialization pointer for the target agent
	// so subsequent `aplexica checkout` / orchestrator fan-out knows the
	// agent is on the new branch.
	updated, err := store.ReadArtifact(kind, artifactID)
	if err != nil {
		return err
	}
	if updated.MaterializedBranchByAgent == nil {
		updated.MaterializedBranchByAgent = map[string]string{}
	}
	updated.MaterializedBranchByAgent[forkToAgent] = normalized
	if err := store.WriteArtifact(updated); err != nil {
		return err
	}

	// Refresh the branch index so the new branch appears in `branch list`.
	if _, err := store.RefreshBranchIndex(kind, artifactID); err != nil {
		return err
	}

	if err := journalBranchOp(forkStoreRoot, "fork", map[string]any{
		"artifactId": artifactID,
		"kind":       string(kind),
		"branch":     normalized,
		"from":       parent.Hash,
		"toAgent":    forkToAgent,
		"rationale":  forkRationale,
	}); err != nil {
		// non-fatal — operation already committed
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: branch-ops journal write failed:", err)
	}

	_ = artifact // future: surface artifact name in output
	fmt.Fprintf(cmd.OutOrStdout(),
		"forked artifact %s at event %s onto branch %q (target agent %q)\n",
		artifactID, parent.EventID, normalized, forkToAgent)
	if kind == acf.KindConversation && !forkNoMaterialize {
		notifyDaemonMaterializeConversation(cmd, artifactID, forkToAgent, normalized)
	}
	return nil
}

func init() {
	home, _ := os.UserHomeDir()
	forkCmd.Flags().StringVar(&forkStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"), "Canonical store root")
	forkCmd.Flags().StringVar(&forkFromEvent, "from", "",
		"Event ID or hash on the source branch to fork from (required)")
	forkCmd.Flags().StringVar(&forkToAgent, "to-agent", "",
		"Target agent name the fork is created for (required)")
	forkCmd.Flags().StringVar(&forkBranch, "branch", "",
		"Branch name (default: <short-event-id>-<target-agent>)")
	forkCmd.Flags().StringVar(&forkRationale, "rationale", "",
		"Optional free-text rationale recorded with the fork event")
	forkCmd.Flags().BoolVar(&forkNoMaterialize, "no-materialize", false,
		"Create the branch and pointer without asking the running daemon to materialize it immediately")
	rootCmd.AddCommand(forkCmd)
}

// findArtifactByID probes every artifact kind until it finds an artifact
// with the given ID, returning (kind, artifact, true, nil) on success or
// (_, _, false, nil) when no kind has the artifact.
func findArtifactByID(store *acf.Store, id string) (acf.Kind, acf.Artifact, bool, error) {
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		a, err := store.ReadArtifact(k, id)
		if err == nil {
			return k, a, true, nil
		}
		if !strings.Contains(err.Error(), "no such file") &&
			!strings.Contains(err.Error(), "cannot find the file") {
			// Real I/O errors are surfaced; ENOENT is "try next kind".
			continue
		}
	}
	return "", acf.Artifact{}, false, nil
}
