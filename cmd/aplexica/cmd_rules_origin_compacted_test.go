package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

// seedMemoryArtifactWithTwoSources builds a memory artifact whose GENESIS
// create event came from `genesisSource` (the TRUE origin) and whose later
// update came from a DIFFERENT `laterSource`. Returns the artifact id.
//
// Distinct timestamps are required so ReadEventsIncludingCompacted's
// timestamp sort restores the genesis create as events[0].
func seedMemoryArtifactWithTwoSources(t *testing.T, storeRoot, genesisSource, laterSource string) string {
	t.Helper()
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	id := acf.NewID()
	base := time.Now().UTC().Add(-1 * time.Hour)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "AGENTS.md",
		CreatedAt:        base,
		UpdatedAt:        base,
	}))

	// Genesis create — the real origin.
	p0, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "# genesis\n"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  base,
		Provenance: acf.Provenance{DeviceID: "device-X", SourceAgent: genesisSource, AdapterVersion: "0.0.0"},
		Payload:    p0,
		ParentHash: "",
	}))

	// Later update from a different agent — the event that SURVIVES compaction.
	p1, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "# updated\n"})
	require.NoError(t, err)
	head, err := store.HeadHash(acf.KindMemory, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  base.Add(1 * time.Second),
		Provenance: acf.Provenance{DeviceID: "device-Y", SourceAgent: laterSource, AdapterVersion: "0.0.0"},
		Payload:    p1,
		ParentHash: head,
	}))
	return id
}

// compactGenesisIntoCompactedLayer runs the REAL retention snapshot+prune
// path so the genesis create event is moved out of the active log into
// <store>/events/.compacted/<kind>/<id>.jsonl.gz. After this the active log
// is just the snapshot event (which carries an EMPTY provenance), so a
// ReadEvents()[0]-based origin reconstruction sees no SourceAgent, while
// ReadEventsIncludingCompacted()[0] still returns the genesis create.
func compactGenesisIntoCompactedLayer(t *testing.T, storeRoot, id string) {
	t.Helper()
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	_, err := retention.CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)

	// Grace deadline well in the past so the freshly-written compacted file
	// (mtime ~now) is NOT grace-deleted — it stays on disk for the reconstruction.
	graceDeadline := time.Now().UTC().Add(-24 * time.Hour)
	moved, deleted, err := retention.PruneArtifact(context.Background(), store, acf.KindMemory, id, graceDeadline)
	require.NoError(t, err)
	require.Equal(t, 2, moved, "genesis create + later update moved into .compacted")
	require.Equal(t, 0, deleted, "compacted file must survive (mtime newer than grace deadline)")

	// Sanity: the active log is now snapshot-only, and its provenance is empty —
	// this is exactly why a ReadEvents-based origin lookup drifts.
	active, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)
	require.Empty(t, active[0].Provenance.SourceAgent,
		"snapshot event carries no source agent — the drift the fix must overcome")

	// And the compacted layer still genuinely holds the true genesis origin first.
	all, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(all), 1)
	require.Equal(t, "claude-code", all[0].Provenance.SourceAgent,
		"genesis create (true origin) is the timestamp-first event including compacted")
}

// TestRules_TestResolvesOriginAcrossCompaction is the regression test for the
// origin-reconstruction bug (FR-05.6 determinism vs live fan-out): after
// retention compacts an artifact's genesis create event into the .compacted
// layer, `rules test` must STILL reconstruct the true origin agent from the
// genesis create — not the oldest SURVIVING event (the snapshot, whose
// provenance is empty). We assert both the reconstructed origin field and the
// behavioural consequence: a match.agentSource = ["claude-code"] rule fires.
func TestRules_TestResolvesOriginAcrossCompaction(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	userRules := filepath.Join(tmp, "rules.toml")

	// origin = claude-code ("X"); a later update from codex survives compaction.
	id := seedMemoryArtifactWithTwoSources(t, storeRoot, "claude-code", "codex")
	compactGenesisIntoCompactedLayer(t, storeRoot, id)

	// A rule keyed on the ORIGIN agent. It only matches when origin
	// reconstruction yields "claude-code".
	require.NoError(t, os.WriteFile(userRules, []byte(`
[[sync.rules]]
name = "origin-claude-tagger"
match.kind = "any"
match.agentSource = ["claude-code"]
assign.tags = ["from-claude-origin"]
mode = "live"
`), 0o644))

	out, err := runRulesCmd(t, "test", id, "--rules-file", userRules, "--store", storeRoot, "--json")
	require.NoError(t, err, "test output:\n%s", out)

	var parsed struct {
		Artifact struct {
			OriginAgent  string `json:"originAgent"`
			OriginDevice string `json:"originDevice"`
		} `json:"artifact"`
		Decision struct {
			MatchedRules []string `json:"matchedRules"`
			AssignedTags []string `json:"assignedTags"`
		} `json:"decision"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &parsed), "raw output:\n%s", out)

	require.Equal(t, "claude-code", parsed.Artifact.OriginAgent,
		"origin agent must reconstruct to the genesis create's source even after it was compacted; got %q\nout:\n%s",
		parsed.Artifact.OriginAgent, out)
	require.Equal(t, "device-X", parsed.Artifact.OriginDevice,
		"origin device must likewise come from the genesis create event")
	require.Contains(t, parsed.Decision.MatchedRules, "origin-claude-tagger",
		"match.agentSource rule should fire on the reconstructed origin; out:\n%s", out)
	require.Contains(t, parsed.Decision.AssignedTags, "from-claude-origin",
		"the origin-keyed tag should be assigned; out:\n%s", out)
}
