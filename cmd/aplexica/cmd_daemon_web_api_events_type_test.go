package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
	"github.com/stretchr/testify/require"
)

// TestEventType_DerivationSharedHelper pins the fix for the global /events feed
// stamping EVERY record "artifact.synced". The global Backfill feed must
// classify each event the same way the agent-detail recentAgentEvents feed
// does: an internal retention snapshot is a checkpoint, an event whose
// provenance device differs from this device arrived over cross-device sync, and
// everything else is a local native import. Both feeds route through the shared
// deriveEventType helper so they cannot drift.
func TestEventType_DerivationSharedHelper(t *testing.T) {
	const localDev = "device-local"

	cases := []struct {
		name     string
		ev       acf.Event
		localDev string
		want     string
	}{
		{
			name:     "snapshot is a checkpoint",
			ev:       acf.Event{Type: acf.EventType(acf.EventTypeSnapshot), Provenance: acf.Provenance{DeviceID: localDev}},
			localDev: localDev,
			want:     "artifact.checkpoint",
		},
		{
			name:     "remote device is a sync",
			ev:       acf.Event{Type: acf.EventTypeUpdate, Provenance: acf.Provenance{DeviceID: "device-other"}},
			localDev: localDev,
			want:     "artifact.synced",
		},
		{
			name:     "local device is an import",
			ev:       acf.Event{Type: acf.EventTypeCreate, Provenance: acf.Provenance{DeviceID: localDev}},
			localDev: localDev,
			want:     "artifact.imported",
		},
		{
			name:     "unknown local device id is an import (no cross-device claim)",
			ev:       acf.Event{Type: acf.EventTypeCreate, Provenance: acf.Provenance{DeviceID: "device-other"}},
			localDev: "",
			want:     "artifact.imported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, deriveEventType(tc.ev, tc.localDev))
		})
	}
}

// TestEventsBackfill_DerivesTypePerEvent drives the bug end-to-end through
// Backfill with no orchestrator (localDev == ""): a retention snapshot must
// surface as "artifact.checkpoint" and a local native import as
// "artifact.imported" — NOT the blanket "artifact.synced" the pre-fix feed
// stamped on every record. The remote-device "artifact.synced" path needs a
// non-empty local device id and is covered by the shared-helper table above.
func TestEventsBackfill_DerivesTypePerEvent(t *testing.T) {
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	base := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

	// Artifact A: a local native import (genesis create event).
	importedID := acf.NewID()
	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: importedID, Kind: acf.KindMemory,
		Name: "imported", CreatedAt: base, UpdatedAt: base,
	}))
	p0, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: importedID, Type: acf.EventTypeCreate,
		Timestamp: base, Payload: p0, Provenance: acf.Provenance{SourceAgent: "claude-code"},
	}))

	// Artifact B: an internal retention checkpoint (snapshot event), newer so it
	// sorts to the head of the newest-first feed.
	snapID := acf.NewID()
	snapTS := base.Add(time.Hour)
	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: snapID, Kind: acf.KindMemory,
		Name: "checkpointed", CreatedAt: snapTS, UpdatedAt: snapTS,
	}))
	p1, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v1"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: snapID, Type: acf.EventType(acf.EventTypeSnapshot),
		Timestamp: snapTS, Payload: p1, SnapshotState: "sha256:deadbeef",
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
	}))

	acc := &eventsWebAccessor{deps: &webAPIDeps{store: s}}
	page, err := acc.Backfill(apiweb.EventQuery{Before: 0, Limit: 100})
	require.NoError(t, err)

	byName := map[string]string{}
	for _, e := range page.Events {
		byName[e.Name] = e.Type
	}
	require.Equal(t, "artifact.checkpoint", byName["checkpointed"],
		"a retention snapshot must surface as a checkpoint, not a sync")
	require.Equal(t, "artifact.imported", byName["imported"],
		"a local native import must surface as an import, not a sync")
}
