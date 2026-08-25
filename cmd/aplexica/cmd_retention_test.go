package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

// runRetentionCmd invokes `aplexica retention …` via rootCmd and returns the
// combined output. --user-path is injected for the set/show subcommands so the
// real ~/.aplexica/config.toml is never touched.
func runRetentionCmd(t *testing.T, userPath string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	full := append([]string{"retention", "--user-path", userPath}, args...)
	rootCmd.SetArgs(full)
	t.Cleanup(func() {
		configUserPath = ""
		configSystemPath = ""
		configProjectPath = ""
		retentionStoreRoot = ""
		retentionRestoreArtifact = ""
		retentionRestoreOut = ""
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// TestRetentionShow prints the effective retention config (with provenance).
func TestRetentionShow(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	out, err := runRetentionCmd(t, userPath, "show")
	require.NoError(t, err)
	// Spot-check a couple of retention keys appear with provenance.
	require.Contains(t, out, "retention.store_high_watermark")
	require.Contains(t, out, "retention.pin_tags")
	require.Contains(t, out, "shipped")
}

// TestRetentionSet_ValidatesAndPersists: an out-of-range value is rejected by
// the schema; a valid value is persisted to the user layer (and then visible
// via show).
func TestRetentionSet_ValidatesAndPersists(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")

	// store_high_watermark is bounded to [0,1]; 1.5 must be rejected.
	_, err := runRetentionCmd(t, userPath, "set", "retention.store_high_watermark", "1.5")
	require.Error(t, err, "out-of-range value must be rejected by the schema")

	// A valid value persists.
	out, err := runRetentionCmd(t, userPath, "set", "retention.store_high_watermark", "0.5")
	require.NoError(t, err)
	require.Contains(t, out, "0.5")

	// And surfaces via show with user provenance.
	out, err = runRetentionCmd(t, userPath, "show")
	require.NoError(t, err)
	require.Contains(t, out, "0.5")
	require.Contains(t, out, "user")
}

// TestRetentionSet_RejectsNonRetentionKey: only retention.* keys are accepted.
func TestRetentionSet_RejectsNonRetentionKey(t *testing.T) {
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	_, err := runRetentionCmd(t, userPath, "set", "daemon.project_scan_interval", "5m")
	require.Error(t, err, "retention set must only accept retention.* keys")
}

// seedCompactedStore builds an artifact, snapshots it, prunes it so a
// pre-snapshot event lands in the .compacted layer, and returns the store
// root, artifact id, and the EventID of a compacted (moved) event.
func seedCompactedStore(t *testing.T) (root, id, compactedEventID string) {
	t.Helper()
	root = t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id = acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "m", CreatedAt: now, UpdatedAt: now,
	}))
	p0, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	genesisID := acf.NewID()
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: genesisID, ArtifactID: id, Type: acf.EventTypeCreate,
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

	// Prune (grace deadline in the past → keep the compacted file).
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindMemory, id, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Greater(t, moved, 0)

	// The genesis event was moved into .compacted.
	merged, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	active, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	activeIDs := map[string]bool{}
	for _, e := range active {
		activeIDs[e.EventID] = true
	}
	for _, e := range merged {
		if !activeIDs[e.EventID] {
			compactedEventID = e.EventID
			break
		}
	}
	require.NotEmpty(t, compactedEventID, "expected at least one compacted-only event")
	return root, id, compactedEventID
}

// TestRetentionRestore_ExtractsCompactedEvent: a pruned/compacted event is
// decoded back to output (extraction for inspection/recovery).
func TestRetentionRestore_ExtractsCompactedEvent(t *testing.T) {
	root, id, evtID := seedCompactedStore(t)
	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{
		"retention", "--user-path", userPath,
		"restore", "--store", root, "--artifact", id, evtID,
	})
	t.Cleanup(func() {
		retentionStoreRoot = ""
		retentionRestoreArtifact = ""
		retentionRestoreOut = ""
		configUserPath = ""
	})
	require.NoError(t, rootCmd.Execute())

	var got acf.Event
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Equal(t, evtID, got.EventID)
}

// TestRetentionRestore_NeverReChains: restore must NOT mutate the active log.
func TestRetentionRestore_NeverReChains(t *testing.T) {
	root, id, evtID := seedCompactedStore(t)
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	before, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)

	tmp := t.TempDir()
	userPath := filepath.Join(tmp, "user.toml")
	outPath := filepath.Join(tmp, "evt.json")

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{
		"retention", "--user-path", userPath,
		"restore", "--store", root, "--artifact", id, "--out", outPath, evtID,
	})
	t.Cleanup(func() {
		retentionStoreRoot = ""
		retentionRestoreArtifact = ""
		retentionRestoreOut = ""
		configUserPath = ""
	})
	require.NoError(t, rootCmd.Execute())

	after, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Equal(t, len(before), len(after), "restore must not append to the active log")
	for i := range before {
		require.Equal(t, before[i].EventID, after[i].EventID, "active chain must be byte-identical")
		require.Equal(t, before[i].Hash, after[i].Hash)
	}
	// The extracted event must NOT be in the active log.
	for _, e := range after {
		require.NotEqual(t, evtID, e.EventID, "restore must never re-chain into the active log")
	}
}
