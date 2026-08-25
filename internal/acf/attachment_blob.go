package acf

import (
	"fmt"
	"io"

	"github.com/aplexica/aplexica/internal/blobstore"
)

// HydrateAttachment populates att.Data on demand from the store's
// content-addressed blob store (internal/blobstore) by ContentHash. The raw
// attachment bytes live out-of-line; Attachment.Data is transient
// (json:"-", excluded from the event hash) and is nil until a consumer needs
// the bytes — materialization to agent-native files, the web API, export.
//
// Resolution rules:
//   - att.Data already populated  -> no-op (no blob I/O).
//   - att.Evicted != nil          -> no-op; the bytes are intentionally gone
//     (the retention engine GC'd the blob), and the caller renders the
//     evicted marker. Not an error.
//   - att.ContentHash == ""       -> no-op; nothing to resolve.
//   - otherwise                   -> Data is loaded from the blob store. A
//     missing blob for a non-evicted attachment is a real dangling
//     reference and returns an error.
//
// It is the single read path future producers/consumers should use, so blob
// resolution lives in one place.
func (s *Store) HydrateAttachment(att *Attachment) error {
	if att == nil {
		return nil
	}
	if len(att.Data) > 0 {
		return nil
	}
	if att.Evicted != nil {
		return nil
	}
	if att.ContentHash == "" {
		return nil
	}
	blobs := &blobstore.Store{Root: s.BlobsDir()}
	rc, err := blobs.Open(att.ContentHash)
	if err != nil {
		return fmt.Errorf("acf: hydrate attachment %s: %w", att.ContentHash, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("acf: read attachment blob %s: %w", att.ContentHash, err)
	}
	att.Data = data
	return nil
}

// HydrateAttachments populates Data for every attachment in the payload via
// HydrateAttachment. Convenience over the per-attachment call for the common
// ConversationPayload case (materialization/web/export). Evicted and
// already-populated attachments are skipped; a missing blob for any
// non-evicted attachment aborts and returns the error.
func (s *Store) HydrateAttachments(p *ConversationPayload) error {
	if p == nil {
		return nil
	}
	for i := range p.Attachments {
		if err := s.HydrateAttachment(&p.Attachments[i]); err != nil {
			return err
		}
	}
	return nil
}
