package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

var (
	checkoutStoreRoot     string
	checkoutBranch        string
	checkoutAgent         string
	checkoutNoMaterialize bool
)

var checkoutCmd = &cobra.Command{
	Use:   "checkout <artifact-id>",
	Short: "Set an agent's materialization pointer to a branch",
	Long: `Update the per-agent materialization pointer recorded on the artifact
so the daemon's fan-out treats <agent> as currently viewing <branch>.

This command verifies the target branch exists in the artifact's branch
index before flipping the pointer. The actual on-disk native-format
rewrite is performed by the daemon's adapter at the next fan-out cycle.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if checkoutBranch == "" {
			return fmt.Errorf("--branch <name> is required")
		}
		if checkoutAgent == "" {
			return fmt.Errorf("--agent <name> is required")
		}
		normalized, err := acf.NormalizeBranchName(checkoutBranch)
		if err != nil {
			return err
		}
		store := &acf.Store{Root: checkoutStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		artifactID := args[0]
		kind, art, found, err := findArtifactByID(store, artifactID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("artifact %s not found in any kind", artifactID)
		}
		bi, err := store.RefreshBranchIndex(kind, artifactID)
		if err != nil {
			return err
		}
		info, ok := bi.Branches[normalized]
		if !ok {
			return fmt.Errorf("branch %q does not exist on artifact %s", normalized, artifactID)
		}
		if info.Archived {
			return fmt.Errorf("branch %q is archived; unarchive it first (`aplexica branch unarchive %s %s`)",
				normalized, artifactID, normalized)
		}
		if art.MaterializedBranchByAgent == nil {
			art.MaterializedBranchByAgent = map[string]string{}
		}
		previous := art.MaterializedBranchByAgent[checkoutAgent]
		art.MaterializedBranchByAgent[checkoutAgent] = normalized
		if err := store.WriteArtifact(art); err != nil {
			return err
		}
		_ = journalBranchOp(checkoutStoreRoot, "checkout", map[string]any{
			"artifactId": artifactID,
			"agent":      checkoutAgent,
			"branch":     normalized,
			"previous":   previous,
		})
		fmt.Fprintf(cmd.OutOrStdout(),
			"agent %q now materializes branch %q for artifact %s (was %q)\n",
			checkoutAgent, normalized, artifactID, previous)
		if kind == acf.KindConversation && !checkoutNoMaterialize {
			notifyDaemonMaterializeConversation(cmd, artifactID, checkoutAgent, normalized)
		}
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()
	checkoutCmd.Flags().StringVar(&checkoutStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"), "Canonical store root")
	checkoutCmd.Flags().StringVar(&checkoutBranch, "branch", "", "Target branch name (required)")
	checkoutCmd.Flags().StringVar(&checkoutAgent, "agent", "", "Agent name whose pointer to update (required)")
	checkoutCmd.Flags().BoolVar(&checkoutNoMaterialize, "no-materialize", false,
		"Update the branch pointer without asking the running daemon to materialize it immediately")
	rootCmd.AddCommand(checkoutCmd)
}
