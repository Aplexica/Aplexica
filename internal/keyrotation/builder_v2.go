package keyrotation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
)

// RotationEndorsementV1 is one independently collected signature from an
// active operational authority in the previous roster. Private keys never
// cross this API; a threshold greater than one is satisfied only by supplying
// distinct real endorsements.
type RotationEndorsementV1 struct {
	SignerKeyID [32]byte
	Signature   [64]byte
}

func PrepareRotationStatement(previous, next identity.VerifiedRoster, current NamespaceKeySnapshot, issuedAt, expiresAt time.Time, nonce [32]byte) (RotationStatementUnsignedV1, error) {
	issuedAt = issuedAt.UTC().Truncate(time.Second)
	expiresAt = expiresAt.UTC().Truncate(time.Second)
	transitionHash, err := RosterTransitionHash(previous, next)
	if err != nil || current.NamespaceID != next.Manifest.Manifest.ScopeID || !current.Finalized || current.Version == 0 ||
		current.Version >= MaxNamespaceKeyVersion || current.AccessGeneration != previous.Manifest.Manifest.AccessGeneration ||
		current.AccessSetHash != previous.Manifest.Manifest.AccessSetHash || current.IssuedRosterHash != [32]byte(previous.Hash) ||
		issuedAt.IsZero() || !expiresAt.After(issuedAt) || nonce == ([32]byte{}) {
		return RotationStatementUnsignedV1{}, fmt.Errorf("keyrotation: invalid rotation proposal")
	}
	removed, added, changed := accessDiff(previous, next)
	statement := RotationStatementUnsignedV1{
		Version: 1, NamespaceID: next.Manifest.Manifest.ScopeID, PreviousVersion: current.Version, NewVersion: current.Version + 1,
		PreviousRosterHash: [32]byte(previous.Hash), NewRosterEpoch: next.Manifest.Manifest.Epoch, NewRosterHash: [32]byte(next.Hash),
		PreviousAccessGeneration: previous.Manifest.Manifest.AccessGeneration, PreviousAccessSetHash: previous.Manifest.Manifest.AccessSetHash,
		NewAccessGeneration: next.Manifest.Manifest.AccessGeneration, NewAccessSetHash: next.Manifest.Manifest.AccessSetHash,
		RosterTransitionHash: transitionHash, AuthorityStateHash: next.Authority.StateHash, AuthorityEpoch: next.Authority.AuthorityEpoch,
		RemovedDeviceIDs: removed, AddedDeviceIDs: added, ChangedDeviceIDs: changed,
		IssuedAtUnix: issuedAt.Unix(), ExpiresAtUnix: expiresAt.Unix(), Nonce: nonce,
	}
	// Metadata verification is repeated after signatures are attached. The
	// unsigned phase still rejects a non-access transition immediately.
	if next.Manifest.Manifest.AccessGeneration != previous.Manifest.Manifest.AccessGeneration+1 ||
		next.Manifest.Manifest.AccessSetHash == previous.Manifest.Manifest.AccessSetHash {
		return RotationStatementUnsignedV1{}, fmt.Errorf("keyrotation: rotation requires an access cutover")
	}
	return statement, nil
}

func EndorseRotationStatement(previous identity.VerifiedRoster, statement RotationStatementUnsignedV1, signerKeyID [32]byte, private ed25519.PrivateKey) (RotationEndorsementV1, error) {
	authority, ok := previous.Authority.Authorities[identity.DeviceKeyID(signerKeyID)]
	if !ok || !activePreviousAuthority(previous, authority, statement.IssuedAtUnix) || !rotationPrivateMatches(private, authority.SigningKeyID, authority.SigningPublicKey) {
		return RotationEndorsementV1{}, fmt.Errorf("keyrotation: active rotation authority unavailable")
	}
	preimage, err := rotationCanonical("aplexica/namespace-rotation-statement/v1", statement)
	if err != nil {
		return RotationEndorsementV1{}, err
	}
	signatureBytes := ed25519.Sign(private, preimage)
	defer clearRotationBytes(signatureBytes)
	var signature [64]byte
	copy(signature[:], signatureBytes)
	return RotationEndorsementV1{SignerKeyID: signerKeyID, Signature: signature}, nil
}

func FinalizeRotationStatement(previous, next identity.VerifiedRoster, statement RotationStatementUnsignedV1, endorsements []RotationEndorsementV1, now time.Time) (SignedRotationStatementV1, error) {
	if len(endorsements) < int(previous.Authority.Threshold) || len(endorsements) > len(previous.Authority.Authorities) {
		return SignedRotationStatementV1{}, fmt.Errorf("keyrotation: insufficient rotation signatures")
	}
	sorted := append([]RotationEndorsementV1(nil), endorsements...)
	sort.Slice(sorted, func(left, right int) bool {
		return bytes.Compare(sorted[left].SignerKeyID[:], sorted[right].SignerKeyID[:]) < 0
	})
	signed := SignedRotationStatementV1{Statement: statement, SignerKeyIDs: make([][32]byte, len(sorted)), Signatures: make([][64]byte, len(sorted))}
	for index, endorsement := range sorted {
		if index > 0 && endorsement.SignerKeyID == sorted[index-1].SignerKeyID {
			return SignedRotationStatementV1{}, fmt.Errorf("keyrotation: duplicate rotation signer")
		}
		signed.SignerKeyIDs[index] = endorsement.SignerKeyID
		signed.Signatures[index] = endorsement.Signature
	}
	if err := VerifyRotationStatement(previous, next, signed, now); err != nil {
		return SignedRotationStatementV1{}, err
	}
	return signed, nil
}

// BuildNamespaceKeyManifest creates a fresh content key, wraps it to exactly
// the next roster plus the recovery recipient, signs the ciphertext-only
// manifest, and clears the plaintext key before returning. The returned value
// is safe to journal or relay; it contains no plaintext content key.
func BuildNamespaceKeyManifest(previous, next identity.VerifiedRoster, statement SignedRotationStatementV1, leaderDeviceID string, leaderPrivate ed25519.PrivateKey, random io.Reader, now time.Time) (SignedNamespaceKeyManifestV1, error) {
	if random == nil || VerifyRotationStatement(previous, next, statement, now) != nil {
		return SignedNamespaceKeyManifestV1{}, fmt.Errorf("keyrotation: invalid finalized rotation")
	}
	var leader *identity.DeviceCertificateUnsignedV1
	for index := range next.Manifest.Manifest.Devices {
		candidate := &next.Manifest.Manifest.Devices[index].Certificate
		if candidate.DeviceID == leaderDeviceID {
			leader = candidate
			break
		}
	}
	if leader == nil || !rotationPrivateMatches(leaderPrivate, leader.SigningKeyID, leader.SigningPublicKey) {
		return SignedNamespaceKeyManifestV1{}, fmt.Errorf("keyrotation: manifest leader unavailable")
	}
	statementHash, err := StatementHash(statement)
	if err != nil {
		return SignedNamespaceKeyManifestV1{}, err
	}
	var contentKey [32]byte
	if _, err := io.ReadFull(random, contentKey[:]); err != nil || contentKey == ([32]byte{}) {
		clearRotationBytes(contentKey[:])
		return SignedNamespaceKeyManifestV1{}, fmt.Errorf("keyrotation: generate content key: %w", err)
	}
	defer clearRotationBytes(contentKey[:])
	type recipient struct {
		typ, id string
		keyID   [32]byte
		public  [32]byte
	}
	recipients := make([]recipient, 0, len(next.Manifest.Manifest.Devices)+1)
	for _, signedCredential := range next.Manifest.Manifest.Devices {
		credential := signedCredential.Certificate
		recipients = append(recipients, recipient{"device", credential.DeviceID, credential.WrapKeyID, credential.WrapPublicKey})
	}
	anchor := next.Authority.Anchor.Anchor
	recipients = append(recipients, recipient{"recovery", "account-recovery", anchor.RecoveryWrapKeyID, anchor.RecoveryWrapPublicKey})
	sort.Slice(recipients, func(left, right int) bool {
		return recipients[left].typ+"\x00"+recipients[left].id < recipients[right].typ+"\x00"+recipients[right].id
	})
	manifest := NamespaceKeyManifestUnsignedV1{
		Version: 1, StatementHash: statementHash, NamespaceID: statement.Statement.NamespaceID,
		KeyVersion: statement.Statement.NewVersion, AccessGeneration: statement.Statement.NewAccessGeneration,
		AccessSetHash: statement.Statement.NewAccessSetHash, IssuedRosterEpoch: statement.Statement.NewRosterEpoch,
		IssuedRosterHash: statement.Statement.NewRosterHash, AuthorityStateHash: statement.Statement.AuthorityStateHash,
		LeaderDeviceID: leader.DeviceID, LeaderSigningKeyID: leader.SigningKeyID,
		Wrapped: make([]WrappedKeyEntryV2, 0, len(recipients)),
	}
	for _, recipient := range recipients {
		context := WrapContextV2{NamespaceID: manifest.NamespaceID, KeyVersion: manifest.KeyVersion, StatementHash: statementHash,
			RecipientType: recipient.typ, RecipientID: recipient.id, RecipientWrapKeyID: recipient.keyID,
			AccessGeneration: manifest.AccessGeneration, AccessSetHash: manifest.AccessSetHash}
		wrapped, err := wrapKeyV2WithReader(random, contentKey, recipient.public, context)
		if err != nil {
			return SignedNamespaceKeyManifestV1{}, err
		}
		manifest.Wrapped = append(manifest.Wrapped, WrappedKeyEntryV2{RecipientType: recipient.typ, RecipientID: recipient.id, RecipientWrapKeyID: recipient.keyID, Wrapped: wrapped})
	}
	preimage, err := rotationCanonical("aplexica/namespace-key-manifest/v1", manifest)
	if err != nil {
		return SignedNamespaceKeyManifestV1{}, err
	}
	signatureBytes := ed25519.Sign(leaderPrivate, preimage)
	defer clearRotationBytes(signatureBytes)
	var signature [64]byte
	copy(signature[:], signatureBytes)
	signedManifest := SignedNamespaceKeyManifestV1{Manifest: manifest, Signature: signature}
	if err := VerifyNamespaceKeyManifest(next, statement, signedManifest); err != nil {
		return SignedNamespaceKeyManifestV1{}, err
	}
	return signedManifest, nil
}

func rotationPrivateMatches(private ed25519.PrivateKey, keyID [32]byte, public [32]byte) bool {
	if len(private) != ed25519.PrivateKeySize {
		return false
	}
	reconstructed := ed25519.NewKeyFromSeed(private.Seed())
	defer clearRotationBytes(reconstructed)
	derived := reconstructed.Public().(ed25519.PublicKey)
	return private.Equal(reconstructed) && [32]byte(derived) == public && sha256.Sum256(derived) == keyID
}

func clearRotationBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
