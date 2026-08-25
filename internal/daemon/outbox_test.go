package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/stretchr/testify/require"
)

func newTestOutbox(t *testing.T) *Outbox {
	t.Helper()
	ob := &Outbox{Root: t.TempDir()}
	if err := ob.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	return ob
}

func newLimitedTestOutbox(t *testing.T, entryBytes, pendingBytes, listBytes int64) *Outbox {
	t.Helper()
	ob := &Outbox{
		Root:            t.TempDir(),
		maxEntries:      100,
		maxEntryBytes:   entryBytes,
		maxPendingBytes: pendingBytes,
		listMaxBytes:    listBytes,
	}
	if err := ob.Init(); err != nil {
		t.Fatalf("init limited outbox: %v", err)
	}
	return ob
}

func assertPendingAccounting(t *testing.T, ob *Outbox) {
	t.Helper()
	entries, err := os.ReadDir(ob.Root)
	if err != nil {
		t.Fatalf("read pending root: %v", err)
	}
	var count int
	var total int64
	wantSizes := make(map[string]int64)
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if _, _, ok := parseOutboxName(de.Name()); !ok {
			continue
		}
		info, err := os.Lstat(filepath.Join(ob.Root, de.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		count++
		total += info.Size()
		wantSizes[de.Name()] = info.Size()
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.count != count || ob.pendingBytes != total {
		t.Fatalf("pending accounting count=%d bytes=%d; disk count=%d bytes=%d", ob.count, ob.pendingBytes, count, total)
	}
	if len(ob.pendingSizes) != len(wantSizes) {
		t.Fatalf("pending size index has %d entries; disk has %d", len(ob.pendingSizes), len(wantSizes))
	}
	for name, size := range wantSizes {
		if ob.pendingSizes[name] != size {
			t.Fatalf("pending size index %s=%d; want %d", name, ob.pendingSizes[name], size)
		}
	}
}

func assertDirtyRescanMarker(t *testing.T, ob *Outbox, eventScope string) {
	t.Helper()
	scope, err := markerScope(eventScope)
	if err != nil {
		t.Fatalf("marker scope: %v", err)
	}
	path := filepath.Join(ob.mutations.Root, markerBase(scope))
	marker, _, err := loadMarker(path, scope)
	if err != nil {
		t.Fatalf("load rescan marker: %v", err)
	}
	if marker.State != "dirty" || marker.MutationGeneration <= marker.CompletedGeneration {
		t.Fatalf("rescan marker lost rejected-write obligation: %+v", marker)
	}
}

func marshalSeedOutboxEntry(t *testing.T, seq uint64, event proto.RemoteEvent) []byte {
	t.Helper()
	data, err := json.Marshal(outboxEntry{
		SchemaVersion: outboxSchemaVersion,
		Seq:           seq,
		EnqueuedAt:    time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC),
		Event:         event,
		Intent:        intentForEvent(event),
	})
	if err != nil {
		t.Fatalf("marshal seed outbox entry: %v", err)
	}
	return data
}

func ev(id string) proto.RemoteEvent {
	return proto.RemoteEvent{
		NamespaceID: "ns-1",
		BranchID:    "main",
		ArtifactID:  "art-1",
		EventID:     id,
		Bytes:       []byte(`{"x":1}`),
	}
}

func securityCutoverEpoch(label string, generation, access, key uint64) securityepoch.SecurityEpoch {
	return securityepoch.SecurityEpoch{
		CoordinatorGeneration: generation,
		AccessGeneration:      access,
		AccessSetHash:         sha256.Sum256([]byte("access-" + label)),
		BarrierID:             sha256.Sum256([]byte("barrier-" + label)),
		KeyMode:               "namespace-key-v1",
		KeyVersion:            key,
	}
}

func securityCutoverEvent(id, namespace string, epoch securityepoch.SecurityEpoch) proto.RemoteEvent {
	event := ev(id)
	event.NamespaceID = namespace
	event.SecurityGeneration = epoch.CoordinatorGeneration
	event.AccessGeneration = epoch.AccessGeneration
	event.AccessSetHash = epoch.AccessSetHash
	event.SecurityBarrierID = epoch.BarrierID
	event.KeyMode = epoch.KeyMode
	event.KeyVersion = epoch.KeyVersion
	return event
}

func TestOutboxPurgeSecurityScopeRequiresExactMarkerAndKeepsOnlyCurrentOrOtherScope(t *testing.T) {
	outbox := newTestOutbox(t)
	namespace := "0197f30a-3c58-7000-8000-000000000001"
	oldEpoch := securityCutoverEpoch("old", 1, 1, 1)
	nextEpoch := securityCutoverEpoch("next", 2, 2, 2)
	for _, event := range []proto.RemoteEvent{
		securityCutoverEvent("old-scope", namespace, oldEpoch),
		securityCutoverEvent("next-scope", namespace, nextEpoch),
		securityCutoverEvent("other-scope", "0197f30a-3c58-7000-8000-000000000002", oldEpoch),
	} {
		require.NoError(t, outbox.Append(event))
	}

	_, err := outbox.PurgeSecurityScope(namespace, nextEpoch)
	require.ErrorContains(t, err, "rescan obligation")
	require.NoError(t, outbox.mutations.RequireSecurityCutover(namespace, nextEpoch))
	purged, err := outbox.PurgeSecurityScope(namespace, nextEpoch)
	require.NoError(t, err)
	require.Equal(t, 1, purged)

	entries, err := outbox.List()
	require.NoError(t, err)
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.Event.EventID)
	}
	require.ElementsMatch(t, []string{"next-scope", "other-scope"}, ids)
	assertPendingAccounting(t, outbox)
}

func TestOutboxPurgeSecurityScopeFailsAtomicallyOnFutureOrEquivocalEntry(t *testing.T) {
	outbox := newTestOutbox(t)
	namespace := "0197f30a-3c58-7000-8000-000000000001"
	oldEpoch := securityCutoverEpoch("old", 1, 1, 1)
	nextEpoch := securityCutoverEpoch("next", 2, 2, 2)
	futureEpoch := securityCutoverEpoch("future", 3, 3, 3)
	require.NoError(t, outbox.Append(securityCutoverEvent("old", namespace, oldEpoch)))
	require.NoError(t, outbox.Append(securityCutoverEvent("future", namespace, futureEpoch)))
	require.NoError(t, outbox.mutations.RequireSecurityCutover(namespace, nextEpoch))

	purged, err := outbox.PurgeSecurityScope(namespace, nextEpoch)
	require.ErrorContains(t, err, "future or equivocal")
	require.Zero(t, purged)
	entries, listErr := outbox.List()
	require.NoError(t, listErr)
	require.Len(t, entries, 2, "validation must finish before any obsolete cache is removed")
}

// TestOutbox_AppendRoundTrip: Append writes a file decodable back to the same
// proto.RemoteEvent.
func TestOutbox_AppendRoundTrip(t *testing.T) {
	ob := newTestOutbox(t)
	in := ev("evt-a")
	in.Sequence = 42
	in.ParentHash = "evt-prev"
	in.CheckpointAlignmentHash = strings.Repeat("a", 64)
	if err := ob.Append(in); err != nil {
		t.Fatalf("append: %v", err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 entry, got %d", len(list))
	}
	got := list[0].Event
	if got.EventID != "evt-a" || got.Sequence != 42 || got.ParentHash != "evt-prev" || got.CheckpointAlignmentHash != in.CheckpointAlignmentHash {
		t.Fatalf("event not faithfully persisted: %+v", got)
	}
	if string(got.Bytes) != `{"x":1}` {
		t.Fatalf("bytes not preserved: %s", got.Bytes)
	}
	if list[0].SchemaVersion != outboxSchemaVersion {
		t.Fatalf("schema version not stamped: %d", list[0].SchemaVersion)
	}
	if list[0].Intent.SourceHeadHash != "evt-prev" || list[0].Intent.CheckpointAlignmentHash != in.CheckpointAlignmentHash {
		t.Fatalf("outbox intent conflated predecessor and checkpoint alignment: %+v", list[0].Intent)
	}
	assertPendingAccounting(t, ob)
}

func TestOutbox_StagedCheckpointPersistsLightweightIntentAndDeletesBodyTerminally(t *testing.T) {
	ob := newTestOutbox(t)
	fileID := strings.Repeat("a", 64)
	digest := strings.Repeat("b", 64)
	size := int64(proto.MaxSealedEventBytes + 1)
	path := filepath.Join(ob.staged(), fileID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	in := ev("staged-checkpoint")
	in.Bytes = nil
	in.Lane = "retained"
	in.BodyDigest = digest
	in.DaemonStagedPayload = &proto.RemoteDaemonStagedPayloadV1{FileID: fileID, SealedBytes: uint64(size), BodyDigest: digest}
	if err := ob.Append(in); err != nil {
		t.Fatal(err)
	}
	list, err := ob.List()
	if err != nil || len(list) != 1 || list[0].Event.DaemonStagedPayload == nil || len(list[0].Event.Bytes) != 0 {
		t.Fatalf("staged list=%+v err=%v", list, err)
	}
	if ob.pendingBytes >= size {
		t.Fatalf("staged body was embedded in JSON outbox: pending=%d body=%d", ob.pendingBytes, size)
	}
	if err := ob.Remove(in.EventID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal outbox removal retained staged body: %v", err)
	}
}

func TestOutbox_InitReconcilesOnlyUnreferencedStagedPayloads(t *testing.T) {
	root := t.TempDir()
	first := &Outbox{Root: root}
	if err := first.Init(); err != nil {
		t.Fatal(err)
	}
	orphan := strings.Repeat("c", 64)
	if err := os.WriteFile(filepath.Join(first.staged(), orphan), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := &Outbox{Root: root}
	if err := second.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second.staged(), orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced staged body survived restart reconciliation: %v", err)
	}
}

// TestOutbox_AppendDuplicateEventIDIsIdempotent verifies a repeated forward of
// the same committed event does not create multiple pending durable files.
func TestOutbox_AppendDuplicateEventIDIsIdempotent(t *testing.T) {
	ob := newTestOutbox(t)
	if err := ob.Append(ev("dup")); err != nil {
		t.Fatalf("append dup: %v", err)
	}
	if err := ob.Append(ev("dup")); err != nil {
		t.Fatalf("append duplicate dup: %v", err)
	}
	if err := ob.Append(ev("next")); err != nil {
		t.Fatalf("append next: %v", err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 pending entries after duplicate append, got %d", len(list))
	}
	if list[0].Event.EventID != "dup" || list[0].Seq != 0 {
		t.Fatalf("first entry = %+v; want original dup at seq 0", list[0])
	}
	if list[1].Event.EventID != "next" || list[1].Seq != 1 {
		t.Fatalf("second entry = %+v; want next at seq 1", list[1])
	}
}

func retainedEv(id, artifact, origin string) proto.RemoteEvent {
	out := ev(id)
	out.ArtifactID = artifact
	out.Origin = origin
	out.Kind = "conversation"
	out.Lane = "retained"
	return out
}

func TestOutbox_AppendRetainedSupersedesOlderPendingSlot(t *testing.T) {
	ob := newTestOutbox(t)
	oldRetained := retainedEv("retained-old", "art-1", "dev-a")
	live := ev("live-old")
	live.ArtifactID = "art-1"
	live.Origin = "dev-a"
	live.Kind = "conversation"
	live.Lane = "live"
	otherOrigin := retainedEv("retained-other-origin", "art-1", "dev-b")
	newRetained := retainedEv("retained-new", "art-1", "dev-a")

	for _, e := range []proto.RemoteEvent{oldRetained, live, otherOrigin, newRetained} {
		if err := ob.Append(e); err != nil {
			t.Fatalf("append %s: %v", e.EventID, err)
		}
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(list))
	for _, e := range list {
		got = append(got, e.Event.EventID)
	}
	want := []string{"live-old", "retained-other-origin", "retained-new"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pending IDs = %v; want %v", got, want)
	}
	assertPendingAccounting(t, ob)
}

func TestOutbox_InitCompactsSupersededRetainedSlots(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, outboxDeadSubdir), outboxDirPerm); err != nil {
		t.Fatalf("mkdir dead: %v", err)
	}
	seed := []proto.RemoteEvent{
		retainedEv("retained-old-1", "art-1", "dev-a"),
		retainedEv("retained-old-2", "art-1", "dev-a"),
		ev("live-kept"),
		retainedEv("retained-new", "art-1", "dev-a"),
	}
	seed[2].ArtifactID = "art-1"
	seed[2].Origin = "dev-a"
	seed[2].Kind = "conversation"
	seed[2].Lane = "live"
	for seq, e := range seed {
		entry := outboxEntry{
			SchemaVersion: outboxSchemaVersion,
			Seq:           uint64(seq),
			EnqueuedAt:    time.Now().UTC(),
			Event:         e,
		}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal seed: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, outboxFileName(uint64(seq), e.EventID)), data, outboxFilePerm); err != nil {
			t.Fatalf("write seed: %v", err)
		}
	}

	ob := &Outbox{Root: root}
	if err := ob.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(list))
	for _, e := range list {
		got = append(got, e.Event.EventID)
	}
	want := []string{"live-kept", "retained-new"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pending IDs = %v; want %v", got, want)
	}
	assertPendingAccounting(t, ob)
}

// TestOutbox_SeqMonotonicAndOrdered: seq is monotonic and zero-padded so
// ReadDir/List order == append order.
func TestOutbox_SeqMonotonicAndOrdered(t *testing.T) {
	ob := newTestOutbox(t)
	ids := []string{"e0", "e1", "e2", "e3", "e4"}
	for _, id := range ids {
		if err := ob.Append(ev(id)); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != len(ids) {
		t.Fatalf("want %d, got %d", len(ids), len(list))
	}
	for i, e := range list {
		if e.Event.EventID != ids[i] {
			t.Fatalf("order broken at %d: want %s got %s", i, ids[i], e.Event.EventID)
		}
		if e.Seq != uint64(i) {
			t.Fatalf("seq not monotonic at %d: got %d", i, e.Seq)
		}
	}
}

// TestOutbox_RemoveOnlyNamed: Remove deletes only the named entry.
func TestOutbox_RemoveOnlyNamed(t *testing.T) {
	ob := newTestOutbox(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := ob.Append(ev(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := ob.Remove("b"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	list, _ := ob.List()
	got := map[string]bool{}
	for _, e := range list {
		got[e.Event.EventID] = true
	}
	if got["b"] || !got["a"] || !got["c"] {
		t.Fatalf("remove deleted the wrong entries: %v", got)
	}
	// Idempotent: removing a missing id is not an error.
	if err := ob.Remove("b"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	assertPendingAccounting(t, ob)
}

// TestOutbox_RemoveCleansLegacyDuplicateEventIDs covers duplicate pending
// files left by older daemons: a terminal ACCEPTED outcome must clear every
// matching file so a stale duplicate cannot keep re-publishing.
func TestOutbox_RemoveCleansLegacyDuplicateEventIDs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, outboxDeadSubdir), outboxDirPerm); err != nil {
		t.Fatalf("mkdir dead: %v", err)
	}
	for seq, id := range []string{"dup", "dup", "other"} {
		entry := outboxEntry{
			SchemaVersion: outboxSchemaVersion,
			Seq:           uint64(seq),
			EnqueuedAt:    time.Now().UTC(),
			Event:         ev(id),
		}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, outboxFileName(uint64(seq), id)), data, outboxFilePerm); err != nil {
			t.Fatalf("write seed: %v", err)
		}
	}
	ob := &Outbox{Root: root}
	if err := ob.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := ob.Remove("dup"); err != nil {
		t.Fatalf("remove dup: %v", err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Event.EventID != "other" {
		t.Fatalf("remove did not clean all duplicate dup files: %+v", list)
	}
}

// TestOutbox_DeadletterMovesAndExcludes: Deadletter moves to dead/ and the
// entry is excluded from List.
func TestOutbox_DeadletterMovesAndExcludes(t *testing.T) {
	ob := newTestOutbox(t)
	if err := ob.Append(ev("x")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ob.Deadletter("x"); err != nil {
		t.Fatalf("deadletter: %v", err)
	}
	list, _ := ob.List()
	if len(list) != 0 {
		t.Fatalf("dead-lettered entry must be excluded from List, got %d", len(list))
	}
	dead, err := os.ReadDir(ob.dead())
	if err != nil {
		t.Fatalf("read dead: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("want 1 dead entry, got %d", len(dead))
	}
	data, err := os.ReadFile(filepath.Join(ob.dead(), dead[0].Name()))
	if err != nil {
		t.Fatalf("read dead tombstone: %v", err)
	}
	if strings.Contains(string(data), `"event"`) || strings.Contains(string(data), `"bytes"`) {
		t.Fatalf("dead letter retained publish payload: %s", data)
	}
	var tombstone deadOutboxEntry
	if err := json.Unmarshal(data, &tombstone); err != nil {
		t.Fatalf("decode dead tombstone: %v", err)
	}
	if tombstone.SchemaVersion != outboxDeadSchemaVersion || tombstone.EventID != "x" {
		t.Fatalf("unexpected dead tombstone: %+v", tombstone)
	}
	assertPendingAccounting(t, ob)
}

// TestOutbox_InitCompactsLegacyDeadletterPayloads covers the on-upgrade repair
// for installs that accumulated multi-gigabyte payload-bearing files under
// state/outbox/dead. The post-start sweep must reclaim them without decoding
// the large body or blocking Init.
func TestOutbox_InitCompactsLegacyDeadletterPayloads(t *testing.T) {
	root := t.TempDir()
	deadRoot := filepath.Join(root, outboxDeadSubdir)
	if err := os.MkdirAll(deadRoot, outboxDirPerm); err != nil {
		t.Fatalf("mkdir dead: %v", err)
	}
	entry := outboxEntry{
		SchemaVersion: outboxSchemaVersion,
		Seq:           7,
		EnqueuedAt:    time.Now().UTC(),
		Event: proto.RemoteEvent{
			EventID: "legacy-large",
			Bytes:   []byte(`"` + strings.Repeat("x", 2<<20) + `"`),
		},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal legacy entry: %v", err)
	}
	name := outboxFileName(entry.Seq, entry.Event.EventID)
	path := filepath.Join(deadRoot, name)
	if err := os.WriteFile(path, data, outboxFilePerm); err != nil {
		t.Fatalf("write legacy entry: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat legacy entry: %v", err)
	}

	ob := &Outbox{Root: root}
	if err := ob.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	afterInit, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after init: %v", err)
	}
	if afterInit.Size() != before.Size() {
		t.Fatalf("Init performed bulk dead-letter migration synchronously: before=%d after=%d", before.Size(), afterInit.Size())
	}
	ob.SweepDeadLetters()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat compacted dead letter: %v", err)
	}
	if info.Size() >= outboxDeadTombstoneMaxBytes {
		t.Fatalf("legacy dead letter not compacted: %d bytes", info.Size())
	}
	if !os.SameFile(before, info) {
		t.Fatal("legacy compaction replaced the inode instead of reclaiming it in place")
	}
	compacted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read compacted dead letter: %v", err)
	}
	var tombstone deadOutboxEntry
	if err := json.Unmarshal(compacted, &tombstone); err != nil {
		t.Fatalf("decode compacted tombstone: %v", err)
	}
	if tombstone.EventID != "legacy-large" || tombstone.Seq != 7 {
		t.Fatalf("unexpected compacted tombstone: %+v", tombstone)
	}
}

func TestOutbox_InitDoesNotFollowDeadletterSymlink(t *testing.T) {
	root := t.TempDir()
	deadRoot := filepath.Join(root, outboxDeadSubdir)
	if err := os.MkdirAll(deadRoot, outboxDirPerm); err != nil {
		t.Fatalf("mkdir dead: %v", err)
	}
	sentinel := filepath.Join(root, "outside-sentinel")
	want := []byte("must not be truncated")
	if err := os.WriteFile(sentinel, want, outboxFilePerm); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	link := filepath.Join(deadRoot, outboxFileName(9, "symlink"))
	if err := os.Symlink(sentinel, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	ob := &Outbox{Root: root}
	if err := ob.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ob.SweepDeadLetters()
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("dead-letter symlink target was modified: %q", got)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat dead-letter symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dead-letter symlink was unexpectedly replaced: mode=%v", info.Mode())
	}
}

func TestOutbox_SweepDeadLettersRejectsHardlinkWithoutTruncatingTarget(t *testing.T) {
	root := t.TempDir()
	ob := &Outbox{Root: root}
	if err := ob.Init(); err != nil {
		t.Fatal(err)
	}

	sentinel := filepath.Join(t.TempDir(), "user-data.txt")
	want := bytes.Repeat([]byte("user-data-must-survive\n"), 512)
	if err := os.WriteFile(sentinel, want, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ob.dead(), outboxFileName(19, "hardlink"))
	if err := os.Link(sentinel, link); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	ob.SweepDeadLetters()

	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("dead-letter hardlink target was modified: got %d bytes, want %d", len(got), len(want))
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Size() != int64(len(want)) {
		t.Fatalf("unsafe dead-letter evidence was not preserved: got %d bytes, want %d", linkInfo.Size(), len(want))
	}
}

func seedDeadTombstone(t *testing.T, ob *Outbox, seq uint64, eventID string) int64 {
	t.Helper()
	tombstone := deadOutboxEntry{
		SchemaVersion:  outboxDeadSchemaVersion,
		Seq:            seq,
		EventID:        eventID,
		DeadletteredAt: time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(tombstone)
	if err != nil {
		t.Fatalf("marshal dead tombstone: %v", err)
	}
	path := filepath.Join(ob.dead(), outboxFileName(seq, eventID))
	if err := os.WriteFile(path, data, outboxFilePerm); err != nil {
		t.Fatalf("write dead tombstone: %v", err)
	}
	return int64(len(data))
}

func TestOutbox_PruneDeadTombstonesOldestFirstByCount(t *testing.T) {
	ob := newTestOutbox(t)
	for seq := uint64(1); seq <= 5; seq++ {
		seedDeadTombstone(t, ob, seq, "event-"+strconv.FormatUint(seq, 10))
	}
	removed, err := ob.pruneDeadTombstones(3, 1<<20)
	if err != nil {
		t.Fatalf("prune dead tombstones: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d; want 2", removed)
	}
	for seq := uint64(1); seq <= 5; seq++ {
		path := filepath.Join(ob.dead(), outboxFileName(seq, "event-"+strconv.FormatUint(seq, 10)))
		_, err := os.Stat(path)
		wantPresent := seq >= 3
		if wantPresent && err != nil {
			t.Fatalf("newer tombstone %d was removed: %v", seq, err)
		}
		if !wantPresent && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old tombstone %d was not removed: %v", seq, err)
		}
	}
}

func TestOutbox_PruneDeadTombstonesOldestFirstByBytes(t *testing.T) {
	ob := newTestOutbox(t)
	var size int64
	for seq := uint64(1); seq <= 4; seq++ {
		size = seedDeadTombstone(t, ob, seq, "event-"+strconv.FormatUint(seq, 10))
	}
	removed, err := ob.pruneDeadTombstones(10, 2*size)
	if err != nil {
		t.Fatalf("prune dead tombstones: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d; want 2", removed)
	}
	entries, err := os.ReadDir(ob.dead())
	if err != nil {
		t.Fatalf("read dead tombstones: %v", err)
	}
	if len(entries) != 2 || entries[0].Name() != outboxFileName(3, "event-3") || entries[1].Name() != outboxFileName(4, "event-4") {
		t.Fatalf("remaining tombstones=%v; want newest sequences 3 and 4", entries)
	}
}

func TestOutbox_AppendSkipsDeadletteredEventID(t *testing.T) {
	ob := newTestOutbox(t)
	if err := ob.Append(ev("x")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := ob.Deadletter("x"); err != nil {
		t.Fatalf("deadletter: %v", err)
	}
	if err := ob.Append(ev("x")); err != nil {
		t.Fatalf("append dead duplicate: %v", err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("dead-lettered duplicate must not become pending again, got %d", len(list))
	}
	dead, err := ob.findDeadFiles("x")
	if err != nil {
		t.Fatalf("find dead: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("want exactly one dead entry for duplicate id, got %d", len(dead))
	}
}

// TestOutbox_InitReseedsSeq: Init re-seeds seq past the max existing filename
// so ordering stays monotonic across restarts.
func TestOutbox_InitReseedsSeq(t *testing.T) {
	root := t.TempDir()
	ob1 := &Outbox{Root: root}
	if err := ob1.Init(); err != nil {
		t.Fatalf("init1: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := ob1.Append(ev(id)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Second instance over the same dir.
	ob2 := &Outbox{Root: root}
	if err := ob2.Init(); err != nil {
		t.Fatalf("init2: %v", err)
	}
	if err := ob2.Append(ev("d")); err != nil {
		t.Fatalf("append d: %v", err)
	}
	list, _ := ob2.List()
	if len(list) != 4 {
		t.Fatalf("want 4, got %d", len(list))
	}
	// The new event must sort LAST (highest seq), proving the reseed.
	if list[3].Event.EventID != "d" {
		t.Fatalf("reseed failed; new event not last: %+v", list)
	}
	if list[3].Seq != 3 {
		t.Fatalf("reseeded seq want 3, got %d", list[3].Seq)
	}
}

// TestOutbox_ConcurrentAppendDistinctSeqs: N concurrent Appends yield N
// distinct seqs (run with -race).
func TestOutbox_ConcurrentAppendDistinctSeqs(t *testing.T) {
	ob := newTestOutbox(t)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "e" + strconv.Itoa(i)
			if err := ob.Append(ev(id)); err != nil {
				t.Errorf("append: %v", err)
			}
		}(i)
	}
	wg.Wait()
	list, _ := ob.List()
	if len(list) != n {
		t.Fatalf("want %d entries, got %d", n, len(list))
	}
	seen := map[uint64]bool{}
	for _, e := range list {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}

// TestOutbox_TornFileRecovery: a truncated/garbage .json and a stray .tmp.*
// sibling are skipped by List (mirrors conflicts.List).
func TestOutbox_TornFileRecovery(t *testing.T) {
	ob := newTestOutbox(t)
	if err := ob.Append(ev("good")); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A torn json with a valid name but garbage body.
	torn := filepath.Join(ob.Root, outboxFileName(999, "torn"))
	if err := os.WriteFile(torn, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write torn: %v", err)
	}
	// A stray atomicfile tmp sibling.
	tmp := filepath.Join(ob.Root, outboxFileName(0, "x")+".tmp.deadbeef")
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Event.EventID != "good" {
		t.Fatalf("torn/tmp files not skipped: %+v", list)
	}
}

func TestOutbox_ListRejectsSymlinkAndReturnsBoundedFIFOPrefix(t *testing.T) {
	ob := newLimitedTestOutbox(t, 8<<10, 64<<10, 64<<10)
	for _, id := range []string{"first", "second", "third"} {
		candidate := ev(id)
		candidate.Bytes = json.RawMessage(`"` + strings.Repeat(id, 80) + `"`)
		if err := ob.Append(candidate); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	firstName, err := ob.findFile("first")
	if err != nil || firstName == "" {
		t.Fatalf("find first: %v", err)
	}
	firstInfo, err := os.Stat(filepath.Join(ob.Root, firstName))
	if err != nil {
		t.Fatalf("stat first: %v", err)
	}
	// One entry fits exactly; the next valid FIFO entry is left for the next
	// periodic scan rather than making this List allocation backlog-sized.
	ob.listMaxBytes = firstInfo.Size()

	sentinelEvent := ev("linked")
	sentinelData := marshalSeedOutboxEntry(t, 99, sentinelEvent)
	sentinel := filepath.Join(t.TempDir(), "sentinel.json")
	if err := os.WriteFile(sentinel, sentinelData, outboxFilePerm); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	link := filepath.Join(ob.Root, outboxFileName(99, "linked"))
	if err := os.Symlink(sentinel, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	list, err := ob.List()
	if err != nil {
		t.Fatalf("list bounded prefix: %v", err)
	}
	if len(list) != 1 || list[0].Event.EventID != "first" {
		t.Fatalf("first bounded prefix = %+v; want only first", list)
	}
	if err := ob.Remove("first"); err != nil {
		t.Fatalf("remove first: %v", err)
	}
	list, err = ob.List()
	if err != nil {
		t.Fatalf("list second bounded prefix: %v", err)
	}
	if len(list) != 1 || list[0].Event.EventID != "second" {
		t.Fatalf("second bounded prefix = %+v; want only second", list)
	}
	if got, err := os.ReadFile(sentinel); err != nil || !bytes.Equal(got, sentinelData) {
		t.Fatalf("List followed or modified outbox symlink: bytes_equal=%v err=%v", bytes.Equal(got, sentinelData), err)
	}
}

func TestOutbox_AppendRejectsOversizedEntryAndRetainsDirtyRescan(t *testing.T) {
	ob := newLimitedTestOutbox(t, 1<<10, 8<<10, 8<<10)
	tooLarge := ev("too-large")
	tooLarge.Bytes = json.RawMessage(`"` + strings.Repeat("x", 2<<10) + `"`)
	if err := ob.Append(tooLarge); err == nil || !strings.Contains(err.Error(), "per-entry capacity") {
		t.Fatalf("oversized append error = %v; want per-entry capacity rejection", err)
	}
	if names, err := ob.findFiles("too-large"); err != nil || len(names) != 0 {
		t.Fatalf("oversized entry was persisted: names=%v err=%v", names, err)
	}
	assertPendingAccounting(t, ob)
	assertDirtyRescanMarker(t, ob, tooLarge.NamespaceID)
}

func TestOutbox_AppendRecoveredReusesExactPersistedCiphertext(t *testing.T) {
	ob := newTestOutbox(t)
	original := ev("exact-retry")
	original.ArtifactID = "artifact-1"
	original.BranchID = "main"
	original.EventHash = watermarkTestDigest("canonical-event")
	original.Lane = "live"
	original.Bytes = json.RawMessage(`{"sealed":"first-random-seal"}`)
	if err := ob.Append(original); err != nil {
		t.Fatal(err)
	}

	resealed := original
	resealed.Bytes = json.RawMessage(`{"sealed":"different-random-seal"}`)
	persisted, err := ob.AppendRecovered(resealed, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted.Bytes) != string(original.Bytes) {
		t.Fatalf("recovery replaced exact retry ciphertext: got %s want %s", persisted.Bytes, original.Bytes)
	}
	entries, err := ob.List()
	if err != nil || len(entries) != 1 || string(entries[0].Event.Bytes) != string(original.Bytes) {
		t.Fatalf("durable exact retry authority changed: entries=%+v err=%v", entries, err)
	}

	conflict := resealed
	conflict.SecurityGeneration++
	if _, err := ob.AppendRecovered(conflict, false); !errors.Is(err, ErrOutboxRecoveryAuthorityConflict) {
		t.Fatalf("generation conflict error=%v; want typed checkpoint conflict", err)
	}
	entries, err = ob.List()
	if err != nil || len(entries) != 1 || string(entries[0].Event.Bytes) != string(original.Bytes) {
		t.Fatalf("authority conflict destroyed old exact bytes: entries=%+v err=%v", entries, err)
	}
}

func TestOutbox_AppendForPublishDuplicateQueuesPersistedAuthority(t *testing.T) {
	ob := newTestOutbox(t)
	original := ev("normal-exact-retry")
	original.ArtifactID = "artifact-1"
	original.BranchID = "main"
	original.EventHash = watermarkTestDigest("normal-event")
	original.Lane = "live"
	original.Bytes = json.RawMessage(`{"sealed":"original"}`)
	original.BodyDigest = sealedBodyDigest(original.Bytes)
	if err := ob.Append(original); err != nil {
		t.Fatal(err)
	}

	resealed := original
	resealed.Bytes = json.RawMessage(`{"sealed":"new-randomness"}`)
	resealed.BodyDigest = sealedBodyDigest(resealed.Bytes)
	persisted, dirty, err := ob.AppendForPublish(resealed)
	if err != nil || dirty {
		t.Fatalf("exact duplicate = dirty %v, err %v", dirty, err)
	}
	if string(persisted.Bytes) != string(original.Bytes) {
		t.Fatalf("normal duplicate queued fresh reseal: got %s want %s", persisted.Bytes, original.Bytes)
	}

	conflict := resealed
	conflict.SecurityGeneration = 2
	if _, _, err := ob.AppendForPublish(conflict); !errors.Is(err, ErrOutboxRecoveryAuthorityConflict) {
		t.Fatalf("normal generation conflict error=%v", err)
	}
	assertDirtyRescanMarker(t, ob, original.NamespaceID)
	entries, err := ob.List()
	if err != nil || len(entries) != 1 || string(entries[0].Event.Bytes) != string(original.Bytes) {
		t.Fatalf("normal conflict changed exact bytes: entries=%+v err=%v", entries, err)
	}
}

func TestOutbox_AppendRecoveredCreatesOnlyExplicitMarkerTarget(t *testing.T) {
	ob := newTestOutbox(t)
	event := ev("missing-authority")
	event.ArtifactID = "artifact"
	event.BranchID = "main"
	event.EventHash = watermarkTestDigest("missing-authority")
	event.Lane = "live"
	event.Bytes = json.RawMessage(`{"sealed":"new"}`)
	if _, err := ob.AppendRecovered(event, false); !errors.Is(err, ErrOutboxRecoveryAuthorityUnavailable) {
		t.Fatalf("unbound reseal error=%v; want exact-authority checkpoint", err)
	}
	if names, err := ob.findFiles(event.EventID); err != nil || len(names) != 0 {
		t.Fatalf("unbound reseal was persisted: names=%v err=%v", names, err)
	}
	persisted, err := ob.AppendRecovered(event, true)
	if err != nil || persisted.EventID != event.EventID {
		t.Fatalf("explicit marker target append=%+v err=%v", persisted, err)
	}
}

func TestOutbox_AggregateBudgetPersistsAcrossRestartAndRemovalFreesCapacity(t *testing.T) {
	root := t.TempDir()
	ob := &Outbox{Root: root, maxEntries: 10, maxEntryBytes: 8 << 10, maxPendingBytes: 32 << 10, listMaxBytes: 32 << 10}
	if err := ob.Init(); err != nil {
		t.Fatalf("init first outbox: %v", err)
	}
	if err := ob.Append(ev("first")); err != nil {
		t.Fatalf("append first: %v", err)
	}
	firstBytes := ob.pendingBytes
	if firstBytes <= 1 {
		t.Fatalf("unexpected first entry size: %d", firstBytes)
	}

	// Reopen the same durable directory and prove exact aggregate accounting is
	// reconstructed before the next capacity decision.
	ob = &Outbox{Root: root, maxEntries: 10, maxEntryBytes: 8 << 10, maxPendingBytes: firstBytes + 256, listMaxBytes: 32 << 10}
	if err := ob.Init(); err != nil {
		t.Fatalf("restart init: %v", err)
	}
	assertPendingAccounting(t, ob)
	if ob.pendingBytes != firstBytes {
		t.Fatalf("restart pending bytes=%d; want %d", ob.pendingBytes, firstBytes)
	}
	if err := ob.Append(ev("second")); err == nil || !strings.Contains(err.Error(), "aggregate byte capacity") {
		t.Fatalf("aggregate append error = %v; want byte-cap rejection", err)
	}
	assertDirtyRescanMarker(t, ob, "ns-1")
	if err := ob.Remove("first"); err != nil {
		t.Fatalf("remove first: %v", err)
	}
	assertPendingAccounting(t, ob)
	if err := ob.Append(ev("second")); err != nil {
		t.Fatalf("append after capacity was freed: %v", err)
	}
	assertPendingAccounting(t, ob)
}

func TestOutbox_InitPreservesEveryOverBudgetPendingCiphertext(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, outboxDeadSubdir), outboxDirPerm); err != nil {
		t.Fatalf("mkdir dead: %v", err)
	}
	var sizes []int64
	for seq, id := range []string{"keep-a", "keep-b", "discard-budget"} {
		data := marshalSeedOutboxEntry(t, uint64(seq), ev(id))
		path := filepath.Join(root, outboxFileName(uint64(seq), id))
		if err := os.WriteFile(path, data, outboxFilePerm); err != nil {
			t.Fatalf("write seed %s: %v", id, err)
		}
		sizes = append(sizes, int64(len(data)))
	}
	entryLimit := int64(4 << 10)
	oversizedName := outboxFileName(3, "discard-oversized")
	oversizedPath := filepath.Join(root, oversizedName)
	if err := os.WriteFile(oversizedPath, []byte("x"), outboxFilePerm); err != nil {
		t.Fatalf("create oversized seed: %v", err)
	}
	if err := os.Truncate(oversizedPath, entryLimit+1); err != nil {
		t.Fatalf("extend oversized seed: %v", err)
	}

	ob := &Outbox{
		Root:            root,
		maxEntries:      10,
		maxEntryBytes:   entryLimit,
		maxPendingBytes: sizes[0] + sizes[1],
		listMaxBytes:    16 << 10,
	}
	if err := ob.Init(); err != nil {
		t.Fatalf("bounded migration init: %v", err)
	}
	assertPendingAccounting(t, ob)
	if ob.count != 4 {
		t.Fatalf("restart pending count=%d; want every one of 4 exact intents preserved", ob.count)
	}
	for _, id := range []string{"keep-a", "keep-b", "discard-budget", "discard-oversized"} {
		if names, err := ob.findFiles(id); err != nil || len(names) != 1 {
			t.Fatalf("restart did not preserve %s byte-for-byte: names=%v err=%v", id, names, err)
		}
	}
}

func TestOutbox_TerminalAndRetainedPathsMaintainByteAccounting(t *testing.T) {
	ob := newTestOutbox(t)
	removeEvent := ev("remove")
	deadEvent := ev("dead")
	purgeEvent := ev("purge")
	purgeEvent.ProjectID = "project-a"
	keepEvent := ev("keep")
	keepEvent.ProjectID = "project-b"
	for _, event := range []proto.RemoteEvent{removeEvent, deadEvent, purgeEvent, keepEvent} {
		if err := ob.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.EventID, err)
		}
	}
	assertPendingAccounting(t, ob)
	if err := ob.Remove(removeEvent.EventID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	assertPendingAccounting(t, ob)
	if err := ob.Deadletter(deadEvent.EventID); err != nil {
		t.Fatalf("deadletter: %v", err)
	}
	assertPendingAccounting(t, ob)
	if purged, err := ob.PurgeProject("project-a"); err != nil || purged != 1 {
		t.Fatalf("purge project: purged=%d err=%v", purged, err)
	}
	assertPendingAccounting(t, ob)
	list, err := ob.List()
	if err != nil || len(list) != 1 || list[0].Event.EventID != keepEvent.EventID {
		t.Fatalf("terminal paths left unexpected pending list=%+v err=%v", list, err)
	}

	oldRetained := retainedEv("retained-old-accounting", "artifact-r", "device-r")
	newRetained := retainedEv("retained-new-accounting", "artifact-r", "device-r")
	if err := ob.Append(oldRetained); err != nil {
		t.Fatalf("append old retained: %v", err)
	}
	assertPendingAccounting(t, ob)
	if err := ob.Append(newRetained); err != nil {
		t.Fatalf("append new retained: %v", err)
	}
	assertPendingAccounting(t, ob)
	if names, err := ob.findFiles(oldRetained.EventID); err != nil || len(names) != 0 {
		t.Fatalf("superseded retained file remains: names=%v err=%v", names, err)
	}
}

func TestOutbox_LegacyEvictionMaintainsByteAccounting(t *testing.T) {
	ob := newTestOutbox(t)
	if err := ob.Append(ev("oldest")); err != nil {
		t.Fatalf("append oldest: %v", err)
	}
	if err := ob.Append(ev("newest")); err != nil {
		t.Fatalf("append newest: %v", err)
	}
	ob.mu.Lock()
	ob.evictOldestLocked()
	ob.mu.Unlock()
	assertPendingAccounting(t, ob)
	list, err := ob.List()
	if err != nil || len(list) != 1 || list[0].Event.EventID != "newest" {
		t.Fatalf("legacy eviction pending list=%+v err=%v", list, err)
	}
}

// TestOutbox_CapRejectsWithoutEvictingLiveIntent: live publication obligations
// are never sacrificed to admit newer work.
func TestOutbox_CapRejectsWithoutEvictingLiveIntent(t *testing.T) {
	ob := newTestOutbox(t)
	// Seed count to the cap so the next Append is rejected without eviction.
	if err := ob.Append(ev("oldest")); err != nil {
		t.Fatalf("append oldest: %v", err)
	}
	ob.mu.Lock()
	ob.count = outboxMaxEntries
	ob.mu.Unlock()
	if err := ob.Append(ev("newest")); err == nil {
		t.Fatal("append above cap unexpectedly succeeded")
	}
	list, _ := ob.List()
	ids := map[string]bool{}
	for _, e := range list {
		ids[e.Event.EventID] = true
	}
	if !ids["oldest"] {
		t.Fatalf("oldest live intent must remain pending")
	}
	if ids["newest"] {
		t.Fatalf("newest must not be persisted above cap")
	}
	dead, _ := os.ReadDir(ob.dead())
	if len(dead) != 0 {
		t.Fatalf("cap must not dead-letter live work, got %d", len(dead))
	}
	assertDirtyRescanMarker(t, ob, "ns-1")
}

// TestOutbox_WarnRateLimited: appending past outboxWarnEntries emits a single
// rate-limited Warn within the interval (interval lowered via a seam).
func TestOutbox_WarnRateLimited(t *testing.T) {
	rec := &recordingLogger{}
	ob := &Outbox{Root: t.TempDir(), logger: rec}
	if err := ob.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ob.warnInterval = time.Hour // ensure at most one within the test
	ob.mu.Lock()
	ob.count = outboxWarnEntries - 1
	ob.mu.Unlock()
	for i := 0; i < 5; i++ {
		if err := ob.Append(ev("w" + strconv.Itoa(i))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if got := rec.warnCount("backlog growing"); got != 1 {
		t.Fatalf("want exactly 1 rate-limited backlog warn, got %d", got)
	}
}

func TestOutbox_ByteBacklogWarnRateLimited(t *testing.T) {
	rec := &recordingLogger{}
	ob := &Outbox{Root: t.TempDir(), logger: rec}
	if err := ob.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	ob.warnInterval = time.Hour
	ob.mu.Lock()
	ob.pendingBytes = outboxWarnBytes
	ob.maybeWarnLocked()
	ob.maybeWarnLocked()
	ob.mu.Unlock()
	if got := rec.warnCount("backlog growing"); got != 1 {
		t.Fatalf("want exactly 1 rate-limited byte backlog warn, got %d", got)
	}
}

// recordingLogger captures log lines for assertions.
type recordingLogger struct {
	mu    sync.Mutex
	infos []string
	warns []string
	errs  []string
}

func (l *recordingLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	l.infos = append(l.infos, msg)
	l.mu.Unlock()
}
func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
}
func (l *recordingLogger) Error(msg string, _ ...any) {
	l.mu.Lock()
	l.errs = append(l.errs, msg)
	l.mu.Unlock()
}
func (l *recordingLogger) warnCount(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, w := range l.warns {
		if strings.Contains(w, substr) {
			n++
		}
	}
	return n
}
