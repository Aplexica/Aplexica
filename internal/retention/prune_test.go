package retention

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestPruneArtifact_MovesPreSnapshotEvents(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))

	// Seed: 1 create + 2 updates (3 events total) before the snapshot.
	p0, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p0,
	}))
	for i := 1; i < 3; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, _ := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(i) * time.Second), Payload: p,
			ParentHash: head,
		}))
	}

	// Snapshot.
	_, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)

	// Prune with grace deadline well in the past (so the freshly-written
	// compacted file's mtime is AFTER it — nothing to delete this call).
	moved, deleted, err := PruneArtifact(context.Background(), store, acf.KindMemory, id, now.Add(-1*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 3, moved, "3 pre-snapshot events moved to .compacted")
	require.Equal(t, 0, deleted, "grace deadline is earlier than our just-written compacted file — nothing to delete yet")

	// Active log now should be just the snapshot.
	active, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)

	// Including-compacted should return all 4.
	all, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, all, 4)
}

func TestPruneArtifact_NoSnapshotIsNoOp(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))
	p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p,
	}))
	moved, _, err := PruneArtifact(context.Background(), store, acf.KindMemory, id, time.Now())
	require.NoError(t, err)
	require.Equal(t, 0, moved, "no snapshot -> no pruning")
}

// buildChainedArtifact seeds an artifact on the main branch with a create
// followed by (updates-1) updates, returning the per-step main-branch head
// hashes (heads[i] is the head after event i). It does NOT create a snapshot.
func buildChainedArtifact(t *testing.T, store *acf.Store, id string, now time.Time, events int) []string {
	t.Helper()
	heads := make([]string, 0, events)
	p0, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p0,
	}))
	h, err := store.HeadHash(acf.KindMemory, id)
	require.NoError(t, err)
	heads = append(heads, h)
	for i := 1; i < events; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, herr := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, herr)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(i) * time.Second), Payload: p,
			ParentHash: head,
		}))
		h, herr = store.HeadHash(acf.KindMemory, id)
		require.NoError(t, herr)
		heads = append(heads, h)
	}
	return heads
}

// movedHashSet reads the .compacted layer and returns the set of event
// hashes that PruneArtifact moved out of the active log.
func movedHashSet(t *testing.T, store *acf.Store, id string) map[string]bool {
	t.Helper()
	active, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	activeSet := map[string]bool{}
	for _, e := range active {
		activeSet[e.Hash] = true
	}
	all, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	moved := map[string]bool{}
	for _, e := range all {
		if !activeSet[e.Hash] {
			moved[e.Hash] = true
		}
	}
	return moved
}

func TestPruneArtifact_ProtectsBranchAncestors(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))

	// Main chain e0..e4 (create + 4 updates). heads[i] is the head hash
	// after event i.
	heads := buildChainedArtifact(t, store, id, now, 5)
	e1Hash := heads[1]

	// Fork a branch off the PRE-snapshot event e1 (its hash is the fork
	// parent), then extend the branch with one more event. Mirrors how
	// cmd_fork.go / cmd_branch.go emit a fork-outer event.
	forkEvt := acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeForkOuter,
		Timestamp: now.Add(10 * time.Second), ParentHash: e1Hash,
		Branch: "alt", ForkSourceBranch: acf.MainBranch, ForkOriginAgent: "codex",
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, forkEvt))
	forkHash, err := store.HeadHashByBranch(acf.KindMemory, id, "alt")
	require.NoError(t, err)
	require.NotEmpty(t, forkHash)
	altPayload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "alt-1"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(11 * time.Second), Branch: "alt",
		ParentHash: forkHash, Payload: altPayload,
	}))
	altHead, err := store.HeadHashByBranch(acf.KindMemory, id, "alt")
	require.NoError(t, err)

	// Snapshot on main (chains off main head e4). Without protection,
	// e0..e3 (everything before the snapshot in append order) plus the
	// fork chain would all be moved.
	_, err = CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)

	_, _, err = PruneArtifact(context.Background(), store, acf.KindMemory, id, now.Add(-1*time.Hour))
	require.NoError(t, err)

	moved := movedHashSet(t, store, id)
	require.False(t, moved[e1Hash], "fork-point ancestor e1 must NOT be moved")
	require.False(t, moved[forkHash], "fork event must NOT be moved")
	require.False(t, moved[altHead], "side-branch tip must NOT be moved")
	require.False(t, moved[heads[0]], "e0 is an ancestor of e1 and must NOT be moved")

	// VerifyChain must be green on BOTH branches after prune. We rebuild
	// each branch's chain in canonical parent-walk order from the re-merged
	// (active + compacted) event set — order-independent of the merge's
	// timestamp sort — and verify it. This is the post-compaction
	// cross-boundary verification path.
	all, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	byHash := indexByHash(all)

	// Main branch verifies from its post-snapshot tip back to genesis.
	mainTip, err := store.HeadHashByBranch(acf.KindMemory, id, acf.MainBranch)
	require.NoError(t, err)
	require.NoError(t, acf.VerifyChain(chainTo(byHash, mainTip)), "main branch must verify after prune")

	// Forked branch verifies from its tip (alt) back through the fork
	// point e1 to genesis.
	require.NoError(t, acf.VerifyChain(chainTo(byHash, altHead)), "forked branch chain must verify after prune")
}

func indexByHash(events []acf.Event) map[string]acf.Event {
	m := make(map[string]acf.Event, len(events))
	for _, e := range events {
		m[e.Hash] = e
	}
	return m
}

// chainTo walks ParentHash from tipHash back to genesis and returns the
// chain in genesis-first order, suitable for acf.VerifyChain.
func chainTo(byHash map[string]acf.Event, tipHash string) []acf.Event {
	var rev []acf.Event
	for h := tipHash; h != ""; {
		e, ok := byHash[h]
		if !ok {
			break
		}
		rev = append(rev, e)
		h = e.ParentHash
	}
	out := make([]acf.Event, len(rev))
	for i := range rev {
		out[len(rev)-1-i] = rev[i]
	}
	return out
}

func TestPruneArtifact_NoFork_ByteIdentical(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))

	// Pre-snapshot events e0..e3 on main, then a snapshot. With no forks,
	// the protected set is exactly the trunk ancestry of the (post-snapshot)
	// main head, so toMove must equal the pre-PR3 behavior: every event
	// strictly before the snapshot moves to .compacted.
	const preSnapshotEvents = 4
	preHeads := buildChainedArtifact(t, store, id, now, preSnapshotEvents)

	_, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)

	moved, deleted, err := PruneArtifact(context.Background(), store, acf.KindMemory, id, now.Add(-1*time.Hour))
	require.NoError(t, err)
	require.Equal(t, preSnapshotEvents, moved, "all pre-snapshot events move (no over-protection)")
	require.Equal(t, 0, deleted)

	// Active log is just the snapshot (matches pre-PR3 behavior).
	active, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)

	// The exact moved set is e0..e3.
	movedSet := movedHashSet(t, store, id)
	require.Len(t, movedSet, preSnapshotEvents)
	for _, h := range preHeads {
		require.True(t, movedSet[h], "pre-snapshot event %s must be moved", h)
	}

	all, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, all, preSnapshotEvents+1)
}

func TestPruneArtifact_PinExemption(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
		Tags: []string{"pinned"},
	}))
	p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p,
	}))
	_, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	moved, _, err := PruneArtifact(context.Background(), store, acf.KindMemory, id, time.Now())
	require.NoError(t, err)
	require.Equal(t, 0, moved, "pinned artifact is exempt from pruning")
}

// rawJSONLBytes returns the JSON-line byte size (json.Marshal + trailing
// newline per event) of a slice of events — the lower-bound size of the bytes
// the events contribute when appended to the compacted log.
func rawJSONLBytes(t *testing.T, events []acf.Event) int64 {
	t.Helper()
	var n int64
	for _, e := range events {
		b, err := json.Marshal(e)
		require.NoError(t, err)
		n += int64(len(b)) + 1
	}
	return n
}

// compactedOnDiskAfterAppend recomputes the EXACT on-disk gz size the apply
// path (applyPrunePlan) would leave on disk after appending toMove to the
// existing compacted file at compactedPath: it decompresses the existing
// file (if any), appends each toMove event as json.Marshal+'\n', then gzips
// the concatenation — byte-for-byte the encoding applyPrunePlan uses. This is
// the ground-truth size a same-pass grace-delete would actually reclaim.
func compactedOnDiskAfterAppend(t *testing.T, compactedPath string, toMove []acf.Event) int64 {
	t.Helper()
	var existing []byte
	if f, oerr := os.Open(compactedPath); oerr == nil {
		gz, gerr := gzip.NewReader(f)
		require.NoError(t, gerr)
		sc := bufio.NewScanner(gz)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			existing = append(existing, sc.Bytes()...)
			existing = append(existing, '\n')
		}
		require.NoError(t, gz.Close())
		require.NoError(t, f.Close())
	} else {
		require.True(t, os.IsNotExist(oerr), "unexpected error opening compacted: %v", oerr)
	}
	var newLines []byte
	for _, e := range toMove {
		b, err := json.Marshal(e)
		require.NoError(t, err)
		newLines = append(newLines, b...)
		newLines = append(newLines, '\n')
	}
	all := append(existing, newLines...)
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	_, werr := gzw.Write(all)
	require.NoError(t, werr)
	require.NoError(t, gzw.Close())
	return int64(buf.Len())
}

// TestPrunePlan_GraceDeleteCountsAppendedBytes guards FR-03.23: when a prior
// .compacted file already exists AND the same pass would both append more
// pre-snapshot events to it and grace-delete it, the dry-run report's
// reclaimed-bytes figure (PrunePlan.CompactedBytes, surfaced as
// OpDeleteCompacted BytesSaved) must cover the file as it will actually be
// deleted — i.e. existing + newly-appended bytes — not just the stale
// pre-append size.
//
// Bug repro: the first prune pass writes a .compacted file (grace deadline in
// the past, so it is kept). A second pass then appends fresh pre-snapshot
// events to that same file and grace-deletes it (deadline in the future). The
// buggy predictCompactedBytes returned only the EXISTING file's os.Stat size,
// under-counting the bytes the second pass actually reclaims.
func TestPrunePlan_GraceDeleteCountsAppendedBytes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))

	// Pass 1: 4 pre-snapshot events + snapshot, prune with a PAST grace
	// deadline so the freshly-written .compacted file is kept (not deleted).
	buildChainedArtifact(t, store, id, now, 4)
	_, err := CreateSnapshot(ctx, store, acf.KindMemory, id)
	require.NoError(t, err)
	moved1, deleted1, err := PruneArtifact(ctx, store, acf.KindMemory, id, now.Add(-1*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 4, moved1)
	require.Equal(t, 0, deleted1, "past grace deadline -> compacted file kept after pass 1")

	compactedPath := filepath.Join(root, "events", ".compacted", kindDirName(acf.KindMemory), id+".jsonl.gz")
	existingInfo, err := os.Stat(compactedPath)
	require.NoError(t, err)
	existingSize := existingInfo.Size()
	require.Positive(t, existingSize, "pass 1 must leave a non-empty compacted file")

	// Append MORE pre-snapshot events on top of the prior snapshot, then take
	// a new snapshot. These new events become the second pass's toMove.
	for i := 0; i < 3; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v2"})
		head, herr := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, herr)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(100+i) * time.Second), Payload: p,
			ParentHash: head,
		}))
	}
	_, err = CreateSnapshot(ctx, store, acf.KindMemory, id)
	require.NoError(t, err)

	// Pass 2 (PLAN only): a FUTURE grace deadline makes the (to-be-rewritten)
	// compacted file older than the deadline -> the plan predicts a
	// grace-delete. An existing compacted file is present and toMove is
	// non-empty, so this is exactly the under-report scenario.
	futureGrace := time.Now().Add(1 * time.Hour)
	plan, err := PlanPruneArtifact(ctx, store, acf.KindMemory, id, futureGrace)
	require.NoError(t, err)
	require.NotEmpty(t, plan.ToMove, "second pass has new pre-snapshot events to move")
	require.True(t, plan.DeleteCompacted, "second pass predicts a grace-delete")

	appendedRaw := rawJSONLBytes(t, plan.ToMove)
	require.Positive(t, appendedRaw, "second pass appends a non-zero number of bytes")

	// Ground truth: the size the compacted file will actually have on disk
	// when apply rewrites it (existing decompressed + new JSONL, re-gzipped)
	// — this is what the grace-delete actually reclaims.
	wantOnDisk := compactedOnDiskAfterAppend(t, compactedPath, plan.ToMove)

	// The reported reclaim must cover the appended bytes, not just the stale
	// pre-append size. Both assertions FAIL against the buggy code that
	// returns only existingSize.
	require.Greater(t, plan.CompactedBytes, existingSize,
		"reported grace-delete bytes must exceed the stale pre-append compacted size (existing=%d)", existingSize)
	require.Equal(t, wantOnDisk, plan.CompactedBytes,
		"reported grace-delete bytes must equal the actual on-disk size apply will delete (existing+appended)")
}
