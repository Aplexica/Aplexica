package syncd

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/project"
)

// ---------------------------------------------------------------------------
// Per-event end-to-end encryption envelope.
//
// Each outbound canonical event is sealed into a SELF-CONTAINED envelope:
//
//   1. plaintext  pt = json(acf.Event)
//   2. content key K = keys.NewContentKey()  (fresh per event — no key
//      distribution, no ordering hazards)
//   3. (nonce, ct) = AES-GCM-256(K, pt)      (keys.SealBody)
//   4. for each recipient device (deviceID, X25519 wrap pub):
//        wrapped = keys.WrapContentKey(K, pub)   (X25519+HKDF+AES-GCM,
//        byte-compatible with the cloud plugin's key hierarchy)
//   5. envelope = {v, alg, nonce, ct, keys:[{device, wrapped}...]} as JSON
//
// The envelope JSON becomes OutboundEvent.Bytes. A conforming remote plugin
// treats Bytes as opaque and publishes it verbatim; it never reads,
// canonicalises, or re-encrypts an artifact body. Only ciphertext and opaque
// routing metadata reach the relay, so the relay never sees plaintext.
//
// INBOUND: a receiving device finds the keys[] entry addressed to ITS device id,
// keys.UnwrapContentKey's K with its private key, then keys.OpenBody's the event.
// A device not in the recipient set returns errNotARecipient and is skipped
// (not an error on the stream).
// ---------------------------------------------------------------------------

// envelopeVersion / envelopeAlg identify the wire format so a future scheme
// (e.g. a shared namespace content key instead of per-event keys) can coexist.
const (
	envelopeVersion = 1
	envelopeAlg     = "aesgcm256+x25519wrap"
)

const (
	// envelopeCompressThreshold: plaintext bodies above this gzip before
	// sealing. Conversation full-state payloads are highly compressible text,
	// so this multiplies the remotePublishMaxEventBytes headroom ~5-10x.
	// 4 KiB per the 2026-07-29 envelope wire-efficiency ADR §3 (D4, lever L3):
	// mid-size bodies compress too, and because compression is
	// keep-only-if-smaller (maybeCompressBody) tiny incompressible bodies
	// still ship raw. Bodies at or below the threshold stay raw and
	// byte-compatible with older receivers; above it the output is plain
	// gzip, which every existing decoder already handles.
	envelopeCompressThreshold = 4 * 1024
	// envelopeMaxDecompressedBytes bounds decompression (defense in depth —
	// the envelope is AEAD-authenticated from a paired device, but a bound
	// costs nothing).
	envelopeMaxDecompressedBytes = 256 << 20
)

var (
	// errNoRecipients is returned by sealEnvelope when the recipient set is
	// empty. The caller MUST drop the outbound event — never fall back to
	// plaintext.
	errNoRecipients = errors.New("syncd: no recipients to encrypt to (dropping outbound; never plaintext)")
	// errNotARecipient is returned by openEnvelope when no keys[] entry is
	// addressed to this device. The caller skips the event (it is for other
	// devices), distinct from an authentication failure.
	errNotARecipient = errors.New("syncd: event is not addressed to this device")
)

// gzipLevelForLane maps an outbound lane to the pre-seal gzip level
// (2026-07-29 envelope wire-efficiency ADR §3 D4, lever L4): retained /
// checkpoint bodies are latency-insensitive (debounced ≥60s upstream) and take
// gzip.BestCompression; the live lane — and any unknown lane, failing to the
// latency-safe choice — keeps gzip.DefaultCompression, the previous implicit
// level everywhere. Level selection changes bytes, never format: the output is
// plain gzip at every level, decodable by every existing receiver unchanged.
func gzipLevelForLane(lane string) int {
	if lane == LaneRetained {
		return gzip.BestCompression
	}
	return gzip.DefaultCompression
}

// maybeCompressBody gzips pt at the given level when it exceeds
// envelopeCompressThreshold, keeping the result only if strictly smaller
// (keep-only-if-smaller, ADR §3 D4). It returns the body to seal and whether
// gzip was applied; on a compression error or a non-shrinking result the
// original bytes ship raw. Shared by the v1 and v2 seal paths so both
// generations apply the identical threshold + keep-only-if-smaller policy.
func maybeCompressBody(pt []byte, level int) ([]byte, bool) {
	if len(pt) <= envelopeCompressThreshold {
		return pt, false
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return pt, false
	}
	if _, werr := zw.Write(pt); werr != nil || zw.Close() != nil || buf.Len() >= len(pt) {
		return pt, false
	}
	return buf.Bytes(), true
}

// recipient is one device the envelope is sealed for.
type recipient struct {
	deviceID string
	pub      [keys.X25519KeySize]byte
}

// wrappedKeyEntry is one per-recipient wrapped content key in the envelope.
type wrappedKeyEntry struct {
	Device  string `json:"device"`
	Wrapped []byte `json:"wrapped"` // base64 on the wire
}

// eventEnvelope is the self-contained per-event E2E envelope. It is the JSON
// carried verbatim as OutboundEvent.Bytes.
type eventEnvelope struct {
	V     int               `json:"v"`
	Alg   string            `json:"alg"`
	Nonce []byte            `json:"nonce"` // base64 on the wire
	CT    []byte            `json:"ct"`    // base64 on the wire
	Keys  []wrappedKeyEntry `json:"keys"`
	// Enc is the plaintext-body encoding applied BEFORE encryption: "" (raw)
	// or "gzip". Compressed envelopes require a receiver that understands
	// this field during a same-fleet upgrade.
	Enc string `json:"enc,omitempty"`
}

// sealedBody is the plaintext that is AES-GCM-sealed into the envelope. The
// canonical event is embedded anonymously so its fields are promoted to the top
// level — wire-compatible with pre-v0.x envelopes that sealed a bare acf.Event
// (an old receiver unmarshaling into acf.Event simply ignores envScope/envProject,
// and a new receiver reading an old body sees no scope and defaults to global).
//
// envScope/envProject carry the artifact's BRD-02 §4.13 project identity so the
// receiver stages/materializes a project-scoped artifact to the right place
// (FR-02.38) instead of defaulting it to global. They ride INSIDE the encrypted
// body — never the plaintext wire — so the relay never learns the (potentially
// repo-identifying) project id. Zero-knowledge invariant preserved.
type sealedBody struct {
	acf.Event
	EnvScope   acf.Scope            `json:"envScope,omitempty"`
	EnvProject *project.ProjectInfo `json:"envProject,omitempty"`
}

// sealEnvelope encrypts the canonical event (plus its artifact scope/project
// identity) for the given recipients. Returns errNoRecipients when recipients is
// empty so the caller drops the event rather than transmitting plaintext.
//
// It compresses at gzip.DefaultCompression — the live-lane level and the
// previous implicit level everywhere. Unlike v2, whose signed header carries
// Routing.Lane into the seal, the v1 seal path receives no lane signal, so a
// retained/checkpoint caller that wants gzip.BestCompression selects it
// explicitly via sealEnvelopeWithLevel.
func sealEnvelope(e acf.Event, scope acf.Scope, proj *project.ProjectInfo, recipients []recipient) ([]byte, error) {
	return sealEnvelopeWithLevel(e, scope, proj, recipients, gzip.DefaultCompression)
}

// sealEnvelopeWithLevel is sealEnvelope with an explicit pre-seal gzip level
// (gzipLevelForLane maps lanes to levels). The level changes bytes, not
// format: output at every level decodes with the existing openEnvelope.
func sealEnvelopeWithLevel(e acf.Event, scope acf.Scope, proj *project.ProjectInfo, recipients []recipient, compressLevel int) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errNoRecipients
	}
	pt, err := json.Marshal(sealedBody{Event: e, EnvScope: scope, EnvProject: proj})
	if err != nil {
		return nil, fmt.Errorf("syncd: marshal event for seal: %w", err)
	}
	enc := ""
	if body, ok := maybeCompressBody(pt, compressLevel); ok {
		pt = body
		enc = "gzip"
	}
	contentKey, err := keys.NewContentKey()
	if err != nil {
		return nil, err
	}
	nonce, ct, err := keys.SealBody(contentKey, pt)
	if err != nil {
		return nil, err
	}
	env := eventEnvelope{
		V:     envelopeVersion,
		Alg:   envelopeAlg,
		Nonce: nonce,
		CT:    ct,
		Keys:  make([]wrappedKeyEntry, 0, len(recipients)),
		Enc:   enc,
	}
	for _, r := range recipients {
		wrapped, werr := keys.WrapContentKey(contentKey, r.pub)
		if werr != nil {
			return nil, fmt.Errorf("syncd: wrap content key for %s: %w", r.deviceID, werr)
		}
		env.Keys = append(env.Keys, wrappedKeyEntry{Device: r.deviceID, Wrapped: wrapped})
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("syncd: marshal envelope: %w", err)
	}
	return out, nil
}

// openEnvelope decrypts the envelope addressed to localDeviceID using this
// device's X25519 private key. Returns the canonical event plus the artifact
// scope/project identity sealed alongside it (empty scope / nil project for an
// old-format envelope that carried only the event). Returns errNotARecipient
// when no keys[] entry targets this device (skip, don't error). Any other error
// is a genuine decode/auth failure.
func openEnvelope(b []byte, localDeviceID string, priv [keys.X25519KeySize]byte) (acf.Event, acf.Scope, *project.ProjectInfo, error) {
	var env eventEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return acf.Event{}, "", nil, fmt.Errorf("syncd: unmarshal envelope: %w", err)
	}
	if env.V != envelopeVersion || env.Alg != envelopeAlg {
		return acf.Event{}, "", nil, fmt.Errorf("syncd: unsupported envelope (v=%d alg=%q)", env.V, env.Alg)
	}
	var wrapped []byte
	found := false
	for _, k := range env.Keys {
		if k.Device == localDeviceID {
			wrapped = k.Wrapped
			found = true
			break
		}
	}
	if !found {
		return acf.Event{}, "", nil, errNotARecipient
	}
	contentKey, err := keys.UnwrapContentKey(wrapped, priv)
	if err != nil {
		return acf.Event{}, "", nil, fmt.Errorf("syncd: unwrap content key: %w", err)
	}
	pt, err := keys.OpenBody(contentKey, env.Nonce, env.CT)
	if err != nil {
		return acf.Event{}, "", nil, fmt.Errorf("syncd: open event body: %w", err)
	}
	switch env.Enc {
	case "":
	case "gzip":
		zr, zerr := gzip.NewReader(bytes.NewReader(pt))
		if zerr != nil {
			return acf.Event{}, "", nil, fmt.Errorf("syncd: open compressed envelope: %w", zerr)
		}
		decompressed, rerr := io.ReadAll(io.LimitReader(zr, envelopeMaxDecompressedBytes))
		if cerr := zr.Close(); rerr == nil && cerr != nil {
			rerr = cerr
		}
		if rerr != nil {
			return acf.Event{}, "", nil, fmt.Errorf("syncd: decompress envelope body: %w", rerr)
		}
		pt = decompressed
	default:
		return acf.Event{}, "", nil, fmt.Errorf("syncd: unsupported envelope body encoding %q", env.Enc)
	}
	var sb sealedBody
	if err := json.Unmarshal(pt, &sb); err != nil {
		return acf.Event{}, "", nil, fmt.Errorf("syncd: unmarshal decrypted event: %w", err)
	}
	return sb.Event, sb.EnvScope, sb.EnvProject, nil
}
