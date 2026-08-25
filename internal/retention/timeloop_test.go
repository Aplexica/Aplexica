package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestTickTimeBasedSnapshots_SkipsLargeConversation(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	native := filepath.Join(t.TempDir(), "large.jsonl")
	require.NoError(t, os.WriteFile(native, nil, 0o600))
	require.NoError(t, os.Truncate(native, AutomaticConversationSnapshotByteLimit+1))

	id := acf.NewID()
	oldTime := time.Now().Add(-2 * time.Hour).UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Name: filepath.Base(native), SourcePath: native, CreatedAt: oldTime, UpdatedAt: oldTime,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: "test", Content: "small"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: oldTime, Payload: payload,
	}))

	snapped, err := TickTimeBasedSnapshots(context.Background(), store, map[acf.Kind]time.Duration{
		acf.KindConversation: time.Hour,
	})
	require.NoError(t, err)
	require.NotContains(t, snapped, id)
	latest, found, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, acf.EventType(acf.EventTypeCreate), latest.Type)
}

// TestRunTimeBasedSnapshotter_SnapshotsStaleArtifacts pins the BRD-03 §4.8.1
// time-based contract end-to-end:
//
//  1. An artifact whose latest event is older than the per-kind maxAge IS
//     snapshotted on the first tick (returned by TickTimeBasedSnapshots).
//  2. A second tick over the SAME store does NOT re-snapshot, because the
//     latest event after tick 1 is the snapshot itself — there's nothing
//     new to encode and the time-based path explicitly skips those.
//
// The second-tick assertion is the load-bearing one. Without the
// "latest event is not a snapshot" gate in TickTimeBasedSnapshots, an
// idle artifact would accumulate one snapshot per tick forever (every
// snapshot is itself older-than-threshold the next tick, until something
// non-snapshot lands). The test would catch that regression.
func TestRunTimeBasedSnapshotter_SnapshotsStaleArtifacts(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	// Create an artifact whose latest event is older than maxAge.
	id := acf.NewID()
	oldTime := time.Now().Add(-2 * time.Hour).UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Name: "old", CreatedAt: oldTime, UpdatedAt: oldTime,
	}))
	payload, _ := acf.EncodePayload(acf.ConversationPayload{Format: "test", Content: `{"t":"x"}` + "\n"})
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: oldTime, Payload: payload,
	}))

	maxAge := map[acf.Kind]time.Duration{
		acf.KindConversation: 1 * time.Hour,
	}

	// First tick: stale artifact gets snapshotted.
	snapped, err := TickTimeBasedSnapshots(context.Background(), store, maxAge)
	require.NoError(t, err)
	require.Contains(t, snapped, id, "stale conversation should be snapshotted on first tick")

	// Confirm the snapshot landed as the head event.
	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 2, "create + snapshot")
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), events[len(events)-1].Type)

	// Second tick: latest event IS the snapshot — must NOT re-snapshot.
	snapped2, err := TickTimeBasedSnapshots(context.Background(), store, maxAge)
	require.NoError(t, err)
	require.NotContains(t, snapped2, id, "should skip artifacts whose latest event is already a snapshot")

	// And no new event landed.
	eventsAfter, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, eventsAfter, 2, "second tick must be a no-op for the already-snapshotted artifact")
}

// TestRunner_SetMaxAge_Live exercises the v0.34.0 hot-reload contract:
// NewRunner takes a defensive copy of the initial map, SetMaxAge atomically
// replaces it, and MaxAge returns a defensive copy of the current state.
// Mutations to the caller's map must NOT affect the Runner's internal map.
func TestRunner_SetMaxAge_Live(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	initial := map[acf.Kind]time.Duration{acf.KindConversation: 1 * time.Hour}
	r := NewRunner(store, initial)
	require.Equal(t, 1*time.Hour, r.MaxAge()[acf.KindConversation])

	// Mutating the caller's map MUST NOT affect the Runner (defensive copy).
	initial[acf.KindConversation] = 99 * time.Hour
	require.Equal(t, 1*time.Hour, r.MaxAge()[acf.KindConversation], "Runner must hold a defensive copy")

	// Hot-reload: SetMaxAge replaces the map atomically.
	r.SetMaxAge(map[acf.Kind]time.Duration{
		acf.KindConversation: 24 * time.Hour,
		acf.KindMemory:       7 * 24 * time.Hour,
	})
	got := r.MaxAge()
	require.Equal(t, 24*time.Hour, got[acf.KindConversation])
	require.Equal(t, 7*24*time.Hour, got[acf.KindMemory])

	// MaxAge must return a defensive copy too.
	got[acf.KindConversation] = 999 * time.Hour
	require.Equal(t, 24*time.Hour, r.MaxAge()[acf.KindConversation], "MaxAge must return a defensive copy")
}
