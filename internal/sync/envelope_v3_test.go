package syncd

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityerr"
)

// v3TestEvent builds a hashed create event authored by device-a with a
// payload of the given content size class.
func v3TestEvent(t *testing.T, content string) acf.Event {
	t.Helper()
	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: content})
	require.NoError(t, err)
	e := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: acf.NewID(),
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"},
		Payload:    payload,
	}
	e.Hash, err = acf.ComputeHash(e)
	require.NoError(t, err)
	return e
}

// v3CompressibleContent is comfortably above the compression threshold under
// both the current 4 KiB and the historical 64 KiB values, so codec
// expectations hold regardless of that independently tuned constant.
func v3CompressibleContent() string {
	return strings.Repeat("the same sentence over and over. ", 3000) // ~96 KiB
}

type v3FixedNamespaceKeyProvider struct {
	snapshot keyrotation.NamespaceKeySnapshot
}

func (p v3FixedNamespaceKeyProvider) Current(context.Context, string) (keyrotation.NamespaceKeySnapshot, error) {
	return p.snapshot, nil
}
func (p v3FixedNamespaceKeyProvider) ByVersion(context.Context, string, uint64) (keyrotation.NamespaceKeySnapshot, error) {
	return p.snapshot, nil
}

func TestEnvelopeV3RoundTripEveryCodecRecipientWrap(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	for _, tc := range []struct {
		name         string
		lane         string
		content      string
		gzipInternal bool
		wantEncoding string
	}{
		{"small-raw", LaneLive, "tiny", false, envelopeV3EncodingRaw},
		{"live-zstd", LaneLive, v3CompressibleContent(), false, envelopeV3EncodingZstd},
		{"retained-zstd", LaneRetained, v3CompressibleContent(), false, envelopeV3EncodingZstd},
		{"gzip-fallback", LaneRetained, v3CompressibleContent(), true, envelopeV3EncodingGzip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := v3TestEvent(t, tc.content)
			h := NewEventHeaderV2(e, acf.KindMemory, "account-scope", e.EventID, tc.lane, 1, roster, barrier)
			var wire []byte
			var err error
			if tc.gzipInternal {
				// The gzip codec stays decodable for fleet-intersection
				// reasons; the public seal entry points emit zstd, so the
				// fallback is exercised through the internal seal.
				wire, err = sealEnvelopeV3(e, acf.ScopeGlobal, nil, h, roster, device, nil, envelopeV3Compression{codec: envelopeV3EncodingGzip, level: gzip.BestCompression})
			} else {
				wire, err = SealEnvelopeV3(e, acf.ScopeGlobal, nil, h, roster, device)
			}
			require.NoError(t, err)
			require.True(t, envelopeIsV3(wire), "v3 wire must satisfy the frozen CBOR-map discriminator")

			var env EventEnvelopeV3
			require.NoError(t, envelopeV3Dec.Unmarshal(wire, &env))
			require.Equal(t, uint16(3), env.Version)
			require.Equal(t, tc.wantEncoding, env.BodyEncoding)

			body, header, err := OpenEnvelopeV3(wire, roster, "device-a", device.WrapPrivate)
			require.NoError(t, err)
			require.Equal(t, e.Hash, body.Event.Hash)
			require.Equal(t, string(e.Payload), string(body.Event.Payload))
			require.Equal(t, tc.lane, header.Routing.Lane)
		})
	}
}

func TestEnvelopeV3RoundTripNamespaceKeyMode(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	e := v3TestEvent(t, v3CompressibleContent())
	nsID := "0197f30a-3c58-7000-8000-0000000000aa"
	snapshot := keyrotation.NamespaceKeySnapshot{
		NamespaceID:       nsID,
		Version:           1,
		AccessGeneration:  roster.Manifest.Manifest.AccessGeneration,
		AccessSetHash:     roster.Manifest.Manifest.AccessSetHash,
		IssuedRosterEpoch: roster.Manifest.Manifest.Epoch,
		IssuedRosterHash:  [32]byte(roster.Hash),
		Finalized:         true,
	}
	_, err := rand.Read(snapshot.Key[:])
	require.NoError(t, err)

	h := NewEventHeaderV2(e, acf.KindMemory, nsID, e.EventID, LaneRetained, 1, roster, barrier)
	h.KeyMode = "namespace-key-v1"
	h.KeyVersion = 1
	wire, err := SealNamespaceEnvelopeV3(e, acf.ScopeNamespace, nil, h, roster, device, snapshot)
	require.NoError(t, err)

	var env EventEnvelopeV3
	require.NoError(t, envelopeV3Dec.Unmarshal(wire, &env))
	require.Empty(t, env.WrappedKeys, "namespace-key mode carries no per-recipient wraps")
	require.Equal(t, envelopeV3EncodingZstd, env.BodyEncoding)

	body, _, err := OpenEnvelopeV3WithNamespaceProvider(wire, roster, "device-a", device.WrapPrivate, v3FixedNamespaceKeyProvider{snapshot: snapshot})
	require.NoError(t, err)
	require.Equal(t, e.Hash, body.Event.Hash)

	// Without namespace-key history the open stage is the sole retryable
	// error, exactly like v2.
	_, _, err = OpenEnvelopeV3(wire, roster, "device-a", device.WrapPrivate)
	require.ErrorIs(t, err, securityerr.ErrStaleRoster)
}

// TestEnvelopeV3DomainSeparation proves a v2 envelope re-tagged as v3 (and
// vice versa) can never cross-verify: the signature domains
// (aplexica.envelope.v3 / aplexica.envelope.v3.sig vs the v2 strings) and the
// signed version value both differ, so transplanting every other field
// verbatim still fails Ed25519 verification.
func TestEnvelopeV3DomainSeparation(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	e := v3TestEvent(t, "tiny") // sub-threshold: BodyEncoding "raw" is valid in both generations

	h2 := NewEventHeaderV2(e, acf.KindMemory, "account-scope", e.EventID, LaneLive, 1, roster, barrier)
	v2Wire, err := SealEnvelopeV2(e, acf.ScopeGlobal, nil, h2, roster, device)
	require.NoError(t, err)
	var v2Env EventEnvelopeV2
	require.NoError(t, json.Unmarshal(v2Wire, &v2Env))
	retagged := EventEnvelopeV3{
		Version: 3, Algorithm: v2Env.Algorithm, BodyEncoding: v2Env.BodyEncoding, Header: v2Env.Header,
		BodyNonce: v2Env.BodyNonce, BodyCiphertext: v2Env.BodyCiphertext, WrappedKeys: v2Env.WrappedKeys,
		SignerDeviceID: v2Env.SignerDeviceID, SignerKeyID: v2Env.SignerKeyID, Signature: v2Env.Signature,
	}
	retaggedWire, err := envelopeV3Enc.Marshal(retagged)
	require.NoError(t, err)
	_, _, err = OpenEnvelopeV3(retaggedWire, roster, "device-a", device.WrapPrivate)
	require.ErrorIs(t, err, securityerr.ErrInvalidSignature, "v2 signature must never verify under the v3 domains")

	h3 := NewEventHeaderV2(e, acf.KindMemory, "account-scope", e.EventID, LaneLive, 1, roster, barrier)
	v3Wire, err := SealEnvelopeV3(e, acf.ScopeGlobal, nil, h3, roster, device)
	require.NoError(t, err)
	var v3Env EventEnvelopeV3
	require.NoError(t, envelopeV3Dec.Unmarshal(v3Wire, &v3Env))
	downgraded := EventEnvelopeV2{
		Version: 2, Algorithm: v3Env.Algorithm, BodyEncoding: v3Env.BodyEncoding, Header: v3Env.Header,
		BodyNonce: v3Env.BodyNonce, BodyCiphertext: v3Env.BodyCiphertext, WrappedKeys: v3Env.WrappedKeys,
		SignerDeviceID: v3Env.SignerDeviceID, SignerKeyID: v3Env.SignerKeyID, Signature: v3Env.Signature,
	}
	downgradedWire, err := json.Marshal(downgraded)
	require.NoError(t, err)
	_, _, err = OpenEnvelopeV2(downgradedWire, roster, "device-a", device.WrapPrivate)
	require.ErrorIs(t, err, securityerr.ErrInvalidSignature, "v3 signature must never verify under the v2 domains")
}

func TestEnvelopeV3CanonicalDeterminism(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	e := v3TestEvent(t, v3CompressibleContent())
	h := NewEventHeaderV2(e, acf.KindMemory, "account-scope", e.EventID, LaneRetained, 1, roster, barrier)
	wire, err := SealEnvelopeV3(e, acf.ScopeGlobal, nil, h, roster, device)
	require.NoError(t, err)

	var env EventEnvelopeV3
	require.NoError(t, envelopeV3Dec.Unmarshal(wire, &env))
	first, err := envelopeV3Enc.Marshal(env)
	require.NoError(t, err)
	second, err := envelopeV3Enc.Marshal(env)
	require.NoError(t, err)
	require.Equal(t, wire, first, "decode/re-encode must be byte-identical (canonical CBOR)")
	require.Equal(t, first, second, "the same value must encode identically every time")
}

func TestEnvelopeV3StrictDecodeAndBounds(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	e := v3TestEvent(t, "tiny")
	h := NewEventHeaderV2(e, acf.KindMemory, "account-scope", e.EventID, LaneLive, 1, roster, barrier)
	wire, err := SealEnvelopeV3(e, acf.ScopeGlobal, nil, h, roster, device)
	require.NoError(t, err)

	t.Run("oversize-envelope", func(t *testing.T) {
		_, _, err := OpenEnvelopeV3(make([]byte, envelopeV3MaxBytes+1), roster, "device-a", device.WrapPrivate)
		require.ErrorIs(t, err, securityerr.ErrLimitExceeded)
	})

	t.Run("unknown-body-encoding-rejects-pre-decrypt", func(t *testing.T) {
		var env EventEnvelopeV3
		require.NoError(t, envelopeV3Dec.Unmarshal(wire, &env))
		env.BodyEncoding = "br"
		tampered, err := envelopeV3Enc.Marshal(env)
		require.NoError(t, err)
		_, _, oerr := OpenEnvelopeV3(tampered, roster, "device-a", device.WrapPrivate)
		require.ErrorIs(t, oerr, securityerr.ErrMetadataMismatch)
	})

	t.Run("unknown-field-rejects", func(t *testing.T) {
		var env EventEnvelopeV3
		require.NoError(t, envelopeV3Dec.Unmarshal(wire, &env))
		extended := struct {
			EventEnvelopeV3
			Extra bool `cbor:"zzExtra"`
		}{EventEnvelopeV3: env, Extra: true}
		extendedWire, err := envelopeV3Enc.Marshal(extended)
		require.NoError(t, err)
		_, _, oerr := OpenEnvelopeV3(extendedWire, roster, "device-a", device.WrapPrivate)
		require.Error(t, oerr, "unknown CBOR fields must reject (DisallowUnknownFields analog)")
	})

	t.Run("trailing-data-rejects", func(t *testing.T) {
		_, _, err := OpenEnvelopeV3(append(append([]byte(nil), wire...), 0x00), roster, "device-a", device.WrapPrivate)
		require.Error(t, err)
	})

	t.Run("signed-routing-tamper-rejects", func(t *testing.T) {
		var env EventEnvelopeV3
		require.NoError(t, envelopeV3Dec.Unmarshal(wire, &env))
		env.Header.Routing.SourceAgent = "tampered"
		tampered, err := envelopeV3Enc.Marshal(env)
		require.NoError(t, err)
		_, _, oerr := OpenEnvelopeV3(tampered, roster, "device-a", device.WrapPrivate)
		require.ErrorIs(t, oerr, securityerr.ErrInvalidSignature)
	})

	t.Run("decompression-bound", func(t *testing.T) {
		huge := make([]byte, envelopeMaxDecompressedBytes+1)
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		require.NoError(t, err)
		compressed := enc.EncodeAll(huge, nil)
		require.NoError(t, enc.Close())
		_, derr := decompressBodyV3(compressed, envelopeV3EncodingZstd)
		require.ErrorIs(t, derr, securityerr.ErrLimitExceeded)
	})
}

func TestRetainedClearV3RoundTripAndTamper(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	e := acf.Event{EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeRedaction, Timestamp: time.Now().UTC(), Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"}}
	h := NewEventHeaderV2(e, acf.KindConversation, "account-scope", RetainedWireEventID(e.EventID, "device-a"), LaneRetained, 1, roster, barrier)
	h.Purpose = "retained-clear"
	h.Routing.Clear = true
	h.Canonical = CanonicalMetadataV2{}
	wire, err := SealRetainedClearV3(h, roster, device)
	require.NoError(t, err)
	require.True(t, envelopeIsV3(wire))
	_, got, err := OpenEnvelopeV3(wire, roster, "device-a", device.WrapPrivate)
	require.NoError(t, err)
	require.True(t, got.Routing.Clear, "clear flag lost")

	var env EventEnvelopeV3
	require.NoError(t, envelopeV3Dec.Unmarshal(wire, &env))
	env.Header.Routing.ArtifactID = "tampered"
	bad, err := envelopeV3Enc.Marshal(env)
	require.NoError(t, err)
	_, _, err = OpenEnvelopeV3(bad, roster, "device-a", device.WrapPrivate)
	require.Error(t, err, "tampered clear accepted")
}

func TestEnvelopeIsV3Discriminator(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	e := v3TestEvent(t, "tiny")

	_, wrapPub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	v1Wire, err := sealEnvelope(e, acf.ScopeGlobal, nil, []recipient{{deviceID: "device-a", pub: wrapPub}})
	require.NoError(t, err)
	require.False(t, envelopeIsV3(v1Wire), "v1 JSON envelope misdetected as v3")

	h := NewEventHeaderV2(e, acf.KindMemory, "account-scope", e.EventID, LaneLive, 1, roster, barrier)
	v2Wire, err := SealEnvelopeV2(e, acf.ScopeGlobal, nil, h, roster, device)
	require.NoError(t, err)
	require.False(t, envelopeIsV3(v2Wire), "v2 JSON envelope misdetected as v3")

	v3Wire, err := SealEnvelopeV3(e, acf.ScopeGlobal, nil, h, roster, device)
	require.NoError(t, err)
	require.True(t, envelopeIsV3(v3Wire))
	require.False(t, envelopeIsV3(nil))
}

func TestEnvelopeV3CompressionForLane(t *testing.T) {
	retained := envelopeV3CompressionForLane(LaneRetained)
	require.Equal(t, envelopeV3Compression{codec: envelopeV3EncodingZstd, level: 9}, retained)
	live := envelopeV3CompressionForLane(LaneLive)
	require.Equal(t, envelopeV3Compression{codec: envelopeV3EncodingZstd, level: 3}, live)
	require.Equal(t, live, envelopeV3CompressionForLane(""), "unknown lane fails to the latency-safe level")
}

// --- ADR D2+D3 seal-time selection -----------------------------------------

type v3FakeCapsPublisher struct {
	enabled bool
	called  bool
}

func (p *v3FakeCapsPublisher) PublishOutbound(OutboundEvent) {}
func (p *v3FakeCapsPublisher) EnvelopeV3Enabled(context.Context) bool {
	p.called = true
	return p.enabled
}

type v3PlainPublisher struct{}

func (v3PlainPublisher) PublishOutbound(OutboundEvent) {}

func v3RosterWithVersions(versionSets ...[]uint16) identity.VerifiedRoster {
	var r identity.VerifiedRoster
	for i, vs := range versionSets {
		r.Manifest.Manifest.Devices = append(r.Manifest.Manifest.Devices, identity.DeviceCertificateV1{
			Certificate: identity.DeviceCertificateUnsignedV1{DeviceID: fmt.Sprintf("device-%d", i), EnvelopeVersions: vs},
		})
	}
	return r
}

func TestEnvelopeV3SelectionTruthTable(t *testing.T) {
	all3 := v3RosterWithVersions([]uint16{2, 3}, []uint16{2, 3}, []uint16{2, 3})
	mixed := v3RosterWithVersions([]uint16{2, 3}, []uint16{2})
	empty := identity.VerifiedRoster{}

	// All devices advertise 3 + account caps on => v3.
	capsOn := &v3FakeCapsPublisher{enabled: true}
	require.True(t, envelopeV3Selected(capsOn, all3))
	require.True(t, capsOn.called)

	// One device without 3 => current format, and the (potentially RPC-
	// backed) caps probe is never spent on a fleet that cannot take v3.
	mixedCaps := &v3FakeCapsPublisher{enabled: true}
	require.False(t, envelopeV3Selected(mixedCaps, mixed))
	require.False(t, mixedCaps.called)

	// Caps off — and equally an RPC error, which the daemon-side gate
	// collapses to false (fail-closed) — => current format.
	require.False(t, envelopeV3Selected(&v3FakeCapsPublisher{enabled: false}, all3))

	// A publisher without the optional capability (old/BYO transport) =>
	// current format.
	require.False(t, envelopeV3Selected(v3PlainPublisher{}, all3))

	// No roster devices => no capability statement => current format.
	require.False(t, envelopeV3Selected(&v3FakeCapsPublisher{enabled: true}, empty))
}

// --- inbound integration ----------------------------------------------------

func v3RemoteEventForTest(t *testing.T, event acf.Event, kind acf.Kind, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, barrier [32]byte) proto.RemoteEvent {
	t.Helper()
	wireBytes, err := SealEnvelopeV3(event, acf.ScopeGlobal, nil, header, roster, device)
	require.NoError(t, err)
	return proto.RemoteEvent{
		NamespaceID: header.Routing.NamespaceID, BranchID: header.Routing.BranchID,
		ArtifactID: event.ArtifactID, EventID: header.Routing.WireEventID, ParentHash: event.ParentHash,
		EventHash: event.Hash, Kind: string(kind), Type: string(event.Type),
		Timestamp: event.Timestamp, Bytes: wireBytes, Sequence: header.Routing.Sequence,
		Origin: event.Provenance.DeviceID, SourceAgent: event.Provenance.SourceAgent, Lane: header.Routing.Lane,
		AccessGeneration: roster.Manifest.Manifest.AccessGeneration, AccessSetHash: roster.Manifest.Manifest.AccessSetHash,
		SecurityBarrierID: barrier, SecurityGeneration: 1, KeyMode: "recipient-wrap-v2",
	}
}

// TestImportInboundEnvelopeV3Applied drives a sealed v3 envelope through the
// full inbound path beside the v1/v2 branches: the frozen discriminator
// dispatches to the v3 opener and the canonical event lands in the store.
func TestImportInboundEnvelopeV3Applied(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	event := v3TestEvent(t, "v3 inbound body")
	header := NewEventHeaderV2(event, acf.KindMemory, "", event.EventID, LaneLive, 1, roster, barrier)
	wire := v3RemoteEventForTest(t, event, acf.KindMemory, header, roster, device, barrier)

	o, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{wire}))
	stored, err := store.ReadEvents(acf.KindMemory, event.ArtifactID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, string(event.Payload), string(stored[0].Payload))
}

// TestImportInboundEnvelopeV3CorruptIsTerminal proves v3 failures classify
// into the existing ImportRejected/quarantine flow unchanged: a corrupt
// immutable envelope is terminal, never retried, and never mutates state.
func TestImportInboundEnvelopeV3CorruptIsTerminal(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	event := v3TestEvent(t, "v3 corrupt body")
	header := NewEventHeaderV2(event, acf.KindMemory, "", event.EventID, LaneLive, 1, roster, barrier)
	wire := v3RemoteEventForTest(t, event, acf.KindMemory, header, roster, device, barrier)

	var env EventEnvelopeV3
	require.NoError(t, envelopeV3Dec.Unmarshal(wire.Bytes, &env))
	require.NotEmpty(t, env.BodyCiphertext)
	env.BodyCiphertext[0] ^= 0xff
	tampered, err := envelopeV3Enc.Marshal(env)
	require.NoError(t, err)
	wire.Bytes = tampered

	o, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	require.Equal(t, []ImportOutcome{ImportRejected}, o.ImportInboundResults([]proto.RemoteEvent{wire}))
	stored, err := store.ReadEvents(acf.KindMemory, event.ArtifactID)
	require.NoError(t, err)
	require.Empty(t, stored, "terminally rejected input must not mutate canonical state")
}
