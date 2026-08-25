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

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/securewire"
	"github.com/aplexica/aplexica/internal/securityerr"
)

const envelopeV2Algorithm = "xchacha20poly1305+ed25519"
const envelopeV2ClearAlgorithm = "ed25519"
const envelopeV2MaxBytes = 64 << 20

type EventEnvelopeV2 struct {
	Version        uint16              `json:"version"`
	Algorithm      string              `json:"algorithm"`
	BodyEncoding   string              `json:"bodyEncoding"`
	Header         EventHeaderV2       `json:"header"`
	BodyNonce      [24]byte            `json:"bodyNonce"`
	BodyCiphertext []byte              `json:"bodyCiphertext"`
	WrappedKeys    []WrappedKeyEntryV2 `json:"wrappedKeys"`
	SignerDeviceID string              `json:"signerDeviceId"`
	SignerKeyID    [32]byte            `json:"signerKeyId"`
	Signature      [64]byte            `json:"signature"`
}
type EventHeaderV2 struct {
	Purpose           string              `json:"purpose"`
	Routing           RoutingMetadataV2   `json:"routing"`
	Canonical         CanonicalMetadataV2 `json:"canonical"`
	RosterEpoch       uint64              `json:"rosterEpoch"`
	RosterHash        [32]byte            `json:"rosterHash"`
	AccessGeneration  uint64              `json:"accessGeneration"`
	AccessSetHash     [32]byte            `json:"accessSetHash"`
	SecurityBarrierID [32]byte            `json:"securityBarrierId"`
	TreeHeadDigest    [32]byte            `json:"treeHeadDigest"`
	KeyMode           string              `json:"keyMode"`
	KeyVersion        uint64              `json:"keyVersion"`
	EventSalt         [32]byte            `json:"eventSalt"`
}
type RoutingMetadataV2 struct {
	NamespaceID       string `json:"namespaceId"`
	BranchID          string `json:"branchId"`
	ArtifactID        string `json:"artifactId"`
	WireEventID       string `json:"wireEventId"`
	ParentHash        string `json:"parentHash"`
	Kind              string `json:"kind"`
	EventType         string `json:"eventType"`
	TimestampUnixNano int64  `json:"timestampUnixNano"`
	Sequence          uint64 `json:"sequence"`
	OriginDevice      string `json:"originDevice"`
	SourceAgent       string `json:"sourceAgent"`
	Lane              string `json:"lane"`
	Clear             bool   `json:"clear"`
}
type CanonicalMetadataV2 struct {
	ArtifactID            string `json:"artifactId"`
	EventID               string `json:"eventId"`
	ParentHash            string `json:"parentHash"`
	Branch                string `json:"branch"`
	EventType             string `json:"eventType"`
	TimestampUnixNano     int64  `json:"timestampUnixNano"`
	ProvenanceDevice      string `json:"provenanceDevice"`
	ProvenanceSourceAgent string `json:"provenanceSourceAgent"`
	EventHash             string `json:"eventHash"`
	AlignedHead           string `json:"alignedHead"`
	AlignedEventID        string `json:"alignedEventId"`
}
type WrappedKeyEntryV2 struct {
	RecipientType   string   `json:"recipientType"`
	RecipientID     string   `json:"recipientId"`
	WrapKeyID       [32]byte `json:"wrapKeyId"`
	EphemeralPublic [32]byte `json:"ephemeralPublic"`
	Nonce           [24]byte `json:"nonce"`
	Ciphertext      []byte   `json:"ciphertext"`
}
type RemoteProjectClaimV2 struct {
	ID             string `json:"id"`
	VCS            string `json:"vcs"`
	RemoteIdentity string `json:"remoteIdentity"`
}
type SealedBodyV2 struct {
	Event      acf.Event             `json:"event"`
	EnvScope   acf.Scope             `json:"envScope"`
	EnvProject *RemoteProjectClaimV2 `json:"envProject,omitempty"`
}

// AuthenticatedEnvelopeV2 contains only values covered by a successfully
// verified envelope signature. HeaderAADSHA256 is a domain-separated digest
// of the exact canonical AAD that participated in that signature; it lets a
// durable authenticated no-op prove ownership without persisting body bytes.
type AuthenticatedEnvelopeV2 struct {
	Header          EventHeaderV2
	SignerDeviceID  string
	SignerKeyID     [32]byte
	HeaderAADSHA256 [32]byte
}

func headerAAD(e EventEnvelopeV2) ([]byte, error) {
	return securewire.Canonical("aplexica/event-envelope/v2", e.Version, e.Algorithm, e.Header, e.BodyEncoding, e.SignerDeviceID, e.SignerKeyID[:])
}
func signatureInput(e EventEnvelopeV2, aad []byte) ([]byte, error) {
	h := sha256.Sum256(e.BodyCiphertext)
	return securewire.Canonical("aplexica/event-signature/v2", aad, e.BodyNonce[:], h[:], e.WrappedKeys)
}
func wrapLess(a, b WrappedKeyEntryV2) bool {
	if a.RecipientType != b.RecipientType {
		return a.RecipientType < b.RecipientType
	}
	if a.RecipientID != b.RecipientID {
		return a.RecipientID < b.RecipientID
	}
	return bytes.Compare(a.WrapKeyID[:], b.WrapKeyID[:]) < 0
}
func wrapsCanonical(w []WrappedKeyEntryV2) bool {
	for i, x := range w {
		if (x.RecipientType != "device" && x.RecipientType != "recovery") || x.RecipientID == "" || x.WrapKeyID == ([32]byte{}) || len(x.Ciphertext) != 48 || (i > 0 && !wrapLess(w[i-1], x)) {
			return false
		}
	}
	return true
}

func metadataForEvent(e acf.Event) CanonicalMetadataV2 {
	return CanonicalMetadataV2{ArtifactID: e.ArtifactID, EventID: e.EventID, ParentHash: e.ParentHash, Branch: e.Branch, EventType: string(e.Type), TimestampUnixNano: e.Timestamp.UnixNano(), ProvenanceDevice: e.Provenance.DeviceID, ProvenanceSourceAgent: e.Provenance.SourceAgent, EventHash: e.Hash, AlignedHead: e.AlignedHead, AlignedEventID: e.AlignedEventID}
}
func validateHeaderBody(h EventHeaderV2, e acf.Event, signer string) error {
	c := h.Canonical
	r := h.Routing
	branch := e.Branch
	if branch == "" {
		branch = acf.MainBranch
	}
	normalized, err := acf.NormalizeBranchName(branch)
	if err != nil {
		return err
	}
	if c != metadataForEvent(e) || r.ArtifactID != c.ArtifactID || r.ParentHash != c.ParentHash || r.EventType != c.EventType || r.TimestampUnixNano != c.TimestampUnixNano || r.OriginDevice != c.ProvenanceDevice || r.SourceAgent != c.ProvenanceSourceAgent || r.OriginDevice != signer || r.BranchID != normalized {
		return securityerr.ErrMetadataMismatch
	}
	want, err := acf.ComputeHash(e)
	if err != nil {
		return err
	}
	if want != e.Hash || c.EventHash != e.Hash {
		return securityerr.ErrMetadataMismatch
	}
	return nil
}

func SealEnvelopeV2(event acf.Event, scope acf.Scope, projectInfo *project.ProjectInfo, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity) ([]byte, error) {
	return sealEnvelopeV2(event, scope, projectInfo, header, roster, device, nil)
}

func SealNamespaceEnvelopeV2(event acf.Event, scope acf.Scope, projectInfo *project.ProjectInfo, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey keyrotation.NamespaceKeySnapshot) ([]byte, error) {
	return sealEnvelopeV2(event, scope, projectInfo, header, roster, device, &namespaceKey)
}

func sealEnvelopeV2(event acf.Event, scope acf.Scope, projectInfo *project.ProjectInfo, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey *keyrotation.NamespaceKeySnapshot) ([]byte, error) {
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
	// Lane-aware pre-seal compression (2026-07-29 envelope wire-efficiency ADR
	// §3 D4): the signed header already carries the outbound lane
	// (Routing.Lane — checkpoints seal with LaneRetained, remote_checkpoint.go),
	// so the retained/checkpoint lane takes gzip.BestCompression and the live
	// lane keeps gzip.DefaultCompression. Plain gzip either way — decodable by
	// every existing openEnvelopeV2 unchanged.
	encoding := "raw"
	if body, ok := maybeCompressBody(plain, gzipLevelForLane(header.Routing.Lane)); ok {
		plain = body
		encoding = "gzip"
	}
	env := EventEnvelopeV2{Version: 2, Algorithm: envelopeV2Algorithm, BodyEncoding: encoding, Header: header, SignerDeviceID: header.Routing.OriginDevice, SignerKeyID: device.SigningKeyID}
	if _, err := io.ReadFull(rand.Reader, env.BodyNonce[:]); err != nil {
		return nil, err
	}
	aad, err := headerAAD(env)
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
	si, err := signatureInput(env, aad)
	if err != nil {
		return nil, err
	}
	env.Signature, err = keys.SignV2(device.SigningPrivate, si)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

func SealRetainedClearV2(header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity) ([]byte, error) {
	return sealRetainedClearV2(header, roster, device, nil)
}

func SealNamespaceRetainedClearV2(header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey keyrotation.NamespaceKeySnapshot) ([]byte, error) {
	return sealRetainedClearV2(header, roster, device, &namespaceKey)
}

func sealRetainedClearV2(header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, namespaceKey *keyrotation.NamespaceKeySnapshot) ([]byte, error) {
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
	env := EventEnvelopeV2{Version: 2, Algorithm: envelopeV2ClearAlgorithm, Header: header, SignerDeviceID: header.Routing.OriginDevice, SignerKeyID: device.SigningKeyID}
	aad, err := headerAAD(env)
	if err != nil {
		return nil, err
	}
	si, err := signatureInput(env, aad)
	if err != nil {
		return nil, err
	}
	env.Signature, err = keys.SignV2(device.SigningPrivate, si)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

func OpenEnvelopeV2(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte) (SealedBodyV2, EventHeaderV2, error) {
	return openEnvelopeV2(data, roster, localDeviceID, localPrivate, nil, nil)
}

func OpenEnvelopeV2WithNamespaceProvider(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte, provider keyrotation.NamespaceKeyProvider) (SealedBodyV2, EventHeaderV2, error) {
	return openEnvelopeV2(data, roster, localDeviceID, localPrivate, provider, nil)
}

// OpenEnvelopeV2AuthenticatedWithNamespaceProvider is the durable-receive
// variant. auth is populated after signature verification even when the local
// device is not a recipient, allowing that authenticated terminal outcome to
// be finalized without pretending a canonical event was materialized.
func OpenEnvelopeV2AuthenticatedWithNamespaceProvider(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte, provider keyrotation.NamespaceKeyProvider) (SealedBodyV2, AuthenticatedEnvelopeV2, error) {
	var auth AuthenticatedEnvelopeV2
	body, _, err := openEnvelopeV2(data, roster, localDeviceID, localPrivate, provider, &auth)
	return body, auth, err
}

func openEnvelopeV2(data []byte, roster identity.VerifiedRoster, localDeviceID string, localPrivate [32]byte, namespaceKeys keyrotation.NamespaceKeyProvider, authenticated *AuthenticatedEnvelopeV2) (SealedBodyV2, EventHeaderV2, error) {
	if len(data) == 0 || len(data) > envelopeV2MaxBytes {
		return SealedBodyV2{}, EventHeaderV2{}, securityerr.ErrLimitExceeded
	}
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), envelopeV2MaxBytes+1))
	dec.DisallowUnknownFields()
	var env EventEnvelopeV2
	if err := dec.Decode(&env); err != nil {
		return SealedBodyV2{}, EventHeaderV2{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return SealedBodyV2{}, EventHeaderV2{}, securityerr.ErrMetadataMismatch
	}
	keyModeOK := (env.Header.KeyMode == "recipient-wrap-v2" && env.Header.KeyVersion == 0) || (env.Header.KeyMode == "namespace-key-v1" && env.Header.KeyVersion > 0 && env.Header.Routing.NamespaceID != "")
	commonOK := env.Version == 2 && env.Header.RosterEpoch == roster.Manifest.Manifest.Epoch && env.Header.RosterHash == [32]byte(roster.Hash) && env.Header.AccessGeneration == roster.Manifest.Manifest.AccessGeneration && env.Header.AccessSetHash == roster.Manifest.Manifest.AccessSetHash && env.Header.SecurityBarrierID != ([32]byte{}) && keyModeOK && env.Header.EventSalt == ([32]byte{})
	clear := env.Algorithm == envelopeV2ClearAlgorithm && env.Header.Purpose == "retained-clear"
	event := env.Algorithm == envelopeV2Algorithm && env.Header.Purpose == "event"
	if !commonOK || (!clear && !event) {
		return SealedBodyV2{}, env.Header, securityerr.ErrMetadataMismatch
	}
	if clear {
		if env.BodyEncoding != "" || len(env.BodyCiphertext) != 0 || env.BodyNonce != ([24]byte{}) || len(env.WrappedKeys) != 0 || !env.Header.Routing.Clear || env.Header.Routing.Lane != LaneRetained || env.Header.Canonical != (CanonicalMetadataV2{}) {
			return SealedBodyV2{}, env.Header, securityerr.ErrMetadataMismatch
		}
	} else if (env.BodyEncoding != "raw" && env.BodyEncoding != "gzip") || (env.Header.KeyMode == "recipient-wrap-v2" && (!wrapsCanonical(env.WrappedKeys) || len(env.WrappedKeys) != len(roster.Manifest.Manifest.Devices)+1)) || (env.Header.KeyMode == "namespace-key-v1" && len(env.WrappedKeys) != 0) {
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
	aad, err := headerAAD(env)
	if err != nil {
		return SealedBodyV2{}, env.Header, err
	}
	si, err := signatureInput(env, aad)
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
	if env.BodyEncoding == "gzip" {
		gz, err := gzip.NewReader(bytes.NewReader(plain))
		if err != nil {
			return SealedBodyV2{}, env.Header, err
		}
		plain, err = io.ReadAll(io.LimitReader(gz, envelopeMaxDecompressedBytes+1))
		cerr := gz.Close()
		if err == nil {
			err = cerr
		}
		if err != nil || len(plain) > envelopeMaxDecompressedBytes {
			return SealedBodyV2{}, env.Header, securityerr.ErrLimitExceeded
		}
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

func NewEventHeaderV2(e acf.Event, kind acf.Kind, namespaceID, wireEventID, lane string, sequence uint64, roster identity.VerifiedRoster, barrier [32]byte) EventHeaderV2 {
	branch := e.Branch
	if branch == "" {
		branch = acf.MainBranch
	}
	normalized, _ := acf.NormalizeBranchName(branch)
	return EventHeaderV2{Purpose: "event", Routing: RoutingMetadataV2{NamespaceID: namespaceID, BranchID: normalized, ArtifactID: e.ArtifactID, WireEventID: wireEventID, ParentHash: e.ParentHash, Kind: string(kind), EventType: string(e.Type), TimestampUnixNano: e.Timestamp.UnixNano(), Sequence: sequence, OriginDevice: e.Provenance.DeviceID, SourceAgent: e.Provenance.SourceAgent, Lane: lane}, Canonical: metadataForEvent(e), RosterEpoch: roster.Manifest.Manifest.Epoch, RosterHash: [32]byte(roster.Hash), AccessGeneration: roster.Manifest.Manifest.AccessGeneration, AccessSetHash: roster.Manifest.Manifest.AccessSetHash, SecurityBarrierID: barrier, KeyMode: "recipient-wrap-v2"}
}
