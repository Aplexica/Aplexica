package acf

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/blobstore"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

// appendConvCreate writes a conversation create event whose payload carries
// the given attachments. ParentHash is "" (genesis).
func appendConvCreate(t *testing.T, s *Store, artifactID string, atts ...Attachment) {
	t.Helper()
	payload, err := EncodePayload(ConversationPayload{
		Format:      ConversationFormatV1,
		Attachments: atts,
	})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: artifactID,
		Type:       EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: Provenance{DeviceID: "dev", SourceAgent: "test", AdapterVersion: "0.0.0"},
		Payload:    payload,
		ParentHash: "",
	}))
}

// TestBundleRestore_PreservesAttachmentBytes is the attachment-bundle guarantee: a
// non-evicted attachment's blob bytes survive a Bundle -> Restore round-trip
// into a fresh store, and the event chain stays green (the chain hashes
// metadata + ContentHash, never the bytes).
func TestBundleRestore_PreservesAttachmentBytes(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	// Put a blob into the source store's content-addressed blob store.
	raw := []byte("the attachment bytes \x00\x01\x02 binary-safe")
	srcBlobs := &blobstore.Store{Root: src.BlobsDir()}
	hash, err := srcBlobs.Put(raw)
	require.NoError(t, err)

	// A conversation artifact whose create event references the blob by hash.
	a := newTestArtifact(NewID())
	a.Kind = KindConversation
	a.Name = "conversation-with-image"
	require.NoError(t, src.WriteArtifact(a))
	appendConvCreate(t, src, a.ArtifactID, Attachment{
		Kind:        "image",
		MimeType:    "image/png",
		ContentHash: hash,
		Bytes:       int64(len(raw)),
	})

	// Bundle to a buffer.
	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	// Restore into a FRESH store.
	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	// The blob must be present in the new store, byte-identical.
	dstBlobs := &blobstore.Store{Root: dst.BlobsDir()}
	require.True(t, dstBlobs.Has(hash), "restored store must contain the attachment blob")
	rc, err := dstBlobs.Open(hash)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, raw, got, "restored blob bytes must be byte-identical")

	// Chain stays green across bundle->restore.
	events, err := dst.ReadEvents(KindConversation, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NoError(t, VerifyChain(events))
}

// TestBundleRestore_EvictedAttachmentMarkerOnly: an attachment whose blob has
// already been GC'd (Evicted marker set, no blob on disk) round-trips its
// marker with no blob, and neither Bundle nor Restore errors on the absence.
func TestBundleRestore_EvictedAttachmentMarkerOnly(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())

	const goneHash = "0000000000000000000000000000000000000000000000000000000000000000"
	a := newTestArtifact(NewID())
	a.Kind = KindConversation
	a.Name = "conversation-evicted"
	require.NoError(t, src.WriteArtifact(a))
	appendConvCreate(t, src, a.ArtifactID, Attachment{
		Kind:        "image",
		MimeType:    "image/png",
		ContentHash: goneHash,
		Bytes:       1234,
		Evicted: &EvictedInfo{
			At:           time.Now().UTC(),
			Reason:       "age",
			OriginalSize: 1234,
			ContentHash:  goneHash,
		},
	})

	// Bundle + Restore must not error despite the missing (evicted) blob.
	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	// No blob should have been written for the evicted slot.
	dstBlobs := &blobstore.Store{Root: dst.BlobsDir()}
	require.False(t, dstBlobs.Has(goneHash), "no blob expected for an evicted attachment")

	// The evicted marker round-trips in the payload, chain intact.
	events, err := dst.ReadEvents(KindConversation, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NoError(t, VerifyChain(events))
	p, err := DecodeConversationPayload(events[0])
	require.NoError(t, err)
	require.Len(t, p.Attachments, 1)
	require.True(t, p.Attachments[0].IsEvicted(), "evicted marker must survive the round-trip")
}

// TestBundleRestore_FilteredBundleIncludesOnlySelectedArtifactBlobs: a bundle
// scoped to one project artifact (BundleOpts.ProjectFilter) carries only that
// artifact's blob, not the other artifact's.
func TestBundleRestore_FilteredBundleIncludesOnlySelectedArtifactBlobs(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	srcBlobs := &blobstore.Store{Root: src.BlobsDir()}

	raw1 := []byte("blob for project A")
	raw2 := []byte("blob for project B")
	hash1, err := srcBlobs.Put(raw1)
	require.NoError(t, err)
	hash2, err := srcBlobs.Put(raw2)
	require.NoError(t, err)

	mkConv := func(projID, name, hash string, n int) {
		a := Artifact{
			AcfSchemaVersion: SchemaVersion,
			ArtifactID:       NewID(),
			Kind:             KindConversation,
			Scope:            ScopeProject,
			Project:          &project.ProjectInfo{ID: projID, Path: "/" + name, VCS: "git"},
			Name:             name,
			CreatedAt:        time.Now().UTC(),
			UpdatedAt:        time.Now().UTC(),
		}
		require.NoError(t, src.WriteArtifact(a))
		appendConvCreate(t, src, a.ArtifactID, Attachment{
			Kind:        "file",
			MimeType:    "application/octet-stream",
			ContentHash: hash,
			Bytes:       int64(n),
		})
	}
	mkConv("github.com/a/x", "conv-a", hash1, len(raw1))
	mkConv("github.com/b/y", "conv-b", hash2, len(raw2))

	// Bundle ONLY project a/x.
	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{
		AplexicaVersion: "0.1.10",
		ProjectFilter:   []string{"github.com/a/x"},
	}))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	dstBlobs := &blobstore.Store{Root: dst.BlobsDir()}
	require.True(t, dstBlobs.Has(hash1), "selected artifact's blob MUST be bundled")
	require.False(t, dstBlobs.Has(hash2), "unselected artifact's blob MUST NOT be bundled")
}

// TestBundleRestore_EvictedAfterCreate_SkipsGCdBlob guards the latest-wins
// rule: a blob created non-evicted then re-asserted evicted by a later append
// (and subsequently GC'd) must NOT be collected for the bundle. Collecting
// every non-evicted reference naively would try to read a deleted blob and
// fail the backup.
func TestBundleRestore_EvictedAfterCreate_SkipsGCdBlob(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	srcBlobs := &blobstore.Store{Root: src.BlobsDir()}

	raw := []byte("attachment that will be evicted")
	hash, err := srcBlobs.Put(raw)
	require.NoError(t, err)

	a := newTestArtifact(NewID())
	a.Kind = KindConversation
	a.Name = "conv-evict-after-create"
	require.NoError(t, src.WriteArtifact(a))

	att := Attachment{Kind: "image", MimeType: "image/png", ContentHash: hash, Bytes: int64(len(raw))}
	// Event 1: create, non-evicted.
	appendConvCreate(t, src, a.ArtifactID, att)

	head, err := src.HeadHash(KindConversation, a.ArtifactID)
	require.NoError(t, err)

	// Event 2: update re-asserting the slot as evicted.
	attEvicted := att
	attEvicted.Evicted = &EvictedInfo{At: time.Now().UTC(), Reason: "age", OriginalSize: int64(len(raw)), ContentHash: hash}
	p2, err := EncodePayload(ConversationPayload{Format: ConversationFormatV1, Attachments: []Attachment{attEvicted}})
	require.NoError(t, err)
	require.NoError(t, src.AppendEvent(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: a.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		Provenance: Provenance{DeviceID: "dev", SourceAgent: "test", AdapterVersion: "0.0.0"},
		Payload:    p2,
		ParentHash: head,
	}))

	// Simulate GC: the blob is gone because its latest assertion is evicted.
	require.NoError(t, srcBlobs.Delete(hash))

	// Bundle must NOT error on the now-missing blob.
	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	dstBlobs := &blobstore.Store{Root: dst.BlobsDir()}
	require.False(t, dstBlobs.Has(hash), "GC'd-then-evicted blob must not be in the bundle")

	events, err := dst.ReadEvents(KindConversation, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.NoError(t, VerifyChain(events))
}

// TestBundleRestore_SnapshotOnlyReference_PreservesBlob: a blob whose ONLY
// reference lives in a payload-bearing snapshot checkpoint (FR-02.32 — the
// post-prune shape, after the compacted segment was grace-deleted) must still
// be carried by the bundle. Omitting it would leave the restored checkpoint
// with a dangling non-evicted ContentHash (blobstore.Open fails) and lose the
// attachment bytes.
func TestBundleRestore_SnapshotOnlyReference_PreservesBlob(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	srcBlobs := &blobstore.Store{Root: src.BlobsDir()}

	raw := []byte("blob referenced only by a snapshot checkpoint")
	hash, err := srcBlobs.Put(raw)
	require.NoError(t, err)

	a := newTestArtifact(NewID())
	a.Kind = KindConversation
	a.Name = "conv-snapshot-only"
	require.NoError(t, src.WriteArtifact(a))
	payload, err := EncodePayload(ConversationPayload{
		Format: ConversationFormatV1,
		Attachments: []Attachment{{
			Kind: "image", MimeType: "image/png", ContentHash: hash, Bytes: int64(len(raw)),
		}},
	})
	require.NoError(t, err)
	require.NoError(t, src.AppendEvent(KindConversation, Event{
		EventID:       NewID(),
		ArtifactID:    a.ArtifactID,
		Type:          EventTypeSnapshot,
		Timestamp:     time.Now().UTC(),
		SnapshotState: "sha256:test",
		Payload:       payload,
	}))

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	dstBlobs := &blobstore.Store{Root: dst.BlobsDir()}
	require.True(t, dstBlobs.Has(hash),
		"a blob referenced only by a snapshot checkpoint must be bundled")
	rc, err := dstBlobs.Open(hash)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, rc.Close())
	require.NoError(t, err)
	require.Equal(t, raw, got, "restored blob bytes must be byte-identical")
}

// TestBundleRestore_BaselineOnlyReference_PreservesBlob is the aligned-chains
// flavor: on an adopting device the baseline event is the only event naming
// the attachment's ContentHash (the origin history never existed locally), so
// the bundle must carry the blob for the restore to resolve it.
func TestBundleRestore_BaselineOnlyReference_PreservesBlob(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	srcBlobs := &blobstore.Store{Root: src.BlobsDir()}

	raw := []byte("blob referenced only by an adopted baseline")
	hash, err := srcBlobs.Put(raw)
	require.NoError(t, err)

	a := newTestArtifact(NewID())
	a.Kind = KindConversation
	a.Name = "conv-baseline-only"
	require.NoError(t, src.WriteArtifact(a))
	payload, err := EncodePayload(ConversationPayload{
		Format: ConversationFormatV1,
		Attachments: []Attachment{{
			Kind: "image", MimeType: "image/png", ContentHash: hash, Bytes: int64(len(raw)),
		}},
	})
	require.NoError(t, err)
	require.NoError(t, src.AdoptBaseline(KindConversation, Event{
		EventID:        NewID(),
		ArtifactID:     a.ArtifactID,
		Type:           EventTypeBaseline,
		Timestamp:      time.Now().UTC(),
		Payload:        payload,
		AlignedHead:    "origin-head-hash",
		AlignedEventID: "origin-event-id",
	}))

	var buf bytes.Buffer
	require.NoError(t, src.Bundle(&buf, BundleOpts{AplexicaVersion: "0.1.10"}))

	dst := &Store{Root: filepath.Join(t.TempDir(), "dst")}
	require.NoError(t, dst.Init())
	require.NoError(t, dst.RestoreWithOptions(&buf, "", RestoreOptions{UnsignedOK: true}))

	dstBlobs := &blobstore.Store{Root: dst.BlobsDir()}
	require.True(t, dstBlobs.Has(hash),
		"a blob referenced only by a baseline checkpoint must be bundled")
}

// TestBundle_AnonymizeOmitsAttachmentBlobs: an anonymized bundle is a
// sanitized copy for sharing/review. The scrubber cannot sanitize binary
// attachment bytes, so they MUST NOT be shipped raw — mirroring the existing
// Anonymize/SecretsRoot mutual exclusion.
func TestBundle_AnonymizeOmitsAttachmentBlobs(t *testing.T) {
	src := &Store{Root: filepath.Join(t.TempDir(), "src")}
	require.NoError(t, src.Init())
	srcBlobs := &blobstore.Store{Root: src.BlobsDir()}
	raw := []byte("binary attachment that cannot be scrubbed")
	hash, err := srcBlobs.Put(raw)
	require.NoError(t, err)

	a := newTestArtifact(NewID())
	a.Kind = KindConversation
	a.Name = "conv-anon"
	require.NoError(t, src.WriteArtifact(a))
	appendConvCreate(t, src, a.ArtifactID, Attachment{
		Kind:        "image",
		MimeType:    "image/png",
		ContentHash: hash,
		Bytes:       int64(len(raw)),
	})

	// bundleAndListNames (bundle_scope_test.go) returns archive entry names
	// excluding meta.json.
	names := bundleAndListNames(t, src, BundleOpts{AplexicaVersion: "test", Anonymize: true})
	for _, n := range names {
		require.False(t, strings.HasPrefix(n, blobsDirName+"/"),
			"anonymized bundle must not ship raw attachment blobs (got %q)", n)
	}
}
