package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	branchStoreRoot       string
	branchListJSON        bool
	branchListIncludeArch bool
	branchCreateFrom      string
)

var branchCmd = &cobra.Command{
	Use:   "branch",
	Short: "Branch lifecycle commands",
}

var branchListCmd = &cobra.Command{
	Use:   "list <artifact-id>",
	Short: "List branches for an artifact",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: branchStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		kind, art, found, err := findArtifactByID(store, args[0])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("artifact %s not found in any kind", args[0])
		}
		branches, err := store.ListBranches(kind, args[0], branchListIncludeArch)
		if err != nil {
			return err
		}
		if branchListJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(branches)
		}
		w := cmd.OutOrStdout()
		materialized := materializedAgentsByBranch(art)
		fmt.Fprintf(w, "%-32s  %-8s  %-24s  %-16s  %s\n", "BRANCH", "EVENTS", "LAST EVENT", "STATE", "MATERIALIZED IN")
		for _, b := range branches {
			state := "active"
			if b.Archived {
				state = "archived"
			} else if b.MergedInto != "" {
				state = "merged→" + b.MergedInto
			}
			ts := ""
			if !b.LastEventAt.IsZero() {
				ts = b.LastEventAt.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%-32s  %-8d  %-24s  %-16s  %s\n", b.Name, b.EventCount, ts, state, strings.Join(materialized[b.Name], ","))
		}
		return nil
	},
}

func materializedAgentsByBranch(art acf.Artifact) map[string][]string {
	out := map[string][]string{}
	for agent, branch := range art.MaterializedBranchByAgent {
		norm, err := acf.NormalizeBranchName(branch)
		if err != nil {
			continue
		}
		out[norm] = append(out[norm], agent)
	}
	for branch := range out {
		sort.Strings(out[branch])
	}
	return out
}

var branchCreateCmd = &cobra.Command{
	Use:   "create <artifact-id> <branch-name>",
	Short: "Create a new branch starting at an event (equivalent to `fork` without an agent target)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if branchCreateFrom == "" {
			return fmt.Errorf("--from <eventId|hash> is required")
		}
		store := &acf.Store{Root: branchStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		artifactID, name := args[0], args[1]
		normalized, err := acf.NormalizeBranchName(name)
		if err != nil {
			return err
		}
		kind, _, found, err := findArtifactByID(store, artifactID)
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
			if events[i].EventID == branchCreateFrom || events[i].Hash == branchCreateFrom {
				parent = &events[i]
				break
			}
		}
		if parent == nil {
			return fmt.Errorf("event %q not found in artifact %s", branchCreateFrom, artifactID)
		}
		srcBranch := parent.Branch
		if srcBranch == "" {
			srcBranch = acf.MainBranch
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
			Provenance: acf.Provenance{
				SourceAgent:    "aplexica-cli",
				AdapterVersion: "aplexica-cli",
			},
		}
		if err := store.AppendEvent(kind, event); err != nil {
			return err
		}
		if _, err := store.RefreshBranchIndex(kind, artifactID); err != nil {
			return err
		}
		_ = journalBranchOp(branchStoreRoot, "branch-create", map[string]any{
			"artifactId": artifactID,
			"branch":     normalized,
			"from":       parent.Hash,
		})
		fmt.Fprintf(cmd.OutOrStdout(), "created branch %q at event %s\n", normalized, parent.EventID)
		return nil
	},
}

var branchRenameCmd = &cobra.Command{
	Use:   "rename <artifact-id> <old> <new>",
	Short: "Rename an existing branch (recorded as an index alias; event history is preserved)",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: branchStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		artifactID, oldName, newNameRaw := args[0], args[1], args[2]
		if oldName == acf.MainBranch {
			return fmt.Errorf("renaming the main branch is not allowed")
		}
		newName, err := acf.NormalizeBranchName(newNameRaw)
		if err != nil {
			return err
		}
		kind, _, found, err := findArtifactByID(store, artifactID)
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
		info, ok := bi.Branches[oldName]
		if !ok {
			return fmt.Errorf("branch %q does not exist on artifact %s", oldName, artifactID)
		}
		if _, exists := bi.Branches[newName]; exists {
			return fmt.Errorf("branch %q already exists on artifact %s", newName, artifactID)
		}
		// Hash-chain integrity: rename does NOT rewrite Event.Branch (branch
		// names are part of the hashed event payload). Instead we persist a
		// rename alias from the immutable event-log name to the new display
		// name; RefreshBranchIndex re-applies it on every rebuild, so the
		// rename survives and sticky metadata (Archived/Tags) follows it.
		orig := oldName
		for evName, disp := range bi.Renames {
			if disp == oldName {
				orig = evName
				break
			}
		}
		if bi.Renames == nil {
			bi.Renames = map[string]string{}
		}
		if orig == newName {
			delete(bi.Renames, orig) // renamed back to its original event-log name
		} else {
			bi.Renames[orig] = newName
		}
		info.Name = newName
		delete(bi.Branches, oldName)
		bi.Branches[newName] = info
		if err := store.WriteBranchIndex(bi); err != nil {
			return err
		}
		_ = journalBranchOp(branchStoreRoot, "branch-rename", map[string]any{
			"artifactId": artifactID,
			"oldName":    oldName,
			"newName":    newName,
		})
		fmt.Fprintf(cmd.OutOrStdout(),
			"renamed branch %q → %q (index only; event payload Branch unchanged for hash-chain integrity)\n",
			oldName, newName)
		return nil
	},
}

var branchArchiveCmd = &cobra.Command{
	Use:   "archive <artifact-id> <branch-name>",
	Short: "Archive a branch",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setBranchArchived(cmd, args[0], args[1], true, "user")
	},
}

var branchUnarchiveCmd = &cobra.Command{
	Use:   "unarchive <artifact-id> <branch-name>",
	Short: "Revive an archived branch",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setBranchArchived(cmd, args[0], args[1], false, "user")
	},
}

func setBranchArchived(cmd *cobra.Command, artifactID, name string, archive bool, reason string) error {
	store := &acf.Store{Root: branchStoreRoot}
	if err := store.Init(); err != nil {
		return err
	}
	if name == acf.MainBranch {
		return fmt.Errorf("the main branch cannot be archived")
	}
	kind, _, found, err := findArtifactByID(store, artifactID)
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
	info, ok := bi.Branches[name]
	if !ok {
		return fmt.Errorf("branch %q does not exist on artifact %s", name, artifactID)
	}
	if archive {
		info.Archived = true
		info.ArchivedAt = time.Now().UTC()
		info.ArchiveReason = reason
	} else {
		info.Archived = false
		info.ArchivedAt = time.Time{}
		info.ArchiveReason = ""
	}
	if err := store.WriteBranchIndex(bi); err != nil {
		return err
	}
	op := "branch-archive"
	if !archive {
		op = "branch-unarchive"
	}
	_ = journalBranchOp(branchStoreRoot, op, map[string]any{
		"artifactId": artifactID,
		"branch":     name,
		"reason":     reason,
	})
	verb := "archived"
	if !archive {
		verb = "unarchived"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s branch %q on artifact %s\n", verb, name, artifactID)
	return nil
}

var branchDeleteCmd = &cobra.Command{
	Use:   "delete <artifact-id> <branch-name>",
	Short: "Delete an archived branch (only allowed after archive; events stay in the log)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		store := &acf.Store{Root: branchStoreRoot}
		if err := store.Init(); err != nil {
			return err
		}
		artifactID, name := args[0], args[1]
		if name == acf.MainBranch {
			return fmt.Errorf("the main branch cannot be deleted")
		}
		kind, _, found, err := findArtifactByID(store, artifactID)
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
		info, ok := bi.Branches[name]
		if !ok {
			return fmt.Errorf("branch %q does not exist on artifact %s", name, artifactID)
		}
		if !info.Archived {
			return fmt.Errorf("branch %q must be archived before delete; run `aplexica branch archive %s %s` first",
				name, artifactID, name)
		}
		delete(bi.Branches, name)
		if err := store.WriteBranchIndex(bi); err != nil {
			return err
		}
		_ = journalBranchOp(branchStoreRoot, "branch-delete", map[string]any{
			"artifactId": artifactID,
			"branch":     name,
		})
		fmt.Fprintf(cmd.OutOrStdout(),
			"deleted branch metadata for %q (event log retained for forensics)\n", name)
		return nil
	},
}

// listBranchesAcrossArtifacts is a small helper for branch list --all (not
// yet wired but kept here for future use); silences unused-import warnings
// when the optimized binary is built.
func listBranchesAcrossArtifacts(store *acf.Store) ([]string, error) {
	var out []string
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			return nil, err
		}
		for _, a := range arts {
			out = append(out, fmt.Sprintf("%s/%s", k, a.ArtifactID))
		}
	}
	sort.Strings(out)
	return out, nil
}

func init() {
	home, _ := os.UserHomeDir()
	defaultStore := filepath.Join(home, ".aplexica", "store")

	for _, c := range []*cobra.Command{branchListCmd, branchCreateCmd, branchRenameCmd, branchArchiveCmd, branchUnarchiveCmd, branchDeleteCmd} {
		c.Flags().StringVar(&branchStoreRoot, "store", defaultStore, "Canonical store root")
	}
	branchListCmd.Flags().BoolVar(&branchListJSON, "json", false, "Emit JSON instead of table")
	branchListCmd.Flags().BoolVar(&branchListIncludeArch, "include-archived", false,
		"Also list branches that have been archived (default hides them)")
	branchCreateCmd.Flags().StringVar(&branchCreateFrom, "from", "",
		"Event ID or hash on the source branch to create from (required)")

	branchCmd.AddCommand(branchListCmd, branchCreateCmd, branchRenameCmd,
		branchArchiveCmd, branchUnarchiveCmd, branchDeleteCmd)
	rootCmd.AddCommand(branchCmd)

	// Suppress unused-warning for the helper kept for future use.
	_ = listBranchesAcrossArtifacts
	_ = strings.HasPrefix
}
