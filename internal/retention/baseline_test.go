package retention

// Aligned-chains delta-sync: a baseline event
// (acf.EventTypeBaseline) carries the full materialized origin state, so for
// retention purposes it is a self-contained checkpoint exactly like a
// payload-bearing FR-02.32 snapshot: it anchors on-snapshot pruning
// (everything strictly before it is superseded for main-branch replay), and a
// baseline-headed artifact needs no fresh snapshot (nothing new to encode).

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// seedBaselineConversation builds a conversation artifact whose log is
// [create, update, baseline, update2]: two local events, then an adopted
// baseline aligned at a foreign origin head, then one post-adoption event
// chained onto the aligned head (the shape a receiver's log has after
// adoption + one verbatim origin delta).
func seedBaselineConversation(t *testing.T, store *acf.Store) (id string, alignedHead string) {
	t.Helper()
	id = acf.NewID()
	now := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Name: "conv", CreatedAt: now, UpdatedAt: now,
	}))

	turn := func(text string, ts time.Time) []acf.ConversationEvent {
		return []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Timestamp: ts, Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: text}},
		}}
	}
	p0, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: turn("local one", now),
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p0,
	}))
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	p1, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: turn("local two", now.Add(time.Second)),
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), Payload: p1, ParentHash: head,
	}))

	// Adopt a baseline aligned at a foreign origin head. The origin-side
	// event bytes never exist locally — only the hash the bookkeeping (and
	// the post-adoption chain) aligns to.
	origin := &acf.Store{Root: t.TempDir()}
	require.NoError(t, origin.Init())
	originArt := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Name: "conv", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, origin.WriteArtifact(originArt))
	originPayload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: turn("origin state", now.Add(2*time.Second)),
	})
	require.NoError(t, err)
	originHeadEvent := acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now.Add(2 * time.Second), Payload: originPayload,
	}
	require.NoError(t, origin.AppendEvent(acf.KindConversation, originHeadEvent))
	alignedHead, err = origin.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)

	require.NoError(t, store.AdoptBaseline(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeBaseline,
		Timestamp: now.Add(3 * time.Second), Payload: originPayload,
		AlignedHead: alignedHead, AlignedEventID: originHeadEvent.EventID,
	}))

	// Post-adoption event chains onto the aligned head.
	p2, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1, Events: turn("post adoption", now.Add(4*time.Second)),
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(4 * time.Second), Payload: p2, ParentHash: alignedHead,
	}))
	return id, alignedHead
}

// TestPruneArtifact_BaselineAnchorsPrune: a baseline is a prune anchor
// exactly like a snapshot — everything strictly before it moves to the
// .compacted layer, the baseline plus the post-adoption tail stays active,
// and the merged (active + compacted) log still verifies across both the
// prune boundary and the baseline's aligned-head reset.
func TestPruneArtifact_BaselineAnchorsPrune(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id, _ := seedBaselineConversation(t, store)

	moved, deleted, err := PruneArtifact(context.Background(), store, acf.KindConversation, id,
		time.Now().UTC().Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 2, moved, "the two pre-baseline events move to .compacted")
	require.Equal(t, 0, deleted)

	active, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, active, 2)
	require.Equal(t, acf.EventTypeBaseline, active[0].Type, "the baseline anchors the active log")
	require.Equal(t, acf.EventType(acf.EventTypeUpdate), active[1].Type)

	merged, err := store.ReadEventsIncludingCompacted(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, merged, 4)
	require.NoError(t, acf.VerifyChain(merged),
		"the merged log verifies across the prune boundary and the aligned-head reset")
}

// TestForceSnapshotsAll_SkipsBaselineHeadedArtifact: an artifact whose log
// tail is a baseline already ends in a full-state checkpoint — the pressure
// sweep (and, via the same guard, the gc dry-run and time-based passes) must
// not append a redundant snapshot on top of it.
func TestForceSnapshotsAll_SkipsBaselineHeadedArtifact(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	now := time.Now().UTC()

	// Baseline-headed conversation (adopt onto an empty artifact).
	convID := acf.NewID()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Timestamp: now, Role: "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "origin state"}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.AdoptBaseline(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: convID, Type: acf.EventTypeBaseline,
		Timestamp: now, Payload: payload,
		AlignedHead:    "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f",
		AlignedEventID: acf.NewID(),
	}))

	// Control: a memory whose head is a plain create still gets snapshotted.
	memID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: memID, Kind: acf.KindMemory,
		Name: "m", CreatedAt: now, UpdatedAt: now,
	}))
	mp, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: memID, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: mp,
	}))

	n, err := ForceSnapshotsAll(context.Background(), store)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the control artifact needs a snapshot")

	events, err := store.ReadEvents(acf.KindConversation, convID)
	require.NoError(t, err)
	require.Len(t, events, 1, "no redundant snapshot appended on top of the baseline")
	require.Equal(t, acf.EventTypeBaseline, events[0].Type)
}
