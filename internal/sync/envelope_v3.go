package syncd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"sort"

	"github.com/fxamacker/cbor/v2"
	"github.com/klauspost/compress/zstd"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/securewire"
	"github.com/aplexica/aplexica/internal/securityerr"
)

// ---------------------------------------------------------------------------
// Envelope v3 (2026-07-29 envelope wire-efficiency ADR, D1+D2).
//
// v3 is a FRAMING + CODEC change over v2, NEVER a crypto change:
//
//   - The envelope serializes as CANONICAL CBOR (fxamacker/cbor
//     core-deterministic mode) instead of JSON, so ciphertext / nonces /
//     wrapped keys ride as CBOR byte strings with no base64 inflation and no
//     JSON key overhead inside the sealed record.
//   - bodyEncoding gains "zstd" beside v2's "raw"/"gzip" (gzip is retained
//     for fleet-intersection reasons: a v3 decoder MUST accept all three).
//   - Everything cryptographic is byte-identical to v2: XChaCha20-Poly1305
//     body AEAD (keys.SealBodyV2/OpenBodyV2), per-recipient X25519 wrap
//     (keys.WrapContentKeyV2) or the namespace-key mode, and an Ed25519
//     envelope signature (keys.SignV2/VerifyV2) over the canonical header
//     AAD. The header/body/roster validation rules are copied from v2
//     verbatim — any divergence there is a security bug.
//
// Domain separation: the AAD and signature-input domains below are DISTINCT
// from v2's ("aplexica/event-envelope/v2" / "aplexica/event-signature/v2"),
// so a v2 envelope re-tagged as v3 (or vice versa) can never cross-verify,
// and a wrapped key minted for a v2 header can never be transplanted under a
// v3 header (the wrap binds sha256 of the domain-tagged AAD).
//
// Rollout (ADR D2+D3): SEALING v3 is gated in remote_sync.go — every
// recipient device in the verified signed roster must advertise 3 in its
// certificate EnvelopeVersions AND the account-level envelope_caps switch
// (remote.envelope_caps plugin RPC, fail-closed) must be on. DECODING v3
// ships one release before encoding is ever exercised; advertising 3 IS the
// decode-capability statement.
// ---------------------------------------------------------------------------

// envelopeV3AADDomain / envelopeV3SignatureDomain are the v3
// domain-separation tags. FROZEN: these exact strings are part of the wire
// contract — changing either invalidates every v3 signature on the fleet.
const (
	// envelopeVersionV3 is the wire version this file seals and opens. It is a
	// protocol constant, not a tunable: the decoder rejects anything else, and
	// the roster advertises the same number so peers can negotiate.
	envelopeVersionV3 = 3
	// CBOR decode bounds. Deliberately tight: a v3 envelope is a flat,
	// fixed-shape structure, so anything deeply nested or wide is malformed or
	// hostile rather than merely unusual.
	envelopeV3MaxNestedLevels  = 32
	envelopeV3MaxArrayElements = 4096
	envelopeV3MaxMapPairs      = 4096
	// cborMajorTypeMap is the CBOR major type (RFC 8949 §3) carried in the top
	// three bits of the first byte. A v3 envelope always starts as a CBOR map,
	// which is exactly what distinguishes it on the wire from v1/v2's JSON '{'.
	cborMajorTypeMap = 5

	envelopeV3AADDomain       = "aplexica.envelope.v3"
	envelopeV3SignatureDomain = "aplexica.envelope.v3.sig"
)

// v3 keeps v2's algorithm identifiers: the crypto suite is unchanged, and the
// distinct serialization + domain tags (not the algorithm string) are what
// separate the generations.
const (
	envelopeV3Algorithm      = envelopeV2Algorithm
	envelopeV3ClearAlgorithm = envelopeV2ClearAlgorithm
	envelopeV3MaxBytes       = 64 << 20
)

// Body encodings a v3 envelope may carry. Unknown values reject pre-decrypt,
// exactly like v2 — which is why sealing v3 at all requires the roster
// intersection: an old receiver must never SEE a codec it cannot name.
const (
	envelopeV3EncodingRaw  = "raw"
	envelopeV3EncodingGzip = "gzip"
	envelopeV3EncodingZstd = "zstd"
)

// Pre-seal zstd levels (ADR D1): retained/checkpoint bodies are
// latency-insensitive (debounced ≥60s upstream) and take level 9; the live
// lane takes level 3. Mapped through zstd.EncoderLevelFromZstd, mirroring
// gzipLevelForLane's lane policy for v1/v2.
const (
	envelopeV3ZstdLevelRetained = 9
	envelopeV3ZstdLevelLive     = 3
)

// envelopeV3Enc is the core-deterministic CBOR encoding mode (RFC 8949 §4.2.1:
// shortest-form arguments, no indefinite lengths, bytewise-sorted map keys).
// Determinism matters because the signed AAD commits to header FIELD VALUES
// via securewire.Canonical, while the outer envelope bytes are what the relay
// stores — a deterministic outer encoding keeps re-encoding byte-stable for
// dedup digests and golden vectors.
var envelopeV3Enc cbor.EncMode

// envelopeV3Dec is the strict decoding mode for inbound v3 envelopes: unknown
// fields reject (the CBOR analog of v2's DisallowUnknownFields), duplicate map
// keys reject, indefinite lengths and tags are forbidden, and field names
// match case-sensitively (the canonical encoder always emits exact names).
var envelopeV3Dec cbor.DecMode

func init() {
	var err error
	envelopeV3Enc, err = cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	envelopeV3Dec, err = cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		MaxNestedLevels:   envelopeV3MaxNestedLevels,
		MaxArrayElements:  envelopeV3MaxArrayElements,
		MaxMapPairs:       envelopeV3MaxMapPairs,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
		FieldNameMatching: cbor.FieldNameMatchingCaseSensitive,
	}.DecMode()
	if err != nil {
		panic(err)
	}
}

// EventEnvelopeV3 mirrors EventEnvelopeV2 field-for-field; only the wire
// serialization (canonical CBOR, byte-string binary fields) and the signature
// domains differ. The header, wrapped-key, and sealed-body types are v2's own
// — reusing them keeps the two generations structurally identical by
// construction. CBOR field names are FROZEN and equal v2's JSON names.
type EventEnvelopeV3 struct {
	Version        uint16              `cbor:"version"`
	Algorithm      string              `cbor:"algorithm"`
	BodyEncoding   string              `cbor:"bodyEncoding,omitempty"`
	Header         EventHeaderV2       `cbor:"header"`
	BodyNonce      [24]byte            `cbor:"bodyNonce"`
	BodyCiphertext []byte              `cbor:"bodyCiphertext,omitempty"`
	WrappedKeys    []WrappedKeyEntryV2 `cbor:"wrappedKeys,omitempty"`
	SignerDeviceID string              `cbor:"signerDeviceId"`
	SignerKeyID    [32]byte            `cbor:"signerKeyId"`
	Signature      [64]byte            `cbor:"signature"`
}

// envelopeIsV3 reports whether opaque sealed bytes are a v3 envelope. v1/v2
// envelopes are JSON objects and always begin with '{' (0x7B, CBOR major
// type 3); a v3 envelope is a canonical-CBOR map whose initial byte carries
// CBOR major type 5. This one-byte discriminator is FROZEN alongside the v3
// domain strings — it is what lets the inbound path dispatch v2/v3 without a
// second outer wrapper field. The decoder still strictly validates
// Version == 3 afterwards.
func envelopeIsV3(data []byte) bool {
	return len(data) > 0 && data[0]>>5 == cborMajorTypeMap
}

func headerAADV3(e EventEnvelopeV3) ([]byte, error) {
	return securewire.Canonical(envelopeV3AADDomain, e.Version, e.Algorithm, e.Header, e.BodyEncoding, e.SignerDeviceID, e.SignerKeyID[:])
}
func signatureInputV3(e EventEnvelopeV3, aad []byte) ([]byte, error) {
	h := sha256.Sum256(e.BodyCiphertext)
	return securewire.Canonical(envelopeV3SignatureDomain, aad, e.BodyNonce[:], h[:], e.WrappedKeys)
}

// envelopeV3Compression selects the pre-seal codec + codec-native level for
// one v3 seal. The exported seal entry points derive it from the signed
// header's Routing.Lane (envelopeV3CompressionForLane); tests exercise the
// gzip fallback codec through the internal seal directly.
type envelopeV3Compression struct {
	codec string
	level int
}

func envelopeV3CompressionForLane(lane string) envelopeV3Compression {
	if lane == LaneRetained {
		return envelopeV3Compression{codec: envelopeV3EncodingZstd, level: envelopeV3ZstdLevelRetained}
	}
	return envelopeV3Compression{codec: envelopeV3EncodingZstd, level: envelopeV3ZstdLevelLive}
}

// maybeCompressBodyV3 compresses pt with the selected codec when it exceeds
// envelopeCompressThreshold, keeping the result only if strictly smaller
// (keep-only-if-smaller, same policy as maybeCompressBody). It returns the
// body to seal and the bodyEncoding to record; on any compression error or a
// non-shrinking result the original bytes ship "raw".
func maybeCompressBodyV3(pt []byte, comp envelopeV3Compression) ([]byte, string) {
	if len(pt) <= envelopeCompressThreshold {
		return pt, envelopeV3EncodingRaw
	}
	switch comp.codec {
	case envelopeV3EncodingGzip:
		if body, ok := maybeCompressBody(pt, comp.level); ok {
			return body, envelopeV3EncodingGzip
		}
	case envelopeV3EncodingZstd:
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(comp.level)),
			zstd.WithEncoderConcurrency(1))
		if err != nil {
			return pt, envelopeV3EncodingRaw
		}
		out := enc.EncodeAll(pt, make([]byte, 0, len(pt)/2))
		_ = enc.Close()
		if len(out) < len(pt) {
			return out, envelopeV3EncodingZstd
		}
	}
	return pt, envelopeV3EncodingRaw
}

// decompressBodyV3 reverses maybeCompressBodyV3 under the shared 256 MiB
// decompression bound. Mirroring v2's gzip branch, any decode failure or an
// over-bound expansion collapses to ErrLimitExceeded.
func decompressBodyV3(plain []byte, encoding string) ([]byte, error) {
	switch encoding {
	case envelopeV3EncodingRaw:
		return plain, nil
	case envelopeV3EncodingGzip:
		gz, err := gzip.NewReader(bytes.NewReader(plain))
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(io.LimitReader(gz, envelopeMaxDecompressedBytes+1))
		cerr := gz.Close()
		if err == nil {
			err = cerr
		}
		if err != nil || len(out) > envelopeMaxDecompressedBytes {
			return nil, securityerr.ErrLimitExceeded
		}
		return out, nil
	case envelopeV3EncodingZstd:
		dec, err := zstd.NewReader(bytes.NewReader(plain),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(envelopeMaxDecompressedBytes)+1))
		if err != nil {
			return nil, securityerr.ErrLimitExceeded
		}
		out, err := io.ReadAll(io.LimitReader(dec.IOReadCloser(), envelopeMaxDecompressedBytes+1))
		dec.Close()
		if err != nil || len(out) > envelopeMaxDecompressedBytes {
			return nil, securityerr.ErrLimitExceeded
		}
		return out, nil
	}
	// The opener validates bodyEncoding before decrypting; reaching here with
	// an unknown value is a programming error, treated as a hard mismatch.
	return nil, securityerr.ErrMetadataMismatch
}

// rosterAdvertisesEnvelopeVersion reports whether EVERY device certificate in
// the verified roster's manifest advertises the given envelope version. An
// empty device set answers false — no roster, no capability statement. This
// is ADR D2's seal-time intersection gate: the certificates are client-signed
// and equality-enforced against the server's stored SupportedEnvelopeVersions
// at sync-generation activation, so the answer is cryptographically bound to
// the exact recipient set the envelope is about to be sealed for.
func rosterAdvertisesEnvelopeVersion(roster identity.VerifiedRoster, version uint16) bool {
	devices := roster.Manifest.Manifest.Devices
	if len(devices) == 0 {
		return false
	}
	for i := range devices {
		advertised := false
		for _, v := range devices[i].Certificate.EnvelopeVersions {
			if v == version {
				advertised = true
				break
			}
		}
		if !advertised {
			return false
		}
	}
	return true
}

// EnvelopeCapsPublisher is an optional additive capability on the remote
// event publisher (same idiom as LargeRetainedCheckpointPublisher): the
// daemon's publish adapter answers the server-asserted account-level
// envelope_caps switch (ADR D3, the fleet-wide kill switch for v3 encoding —
// layered ON TOP of the D2 roster intersection, never instead of it).
// Implementations MUST fail closed: reconnecting plugin, RPC error, missing
// entitlement, or an unimplemented method all answer false.
type EnvelopeCapsPublisher interface {
	EnvelopeV3Enabled(ctx context.Context) bool
}

// envelopeV3Selected is the complete ADR D2+D3 seal-time gate: v3 is selected
// iff the publisher advertises the account switch AND every device in the
// verified roster the event is about to be sealed for advertises envelope
// version 3. Any other combination — publisher without the capability, caps
// off, an RPC failure inside the gate, one device still on an older release —
// keeps today's format. The roster check runs first so a not-yet-ready fleet
// never spends the (cached) capability RPC.
func envelopeV3Selected(pub RemoteEventPublisher, roster identity.VerifiedRoster) bool {
	caps, ok := pub.(EnvelopeCapsPublisher)
	if !ok || !rosterAdvertisesEnvelopeVersion(roster, envelopeVersionV3) {
		return false
	}
	return caps.EnvelopeV3Enabled(context.Background())
}

func SealEnvelopeV3(event acf.Event, scope acf.Scope, projectInfo *project.ProjectInfo, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity) ([]byte, error) {
	return sealEnvelopeV3(event, scope, projectInfo, header, roster, device, nil, envelopeV3CompressionForLane(header.Routing.Lane))
}

func SealNamespaceEnvelopeV3(event acf.Event, scope acf.Scope, projectInfo *project.ProjectInfo, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey keyrotation.NamespaceKeySnapshot) ([]byte, error) {
	return sealEnvelopeV3(event, scope, projectInfo, header, roster, device, &namespaceKey, envelopeV3CompressionForLane(header.Routing.Lane))
}

// sealEnvelopeV3 is sealEnvelopeV2 with the v3 framing: identical header /
// roster / key-mode validation, identical wrap + AEAD + signing calls, but a
// codec-parameterized pre-seal compressor, the v3 AAD/signature domains, and
// a canonical-CBOR wire encoding.
func sealEnvelopeV3(event acf.Event, scope acf.Scope, projectInfo *project.ProjectInfo, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey *keyrotation.NamespaceKeySnapshot, comp envelopeV3Compression) ([]byte, error) {
	if header.Purpose != "event" || header.RosterEpoch != roster.Manifest.Manifest.Epoch || header.RosterHash != [32]byte(roster.Hash) || header.AccessGeneration != roster.Manifest.Manifest.AccessGeneration || header.AccessSetHash != roster.Manifest.Manifest.AccessSetHash || header.EventSalt != ([32]byte{}) || header.SecurityBarrierID == ([32]byte{}) {
		return nil, securityerr.ErrMetadataMismatch
	}
	if namespaceKey == nil {
		if header.KeyMode != "recipient-wrap-v2" || header.KeyVersion != 0 {
			return nil, securityerr.ErrMetadataMismatch
		}
	} else if !namespaceKey.Finalized || scope != acf.ScopeNamespace || event.ArtifactID == "" || header.KeyMode != "namespace-key-v1" || header.KeyVersion != namespaceKey.Version || header.Routing.NamespaceID != namespaceKey.NamespaceID || header.AccessGeneration != namespaceKey.AccessGeneration || header.AccessSetHash != namespaceKey.AccessSetHash || header.RosterEpoch != namespaceKey.IssuedRosterEpoch || header.RosterHash != namespaceKey.IssuedRosterHash || namespaceKey.Key == ([32]byte{}) {
		return nil, securityerr.ErrMetadataMismatch
	}
	if header.Routing.OriginDevice == "" || header.Routing.OriginDevice != event.Provenance.DeviceID {
		return nil, securityerr.ErrMetadataMismatch
	}
	active := false
	for _, c := range roster.Manifest.Manifest.Devices {
		if c.Certificate.DeviceID == header.Routing.OriginDevice && c.Certificate.SigningKeyID == device.SigningKeyID && c.Certificate.WrapKeyID == device.WrapKeyID {
			active = true
		}
	}
	if !active {
		return nil, securityerr.ErrUntrustedRoster
	}
	header.Canonical = metadataForEvent(event)
	if err := validateHeaderBody(header, event, header.Routing.OriginDevice); err != nil {
		return nil, err
	}
	var claim *RemoteProjectClaimV2
	if scope == acf.ScopeProject {
		if projectInfo == nil {
			return nil, securityerr.ErrMetadataMismatch
		}
		remote, err := project.NormalizeRemoteIdentity(projectInfo.ID, projectInfo.VCS)
		if err != nil {
			return nil, err
		}
		claim = &RemoteProjectClaimV2{ID: projectInfo.ID, VCS: projectInfo.VCS, RemoteIdentity: remote}
	}
	plain, err := json.Marshal(SealedBodyV2{event, scope, claim})
	if err != nil {
		return nil, err
	}
	plain, encoding := maybeCompressBodyV3(plain, comp)
	env := EventEnvelopeV3{Version: envelopeVersionV3, Algorithm: envelopeV3Algorithm, BodyEncoding: encoding, Header: header, SignerDeviceID: header.Routing.OriginDevice, SignerKeyID: device.SigningKeyID}
	if _, err := io.ReadFull(rand.Reader, env.BodyNonce[:]); err != nil {
		return nil, err
	}
	aad, err := headerAADV3(env)
	if err != nil {
		return nil, err
	}
	hh := sha256.Sum256(aad)
	var eventKey [32]byte
	if namespaceKey != nil {
		eventKey = namespaceKey.Key
	} else {
		if _, err := io.ReadFull(rand.Reader, eventKey[:]); err != nil {
			return nil, err
		}
		for _, c := range roster.Manifest.Manifest.Devices {
			w, err := keys.WrapContentKeyV2(eventKey, c.Certificate.WrapPublicKey, hh, "device", c.Certificate.DeviceID, c.Certificate.WrapKeyID)
			if err != nil {
				return nil, err
			}
			env.WrappedKeys = append(env.WrappedKeys, WrappedKeyEntryV2{"device", c.Certificate.DeviceID, c.Certificate.WrapKeyID, w.EphemeralPublic, w.Nonce, w.Ciphertext})
		}
		a := roster.Authority.Anchor.Anchor
		w, err := keys.WrapContentKeyV2(eventKey, a.RecoveryWrapPublicKey, hh, "recovery", "account-recovery", a.RecoveryWrapKeyID)
		if err != nil {
			return nil, err
		}
		env.WrappedKeys = append(env.WrappedKeys, WrappedKeyEntryV2{"recovery", "account-recovery", a.RecoveryWrapKeyID, w.EphemeralPublic, w.Nonce, w.Ciphertext})
		sort.Slice(env.WrappedKeys, func(i, j int) bool { return wrapLess(env.WrappedKeys[i], env.WrappedKeys[j]) })
	}
	env.BodyCiphertext, err = keys.SealBodyV2(eventKey, plain, aad, env.BodyNonce)
	if err != nil {
		return nil, err
	}
	si, err := signatureInputV3(env, aad)
	if err != nil {
		return nil, err
	}
	env.Signature, err = keys.SignV2(device.SigningPrivate, si)
	if err != nil {
		return nil, err
	}
	return envelopeV3Enc.Marshal(env)
}

func SealRetainedClearV3(header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity) ([]byte, error) {
	return sealRetainedClearV3(header, roster, device, nil)
}

func SealNamespaceRetainedClearV3(header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey keyrotation.NamespaceKeySnapshot) ([]byte, error) {
	return sealRetainedClearV3(header, roster, device, &namespaceKey)
}

func sealRetainedClearV3(header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey *keyrotation.NamespaceKeySnapshot) ([]byte, error) {
	if header.Purpose != "retained-clear" || !header.Routing.Clear || header.Routing.Lane != LaneRetained || header.Canonical != (CanonicalMetadataV2{}) || header.RosterEpoch != roster.Manifest.Manifest.Epoch || header.RosterHash != [32]byte(roster.Hash) || header.AccessGeneration != roster.Manifest.Manifest.AccessGeneration || header.AccessSetHash != roster.Manifest.Manifest.AccessSetHash || header.SecurityBarrierID == ([32]byte{}) || header.EventSalt != ([32]byte{}) {
		return nil, securityerr.ErrMetadataMismatch
	}
	if namespaceKey == nil {
		if header.KeyMode != "recipient-wrap-v2" || header.KeyVersion != 0 {
			return nil, securityerr.ErrMetadataMismatch
		}
	} else if !namespaceKey.Finalized || header.KeyMode != "namespace-key-v1" || header.KeyVersion != namespaceKey.Version || header.Routing.NamespaceID != namespaceKey.NamespaceID || header.AccessGeneration != namespaceKey.AccessGeneration || header.AccessSetHash != namespaceKey.AccessSetHash || header.RosterEpoch != namespaceKey.IssuedRosterEpoch || header.RosterHash != namespaceKey.IssuedRosterHash {
		return nil, securityerr.ErrMetadataMismatch
	}
	active := false
	for _, c := range roster.Manifest.Manifest.Devices {
		if c.Certificate.DeviceID == header.Routing.OriginDevice && c.Certificate.SigningKeyID == device.SigningKeyID {
			active = true
		}
	}
	if !active {
		return nil, securityerr.ErrUntrustedRoster
	}
	env := EventEnvelopeV3{Version: envelopeVersionV3, Algorithm: envelopeV3ClearAlgorithm, Header: header, SignerDeviceID: header.Routing.OriginDevice, SignerKeyID: device.SigningKeyID}
	aad, err := headerAADV3(env)
	if err != nil {
		return nil, err
	}
	si, err := signatureInputV3(env, aad)
	if err != nil {
		return nil, err
	}
	env.Signature, err = keys.SignV2(device.SigningPrivate, si)
	if err != nil {
		return nil, err
	}
	return envelopeV3Enc.Marshal(env)
}

func OpenEnvelopeV3(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte) (SealedBodyV2, EventHeaderV2, error) {
	return openEnvelopeV3(data, roster, localDeviceID, localPrivate, nil, nil)
}

func OpenEnvelopeV3WithNamespaceProvider(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte, provider keyrotation.NamespaceKeyProvider) (SealedBodyV2, EventHeaderV2, error) {
	return openEnvelopeV3(data, roster, localDeviceID, localPrivate, provider, nil)
}

// OpenEnvelopeV3AuthenticatedWithNamespaceProvider is the durable-receive
// variant, signature-compatible with its v2 counterpart so the inbound path
// dispatches between the two generations with a single function value. The
// evidence digest wraps the v3 AAD in the SAME
// "aplexica/inbound-authenticated-header-evidence/v1" domain — the AAD itself
// already carries the v3 domain tag, so v2/v3 evidence can never collide.
func OpenEnvelopeV3AuthenticatedWithNamespaceProvider(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte, provider keyrotation.NamespaceKeyProvider) (SealedBodyV2, AuthenticatedEnvelopeV2, error) {
	var auth AuthenticatedEnvelopeV2
	body, _, err := openEnvelopeV3(data, roster, localDeviceID, localPrivate, provider, &auth)
	return body, auth, err
}

// openEnvelopeV3 is openEnvelopeV2 with the v3 framing: strict canonical-CBOR
// decode (unknown fields, duplicate keys, indefinite lengths, and tags all
// reject), the "zstd" body encoding beside "raw"/"gzip", and the v3 signature
// domains. Every roster / key-mode / recipient / header-body check is v2's,
// verbatim.
func openEnvelopeV3(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte, namespaceKeys keyrotation.NamespaceKeyProvider, authenticated *AuthenticatedEnvelopeV2) (SealedBodyV2, EventHeaderV2, error) {
	if len(data) == 0 || len(data) > envelopeV3MaxBytes {
		return SealedBodyV2{}, EventHeaderV2{}, securityerr.ErrLimitExceeded
	}
	var env EventEnvelopeV3
	// cbor.DecMode.Unmarshal consumes exactly one data item and rejects
	// trailing bytes (ExtraneousDataError), the CBOR analog of v2's
	// decode-then-EOF check.
	if err := envelopeV3Dec.Unmarshal(data, &env); err != nil {
		return SealedBodyV2{}, EventHeaderV2{}, err
	}
	keyModeOK := (env.Header.KeyMode == "recipient-wrap-v2" && env.Header.KeyVersion == 0) || (env.Header.KeyMode == "namespace-key-v1" && env.Header.KeyVersion > 0 && env.Header.Routing.NamespaceID != "")
	commonOK := env.Version == envelopeVersionV3 && env.Header.RosterEpoch == roster.Manifest.Manifest.Epoch && env.Header.RosterHash == [32]byte(roster.Hash) && env.Header.AccessGeneration == roster.Manifest.Manifest.AccessGeneration && env.Header.AccessSetHash == roster.Manifest.Manifest.AccessSetHash && env.Header.SecurityBarrierID != ([32]byte{}) && keyModeOK && env.Header.EventSalt == ([32]byte{})
	clear := env.Algorithm == envelopeV3ClearAlgorithm && env.Header.Purpose == "retained-clear"
	event := env.Algorithm == envelopeV3Algorithm && env.Header.Purpose == "event"
	if !commonOK || (!clear && !event) {
		return SealedBodyV2{}, env.Header, securityerr.ErrMetadataMismatch
	}
	if clear {
		if env.BodyEncoding != "" || len(env.BodyCiphertext) != 0 || env.BodyNonce != ([24]byte{}) || len(env.WrappedKeys) != 0 || !env.Header.Routing.Clear || env.Header.Routing.Lane != LaneRetained || env.Header.Canonical != (CanonicalMetadataV2{}) {
			return SealedBodyV2{}, env.Header, securityerr.ErrMetadataMismatch
		}
	} else if (env.BodyEncoding != envelopeV3EncodingRaw && env.BodyEncoding != envelopeV3EncodingGzip && env.BodyEncoding != envelopeV3EncodingZstd) || (env.Header.KeyMode == "recipient-wrap-v2" && (!wrapsCanonical(env.WrappedKeys) || len(env.WrappedKeys) != len(roster.Manifest.Manifest.Devices)+1)) || (env.Header.KeyMode == "namespace-key-v1" && len(env.WrappedKeys) != 0) {
		return SealedBodyV2{}, env.Header, securityerr.ErrMetadataMismatch
	}
	var signer *identity.DeviceCertificateUnsignedV1
	for i := range roster.Manifest.Manifest.Devices {
		c := &roster.Manifest.Manifest.Devices[i].Certificate
		if c.DeviceID == env.SignerDeviceID && c.SigningKeyID == env.SignerKeyID {
			signer = c
			break
		}
	}
	if signer == nil {
		return SealedBodyV2{}, env.Header, securityerr.ErrUntrustedRoster
	}
	if env.Header.Routing.OriginDevice == "" || env.Header.Routing.OriginDevice != env.SignerDeviceID {
		return SealedBodyV2{}, env.Header, securityerr.ErrMetadataMismatch
	}
	aad, err := headerAADV3(env)
	if err != nil {
		return SealedBodyV2{}, env.Header, err
	}
	si, err := signatureInputV3(env, aad)
	if err != nil {
		return SealedBodyV2{}, env.Header, err
	}
	if !keys.VerifyV2(ed25519.PublicKey(signer.SigningPublicKey[:]), si, env.Signature) {
		return SealedBodyV2{}, env.Header, securityerr.ErrInvalidSignature
	}
	evidenceInput, err := securewire.Canonical("aplexica/inbound-authenticated-header-evidence/v1", aad)
	if err != nil {
		return SealedBodyV2{}, env.Header, err
	}
	if authenticated != nil {
		*authenticated = AuthenticatedEnvelopeV2{
			Header:          env.Header,
			SignerDeviceID:  env.SignerDeviceID,
			SignerKeyID:     env.SignerKeyID,
			HeaderAADSHA256: sha256.Sum256(evidenceInput),
		}
	}
	if clear {
		return SealedBodyV2{}, env.Header, nil
	}
	var ek [32]byte
	if env.Header.KeyMode == "namespace-key-v1" {
		if namespaceKeys == nil {
			return SealedBodyV2{}, env.Header, securityerr.ErrStaleRoster
		}
		snapshot, err := namespaceKeys.ByVersion(context.Background(), env.Header.Routing.NamespaceID, env.Header.KeyVersion)
		if err != nil || !snapshot.Finalized || snapshot.NamespaceID != env.Header.Routing.NamespaceID || snapshot.Version != env.Header.KeyVersion || snapshot.AccessGeneration != env.Header.AccessGeneration || snapshot.AccessSetHash != env.Header.AccessSetHash || snapshot.IssuedRosterEpoch != env.Header.RosterEpoch || snapshot.IssuedRosterHash != env.Header.RosterHash || snapshot.Key == ([32]byte{}) {
			return SealedBodyV2{}, env.Header, securityerr.ErrStaleRoster
		}
		ek = snapshot.Key
	} else {
		hh := sha256.Sum256(aad)
		expected := map[string][32]byte{"recovery/account-recovery": roster.Authority.Anchor.Anchor.RecoveryWrapKeyID}
		for _, c := range roster.Manifest.Manifest.Devices {
			expected["device/"+c.Certificate.DeviceID] = c.Certificate.WrapKeyID
		}
		var mine *WrappedKeyEntryV2
		for i := range env.WrappedKeys {
			w := &env.WrappedKeys[i]
			id, ok := expected[w.RecipientType+"/"+w.RecipientID]
			if !ok || id != w.WrapKeyID {
				return SealedBodyV2{}, env.Header, securityerr.ErrMetadataMismatch
			}
			delete(expected, w.RecipientType+"/"+w.RecipientID)
			if w.RecipientType == "device" && w.RecipientID == localDeviceID {
				mine = w
			}
		}
		if len(expected) != 0 || mine == nil {
			return SealedBodyV2{}, env.Header, errNotARecipient
		}
		var err error
		ek, err = keys.UnwrapContentKeyV2(keys.WrappedKeyV2{EphemeralPublic: mine.EphemeralPublic, Nonce: mine.Nonce, Ciphertext: mine.Ciphertext}, localPrivate, hh, mine.RecipientType, mine.RecipientID, mine.WrapKeyID)
		if err != nil {
			return SealedBodyV2{}, env.Header, err
		}
	}
	plain, err := keys.OpenBodyV2(ek, env.BodyCiphertext, aad, env.BodyNonce)
	if err != nil {
		return SealedBodyV2{}, env.Header, err
	}
	plain, err = decompressBodyV3(plain, env.BodyEncoding)
	if err != nil {
		return SealedBodyV2{}, env.Header, err
	}
	bd := json.NewDecoder(bytes.NewReader(plain))
	bd.DisallowUnknownFields()
	var body SealedBodyV2
	if err := bd.Decode(&body); err != nil {
		return body, env.Header, err
	}
	if err := bd.Decode(&struct{}{}); err != io.EOF {
		return body, env.Header, securityerr.ErrMetadataMismatch
	}
	if err := validateHeaderBody(env.Header, body.Event, env.SignerDeviceID); err != nil {
		return body, env.Header, err
	}
	return body, env.Header, nil
}
