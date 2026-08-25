package retention

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// seedOldConvWithAttachment writes a conversation artifact whose genesis
// create event (40 days old) carries one content-addressed attachment, plus a
// recent update event so there is a tail past any snapshot the sweep takes.
// Returns the artifact id and the attachment content hash.
func seedOldConvWithAttachment(t *testing.T, store *acf.Store, blobs interface {
	Put([]byte) (string, error)
}) string {
	t.Helper()
	id := acf.NewID()
	now := time.Now().UTC()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))

	raw := []byte("sweep-old-attachment-bytes")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now.Add(-40 * 24 * time.Hour),
		Payload: attachmentPayload(t, acf.Attachment{
			Kind: "file", ContentHash: hash, Bytes: int64(len(raw)),
		}),
	}))
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(-35 * 24 * time.Hour),
		Payload:    attachmentPayload(t),
		ParentHash: head,
	}))
	return id
}

// opSequence collapses a report's actions into the ordered list of distinct
// op "phases" they belong to, so a test can assert phase ORDER without
// counting how many actions each phase emitted.
func opSequence(r GCReport) []string {
	var phases []string
	last := ""
	for _, a := range r.Actions {
		phase := a.Op
		if phase != last {
			phases = append(phases, phase)
			last = phase
		}
	}
	return phases
}

func contains(seq []string, op string) bool {
	for _, s := range seq {
		if s == op {
			return true
		}
	}
	return false
}

// TestRunPressureSweep_Order: when the watermark check stays over for the
// whole sweep, the actions appear in the fixed order
// attachments -> blobGC -> snapshot -> prune.
func TestRunPressureSweep_Order(t *testing.T) {
	store, blobs := newConvStore(t)
	seedOldConvWithAttachment(t, store, blobs)

	cfg := DefaultConfig()
	cfg.AttachmentsOnly = true
	cfg.AttachmentMinAge = 24 * time.Hour
	cfg.KeepLastNSnapshots = 0 // allow snapshot pruning

	// Stay over watermark for the entire sweep.
	report, err := RunPressureSweep(context.Background(), store, blobs, cfg, func() bool { return true })
	require.NoError(t, err)

	seq := opSequence(report)
	// Expect the phases in this relative order. Filter to the phases we
	// know about and assert their relative positions.
	idx := func(op string) int {
		for i, s := range seq {
			if s == op {
				return i
			}
		}
		return -1
	}
	require.True(t, contains(seq, OpEvictAttachment), "attachments phase ran: %v", seq)
	require.True(t, contains(seq, OpSnapshot), "snapshot phase ran: %v", seq)
	require.True(t, contains(seq, OpPruneEvents), "prune phase ran: %v", seq)

	require.Less(t, idx(OpEvictAttachment), idx(OpSnapshot), "attachments before snapshot: %v", seq)
	require.Less(t, idx(OpSnapshot), idx(OpPruneEvents), "snapshot before prune: %v", seq)
	if contains(seq, OpGCBlob) {
		require.Less(t, idx(OpEvictAttachment), idx(OpGCBlob), "attachments before blobGC: %v", seq)
		require.Less(t, idx(OpGCBlob), idx(OpSnapshot), "blobGC before snapshot: %v", seq)
	}
}

// TestRunPressureSweep_EarlyExitOnRelief: when overWatermark flips to false
// after the attachments+blobGC phase, the sweep returns WITHOUT taking
// snapshots or pruning.
func TestRunPressureSweep_EarlyExitOnRelief(t *testing.T) {
	store, blobs := newConvStore(t)
	seedOldConvWithAttachment(t, store, blobs)

	cfg := DefaultConfig()
	cfg.AttachmentsOnly = true
	cfg.AttachmentMinAge = 24 * time.Hour
	cfg.KeepLastNSnapshots = 0

	// First call (after attachments) reports relieved.
	report, err := RunPressureSweep(context.Background(), store, blobs, cfg, func() bool { return false })
	require.NoError(t, err)

	seq := opSequence(report)
	require.True(t, contains(seq, OpEvictAttachment), "attachments phase still ran: %v", seq)
	require.False(t, contains(seq, OpSnapshot), "no snapshot after early relief: %v", seq)
	require.False(t, contains(seq, OpPruneEvents), "no prune after early relief: %v", seq)
}

// TestRunPressureSweep_AttachmentsOnlyFalse: with AttachmentsOnly=false the
// attachment-eviction phase is skipped entirely; the sweep goes straight to
// snapshot then prune (still-over).
func TestRunPressureSweep_AttachmentsOnlyFalse(t *testing.T) {
	store, blobs := newConvStore(t)
	seedOldConvWithAttachment(t, store, blobs)

	cfg := DefaultConfig()
	cfg.AttachmentsOnly = false
	cfg.KeepLastNSnapshots = 0

	report, err := RunPressureSweep(context.Background(), store, blobs, cfg, func() bool { return true })
	require.NoError(t, err)

	seq := opSequence(report)
	require.False(t, contains(seq, OpEvictAttachment), "no eviction when AttachmentsOnly=false: %v", seq)
	require.False(t, contains(seq, OpGCBlob), "no blob GC when AttachmentsOnly=false: %v", seq)
	require.True(t, contains(seq, OpSnapshot), "snapshot phase ran: %v", seq)
	require.True(t, contains(seq, OpPruneEvents), "prune phase ran: %v", seq)
}

// TestRunPressureSweep_KeepAllNoSnapshotPrune: with KeepLastNSnapshots == -1
// ("all") neither snapshot nor prune may run. A snapshot cannot enable any
// reclaim under keep-all and would only duplicate the full materialized body.
func TestRunPressureSweep_KeepAllNoSnapshotPrune(t *testing.T) {
	store, blobs := newConvStore(t)
	seedOldConvWithAttachment(t, store, blobs)

	cfg := DefaultConfig()
	cfg.AttachmentsOnly = false
	cfg.KeepLastNSnapshots = keepAll // -1 / "all"

	report, err := RunPressureSweep(context.Background(), store, blobs, cfg, func() bool { return true })
	require.NoError(t, err)

	seq := opSequence(report)
	require.False(t, contains(seq, OpSnapshot), "keep-all must not create unreclaimable snapshots: %v", seq)
	require.False(t, contains(seq, OpPruneEvents), "keep_last_n=all must not prune snapshots: %v", seq)
}

// TestRunPressureSweep_HistoryPruneGated: a momentary spike that is relieved
// by the snapshot phase must NOT trigger history pruning. overWatermark stays
// true through attachments/blobGC, then flips false right after the snapshot
// re-check — prune must be skipped.
func TestRunPressureSweep_HistoryPruneGated(t *testing.T) {
	store, blobs := newConvStore(t)
	seedOldConvWithAttachment(t, store, blobs)

	cfg := DefaultConfig()
	cfg.AttachmentsOnly = true
	cfg.AttachmentMinAge = 24 * time.Hour
	cfg.KeepLastNSnapshots = 0

	calls := 0
	// First re-check (after attachments+blobGC): still over -> proceed to
	// snapshot. Second re-check (after snapshot): relieved -> skip prune.
	over := func() bool {
		calls++
		return calls < 2
	}
	report, err := RunPressureSweep(context.Background(), store, blobs, cfg, over)
	require.NoError(t, err)

	seq := opSequence(report)
	require.True(t, contains(seq, OpSnapshot), "snapshot phase ran: %v", seq)
	require.False(t, contains(seq, OpPruneEvents), "prune gated: relieved after snapshot: %v", seq)
}
