package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestAutomaticSnapshotAllowed_BoundsLargeConversationWithoutReadingIt(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	native := filepath.Join(t.TempDir(), "conversation.jsonl")
	require.NoError(t, os.WriteFile(native, []byte("small\n"), 0o600))

	id := acf.NewID()
	now := time.Now().UTC()
	artifact := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Name:             filepath.Base(native),
		SourcePath:       native,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, store.WriteArtifact(artifact))
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: "test", Content: "small"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
	}))
	require.True(t, AutomaticSnapshotAllowed(store, artifact))

	require.NoError(t, os.Truncate(native, AutomaticConversationSnapshotByteLimit+1))
	require.False(t, AutomaticSnapshotAllowed(store, artifact),
		"automatic retention must not materialize a large live transcript")
}

func TestCreateSnapshot_AppendsSnapshotEvent(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, store.WriteArtifact(art))
	payload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v1"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
	}))

	snap, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), snap.Type)
	require.NotEmpty(t, snap.SnapshotState)
	require.Equal(t, id, snap.ArtifactID)

	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), events[1].Type)
}

// TestCreateSnapshot_CarriesMaterializedPayload proves the FR-02.32 root fix:
// the snapshot event carries the latest materialized payload (not nil), the
// payload equals the most-recent create/update payload, SnapshotState is its
// content hash, and the full log (including the payload-bearing snapshot) still
// passes acf.VerifyChain.
func TestCreateSnapshot_CarriesMaterializedPayload(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))
	createPayload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v1"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: createPayload,
	}))
	head, _ := store.HeadHash(acf.KindMemory, id)
	updatePayload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v2"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), Payload: updatePayload, ParentHash: head,
	}))

	snap, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	require.True(t, acf.HasPayload(snap.Payload), "snapshot must carry a materialized payload (FR-02.32)")
	require.JSONEq(t, string(updatePayload), string(snap.Payload),
		"snapshot payload must equal the latest materialized (update) payload")
	sum := sha256.Sum256(updatePayload)
	require.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), snap.SnapshotState,
		"SnapshotState must be the content hash of the carried payload")

	// The whole log, including the payload-bearing snapshot, must verify.
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.NoError(t, acf.VerifyChain(events),
		"chain must verify with a payload-bearing snapshot at the head")
}

func TestCreateSnapshot_ConversationDeltaCarriesFullMaterializedPayload(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Name: "conversation", CreatedAt: now, UpdatedAt: now,
	}))
	base, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "hello"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: base,
	}))
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	delta, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "next"}}},
			{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "ok"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), Payload: delta, ParentHash: head,
	}))

	snap, err := CreateSnapshot(context.Background(), store, acf.KindConversation, id)
	require.NoError(t, err)
	var p acf.ConversationPayload
	require.NoError(t, json.Unmarshal(snap.Payload, &p))
	require.Equal(t, acf.ConversationFormatV1, p.Format)
	require.Len(t, p.Events, 4)
	require.Equal(t, "hello", p.Events[0].Content[0].Text)
	require.Equal(t, "next", p.Events[2].Content[0].Text)
}

// TestCreateSnapshot_PayloadlessSnapshotHashUnaffected proves the hash-safety
// invariant: carrying a payload on NEW snapshots does NOT retroactively change
// the hash of an existing payload-less snapshot. We append a payload-less
// snapshot by hand (the pre-FR-02.32 shape), record its hash, then confirm the
// hash recomputes identically — i.e. nothing in the serialization of an
// already-written snapshot moved.
func TestCreateSnapshot_PayloadlessSnapshotHashUnaffected(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))
	p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v1"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p,
	}))
	head, _ := store.HeadHash(acf.KindMemory, id)
	legacySnap := acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeSnapshot,
		Timestamp: now.Add(time.Second), ParentHash: head,
		SnapshotState: "sha256:deadbeef", Payload: nil,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, legacySnap))

	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	stored := events[1]
	require.False(t, acf.HasPayload(stored.Payload), "legacy snapshot stays payload-less")
	recomputed, err := acf.ComputeHash(stored)
	require.NoError(t, err)
	require.Equal(t, stored.Hash, recomputed,
		"an existing payload-less snapshot's hash must not change")
	require.NoError(t, acf.VerifyChain(events))
}

func TestCreateSnapshot_StateIsContentHash(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))
	payload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "stable"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
	}))

	snap1, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	snap2, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	// Same logical content -> same SnapshotState SHA.
	require.Equal(t, snap1.SnapshotState, snap2.SnapshotState)
	require.True(t, strings.HasPrefix(snap1.SnapshotState, "sha256:"))
}
