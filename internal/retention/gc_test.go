package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
	"github.com/stretchr/testify/require"
)

// fakeAckGate is a test PeerAckGate whose AllPeersAcked verdict is fixed,
// independent of kind/artifact — enough to prove RunGC honors a gate that
// reports every peer has acknowledged the artifact.
type fakeAckGate struct{ acked bool }

func (g fakeAckGate) AllPeersAcked(_ acf.Kind, _ string) bool { return g.acked }

// hashStoreTree returns a content+layout fingerprint of every regular file
// under root: a sha256 over the sorted (relPath, size, sha256(content))
// tuples. A dry-run that mutates nothing leaves this fingerprint unchanged.
func hashStoreTree(t *testing.T, root string) string {
	t.Helper()
	type entry struct {
		rel  string
		size int64
		sum  string
	}
	var entries []entry
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rrerr := filepath.Rel(root, path)
		if rrerr != nil {
			return rrerr
		}
		sum := sha256.Sum256(data)
		entries = append(entries, entry{rel: rel, size: info.Size(), sum: hex.EncodeToString(sum[:])})
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

// listStoreFiles returns the sorted set of relative file paths under root.
func listStoreFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, rel)
		return nil
	}))
	sort.Strings(files)
	return files
}

// seedPrunableMemory writes a memory artifact with `pre` pre-snapshot events
// followed by a snapshot, so a prune would compact `pre` events past the
// snapshot. Returns the artifact id.
func seedPrunableMemory(t *testing.T, store *acf.Store, pre int) string {
	t.Helper()
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
	for i := 1; i < pre; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, _ := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(i) * time.Second), Payload: p, ParentHash: head,
		}))
	}
	_, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	// Intentionally COLD: the branch index is NOT pre-warmed. The dry-run/Plan
	// path must resolve protected ancestors read-only (LoadBranchIndex) and
	// write nothing — even RefreshBranchIndex on a main-only artifact would
	// persist branches/<kind>/<id>.json — so the "mutates nothing" tests are
	// genuine regression guards against re-introducing a writing refresh.
	return id
}

// seedEvictableConv writes a conversation artifact with one old
// attachment-bearing event so attachment eviction + blob GC have work to do.
// Returns the artifact id and the attachment content hash.
func seedEvictableConv(t *testing.T, store *acf.Store, blobs *blobstore.Store) (string, string) {
	t.Helper()
	id := acf.NewID()
	now := time.Now().UTC()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	raw := []byte("old-attachment-bytes-for-gc")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now.Add(-40 * 24 * time.Hour),
		Payload:   attachmentPayload(t, acf.Attachment{Kind: "file", MimeType: "application/octet-stream", ContentHash: hash, Bytes: int64(len(raw))}),
	}))
	return id, hash
}

func countActions(report GCReport, op string) int {
	n := 0
	for _, a := range report.Actions {
		if a.Op == op {
			n++
		}
	}
	return n
}

// pruneCfg is a Config that permits history compaction (keep_last_n != "all").
func pruneCfg() Config {
	cfg := DefaultConfig()
	cfg.AttachmentsOnly = true
	cfg.AttachmentMinAge = 30 * 24 * time.Hour
	cfg.KeepLastNSnapshots = 0 // not "all" — pruning is allowed
	return cfg
}

// TestRunGC_DryRunMutatesNothing: a dry-run leaves the store byte-for-byte
// and file-set identical, yet the report still lists planned actions — and a
// history-compaction prune is reported as blocked without --force-local-only.
func TestRunGC_DryRunMutatesNothing(t *testing.T) {
	store, blobs := newConvStore(t)
	seedPrunableMemory(t, store, 4)
	seedEvictableConv(t, store, blobs)

	before := hashStoreTree(t, store.Root)
	beforeFiles := listStoreFiles(t, store.Root)

	report, err := RunGC(context.Background(), store, blobs, pruneCfg(), GCOptions{
		DryRun: true, ForceLocalOnly: false, AckGate: NoPeerAck{},
	})
	require.NoError(t, err)
	require.True(t, report.DryRun, "dry-run report flags DryRun")

	require.Equal(t, before, hashStoreTree(t, store.Root), "dry-run must not change any file content")
	require.Equal(t, beforeFiles, listStoreFiles(t, store.Root), "dry-run must not add or remove files")

	require.NotEmpty(t, report.Actions, "dry-run still reports planned actions")
	require.Positive(t, countActions(report, OpPruneBlocked),
		"a history-compaction prune is reported blocked without --force-local-only")
}

// TestRunGC_NoForceSkipsHistoryPrune: with the default NoPeerAck gate and no
// --force-local-only, an apply pass does NOT compact history (the active log
// keeps its pre-snapshot events), but attachments are still evicted.
func TestRunGC_NoForceSkipsHistoryPrune(t *testing.T) {
	store, blobs := newConvStore(t)
	memID := seedPrunableMemory(t, store, 4)
	_, hash := seedEvictableConv(t, store, blobs)

	beforeActive, err := store.ReadEvents(acf.KindMemory, memID)
	require.NoError(t, err)

	report, err := RunGC(context.Background(), store, blobs, pruneCfg(), GCOptions{
		DryRun: false, ForceLocalOnly: false, AckGate: NoPeerAck{},
	})
	require.NoError(t, err)
	require.False(t, report.DryRun)

	// History is NOT compacted: the pre-snapshot create event is still in the
	// ACTIVE log. The blocked prune also suppresses any preparatory snapshot.
	afterActive, err := store.ReadEvents(acf.KindMemory, memID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(afterActive), len(beforeActive),
		"no prune occurred, so no active event was moved out")
	require.Equal(t, acf.EventType(acf.EventTypeCreate), afterActive[0].Type,
		"the original create event is still the first active event (not compacted)")
	require.Positive(t, countActions(report, OpPruneBlocked), "the skipped prune is recorded")

	// Attachments were still evicted (an eviction marker appended).
	require.Positive(t, countActions(report, OpEvictAttachment), "attachments evicted regardless of the prune gate")
	_ = hash
}

// TestRunGC_ForceLocalOnlyPrunes: with --force-local-only the same store DOES
// compact history (the active memory log shrinks to just the snapshot).
func TestRunGC_ForceLocalOnlyPrunes(t *testing.T) {
	store, blobs := newConvStore(t)
	memID := seedPrunableMemory(t, store, 4)
	seedEvictableConv(t, store, blobs)

	report, err := RunGC(context.Background(), store, blobs, pruneCfg(), GCOptions{
		DryRun: false, ForceLocalOnly: true, AckGate: NoPeerAck{},
	})
	require.NoError(t, err)

	afterActive, err := store.ReadEvents(acf.KindMemory, memID)
	require.NoError(t, err)
	require.Len(t, afterActive, 1, "force-local-only compacts pre-snapshot history")
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), afterActive[0].Type)
	require.Positive(t, countActions(report, OpPruneEvents), "the prune is reported as applied")
	require.Zero(t, countActions(report, OpPruneBlocked), "nothing blocked under force-local-only")
}

// TestRunGC_AckGateAllowsPrune: a gate that reports every peer has acked the
// artifact permits the prune WITHOUT --force-local-only.
func TestRunGC_AckGateAllowsPrune(t *testing.T) {
	store, blobs := newConvStore(t)
	memID := seedPrunableMemory(t, store, 4)

	report, err := RunGC(context.Background(), store, blobs, pruneCfg(), GCOptions{
		DryRun: false, ForceLocalOnly: false, AckGate: fakeAckGate{acked: true},
	})
	require.NoError(t, err)

	afterActive, err := store.ReadEvents(acf.KindMemory, memID)
	require.NoError(t, err)
	require.Len(t, afterActive, 1, "an all-peers-acked gate permits the prune without force")
	require.Positive(t, countActions(report, OpPruneEvents))
	require.Zero(t, countActions(report, OpPruneBlocked))
}

// TestNoPeerAck_AlwaysFalse documents the blocked-relay seam: the default gate
// never reports an ack (no transport ACK-cursor API yet, FR-03.24).
func TestNoPeerAck_AlwaysFalse(t *testing.T) {
	require.False(t, NoPeerAck{}.AllPeersAcked(acf.KindConversation, "any"))
	require.False(t, NoPeerAck{}.AllPeersAcked(acf.KindMemory, ""))
}

// seedUnsnapshottedMemory writes a memory artifact with a create + one update
// event and NO snapshot, so the apply pass's snapshot phase (ForceSnapshotsAll)
// would snapshot it. Returns the artifact id.
func seedUnsnapshottedMemory(t *testing.T, store *acf.Store) string {
	t.Helper()
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
	p1, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v1"})
	head, _ := store.HeadHash(acf.KindMemory, id)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), Payload: p1, ParentHash: head,
	}))
	return id
}

// TestRunGC_DryRunProjectsSnapshotPhase is the dry-run-faithfulness regression
// guard: for an ACK-authorized artifact, dry-run reports the same snapshot
// phase that apply performs.
func TestRunGC_DryRunProjectsSnapshotPhase(t *testing.T) {
	store, blobs := newConvStore(t)
	seedUnsnapshottedMemory(t, store)

	// Dry-run: must project the snapshot the apply pass would take.
	dryReport, err := RunGC(context.Background(), store, blobs, pruneCfg(), GCOptions{
		DryRun: true, ForceLocalOnly: false, AckGate: fakeAckGate{acked: true},
	})
	require.NoError(t, err)
	require.True(t, dryReport.DryRun)
	require.Positive(t, countActions(dryReport, OpSnapshot),
		"dry-run must enumerate the snapshot phase the apply pass performs")

	// Apply pass over an identical store emits the same snapshot phase. The
	// dry-run is a faithful projection, so the OpSnapshot counts match.
	applyStore, applyBlobs := newConvStore(t)
	seedUnsnapshottedMemory(t, applyStore)
	applyReport, err := RunGC(context.Background(), applyStore, applyBlobs, pruneCfg(), GCOptions{
		DryRun: false, ForceLocalOnly: false, AckGate: fakeAckGate{acked: true},
	})
	require.NoError(t, err)
	require.Equal(t, countActions(applyReport, OpSnapshot), countActions(dryReport, OpSnapshot),
		"dry-run snapshot phase must match the apply pass")
}

// TestRunGC_DoesNotAppendUnconsumableSnapshot proves the storage-safety
// invariant: a manual GC must not duplicate the current full state when its
// following prune is blocked or disabled. In both cases the event log remains
// byte-for-byte unchanged by the snapshot phase.
func TestRunGC_DoesNotAppendUnconsumableSnapshot(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		opts GCOptions
		tags []string
	}{
		{
			name: "peer acknowledgement missing",
			cfg:  pruneCfg(),
			opts: GCOptions{AckGate: NoPeerAck{}},
		},
		{
			name: "snapshot retention keeps all",
			cfg: func() Config {
				cfg := pruneCfg()
				cfg.KeepLastNSnapshots = keepAll
				return cfg
			}(),
			opts: GCOptions{ForceLocalOnly: true, AckGate: NoPeerAck{}},
		},
		{
			name: "pinned artifact",
			cfg:  pruneCfg(),
			opts: GCOptions{ForceLocalOnly: true, AckGate: NoPeerAck{}},
			tags: []string{"pinned"},
		},
		{
			name: "keep forever artifact",
			cfg:  pruneCfg(),
			opts: GCOptions{ForceLocalOnly: true, AckGate: NoPeerAck{}},
			tags: []string{"keep-forever"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, blobs := newConvStore(t)
			id := seedUnsnapshottedMemory(t, store)
			if len(tt.tags) > 0 {
				art, err := store.ReadArtifact(acf.KindMemory, id)
				require.NoError(t, err)
				art.Tags = append([]string(nil), tt.tags...)
				require.NoError(t, store.WriteArtifact(art))
			}
			before, err := store.ReadEvents(acf.KindMemory, id)
			require.NoError(t, err)

			report, err := RunGC(context.Background(), store, blobs, tt.cfg, tt.opts)
			require.NoError(t, err)
			require.Zero(t, countActions(report, OpSnapshot))

			after, err := store.ReadEvents(acf.KindMemory, id)
			require.NoError(t, err)
			require.Equal(t, before, after,
				"GC must not append a full-state snapshot when it cannot consume it")
		})
	}
}
