package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

// runGcCmd invokes `aplexica gc …` via rootCmd and returns combined output.
func runGcCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"gc"}, args...))
	t.Cleanup(func() {
		gcStoreRoot = ""
		gcDryRun = false
		gcForceLocalOnly = false
		gcJSON = false
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// hashTree fingerprints every regular file under root by content.
func hashTree(t *testing.T, root string) string {
	t.Helper()
	type entry struct {
		rel string
		sum string
	}
	var entries []entry
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		sum := sha256.Sum256(data)
		entries = append(entries, entry{rel: rel, sum: hex.EncodeToString(sum[:])})
		return nil
	}))
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e.rel))
		h.Write([]byte{0})
		h.Write([]byte(e.sum))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// seedGcStore stands up a store with a memory artifact carrying pre-snapshot
// history, so a dry-run has a history-compaction prune to report. The branch
// index is warmed so the "mutates nothing" check ignores the rebuildable cache.
func seedGcStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "m", CreatedAt: now, UpdatedAt: now,
	}))
	p0, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p0,
	}))
	for i := 1; i < 4; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, _ := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(i) * time.Second), Payload: p, ParentHash: head,
		}))
	}
	_, err := retention.CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	_, err = store.RefreshBranchIndex(acf.KindMemory, id)
	require.NoError(t, err)
	return root
}

// TestGcCmd_DryRun: `aplexica gc --dry-run` runs, prints a report, and mutates
// nothing. The report mentions the prune is blocked without --force-local-only.
func TestGcCmd_DryRun(t *testing.T) {
	root := seedGcStore(t)
	// Enable history compaction in the resolved config (the shipped default
	// keeps every snapshot — "all" — which disables pruning). An operator who
	// caps snapshots is exactly who would have a blocked prune to preview.
	t.Setenv("APLEXICA_RETENTION_KEEP_LAST_N_SNAPSHOTS", "0")
	before := hashTree(t, root)

	out, err := runGcCmd(t, "--store", root, "--dry-run")
	require.NoError(t, err, "gc --dry-run output:\n%s", out)
	require.Equal(t, before, hashTree(t, root), "dry-run must not mutate the store")
	require.Contains(t, out, retention.OpPruneBlocked, "the blocked prune is in the markdown report")
	require.Contains(t, out, "--force-local-only", "the report tells the operator how to unblock")
}

// TestGcCmd_DryRunJSON: --json emits a parseable GCReport with DryRun=true.
func TestGcCmd_DryRunJSON(t *testing.T) {
	root := seedGcStore(t)

	out, err := runGcCmd(t, "--store", root, "--dry-run", "--json")
	require.NoError(t, err, "gc --dry-run --json output:\n%s", out)

	var report retention.GCReport
	require.NoError(t, json.Unmarshal([]byte(out), &report), "output must be valid GCReport JSON:\n%s", out)
	require.True(t, report.DryRun)
}

// TestRetentionPreviewCmd_DryRun: `aplexica retention preview` is gc --dry-run
// under the hood — it prints a report and mutates nothing.
func TestRetentionPreviewCmd_DryRun(t *testing.T) {
	root := seedGcStore(t)
	t.Setenv("APLEXICA_RETENTION_KEEP_LAST_N_SNAPSHOTS", "0")
	before := hashTree(t, root)

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"retention", "preview", "--store", root})
	t.Cleanup(func() { retentionStoreRoot = "" })
	err := rootCmd.Execute()
	require.NoError(t, err, "retention preview output:\n%s", out.String())
	require.Equal(t, before, hashTree(t, root), "preview must not mutate the store")
	require.Contains(t, out.String(), retention.OpPruneBlocked)
}
