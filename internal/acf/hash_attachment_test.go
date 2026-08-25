package acf

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// eventWithAttachment builds a conversation event carrying a single
// attachment, optionally populating the in-memory Data bytes.
func eventWithAttachment(t *testing.T, withData bool, evicted *EvictedInfo) Event {
	t.Helper()
	att := Attachment{
		Kind:        "image",
		MimeType:    "image/png",
		ContentHash: "abc123def456",
		Bytes:       8,
		Evicted:     evicted,
	}
	if withData {
		att.Data = []byte("rawbytes")
	}
	payload, err := EncodePayload(ConversationPayload{
		Format:      ConversationFormatV1,
		Attachments: []Attachment{att},
	})
	require.NoError(t, err)
	return Event{
		EventID:    "01956a39-1111-7890-abcd-ef0123456789",
		ArtifactID: "01956a39-2222-7890-abcd-ef0123456789",
		Type:       EventTypeCreate,
		Timestamp:  time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
		Payload:    payload,
	}
}

// TestComputeHash_AttachmentDataExcluded is the attachment-hash golden test: the raw
// attachment bytes (Attachment.Data, tagged json:"-") MUST NOT affect the
// event hash. Whether Data is populated or nil in memory, ComputeHash is
// byte-identical — this is the structural property that makes blob
// eviction append-only (an evicted blob can never have perturbed a
// historical event hash).
func TestComputeHash_AttachmentDataExcluded(t *testing.T) {
	withBytes := eventWithAttachment(t, true, nil)
	withoutBytes := eventWithAttachment(t, false, nil)

	hWith, err := ComputeHash(withBytes)
	require.NoError(t, err)
	hWithout, err := ComputeHash(withoutBytes)
	require.NoError(t, err)

	require.Equal(t, hWithout, hWith,
		"attachment Data must be excluded from the hash (populated vs nil)")

	// And the on-wire payloads must be byte-identical too — Data never
	// reaches json.Marshal.
	require.Equal(t, string(withoutBytes.Payload), string(withBytes.Payload),
		"attachment Data must never be serialized")
}

// TestComputeHash_EvictedMarkerChangesHash proves the complement: setting
// the canonical Evicted marker DOES change the hash. The marker is canonical
// content (it is serialized); the bytes are not. This is why re-asserting an
// evicted payload as a NEW appended event yields a distinct hash without
// disturbing the original event.
func TestComputeHash_EvictedMarkerChangesHash(t *testing.T) {
	plain := eventWithAttachment(t, true, nil)
	evictedMarker := &EvictedInfo{
		At:           time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Reason:       "age",
		OriginalSize: 8,
		ContentHash:  "abc123def456",
	}
	evicted := eventWithAttachment(t, false, evictedMarker)

	hPlain, err := ComputeHash(plain)
	require.NoError(t, err)
	hEvicted, err := ComputeHash(evicted)
	require.NoError(t, err)

	require.NotEqual(t, hPlain, hEvicted,
		"setting the Evicted marker must change the event hash (canonical content)")
}
