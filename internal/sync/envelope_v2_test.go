package syncd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/aplexica/aplexica/internal/securewire"
	"github.com/aplexica/aplexica/internal/securityerr"
)

func signedTestRoster(t *testing.T) (identity.VerifiedRoster, keys.DeviceIdentity) {
	t.Helper()
	s := &secrets.Store{Root: filepath.Join(t.TempDir(), "secrets")}
	id, err := (&keys.DeviceIdentityStore{Secrets: s}).LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	recoveryPub, recoveryPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, recoveryWrap, err := keys.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	var recoveryRoot [32]byte
	copy(recoveryRoot[:], recoveryPub)
	authority := identity.RosterAuthorityV1{DeviceID: "device-a", SigningKeyID: id.SigningKeyID}
	copy(authority.SigningPublicKey[:], id.SigningPublic)
	anchorU := identity.AccountTrustAnchorUnsignedV1{Version: 1, ServiceOrigin: "https://api.aplexica.com", AccountID: "account-a", PersonalScopeID: "0197f30a-3c58-7000-8000-000000000001", RecoveryKDFProfileID: "argon2id-256m-t3-p1-v1", RecoveryRootPublicKey: recoveryRoot, RecoveryWrapPublicKey: recoveryWrap, RecoveryWrapKeyID: sha256.Sum256(recoveryWrap[:]), AuthorityEpoch: 1, Authorities: []identity.RosterAuthorityV1{authority}, AuthorityThreshold: 1}
	ab, err := securewire.Canonical("aplexica/account-trust-anchor/v1", anchorU)
	if err != nil {
		t.Fatal(err)
	}
	anchor := identity.AccountTrustAnchorV1{Anchor: anchorU}
	copy(anchor.RecoverySignature[:], ed25519.Sign(recoveryPriv, ab))
	va, err := identity.VerifyTrustAnchor(anchor, recoveryPub)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute).Unix()
	certU := identity.DeviceCertificateUnsignedV1{Version: 1, AccountID: "account-a", UserID: "user-a", DeviceID: "device-a", KeyEpoch: 1, SigningKeyID: id.SigningKeyID, WrapKeyID: id.WrapKeyID, WrapPublicKey: id.WrapPublic, EnvelopeVersions: []uint16{2}, NotBeforeUnix: now, NotAfterUnix: now + 86400, IssuanceMode: "pairing", IssuedUnderAuthorityEpoch: 1, IssuingAuthorityStateHash: va.StateHash}
	copy(certU.SigningPublicKey[:], id.SigningPublic)
	cb, err := securewire.Canonical("aplexica/device-credential/v1", certU)
	if err != nil {
		t.Fatal(err)
	}
	cert := identity.DeviceCertificateV1{Certificate: certU, IssuerKeyIDs: [][32]byte{id.SigningKeyID}, IssuanceSignatures: make([][64]byte, 1)}
	copy(cert.IssuanceSignatures[0][:], ed25519.Sign(id.SigningPrivate, cb))
	m := identity.RosterManifestUnsignedV1{Version: 1, ScopeType: "account", ScopeID: anchorU.PersonalScopeID, Epoch: 1, TrustAnchorHash: [32]byte(va.AnchorHash), AuthorityStateHash: va.StateHash, AuthorityEpoch: 1, AccessGeneration: 1, IssuedAtUnix: time.Now().Unix(), NotAfterUnix: time.Now().Add(6 * time.Hour).Unix(), MinEnvelopeVersion: 2, Devices: []identity.DeviceCertificateV1{cert}}
	m.AccessSetHash, err = identity.AccessSetHash(m)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := securewire.Canonical("aplexica/roster-manifest/v1", m)
	if err != nil {
		t.Fatal(err)
	}
	r := identity.RosterManifestV1{Manifest: m, SignerKeyIDs: [][32]byte{id.SigningKeyID}, Signatures: make([][64]byte, 1)}
	copy(r.Signatures[0][:], ed25519.Sign(id.SigningPrivate, mb))
	vr, err := identity.VerifyGenesis(va, r)
	if err != nil {
		t.Fatal(err)
	}
	return vr, id
}

func TestEnvelopeV2RoundTripAndSignedRouting(t *testing.T) {
	roster, device := signedTestRoster(t)
	e := acf.Event{EventID: "0197f30a-3c58-7000-8000-000000000002", ArtifactID: "0197f30a-3c58-7000-8000-000000000003", Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(), Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex", AgentVersion: "1", AdapterVersion: "1"}, Payload: json.RawMessage(`{"format":"markdown","content":"secret"}`)}
	e.Hash, _ = acf.ComputeHash(e)
	var barrier [32]byte
	barrier[0] = 1
	h := NewEventHeaderV2(e, acf.KindMemory, "account-scope", e.EventID, "live", 1, roster, barrier)
	wire, err := SealEnvelopeV2(e, acf.ScopeGlobal, nil, h, roster, device)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := OpenEnvelopeV2(wire, roster, "device-a", device.WrapPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if body.Event.Hash != e.Hash {
		t.Fatal("event changed")
	}
	var env EventEnvelopeV2
	if err := json.Unmarshal(wire, &env); err != nil {
		t.Fatal(err)
	}
	env.Header.Routing.SourceAgent = "tampered"
	bad, _ := json.Marshal(env)
	if _, _, err := OpenEnvelopeV2(bad, roster, "device-a", device.WrapPrivate); err == nil {
		t.Fatal("signed routing tamper accepted")
	}
}

func TestEnvelopeV2AuthenticatedEvidenceSurvivesNotRecipientButRejectsSignerOriginSubstitution(t *testing.T) {
	roster, device := signedTestRoster(t)
	e := acf.Event{EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(), Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"}, Payload: json.RawMessage(`{"format":"markdown","content":"secret"}`)}
	e.Hash, _ = acf.ComputeHash(e)
	var barrier [32]byte
	barrier[0] = 1
	h := NewEventHeaderV2(e, acf.KindMemory, "", e.EventID, LaneLive, 1, roster, barrier)
	wire, err := SealEnvelopeV2(e, acf.ScopeGlobal, nil, h, roster, device)
	if err != nil {
		t.Fatal(err)
	}

	_, authenticated, err := OpenEnvelopeV2AuthenticatedWithNamespaceProvider(wire, roster, "device-b", device.WrapPrivate, nil)
	if !errors.Is(err, errNotARecipient) {
		t.Fatalf("wanted authenticated not-recipient, got %v", err)
	}
	if authenticated.Header != h || authenticated.SignerDeviceID != "device-a" || authenticated.SignerKeyID != device.SigningKeyID || authenticated.HeaderAADSHA256 == ([32]byte{}) {
		t.Fatalf("authenticated evidence incomplete: %+v", authenticated)
	}
	var env EventEnvelopeV2
	if err := json.Unmarshal(wire, &env); err != nil {
		t.Fatal(err)
	}
	aad, err := headerAAD(env)
	if err != nil {
		t.Fatal(err)
	}
	evidenceInput, err := securewire.Canonical("aplexica/inbound-authenticated-header-evidence/v1", aad)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.HeaderAADSHA256 != sha256.Sum256(evidenceInput) {
		t.Fatal("authenticated header evidence digest is not the domain-separated signed AAD digest")
	}

	// Re-sign a header that claims a different origin while retaining the real
	// signer identity. Signature validity alone must not authorize that
	// substitution, including on not-recipient/clear paths that never decrypt.
	env.Header.Routing.OriginDevice = "device-b"
	aad, err = headerAAD(env)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signatureInput(env, aad)
	if err != nil {
		t.Fatal(err)
	}
	env.Signature, err = keys.SignV2(device.SigningPrivate, signed)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	_, rejectedAuth, err := OpenEnvelopeV2AuthenticatedWithNamespaceProvider(tampered, roster, "device-b", device.WrapPrivate, nil)
	if !errors.Is(err, securityerr.ErrMetadataMismatch) || rejectedAuth != (AuthenticatedEnvelopeV2{}) {
		t.Fatalf("signer/origin substitution accepted: auth=%+v err=%v", rejectedAuth, err)
	}
}

// TestEnvelopeV2LaneAwareCompressionLevels verifies the lane signal already in
// the signed header (Routing.Lane) drives the pre-seal gzip level (2026-07-29
// envelope wire-efficiency ADR §3 D4): retained/checkpoint-lane bodies seal at
// gzip.BestCompression (RFC 1952 XFL header byte 2), live-lane bodies at
// gzip.DefaultCompression (XFL 0), sub-threshold bodies stay "raw" — and every
// variant opens with the EXISTING OpenEnvelopeV2 unchanged (golden
// round-trip).
func TestEnvelopeV2LaneAwareCompressionLevels(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	big := strings.Repeat("the same sentence over and over. ", 400) // ~13 KiB, compressible
	for _, tc := range []struct {
		name         string
		lane         string
		content      string
		wantEncoding string
		wantXFL      byte
	}{
		{"retained-best", LaneRetained, big, "gzip", 2},
		{"live-default", LaneLive, big, "gzip", 0},
		{"small-raw", LaneRetained, "tiny", "raw", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := acf.Event{EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(), Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"}, Payload: json.RawMessage(fmt.Sprintf(`{"format":"markdown","content":%q}`, tc.content))}
			e.Hash, _ = acf.ComputeHash(e)
			h := NewEventHeaderV2(e, acf.KindConversation, "account-scope", e.EventID, tc.lane, 1, roster, barrier)
			wire, err := SealEnvelopeV2(e, acf.ScopeGlobal, nil, h, roster, device)
			if err != nil {
				t.Fatal(err)
			}
			var env EventEnvelopeV2
			if err := json.Unmarshal(wire, &env); err != nil {
				t.Fatal(err)
			}
			if env.BodyEncoding != tc.wantEncoding {
				t.Fatalf("BodyEncoding = %q, want %q", env.BodyEncoding, tc.wantEncoding)
			}
			if tc.wantEncoding == "gzip" {
				// Peel the AEAD open only (no gzip) to inspect the compressed
				// body: Go's gzip records the level in the RFC 1952 XFL header
				// byte (offset 8): 2 at BestCompression, 0 at Default.
				aad, err := headerAAD(env)
				if err != nil {
					t.Fatal(err)
				}
				hh := sha256.Sum256(aad)
				var mine *WrappedKeyEntryV2
				for i := range env.WrappedKeys {
					if env.WrappedKeys[i].RecipientType == "device" && env.WrappedKeys[i].RecipientID == "device-a" {
						mine = &env.WrappedKeys[i]
					}
				}
				if mine == nil {
					t.Fatal("no wrapped key for device-a")
				}
				ek, err := keys.UnwrapContentKeyV2(keys.WrappedKeyV2{EphemeralPublic: mine.EphemeralPublic, Nonce: mine.Nonce, Ciphertext: mine.Ciphertext}, device.WrapPrivate, hh, mine.RecipientType, mine.RecipientID, mine.WrapKeyID)
				if err != nil {
					t.Fatal(err)
				}
				compressed, err := keys.OpenBodyV2(ek, env.BodyCiphertext, aad, env.BodyNonce)
				if err != nil {
					t.Fatal(err)
				}
				if compressed[8] != tc.wantXFL {
					t.Fatalf("gzip XFL byte = %d, want %d", compressed[8], tc.wantXFL)
				}
			}
			body, _, err := OpenEnvelopeV2(wire, roster, "device-a", device.WrapPrivate)
			if err != nil {
				t.Fatal(err)
			}
			if string(body.Event.Payload) != string(e.Payload) {
				t.Fatal("payload changed across seal/open")
			}
		})
	}
}

func TestRetainedClearV2RequiresValidSignature(t *testing.T) {
	roster, device := signedTestRoster(t)
	e := acf.Event{EventID: "0197f30a-3c58-7000-8000-000000000002", ArtifactID: "0197f30a-3c58-7000-8000-000000000003", Type: acf.EventTypeRedaction, Timestamp: time.Now().UTC(), Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"}}
	var barrier [32]byte
	barrier[0] = 1
	h := NewEventHeaderV2(e, acf.KindConversation, "account-scope", RetainedWireEventID(e.EventID, "device-a"), LaneRetained, 1, roster, barrier)
	h.Purpose = "retained-clear"
	h.Routing.Clear = true
	h.Canonical = CanonicalMetadataV2{}
	wire, err := SealRetainedClearV2(h, roster, device)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := OpenEnvelopeV2(wire, roster, "device-a", device.WrapPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Routing.Clear {
		t.Fatal("clear flag lost")
	}
	var env EventEnvelopeV2
	if err := json.Unmarshal(wire, &env); err != nil {
		t.Fatal(err)
	}
	env.Header.Routing.ArtifactID = "tampered"
	bad, _ := json.Marshal(env)
	if _, _, err := OpenEnvelopeV2(bad, roster, "device-a", device.WrapPrivate); err == nil {
		t.Fatal("tampered clear accepted")
	}
}
