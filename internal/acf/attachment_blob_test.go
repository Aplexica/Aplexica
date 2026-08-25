package acf

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/blobstore"
	"github.com/stretchr/testify/require"
)

// TestHydrateAttachment_LoadsBytesFromBlobStore: a non-evicted attachment with
// empty Data has its bytes resolved on demand from the content-addressed blob
// store by ContentHash.
func TestHydrateAttachment_LoadsBytesFromBlobStore(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	blobs := &blobstore.Store{Root: s.BlobsDir()}
	raw := []byte("hydrate me \x00\x01")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	att := Attachment{Kind: "image", MimeType: "image/png", ContentHash: hash, Bytes: int64(len(raw))}
	require.Empty(t, att.Data)
	require.NoError(t, s.HydrateAttachment(&att))
	require.Equal(t, raw, att.Data, "Data must be populated from the blob store")
}

// TestHydrateAttachment_EvictedStaysNil: an evicted attachment's bytes are
// intentionally gone — hydration is a no-op, not an error.
func TestHydrateAttachment_EvictedStaysNil(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	att := Attachment{
		Kind: "image", MimeType: "image/png",
		ContentHash: "deadbeef", Bytes: 10,
		Evicted: &EvictedInfo{At: time.Now().UTC(), Reason: "age", OriginalSize: 10, ContentHash: "deadbeef"},
	}
	require.NoError(t, s.HydrateAttachment(&att), "evicted attachment is not an error")
	require.Nil(t, att.Data, "evicted attachment Data stays nil (bytes intentionally gone)")
}

// TestHydrateAttachment_AlreadyPopulatedUnchanged: an attachment already
// carrying Data short-circuits before any blob lookup (the ContentHash here is
// bogus, proving the blob store is not consulted).
func TestHydrateAttachment_AlreadyPopulatedUnchanged(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	att := Attachment{ContentHash: "not-a-real-hash", Data: []byte("already here")}
	require.NoError(t, s.HydrateAttachment(&att))
	require.Equal(t, []byte("already here"), att.Data)
}

// TestHydrateAttachment_MissingNonEvictedBlobErrors: a non-evicted attachment
// whose blob is absent is a real dangling reference and surfaces an error.
func TestHydrateAttachment_MissingNonEvictedBlobErrors(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	att := Attachment{ContentHash: "0000000000000000000000000000000000000000000000000000000000000000", Bytes: 5}
	require.Error(t, s.HydrateAttachment(&att))
}

// TestHydrateAttachments_PopulatesAllNonEvicted: the slice convenience over a
// ConversationPayload hydrates every non-evicted attachment and leaves evicted
// ones nil.
func TestHydrateAttachments_PopulatesAllNonEvicted(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	blobs := &blobstore.Store{Root: s.BlobsDir()}
	raw := []byte("payload bytes")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	p := &ConversationPayload{
		Format: ConversationFormatV1,
		Attachments: []Attachment{
			{Kind: "image", ContentHash: hash, Bytes: int64(len(raw))},
			{Kind: "image", ContentHash: "x", Evicted: &EvictedInfo{Reason: "age"}},
		},
	}
	require.NoError(t, s.HydrateAttachments(p))
	require.Equal(t, raw, p.Attachments[0].Data, "non-evicted attachment is hydrated")
	require.Nil(t, p.Attachments[1].Data, "evicted attachment stays nil")
}
