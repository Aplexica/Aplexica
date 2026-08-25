package syncd

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/stretchr/testify/require"
)

// sampleEvent builds a canonical event whose payload carries a recognisable
// plaintext marker for leak assertions.
func sampleEvent() acf.Event {
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "TOPSECRET-MARKER"})
	return acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: acf.NewID(),
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "dev-A", SourceAgent: "claude-code"},
		Payload:    payload,
	}
}

// TestEnvelope_RoundTripAcrossDevices is the core E2E test: device A seals an
// event to a recipient set that includes device B's wrap pubkey; device B
// opens it with its own private key.
func TestEnvelope_RoundTripAcrossDevices(t *testing.T) {
	privA, pubA, err := keys.NewDeviceKey()
	require.NoError(t, err)
	privB, pubB, err := keys.NewDeviceKey()
	require.NoError(t, err)
	_ = privA

	ev := sampleEvent()

	recipients := []recipient{
		{deviceID: "dev-A", pub: pubA},
		{deviceID: "dev-B", pub: pubB},
	}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, recipients)
	require.NoError(t, err)

	// Zero-knowledge: the sealed bytes must NOT contain the plaintext marker.
	require.False(t, bytes.Contains(sealed, []byte("TOPSECRET-MARKER")),
		"sealed envelope leaks plaintext")
	// It must be valid JSON (the plugin passes Bytes through as json.RawMessage).
	require.True(t, json.Valid(sealed), "envelope must be valid JSON")

	// Device B opens with its private key.
	got, _, _, err := openEnvelope(sealed, "dev-B", privB)
	require.NoError(t, err)
	require.Equal(t, ev.EventID, got.EventID)
	require.Equal(t, ev.ArtifactID, got.ArtifactID)
	require.Equal(t, string(ev.Payload), string(got.Payload))

	// Device A can also open its own re-import (sender is in the recipient set).
	gotA, _, _, err := openEnvelope(sealed, "dev-A", privA)
	require.NoError(t, err)
	require.Equal(t, ev.EventID, gotA.EventID)
}

// TestEnvelope_NonRecipientSkips verifies a device NOT in the recipient set
// gets ErrNotARecipient (the caller skips, not errors the stream).
func TestEnvelope_NonRecipientSkips(t *testing.T) {
	_, pubA, _ := keys.NewDeviceKey()
	privC, _, _ := keys.NewDeviceKey()
	ev := sampleEvent()
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-A", pub: pubA}})
	require.NoError(t, err)

	_, _, _, err = openEnvelope(sealed, "dev-C", privC)
	require.ErrorIs(t, err, errNotARecipient)
}

// TestEnvelope_TamperFails verifies a flipped ciphertext byte fails auth.
func TestEnvelope_TamperFails(t *testing.T) {
	privB, pubB, _ := keys.NewDeviceKey()
	ev := sampleEvent()
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-B", pub: pubB}})
	require.NoError(t, err)

	var env eventEnvelope
	require.NoError(t, json.Unmarshal(sealed, &env))
	env.CT[0] ^= 0xFF
	tampered, _ := json.Marshal(env)

	_, _, _, err = openEnvelope(tampered, "dev-B", privB)
	require.Error(t, err)
	require.NotErrorIs(t, err, errNotARecipient, "tamper is an auth failure, not a not-recipient skip")
}

// TestEnvelope_WrongDeviceWrappedKeyFails verifies that even if an attacker
// relabels another recipient's wrapped blob with this device's id, the unwrap
// fails (the blob was wrapped to a different pubkey).
func TestEnvelope_WrongDeviceWrappedKeyFails(t *testing.T) {
	_, pubA, _ := keys.NewDeviceKey()
	privB, _, _ := keys.NewDeviceKey()
	ev := sampleEvent()
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-A", pub: pubA}})
	require.NoError(t, err)

	var env eventEnvelope
	require.NoError(t, json.Unmarshal(sealed, &env))
	// Relabel dev-A's wrapped key as dev-B.
	env.Keys[0].Device = "dev-B"
	relabeled, _ := json.Marshal(env)

	_, _, _, err = openEnvelope(relabeled, "dev-B", privB)
	require.Error(t, err)
	require.NotErrorIs(t, err, errNotARecipient)
}

// TestEnvelope_EmptyRecipientsRejected verifies sealEnvelope refuses an empty
// recipient set (the caller must DROP, never fall back to plaintext).
func TestEnvelope_EmptyRecipientsRejected(t *testing.T) {
	_, err := sealEnvelope(sampleEvent(), acf.ScopeGlobal, nil, nil)
	require.ErrorIs(t, err, errNoRecipients)
}

// TestEnvelope_ZeroKnowledge_NoPlaintextOnWire is the CI-friendly regression
// guard for the zero-knowledge invariant: a sealed envelope must contain
// NEITHER the payload plaintext NOR any recognisable canonical-event field name
// (e.g. "payload", "provenance", "eventId"). If a future change reverts the
// outbound path to plaintext-canonical-bytes, this fails loudly.
func TestEnvelope_ZeroKnowledge_NoPlaintextOnWire(t *testing.T) {
	_, pub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	ev := sampleEvent()
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-X", pub: pub}})
	require.NoError(t, err)

	for _, marker := range []string{
		"TOPSECRET-MARKER", // payload content
		"payload",          // acf.Event JSON field
		"provenance",       // acf.Event JSON field
		"eventId",          // acf.Event JSON field
		ev.EventID,         // the canonical event id is NOT in the body...
	} {
		require.NotContains(t, string(sealed), marker,
			"sealed envelope leaks plaintext marker %q (zero-knowledge violation)", marker)
	}
	// Sanity: the envelope IS the expected shape (alg present, ct present).
	require.Contains(t, string(sealed), envelopeAlg)
}

// Large text bodies must compress before sealing (the 4MB outbound cap
// otherwise permanently stops long conversations from syncing) and must
// round-trip exactly.
func TestEnvelope_CompressesLargeBodies(t *testing.T) {
	priv, pub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	big := strings.Repeat("the same sentence over and over. ", 40_000) // ~1.3MB, compressible
	ev := acf.Event{
		EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(),
		Payload:   json.RawMessage(fmt.Sprintf(`{"format":"text","content":%q}`, big)),
	}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-1", pub: pub}})
	require.NoError(t, err)
	require.Less(t, len(sealed), len(ev.Payload)/2, "large text body must shrink on the wire")

	got, _, _, err := openEnvelope(sealed, "dev-1", priv)
	require.NoError(t, err)
	require.Equal(t, ev.Payload, got.Payload)
}

// Small events must stay uncompressed and byte-compatible with old receivers.
func TestEnvelope_SmallBodiesStayRaw(t *testing.T) {
	_, pub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	ev := acf.Event{EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate,
		Timestamp: time.Now().UTC(), Payload: json.RawMessage(`{"small":true}`)}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-1", pub: pub}})
	require.NoError(t, err)
	require.NotContains(t, string(sealed), `"enc"`, "small envelopes must not carry the enc field")
}

// --- Compression tuning (2026-07-29 envelope wire-efficiency ADR §3 D4/L3/L4) ---

// compressibleEvent builds an event whose sealed plaintext is dominated by a
// highly compressible text payload of exactly contentLen bytes.
func compressibleEvent(contentLen int) acf.Event {
	content := strings.Repeat("the same sentence over and over. ", contentLen/33+1)[:contentLen]
	return acf.Event{
		EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(),
		Payload:   json.RawMessage(fmt.Sprintf(`{"format":"text","content":%q}`, content)),
	}
}

// TestEnvelope_CompressThresholdBoundary pins the 4 KiB threshold (ADR §3 D4,
// lever L3): a ~3 KiB plaintext stays raw, a ~5 KiB compressible plaintext
// ships gzip — and both round-trip through the existing decoder unchanged.
func TestEnvelope_CompressThresholdBoundary(t *testing.T) {
	priv, pub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		contentLen int
		wantEnc    string
	}{
		{"3KiB-raw", 3 * 1024, ""},
		{"5KiB-gzip", 5 * 1024, "gzip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := compressibleEvent(tc.contentLen)
			// Self-check the fixture actually sits on the intended side of the
			// threshold (mirrors the exact plaintext the seal produces).
			pt, err := json.Marshal(sealedBody{Event: ev, EnvScope: acf.ScopeGlobal})
			require.NoError(t, err)
			if tc.wantEnc == "" {
				require.LessOrEqual(t, len(pt), envelopeCompressThreshold, "fixture must sit at or below the threshold")
			} else {
				require.Greater(t, len(pt), envelopeCompressThreshold, "fixture must exceed the threshold")
			}
			sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-1", pub: pub}})
			require.NoError(t, err)
			var env eventEnvelope
			require.NoError(t, json.Unmarshal(sealed, &env))
			require.Equal(t, tc.wantEnc, env.Enc)
			got, _, _, err := openEnvelope(sealed, "dev-1", priv)
			require.NoError(t, err)
			require.Equal(t, ev.Payload, got.Payload)
		})
	}
}

// TestEnvelope_IncompressibleBodyShipsRaw pins keep-only-if-smaller (ADR §3
// D4): random bytes above the threshold do not shrink under gzip, so the body
// ships raw even though it crosses the threshold; a body exactly at the
// threshold is never compressed at all.
func TestEnvelope_IncompressibleBodyShipsRaw(t *testing.T) {
	random := make([]byte, 8*1024)
	_, err := rand.Read(random)
	require.NoError(t, err)
	body, compressed := maybeCompressBody(random, gzip.BestCompression)
	require.False(t, compressed, "incompressible input must not be kept compressed")
	require.Equal(t, random, body)

	atThreshold := bytes.Repeat([]byte("a"), envelopeCompressThreshold)
	body, compressed = maybeCompressBody(atThreshold, gzip.DefaultCompression)
	require.False(t, compressed, "bodies at the threshold stay raw")
	require.Equal(t, atThreshold, body)
}

// TestEnvelope_GzipLevelForLane pins the lane→level mapping (ADR §3 D4, lever
// L4): retained/checkpoint bodies are debounced ≥60s and afford
// BestCompression; live and unknown lanes keep the previous implicit default.
func TestEnvelope_GzipLevelForLane(t *testing.T) {
	require.Equal(t, gzip.BestCompression, gzipLevelForLane(LaneRetained))
	require.Equal(t, gzip.DefaultCompression, gzipLevelForLane(LaneLive))
	require.Equal(t, gzip.DefaultCompression, gzipLevelForLane(""))
}

// TestEnvelope_SealLevelPlumbing verifies the explicit compression-level
// parameter (ADR §3 D4, lever L4) reaches the gzip writer — Go's gzip records
// the level in the RFC 1952 XFL header byte (offset 8): 2 at BestCompression,
// 0 at DefaultCompression — and that BestCompression output opens with the
// EXISTING openEnvelope unchanged. sealEnvelope without an explicit level must
// keep the previous implicit default.
func TestEnvelope_SealLevelPlumbing(t *testing.T) {
	priv, pub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	ev := compressibleEvent(64 * 1024)

	// peelCompressedBody opens the AEAD only (no gzip) to expose the exact
	// compressed bytes the seal produced.
	peelCompressedBody := func(t *testing.T, sealed []byte) []byte {
		t.Helper()
		var env eventEnvelope
		require.NoError(t, json.Unmarshal(sealed, &env))
		require.Equal(t, "gzip", env.Enc)
		contentKey, err := keys.UnwrapContentKey(env.Keys[0].Wrapped, priv)
		require.NoError(t, err)
		compressedPT, err := keys.OpenBody(contentKey, env.Nonce, env.CT)
		require.NoError(t, err)
		return compressedPT
	}

	for _, tc := range []struct {
		name    string
		level   int
		wantXFL byte
	}{
		{"best", gzip.BestCompression, 2},
		{"default", gzip.DefaultCompression, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealed, err := sealEnvelopeWithLevel(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-1", pub: pub}}, tc.level)
			require.NoError(t, err)
			require.Equal(t, tc.wantXFL, peelCompressedBody(t, sealed)[8], "gzip XFL byte must reflect the requested level")
			// Golden round-trip: the existing decoder opens it unchanged.
			got, _, _, err := openEnvelope(sealed, "dev-1", priv)
			require.NoError(t, err)
			require.Equal(t, ev.Payload, got.Payload)
		})
	}

	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-1", pub: pub}})
	require.NoError(t, err)
	require.Equal(t, byte(0), peelCompressedBody(t, sealed)[8], "sealEnvelope must keep the previous implicit default level")
}

func TestEnvelope_UnknownEncRejected(t *testing.T) {
	priv, pub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	ev := acf.Event{EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate,
		Timestamp: time.Now().UTC(), Payload: json.RawMessage(`{"x":1}`)}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: "dev-1", pub: pub}})
	require.NoError(t, err)
	var env eventEnvelope
	require.NoError(t, json.Unmarshal(sealed, &env))
	env.Enc = "zstd"
	tampered, err := json.Marshal(env)
	require.NoError(t, err)
	_, _, _, err = openEnvelope(tampered, "dev-1", priv)
	require.Error(t, err)
}
