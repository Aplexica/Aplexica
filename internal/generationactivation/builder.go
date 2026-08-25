package generationactivation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/fxamacker/cbor/v2"
)

var canonicalEncoding cbor.EncMode
var strictDecoding cbor.DecMode

func init() {
	var err error
	canonicalEncoding, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	strictDecoding, err = cbor.DecOptions{
		DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden,
		TagsMd: cbor.TagsForbidden, MaxNestedLevels: 32, MaxArrayElements: 1024,
		MaxMapPairs: 1024, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
	}.DecMode()
	if err != nil {
		panic(err)
	}
}

type BuildInput struct {
	AccountID               string
	NamespaceID             string
	StreamEpoch             string
	Roster                  identity.VerifiedRoster
	SecurityEpoch           SecurityEpochState
	DeviceID                string
	DeviceIdentity          keys.DeviceIdentity
	PreviousAuthorityDigest [32]byte
	Now                     time.Time
	Random                  io.Reader
}

func Build(input BuildInput) (GenerationActivationAttestationV1, []byte, [32]byte, error) {
	unsigned, binding, err := Prepare(input)
	if err != nil {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, err
	}
	endorsement, err := Endorse(input, unsigned)
	if err != nil {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, err
	}
	signed, blob, finalizedBinding, err := Finalize(input, unsigned, []ActivationEndorsementV1{endorsement})
	if err != nil {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, err
	}
	if binding != finalizedBinding {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, ErrInvalidState
	}
	return signed, blob, binding, nil
}

// Prepare creates the one canonical unsigned activation that every authority
// must inspect and endorse. It contains only public hashes/metadata and a
// random nonce, so it can be transported to other authority devices without
// exposing any private key.
func Prepare(input BuildInput) (GenerationActivationUnsignedV1, [32]byte, error) {
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	} else {
		input.Now = input.Now.UTC()
	}
	if input.Random == nil {
		input.Random = rand.Reader
	}
	unsigned, err := prepareUnsigned(input)
	if err != nil {
		return GenerationActivationUnsignedV1{}, [32]byte{}, err
	}
	var nonce [32]byte
	if _, err := io.ReadFull(input.Random, nonce[:]); err != nil || nonce == ([32]byte{}) {
		return GenerationActivationUnsignedV1{}, [32]byte{}, fmt.Errorf("generation activation: nonce: %w", err)
	}
	unsigned.IssuedAtUnix = input.Now.Unix()
	unsigned.NotAfterUnix = input.Now.Add(10 * time.Minute).Unix()
	unsigned.Nonce = nonce
	binding, err := BindingDigest(unsigned)
	if err != nil {
		return GenerationActivationUnsignedV1{}, [32]byte{}, err
	}
	return unsigned, binding, nil
}

// Endorse signs an already prepared activation using exactly one local active
// authority. Different devices call this independently for threshold > 1.
func Endorse(input BuildInput, unsigned GenerationActivationUnsignedV1) (ActivationEndorsementV1, error) {
	if err := validatePrepared(input, unsigned); err != nil {
		return ActivationEndorsementV1{}, err
	}
	manifest := input.Roster.Manifest.Manifest
	if len(input.DeviceIdentity.SigningPrivate) != ed25519.PrivateKeySize || len(input.DeviceIdentity.SigningPublic) != ed25519.PublicKeySize ||
		sha256.Sum256(input.DeviceIdentity.SigningPublic) != input.DeviceIdentity.SigningKeyID ||
		!input.DeviceIdentity.SigningPrivate.Public().(ed25519.PublicKey).Equal(input.DeviceIdentity.SigningPublic) {
		return ActivationEndorsementV1{}, ErrSigningAuthorityUnavailable
	}
	authority, authorized := input.Roster.Authority.Authorities[identity.DeviceKeyID(input.DeviceIdentity.SigningKeyID)]
	if !authorized || authority.DeviceID != input.DeviceID || authority.SigningKeyID != input.DeviceIdentity.SigningKeyID || authority.SigningPublicKey != [32]byte(input.DeviceIdentity.SigningPublic) ||
		!activeActivationAuthority(manifest, authority, unsigned.IssuedAtUnix, unsigned.NotAfterUnix) {
		return ActivationEndorsementV1{}, ErrSigningAuthorityUnavailable
	}
	signingBytes, err := canonicalEncoding.Marshal([]any{attestationDomain, unsigned})
	if err != nil {
		return ActivationEndorsementV1{}, err
	}
	signatureBytes := ed25519.Sign(input.DeviceIdentity.SigningPrivate, signingBytes)
	defer clearBuilderBytes(signatureBytes)
	var signature [64]byte
	copy(signature[:], signatureBytes)
	return ActivationEndorsementV1{SignerKeyID: input.DeviceIdentity.SigningKeyID, Signature: signature}, nil
}

// Finalize verifies an externally collected threshold and returns the exact
// canonical wire object. The order supplied by transports is irrelevant; the
// wire arrays are always sorted by key ID.
func Finalize(input BuildInput, unsigned GenerationActivationUnsignedV1, endorsements []ActivationEndorsementV1) (GenerationActivationAttestationV1, []byte, [32]byte, error) {
	if err := validatePrepared(input, unsigned); err != nil {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, err
	}
	if len(endorsements) < int(input.Roster.Authority.Threshold) || len(endorsements) > len(input.Roster.Authority.Authorities) {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, ErrSigningAuthorityUnavailable
	}
	sorted := append([]ActivationEndorsementV1(nil), endorsements...)
	sort.Slice(sorted, func(left, right int) bool {
		return bytes.Compare(sorted[left].SignerKeyID[:], sorted[right].SignerKeyID[:]) < 0
	})
	preimage, err := canonicalEncoding.Marshal([]any{attestationDomain, unsigned})
	if err != nil {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, err
	}
	signed := GenerationActivationAttestationV1{Attestation: unsigned, SignerKeyIDs: make([][32]byte, len(sorted)), Signatures: make([][64]byte, len(sorted))}
	for index, endorsement := range sorted {
		if index > 0 && endorsement.SignerKeyID == sorted[index-1].SignerKeyID {
			return GenerationActivationAttestationV1{}, nil, [32]byte{}, ErrSigningAuthorityUnavailable
		}
		authority, ok := input.Roster.Authority.Authorities[identity.DeviceKeyID(endorsement.SignerKeyID)]
		if !ok || !activeActivationAuthority(input.Roster.Manifest.Manifest, authority, unsigned.IssuedAtUnix, unsigned.NotAfterUnix) ||
			!ed25519.Verify(authority.SigningPublicKey[:], preimage, endorsement.Signature[:]) {
			return GenerationActivationAttestationV1{}, nil, [32]byte{}, ErrSigningAuthorityUnavailable
		}
		signed.SignerKeyIDs[index] = endorsement.SignerKeyID
		signed.Signatures[index] = endorsement.Signature
	}
	blob, err := canonicalEncoding.Marshal(signed)
	if err != nil {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, err
	}
	binding, err := BindingDigest(unsigned)
	if err != nil {
		return GenerationActivationAttestationV1{}, nil, [32]byte{}, err
	}
	return signed, blob, binding, nil
}

func validatePrepared(input BuildInput, unsigned GenerationActivationUnsignedV1) error {
	baseline, err := prepareUnsigned(input)
	if err != nil {
		return err
	}
	copy := unsigned
	copy.IssuedAtUnix = 0
	copy.NotAfterUnix = 0
	copy.Nonce = [32]byte{}
	if copy != baseline || unsigned.Nonce == ([32]byte{}) || unsigned.NotAfterUnix <= unsigned.IssuedAtUnix ||
		unsigned.NotAfterUnix > unsigned.IssuedAtUnix+int64(10*time.Minute/time.Second) {
		return ErrInvalidState
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if unsigned.IssuedAtUnix > now.Add(5*time.Minute).Unix() || now.Unix() >= unsigned.NotAfterUnix {
		return ErrInvalidState
	}
	return nil
}

func activeActivationAuthority(manifest identity.RosterManifestUnsignedV1, authority identity.RosterAuthorityV1, issuedAt, notAfter int64) bool {
	for _, signedCredential := range manifest.Devices {
		credential := signedCredential.Certificate
		if credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID &&
			credential.SigningPublicKey == authority.SigningPublicKey && credential.NotBeforeUnix <= issuedAt && credential.NotAfterUnix >= notAfter {
			return true
		}
	}
	return false
}

func clearBuilderBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func prepareUnsigned(input BuildInput) (GenerationActivationUnsignedV1, error) {
	manifest := input.Roster.Manifest.Manifest
	wantScopeType, wantScopeID := "account", input.Roster.Authority.Anchor.Anchor.PersonalScopeID
	if input.NamespaceID != "" {
		wantScopeType, wantScopeID = "namespace", input.NamespaceID
	}
	modeOK := input.SecurityEpoch.KeyMode == "recipient-wrap-v2" && input.SecurityEpoch.KeyVersion == 0 ||
		input.SecurityEpoch.KeyMode == "namespace-key-v1" && input.SecurityEpoch.KeyVersion > 0 && input.NamespaceID != ""
	if !validOpaque(input.AccountID, 256) || !validOpaque(input.DeviceID, 256) || !validOpaque(input.StreamEpoch, 256) ||
		input.AccountID != input.Roster.Authority.Anchor.Anchor.AccountID || manifest.ScopeType != wantScopeType || manifest.ScopeID != wantScopeID ||
		input.SecurityEpoch.Version != 1 || input.SecurityEpoch.ScopeType != wantScopeType || input.SecurityEpoch.ScopeID != wantScopeID ||
		input.SecurityEpoch.RosterHash != [32]byte(input.Roster.Hash) || input.SecurityEpoch.AccessGeneration != manifest.AccessGeneration ||
		input.SecurityEpoch.AccessSetHash != manifest.AccessSetHash || input.SecurityEpoch.CoordinatorGeneration == 0 ||
		input.SecurityEpoch.BarrierID == ([32]byte{}) || input.SecurityEpoch.TreeHeadDigest == ([32]byte{}) || !modeOK {
		return GenerationActivationUnsignedV1{}, ErrInvalidState
	}
	observedKeyVersion := uint64(0)
	if input.NamespaceID != "" && input.SecurityEpoch.KeyMode == "namespace-key-v1" {
		observedKeyVersion = input.SecurityEpoch.KeyVersion
	}
	unsigned := GenerationActivationUnsignedV1{
		Version: AttestationVersion, AccountID: input.AccountID, NamespaceID: input.NamespaceID,
		RosterScopeType: manifest.ScopeType, RosterScopeID: manifest.ScopeID, StreamEpoch: input.StreamEpoch,
		RosterEpoch: manifest.Epoch, RosterHash: [32]byte(input.Roster.Hash),
		AuthorityEpoch: input.Roster.Authority.AuthorityEpoch, AuthorityStateHash: input.Roster.Authority.StateHash,
		AccessGeneration: manifest.AccessGeneration, AccessSetHash: manifest.AccessSetHash,
		SecurityGeneration: input.SecurityEpoch.CoordinatorGeneration, SecurityBarrier: input.SecurityEpoch.BarrierID,
		KeyMode: input.SecurityEpoch.KeyMode, KeyVersion: input.SecurityEpoch.KeyVersion,
		ServerObservedNamespaceKeyVersion: observedKeyVersion, PreviousAuthorityDigest: input.PreviousAuthorityDigest,
	}
	return unsigned, nil
}

// BindingDigest omits freshness and nonce while retaining every field that
// defines the activated generation. It is only a local idempotency key; the
// full canonical attestation remains the server authority source digest.
func BindingDigest(unsigned GenerationActivationUnsignedV1) ([32]byte, error) {
	unsigned.IssuedAtUnix = 0
	unsigned.NotAfterUnix = 0
	unsigned.Nonce = [32]byte{}
	unsigned.PreviousAuthorityDigest = [32]byte{}
	raw, err := canonicalEncoding.Marshal([]any{bindingDigestDomain, unsigned})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// EndorsementRoundDigest identifies one exact generation transition while
// deliberately omitting only the short-lived proposal nonce and validity
// window. Unlike BindingDigest it retains PreviousAuthorityDigest: authority
// devices must never exchange signatures across two different server CAS
// predecessors, even when every local security-generation field is identical.
func EndorsementRoundDigest(unsigned GenerationActivationUnsignedV1) ([32]byte, error) {
	unsigned.IssuedAtUnix = 0
	unsigned.NotAfterUnix = 0
	unsigned.Nonce = [32]byte{}
	raw, err := canonicalEncoding.Marshal([]any{"aplexica/durable-sync-generation-activation-endorsement-round/v1", unsigned})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// EncodeUnsignedCanonical returns the exact public proposal bytes exchanged
// between authority devices. It contains no private material.
func EncodeUnsignedCanonical(unsigned GenerationActivationUnsignedV1) ([]byte, error) {
	return canonicalEncoding.Marshal(unsigned)
}

// DecodeUnsignedCanonical rejects malleable or oversized peer proposals before
// any local authority key is allowed to inspect or sign them.
func DecodeUnsignedCanonical(blob []byte) (GenerationActivationUnsignedV1, error) {
	var unsigned GenerationActivationUnsignedV1
	if len(blob) == 0 || len(blob) > 1<<20 || strictDecoding.Unmarshal(blob, &unsigned) != nil {
		return GenerationActivationUnsignedV1{}, ErrInvalidState
	}
	canonical, err := canonicalEncoding.Marshal(unsigned)
	if err != nil || !bytes.Equal(canonical, blob) {
		return GenerationActivationUnsignedV1{}, ErrInvalidState
	}
	return unsigned, nil
}

func DecodeCanonical(blob []byte) (GenerationActivationAttestationV1, error) {
	var signed GenerationActivationAttestationV1
	if len(blob) == 0 || len(blob) > 1<<20 || strictDecoding.Unmarshal(blob, &signed) != nil {
		return GenerationActivationAttestationV1{}, ErrInvalidState
	}
	canonical, err := canonicalEncoding.Marshal(signed)
	if err != nil || string(canonical) != string(blob) {
		return GenerationActivationAttestationV1{}, ErrInvalidState
	}
	return signed, nil
}

func validOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
