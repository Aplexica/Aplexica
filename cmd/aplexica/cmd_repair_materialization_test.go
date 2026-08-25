package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// writeDeferredJournal seeds the store's retry journal directly. The wire
// shape is the daemon's; writing it literally here keeps this test honest
// about the format an operator's machine actually has on disk.
func writeDeferredJournal(t *testing.T, storeRoot string) {
	t.Helper()
	journal := map[string]any{
		"version": 2,
		"queues": []map[string]any{{
			"target":            "claude-code",
			"conversationsOnly": true,
			"entries": []map[string]any{{
				"artifactId":    "artifact-pending",
				"originAgent":   "codex",
				"attempts":      12,
				"firstDeferred": "2026-07-25T09:00:00Z",
				"lastError":     "syncd: inbound native materialization incomplete",
			}},
			"abandoned": []map[string]any{{
				"artifactId":  "artifact-stuck",
				"originAgent": "codex",
				"attempts":    64,
				"abandonedAt": "2026-07-26T09:00:00Z",
				"lastError":   "syncd: inbound native materialization incomplete",
			}},
		}},
	}
	raw, err := json.Marshal(journal)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(storeRoot, ".deferred-materialization-dirty.json"), raw, 0o600))
}

func newDeferredJournalStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	writeDeferredJournal(t, store.Root)
	return store.Root
}

type repairMaterializationOpts struct {
	storeRoot  string
	stateDir   string
	agent      string
	artifactID string
	drop       bool
	asJSON     bool
}

// runRepairMaterializationCmd drives the command's RunE against a throwaway
// cobra.Command rather than the shared root, so one test's flags cannot leak
// into another's.
func runRepairMaterializationCmd(t *testing.T, opts repairMaterializationOpts) string {
	t.Helper()
	saved := [...]any{
		repairMaterializationStore, repairMaterializationStateDir, repairMaterializationAgent,
		repairMaterializationArtifact, repairMaterializationDrop, repairMaterializationJSON,
	}
	t.Cleanup(func() {
		repairMaterializationStore = saved[0].(string)
		repairMaterializationStateDir = saved[1].(string)
		repairMaterializationAgent = saved[2].(string)
		repairMaterializationArtifact = saved[3].(string)
		repairMaterializationDrop = saved[4].(bool)
		repairMaterializationJSON = saved[5].(bool)
	})
	repairMaterializationStore = opts.storeRoot
	repairMaterializationStateDir = opts.stateDir
	repairMaterializationAgent = opts.agent
	repairMaterializationArtifact = opts.artifactID
	repairMaterializationDrop = opts.drop
	repairMaterializationJSON = opts.asJSON

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	require.NoError(t, runRepairMaterialization(cmd, nil))
	return buf.String()
}

// With no daemon socket the command falls back to the on-disk journal, which
// is how an operator inspects a machine whose daemon is stopped.
func TestRepairMaterialization_ListsJournalWhenDaemonIsDown(t *testing.T) {
	storeRoot := newDeferredJournalStore(t)
	out := runRepairMaterializationCmd(t, repairMaterializationOpts{
		storeRoot: storeRoot, stateDir: filepath.Join(t.TempDir(), "no-daemon"),
	})

	require.Contains(t, out, "daemon not running")
	require.Contains(t, out, "artifact-pending")
	require.Contains(t, out, "attempts=12")
	require.Contains(t, out, "artifact-stuck")
	require.Contains(t, out, "abandoned")
	// The summary no longer tells everyone to "repair the canonical head, then
	// re-run with --drop": that resolves one class and forfeits the write for
	// every other. Each row now carries its own explanation and, when one
	// exists, its own command.
	require.Contains(t, out, "stopped being retried")
	require.Contains(t, out, "explain")
}

func TestRepairMaterialization_FiltersByArtifact(t *testing.T) {
	storeRoot := newDeferredJournalStore(t)
	out := runRepairMaterializationCmd(t, repairMaterializationOpts{
		storeRoot: storeRoot, stateDir: filepath.Join(t.TempDir(), "no-daemon"),
		artifactID: "artifact-stuck",
	})

	require.Contains(t, out, "artifact-stuck")
	require.NotContains(t, out, "artifact-pending")
}

func TestRepairMaterialization_DropRewritesJournal(t *testing.T) {
	storeRoot := newDeferredJournalStore(t)
	stateDir := filepath.Join(t.TempDir(), "no-daemon")

	out := runRepairMaterializationCmd(t, repairMaterializationOpts{
		storeRoot: storeRoot, stateDir: stateDir, drop: true, artifactID: "artifact-stuck",
	})
	require.Contains(t, out, "Dropped 1 deferred materialization entry")

	out = runRepairMaterializationCmd(t, repairMaterializationOpts{
		storeRoot: storeRoot, stateDir: stateDir, asJSON: true,
	})
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 1)
	require.Equal(t, "artifact-pending", rows[0]["artifactId"])
	require.Equal(t, "pending", rows[0]["state"])
}

func TestRepairMaterialization_EmptyJournalReportsNothingStuck(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	out := runRepairMaterializationCmd(t, repairMaterializationOpts{
		storeRoot: store.Root, stateDir: filepath.Join(root, "no-daemon"),
	})
	require.Contains(t, out, "No deferred native materializations")
}
