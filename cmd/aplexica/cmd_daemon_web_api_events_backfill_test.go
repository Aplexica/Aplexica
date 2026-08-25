package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
	"github.com/stretchr/testify/require"
)

// TestEventsBackfill_DoesNotDropSameMillisecondEventsAcrossPages: the events
// feed truncates timestamps to milliseconds, so a burst can produce several
// events sharing one Seq. The exclusive Seq cursor must not drop same-ms events
// that straddle a page boundary (a single assistant turn's events commonly
// share a timestamp — BRD-02 §5.4 round-trip / FR-03 events feed).
func TestEventsBackfill_DoesNotDropSameMillisecondEventsAcrossPages(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	id := acf.NewID()
	ts := time.Unix(1700000000, 123*int64(time.Millisecond)).UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "m", CreatedAt: ts, UpdatedAt: ts,
	}))

	// Three events ALL stamped the same millisecond.
	p0, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: ts, Payload: p0,
	}))
	for i := 1; i < 3; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, _ := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: ts, Payload: p, ParentHash: head,
		}))
	}

	acc := &eventsWebAccessor{deps: &webAPIDeps{store: store}}
	seen := 0
	before := int64(0)
	for i := 0; i < 10; i++ { // bounded to avoid an infinite loop on a bug
		page, err := acc.Backfill(apiweb.EventQuery{Before: before, Limit: 2})
		require.NoError(t, err)
		seen += len(page.Events)
		if len(page.Events) == 0 || page.NextBefore <= 0 || page.NextBefore == before {
			break
		}
		before = page.NextBefore
	}
	require.Equal(t, 3, seen, "all 3 same-millisecond events must surface across pages")
}
