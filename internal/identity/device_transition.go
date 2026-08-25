package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/securityerr"
)

var (
	ErrInvalidDeviceTransition       = errors.New("identity: invalid device transition")
	ErrDeviceTransitionSignerOffline = errors.New("identity: active transition authority unavailable")
)

// DeviceTransitionEndorsementV1 is an externally collectable operational-
// authority signature. Finalizers sort and verify distinct endorsements; they
// never assume that another authority private key exists on this device.
type DeviceTransitionEndorsementV1 struct {
	SignerKeyID [32]byte
	Signature   [64]byte
}

type PairingCredentialProposalV1 struct {
	Transcript PairingTranscriptV1
	UserID     string
	KeyEpoch   uint64
	JoinNonce  [32]byte
	IssuedAt   time.Time
	NotAfter   time.Time
}

type RecoveryCredentialProposalV1 struct {
	Transition  AuthorityTransitionV1
	Enrollments []RecoveryEnrollmentV1
	DeviceID    string
	UserID      string
	KeyEpoch    uint64
}

// PreparePairingCredential binds a candidate's long-term public keys to the
// exact locally pinned roster and authenticated pairing transcript. The caller
// must next attach the candidate-possession and active-device approval proofs,
// then collect the current operational-authority threshold.
func PreparePairingCredential(previous VerifiedRoster, proposal PairingCredentialProposalV1) (DeviceCertificateUnsignedV1, error) {
	t := proposal.Transcript
	issuedAt := proposal.IssuedAt.UTC().Truncate(time.Second)
	notAfter := proposal.NotAfter.UTC().Truncate(time.Second)
	transcriptHash, err := TranscriptHash(t)
	if err != nil {
		return DeviceCertificateUnsignedV1{}, err
	}
	anchor := previous.Authority.Anchor.Anchor
	previousHash, err := HashRoster(previous.Manifest)
	if err != nil || previousHash != previous.Hash || t.Version != 1 || t.ServiceOrigin != anchor.ServiceOrigin ||
		t.AccountID != anchor.AccountID || t.TrustAnchorHash != [32]byte(previous.Authority.AnchorHash) ||
		t.CurrentRosterHash != [32]byte(previous.Hash) || !validText(proposal.UserID, 256) ||
		!validText(t.CandidateDeviceID, 256) || !validText(t.ApproverDeviceID, 256) ||
		proposal.JoinNonce == ([32]byte{}) || proposal.KeyEpoch == 0 || issuedAt.IsZero() ||
		issuedAt.Unix() < previous.Manifest.Manifest.IssuedAtUnix || issuedAt.Unix() >= previous.Manifest.Manifest.NotAfterUnix ||
		!notAfter.After(issuedAt) || notAfter.After(issuedAt.Add(existingAccountGenesisCredentialLifetime)) ||
		!sortedUniqueVersions(t.CandidateEnvelopeVersions) || t.CandidateSigningPublic == ([32]byte{}) ||
		t.CandidateWrapPublic == ([32]byte{}) {
		return DeviceCertificateUnsignedV1{}, ErrInvalidDeviceTransition
	}
	var prior *DeviceCertificateUnsignedV1
	for index := range previous.Manifest.Manifest.Devices {
		candidate := &previous.Manifest.Manifest.Devices[index].Certificate
		if candidate.DeviceID == t.CandidateDeviceID {
			prior = candidate
		}
	}
	if prior == nil {
		if proposal.KeyEpoch != 1 {
			return DeviceCertificateUnsignedV1{}, ErrInvalidDeviceTransition
		}
	} else if prior.KeyEpoch == math.MaxUint64 || proposal.KeyEpoch != prior.KeyEpoch+1 {
		return DeviceCertificateUnsignedV1{}, ErrInvalidDeviceTransition
	}
	approver, err := activePairingApprover(previous, t.ApproverDeviceID, [32]byte{}, issuedAt.Unix())
	if err != nil {
		return DeviceCertificateUnsignedV1{}, err
	}
	credential := DeviceCertificateUnsignedV1{
		Version: 1, AccountID: anchor.AccountID, UserID: proposal.UserID, DeviceID: t.CandidateDeviceID,
		KeyEpoch: proposal.KeyEpoch, SigningKeyID: sha256.Sum256(t.CandidateSigningPublic[:]),
		SigningPublicKey: t.CandidateSigningPublic, WrapKeyID: sha256.Sum256(t.CandidateWrapPublic[:]),
		WrapPublicKey: t.CandidateWrapPublic, EnvelopeVersions: append([]uint16(nil), t.CandidateEnvelopeVersions...),
		NotBeforeUnix: issuedAt.Unix(), NotAfterUnix: notAfter.Unix(), IssuanceMode: "pairing",
		IssuedUnderAuthorityEpoch: previous.Authority.AuthorityEpoch, JoinNonce: proposal.JoinNonce,
		EnrollmentContextHash: transcriptHash, ApproverDeviceID: approver.DeviceID,
		ApproverSigningKeyID: approver.SigningKeyID, IssuingAuthorityStateHash: previous.Authority.StateHash,
	}
	return credential, nil
}

func SignDevicePossession(credential DeviceCertificateUnsignedV1, private ed25519.PrivateKey) ([64]byte, error) {
	if !privateMatchesSigningKey(private, credential.SigningKeyID, credential.SigningPublicKey) {
		return [64]byte{}, ErrInvalidDeviceTransition
	}
	return signExistingAccountValue(private, "aplexica/device-possession/v1", devicePossessionPreimage(credential))
}

func SignPairingApproval(previous VerifiedRoster, credential DeviceCertificateUnsignedV1, transcript PairingTranscriptV1, private ed25519.PrivateKey) ([64]byte, error) {
	hash, err := TranscriptHash(transcript)
	if err != nil || hash != credential.EnrollmentContextHash || transcript.CurrentRosterHash != [32]byte(previous.Hash) ||
		transcript.CandidateDeviceID != credential.DeviceID || transcript.ApproverDeviceID != credential.ApproverDeviceID {
		return [64]byte{}, ErrInvalidDeviceTransition
	}
	approver, err := activePairingApprover(previous, credential.ApproverDeviceID, credential.ApproverSigningKeyID, credential.NotBeforeUnix)
	if err != nil || !privateMatchesSigningKey(private, approver.SigningKeyID, approver.SigningPublicKey) {
		return [64]byte{}, ErrInvalidDeviceTransition
	}
	preimage := pairingApprovalPreimage{Possession: devicePossessionPreimage(credential), PreviousRosterHash: [32]byte(previous.Hash)}
	return signExistingAccountValue(private, "aplexica/pairing-approval/v1", preimage)
}

func EndorseDeviceCredential(previous VerifiedRoster, credential DeviceCertificateUnsignedV1, signerKeyID [32]byte, private ed25519.PrivateKey) (DeviceTransitionEndorsementV1, error) {
	authority, err := activeRosterAuthority(previous.Authority, previous.Manifest.Manifest.Devices, signerKeyID, credential.NotBeforeUnix)
	if err != nil || !privateMatchesSigningKey(private, authority.SigningKeyID, authority.SigningPublicKey) {
		return DeviceTransitionEndorsementV1{}, ErrDeviceTransitionSignerOffline
	}
	signature, err := signExistingAccountValue(private, "aplexica/device-credential/v1", credential)
	if err != nil {
		return DeviceTransitionEndorsementV1{}, err
	}
	return DeviceTransitionEndorsementV1{SignerKeyID: signerKeyID, Signature: signature}, nil
}

func FinalizeDeviceCredential(previous VerifiedRoster, credential DeviceCertificateUnsignedV1, endorsements []DeviceTransitionEndorsementV1) (DeviceCertificateV1, error) {
	ids, signatures, err := canonicalTransitionEndorsements(previous, endorsements, "aplexica/device-credential/v1", credential, credential.NotBeforeUnix)
	if err != nil {
		return DeviceCertificateV1{}, err
	}
	signed := DeviceCertificateV1{Certificate: credential, IssuerKeyIDs: ids, IssuanceSignatures: signatures}
	if err := verifyCertificateProofs(signed, previous, previous.Authority); err != nil {
		return DeviceCertificateV1{}, err
	}
	return signed, nil
}

// PrepareRecoveryCredential consumes only a complete root-signed recovery
// transition/enrollment package. It never treats cloud device rows or an
// unsigned recovery public key as authority.
func PrepareRecoveryCredential(previous VerifiedRoster, proposal RecoveryCredentialProposalV1) (DeviceCertificateUnsignedV1, VerifiedAuthorityState, error) {
	if proposal.Transition.Transition.AuthorizationMode != "recovery" || !validText(proposal.UserID, 256) || proposal.KeyEpoch == 0 {
		return DeviceCertificateUnsignedV1{}, VerifiedAuthorityState{}, ErrInvalidDeviceTransition
	}
	candidate, err := VerifyAuthorityTransition(previous.Authority, proposal.Transition)
	if err != nil {
		return DeviceCertificateUnsignedV1{}, VerifiedAuthorityState{}, err
	}
	enrollment, err := verifiedRecoveryEnrollment(previous, proposal.Transition, proposal.Enrollments, proposal.DeviceID)
	if err != nil {
		return DeviceCertificateUnsignedV1{}, VerifiedAuthorityState{}, err
	}
	prior, existed := findCredential(previous.Manifest.Manifest.Devices, proposal.DeviceID)
	if existed {
		if prior.KeyEpoch == math.MaxUint64 || proposal.KeyEpoch != prior.KeyEpoch+1 {
			return DeviceCertificateUnsignedV1{}, VerifiedAuthorityState{}, ErrInvalidDeviceTransition
		}
	} else if proposal.KeyEpoch != 1 {
		return DeviceCertificateUnsignedV1{}, VerifiedAuthorityState{}, ErrInvalidDeviceTransition
	}
	contextHash, err := RecoveryEnrollmentContextHash(enrollment)
	if err != nil {
		return DeviceCertificateUnsignedV1{}, VerifiedAuthorityState{}, err
	}
	u := enrollment.Enrollment
	credential := DeviceCertificateUnsignedV1{Version: 1, AccountID: u.AccountID, UserID: proposal.UserID, DeviceID: u.CandidateDeviceID,
		KeyEpoch: proposal.KeyEpoch, SigningKeyID: u.CandidateSigningKeyID, SigningPublicKey: u.CandidateSigningPublic,
		WrapKeyID: u.CandidateWrapKeyID, WrapPublicKey: u.CandidateWrapPublic, EnvelopeVersions: append([]uint16(nil), u.EnvelopeVersions...),
		NotBeforeUnix: u.NotBeforeUnix, NotAfterUnix: u.NotAfterUnix, IssuanceMode: "recovery", IssuedUnderAuthorityEpoch: candidate.AuthorityEpoch,
		JoinNonce: u.JoinNonce, EnrollmentContextHash: contextHash, IssuingAuthorityStateHash: candidate.StateHash}
	return credential, candidate, nil
}

func EndorseRecoveryCredential(previous VerifiedRoster, transition AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, credential DeviceCertificateUnsignedV1, signerKeyID [32]byte, private ed25519.PrivateKey) (DeviceTransitionEndorsementV1, error) {
	candidate, err := VerifyAuthorityTransition(previous.Authority, transition)
	if err != nil || validateRecoveryCredentialProposal(previous, candidate, transition, enrollments, credential) != nil {
		return DeviceTransitionEndorsementV1{}, ErrInvalidDeviceTransition
	}
	authority, ok := candidate.Authorities[DeviceKeyID(signerKeyID)]
	if !ok || !recoveryAuthorityHasLineage(previous, transition, enrollments, authority) || !privateMatchesSigningKey(private, authority.SigningKeyID, authority.SigningPublicKey) {
		return DeviceTransitionEndorsementV1{}, ErrDeviceTransitionSignerOffline
	}
	signature, err := signExistingAccountValue(private, "aplexica/device-credential/v1", credential)
	if err != nil {
		return DeviceTransitionEndorsementV1{}, err
	}
	return DeviceTransitionEndorsementV1{SignerKeyID: signerKeyID, Signature: signature}, nil
}

func FinalizeRecoveryCredential(previous VerifiedRoster, transition AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, credential DeviceCertificateUnsignedV1, endorsements []DeviceTransitionEndorsementV1) (DeviceCertificateV1, error) {
	candidate, err := VerifyAuthorityTransition(previous.Authority, transition)
	if err != nil || validateRecoveryCredentialProposal(previous, candidate, transition, enrollments, credential) != nil || len(endorsements) < int(candidate.Threshold) || len(endorsements) > len(candidate.Authorities) {
		return DeviceCertificateV1{}, ErrInvalidDeviceTransition
	}
	sorted := append([]DeviceTransitionEndorsementV1(nil), endorsements...)
	sort.Slice(sorted, func(left, right int) bool {
		return bytes.Compare(sorted[left].SignerKeyID[:], sorted[right].SignerKeyID[:]) < 0
	})
	signed := DeviceCertificateV1{Certificate: credential, IssuerKeyIDs: make([][32]byte, len(sorted)), IssuanceSignatures: make([][64]byte, len(sorted))}
	for index, endorsement := range sorted {
		if index > 0 && endorsement.SignerKeyID == sorted[index-1].SignerKeyID {
			return DeviceCertificateV1{}, securityerr.ErrInvalidSignature
		}
		authority, ok := candidate.Authorities[DeviceKeyID(endorsement.SignerKeyID)]
		if !ok || !recoveryAuthorityHasLineage(previous, transition, enrollments, authority) ||
			verifySig(authority.SigningPublicKey[:], "aplexica/device-credential/v1", credential, endorsement.Signature[:]) != nil {
			return DeviceCertificateV1{}, securityerr.ErrInvalidSignature
		}
		signed.IssuerKeyIDs[index], signed.IssuanceSignatures[index] = endorsement.SignerKeyID, endorsement.Signature
	}
	return signed, nil
}

func PrepareRecoveryRosterTransition(previous VerifiedRoster, transition AuthorityTransitionV1, devices []DeviceCertificateV1, now time.Time) (RosterManifestUnsignedV1, VerifiedAuthorityState, error) {
	candidate, err := VerifyAuthorityTransition(previous.Authority, transition)
	if err != nil || len(devices) == 0 {
		return RosterManifestUnsignedV1{}, VerifiedAuthorityState{}, ErrInvalidDeviceTransition
	}
	now = now.UTC().Truncate(time.Second)
	if now.IsZero() || previous.Manifest.Manifest.Epoch == math.MaxUint64 {
		return RosterManifestUnsignedV1{}, VerifiedAuthorityState{}, ErrInvalidDeviceTransition
	}
	devices = append([]DeviceCertificateV1(nil), devices...)
	sort.Slice(devices, func(left, right int) bool {
		return devices[left].Certificate.DeviceID < devices[right].Certificate.DeviceID
	})
	prior := previous.Manifest.Manifest
	next := cloneRosterManifestUnsigned(prior)
	next.Epoch, next.PreviousHash = prior.Epoch+1, [32]byte(previous.Hash)
	next.AuthorityEpoch, next.AuthorityStateHash = candidate.AuthorityEpoch, candidate.StateHash
	next.IssuedAtUnix, next.NotAfterUnix, next.Devices = now.Unix(), now.Add(existingAccountGenesisRosterLifetime).Unix(), devices
	for _, signed := range devices {
		if signed.Certificate.NotAfterUnix < next.NotAfterUnix {
			next.NotAfterUnix = signed.Certificate.NotAfterUnix
		}
	}
	if next.NotAfterUnix <= next.IssuedAtUnix {
		return RosterManifestUnsignedV1{}, VerifiedAuthorityState{}, ErrInvalidDeviceTransition
	}
	next.AccessSetHash, err = AccessSetHash(next)
	if err != nil {
		return RosterManifestUnsignedV1{}, VerifiedAuthorityState{}, err
	}
	if next.AccessSetHash == prior.AccessSetHash {
		next.AccessGeneration = prior.AccessGeneration
	} else if prior.AccessGeneration == math.MaxUint64 {
		return RosterManifestUnsignedV1{}, VerifiedAuthorityState{}, ErrInvalidDeviceTransition
	} else {
		next.AccessGeneration = prior.AccessGeneration + 1
	}
	return next, candidate, nil
}

func EndorseRecoveryRoster(candidate VerifiedAuthorityState, proposal RosterManifestUnsignedV1, signerKeyID [32]byte, private ed25519.PrivateKey) (DeviceTransitionEndorsementV1, error) {
	authority, ok := candidate.Authorities[DeviceKeyID(signerKeyID)]
	if !ok || !privateMatchesSigningKey(private, authority.SigningKeyID, authority.SigningPublicKey) {
		return DeviceTransitionEndorsementV1{}, ErrDeviceTransitionSignerOffline
	}
	active := false
	for _, signed := range proposal.Devices {
		credential := signed.Certificate
		if credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID && credential.SigningPublicKey == authority.SigningPublicKey && credential.NotBeforeUnix <= proposal.IssuedAtUnix && proposal.IssuedAtUnix < credential.NotAfterUnix {
			active = true
		}
	}
	if !active {
		return DeviceTransitionEndorsementV1{}, ErrDeviceTransitionSignerOffline
	}
	signature, err := signExistingAccountValue(private, "aplexica/roster-manifest/v1", proposal)
	return DeviceTransitionEndorsementV1{SignerKeyID: signerKeyID, Signature: signature}, err
}

func FinalizeRecoveryTransition(previous VerifiedRoster, transition AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, proposal RosterManifestUnsignedV1, endorsements []DeviceTransitionEndorsementV1) (AtomicAuthorityRosterTransitionV1, VerifiedRoster, error) {
	candidate, err := VerifyAuthorityTransition(previous.Authority, transition)
	if err != nil || len(endorsements) < int(candidate.Threshold) || len(endorsements) > len(candidate.Authorities) {
		return AtomicAuthorityRosterTransitionV1{}, VerifiedRoster{}, ErrInvalidDeviceTransition
	}
	sorted := append([]DeviceTransitionEndorsementV1(nil), endorsements...)
	sort.Slice(sorted, func(left, right int) bool {
		return bytes.Compare(sorted[left].SignerKeyID[:], sorted[right].SignerKeyID[:]) < 0
	})
	roster := RosterManifestV1{Manifest: proposal, SignerKeyIDs: make([][32]byte, len(sorted)), Signatures: make([][64]byte, len(sorted))}
	for index, endorsement := range sorted {
		if index > 0 && endorsement.SignerKeyID == sorted[index-1].SignerKeyID {
			return AtomicAuthorityRosterTransitionV1{}, VerifiedRoster{}, securityerr.ErrInvalidSignature
		}
		authority, ok := candidate.Authorities[DeviceKeyID(endorsement.SignerKeyID)]
		if !ok || verifySig(authority.SigningPublicKey[:], "aplexica/roster-manifest/v1", proposal, endorsement.Signature[:]) != nil {
			return AtomicAuthorityRosterTransitionV1{}, VerifiedRoster{}, securityerr.ErrInvalidSignature
		}
		roster.SignerKeyIDs[index], roster.Signatures[index] = endorsement.SignerKeyID, endorsement.Signature
	}
	pkg := AtomicAuthorityRosterTransitionV1{AuthorityTransition: transition, RecoveryEnrollments: append([]RecoveryEnrollmentV1(nil), enrollments...), NextRoster: roster}
	verified, err := VerifyAtomicAuthorityRosterTransition(previous, transition, enrollments, roster)
	return pkg, verified, err
}

// PrepareDeviceRosterTransition inserts or replaces exactly one authenticated
// credential, computes the access projection, and increments AccessGeneration
// iff that projection changed. Credential-only renewal therefore remains a
// same-access freshness transition.
func PrepareDeviceRosterTransition(previous VerifiedRoster, credential DeviceCertificateV1, now time.Time) (RosterManifestUnsignedV1, error) {
	now = now.UTC().Truncate(time.Second)
	prior := previous.Manifest.Manifest
	if now.IsZero() || now.Unix() < prior.IssuedAtUnix || now.Unix() >= prior.NotAfterUnix || prior.Epoch == math.MaxUint64 ||
		credential.Certificate.AccountID != previous.Authority.Anchor.Anchor.AccountID ||
		credential.Certificate.NotBeforeUnix > now.Unix() || credential.Certificate.NotAfterUnix <= now.Unix() {
		return RosterManifestUnsignedV1{}, ErrInvalidDeviceTransition
	}
	if err := verifyCertificateProofs(credential, previous, previous.Authority); err != nil {
		return RosterManifestUnsignedV1{}, err
	}
	devices := make([]DeviceCertificateV1, 0, len(prior.Devices)+1)
	replaced := false
	for _, existing := range prior.Devices {
		if existing.Certificate.DeviceID == credential.Certificate.DeviceID {
			devices = append(devices, credential)
			replaced = true
		} else {
			devices = append(devices, existing)
		}
	}
	if !replaced {
		devices = append(devices, credential)
	}
	sort.Slice(devices, func(left, right int) bool {
		return devices[left].Certificate.DeviceID < devices[right].Certificate.DeviceID
	})
	next := cloneRosterManifestUnsigned(prior)
	next.Epoch = prior.Epoch + 1
	next.PreviousHash = [32]byte(previous.Hash)
	next.IssuedAtUnix = now.Unix()
	next.NotAfterUnix = now.Add(existingAccountGenesisRosterLifetime).Unix()
	next.Devices = devices
	for _, signed := range devices {
		if signed.Certificate.NotAfterUnix < next.NotAfterUnix {
			next.NotAfterUnix = signed.Certificate.NotAfterUnix
		}
	}
	if next.NotAfterUnix <= next.IssuedAtUnix {
		return RosterManifestUnsignedV1{}, ErrInvalidDeviceTransition
	}
	accessHash, err := AccessSetHash(next)
	if err != nil {
		return RosterManifestUnsignedV1{}, err
	}
	next.AccessSetHash = accessHash
	if accessHash == prior.AccessSetHash {
		next.AccessGeneration = prior.AccessGeneration
	} else {
		if prior.AccessGeneration == math.MaxUint64 {
			return RosterManifestUnsignedV1{}, ErrInvalidDeviceTransition
		}
		next.AccessGeneration = prior.AccessGeneration + 1
	}
	return next, nil
}

func EndorseDeviceRosterTransition(previous VerifiedRoster, proposal RosterManifestUnsignedV1, signerKeyID [32]byte, private ed25519.PrivateKey) (DeviceTransitionEndorsementV1, error) {
	if err := validateDeviceRosterProposal(previous, proposal); err != nil {
		return DeviceTransitionEndorsementV1{}, err
	}
	authority, err := activeRosterAuthority(previous.Authority, previous.Manifest.Manifest.Devices, signerKeyID, proposal.IssuedAtUnix)
	if err != nil || !privateMatchesSigningKey(private, authority.SigningKeyID, authority.SigningPublicKey) {
		return DeviceTransitionEndorsementV1{}, ErrDeviceTransitionSignerOffline
	}
	signature, err := signExistingAccountValue(private, "aplexica/roster-manifest/v1", proposal)
	if err != nil {
		return DeviceTransitionEndorsementV1{}, err
	}
	return DeviceTransitionEndorsementV1{SignerKeyID: signerKeyID, Signature: signature}, nil
}

func FinalizeDeviceRosterTransition(previous VerifiedRoster, proposal RosterManifestUnsignedV1, endorsements []DeviceTransitionEndorsementV1) (RosterManifestV1, VerifiedRoster, error) {
	if err := validateDeviceRosterProposal(previous, proposal); err != nil {
		return RosterManifestV1{}, VerifiedRoster{}, err
	}
	ids, signatures, err := canonicalTransitionEndorsements(previous, endorsements, "aplexica/roster-manifest/v1", proposal, proposal.IssuedAtUnix)
	if err != nil {
		return RosterManifestV1{}, VerifiedRoster{}, err
	}
	signed := RosterManifestV1{Manifest: proposal, SignerKeyIDs: ids, Signatures: signatures}
	verified, err := VerifyTransition(previous, previous.Authority, signed)
	if err != nil {
		return RosterManifestV1{}, VerifiedRoster{}, err
	}
	return signed, verified, nil
}

func validateDeviceRosterProposal(previous VerifiedRoster, proposal RosterManifestUnsignedV1) error {
	prior := previous.Manifest.Manifest
	if proposal.Version != prior.Version || proposal.ScopeType != prior.ScopeType || proposal.ScopeID != prior.ScopeID ||
		proposal.Epoch != prior.Epoch+1 || proposal.PreviousHash != [32]byte(previous.Hash) ||
		proposal.TrustAnchorHash != prior.TrustAnchorHash || proposal.AuthorityStateHash != prior.AuthorityStateHash ||
		proposal.AuthorityEpoch != prior.AuthorityEpoch || proposal.MinEnvelopeVersion < prior.MinEnvelopeVersion ||
		proposal.IssuedAtUnix < prior.IssuedAtUnix || proposal.IssuedAtUnix >= prior.NotAfterUnix ||
		proposal.NotAfterUnix <= proposal.IssuedAtUnix || proposal.NotAfterUnix > proposal.IssuedAtUnix+int64(existingAccountGenesisRosterLifetime/time.Second) {
		return ErrInvalidDeviceTransition
	}
	accessHash, err := AccessSetHash(proposal)
	if err != nil || accessHash != proposal.AccessSetHash {
		return ErrInvalidDeviceTransition
	}
	changed := proposal.AccessSetHash != prior.AccessSetHash
	if changed && (prior.AccessGeneration == math.MaxUint64 || proposal.AccessGeneration != prior.AccessGeneration+1) ||
		!changed && proposal.AccessGeneration != prior.AccessGeneration {
		return ErrInvalidDeviceTransition
	}
	return nil
}

func canonicalTransitionEndorsements(previous VerifiedRoster, endorsements []DeviceTransitionEndorsementV1, domain string, value any, at int64) ([][32]byte, [][64]byte, error) {
	if len(endorsements) < int(previous.Authority.Threshold) || len(endorsements) > len(previous.Authority.Authorities) {
		return nil, nil, securityerr.ErrInvalidSignature
	}
	sorted := append([]DeviceTransitionEndorsementV1(nil), endorsements...)
	sort.Slice(sorted, func(left, right int) bool {
		return bytes.Compare(sorted[left].SignerKeyID[:], sorted[right].SignerKeyID[:]) < 0
	})
	ids := make([][32]byte, len(sorted))
	signatures := make([][64]byte, len(sorted))
	for index, endorsement := range sorted {
		if index > 0 && endorsement.SignerKeyID == sorted[index-1].SignerKeyID {
			return nil, nil, securityerr.ErrInvalidSignature
		}
		authority, err := activeRosterAuthority(previous.Authority, previous.Manifest.Manifest.Devices, endorsement.SignerKeyID, at)
		if err != nil || verifySig(authority.SigningPublicKey[:], domain, value, endorsement.Signature[:]) != nil {
			return nil, nil, securityerr.ErrInvalidSignature
		}
		ids[index] = endorsement.SignerKeyID
		signatures[index] = endorsement.Signature
	}
	return ids, signatures, nil
}

func activePairingApprover(previous VerifiedRoster, deviceID string, keyID [32]byte, at int64) (DeviceCertificateUnsignedV1, error) {
	for _, signed := range previous.Manifest.Manifest.Devices {
		credential := signed.Certificate
		keyMatches := keyID == ([32]byte{}) || credential.SigningKeyID == keyID
		if credential.DeviceID == deviceID && keyMatches && credential.NotBeforeUnix <= at && at < credential.NotAfterUnix {
			return credential, nil
		}
	}
	return DeviceCertificateUnsignedV1{}, fmt.Errorf("%w: pairing approver is not active", securityerr.ErrUntrustedRoster)
}

func privateMatchesSigningKey(private ed25519.PrivateKey, keyID [32]byte, public [32]byte) bool {
	if len(private) != ed25519.PrivateKeySize {
		return false
	}
	reconstructed := ed25519.NewKeyFromSeed(private.Seed())
	defer clearBytes(reconstructed)
	derived := reconstructed.Public().(ed25519.PublicKey)
	return private.Equal(reconstructed) && [32]byte(derived) == public && sha256.Sum256(derived) == keyID
}

func findCredential(devices []DeviceCertificateV1, deviceID string) (DeviceCertificateUnsignedV1, bool) {
	for _, signed := range devices {
		if signed.Certificate.DeviceID == deviceID {
			return signed.Certificate, true
		}
	}
	return DeviceCertificateUnsignedV1{}, false
}

func verifiedRecoveryEnrollment(previous VerifiedRoster, transition AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, deviceID string) (RecoveryEnrollmentV1, error) {
	transitionHash, err := AuthorityTransitionHash(transition)
	if err != nil {
		return RecoveryEnrollmentV1{}, err
	}
	for _, enrollment := range enrollments {
		u := enrollment.Enrollment
		if u.CandidateDeviceID != deviceID {
			continue
		}
		if u.Version != 1 || u.AccountID != previous.Authority.Anchor.Anchor.AccountID || u.TrustAnchorHash != [32]byte(previous.Authority.AnchorHash) ||
			u.AuthorityTransitionHash != transitionHash || u.CandidateSigningKeyID != sha256.Sum256(u.CandidateSigningPublic[:]) ||
			u.CandidateWrapKeyID != sha256.Sum256(u.CandidateWrapPublic[:]) || !sortedUniqueVersions(u.EnvelopeVersions) ||
			u.JoinNonce == ([32]byte{}) || u.RecoveryNonce == ([32]byte{}) || u.NotAfterUnix <= u.NotBeforeUnix ||
			verifySig(previous.Authority.Anchor.Anchor.RecoveryRootPublicKey[:], "aplexica/recovery-device-enrollment/v1", u, enrollment.RecoverySignature[:]) != nil {
			return RecoveryEnrollmentV1{}, ErrInvalidDeviceTransition
		}
		return enrollment, nil
	}
	return RecoveryEnrollmentV1{}, ErrInvalidDeviceTransition
}

func validateRecoveryCredentialProposal(previous VerifiedRoster, candidate VerifiedAuthorityState, transition AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, credential DeviceCertificateUnsignedV1) error {
	enrollment, err := verifiedRecoveryEnrollment(previous, transition, enrollments, credential.DeviceID)
	if err != nil {
		return err
	}
	contextHash, err := RecoveryEnrollmentContextHash(enrollment)
	u := enrollment.Enrollment
	if err != nil || credential.Version != 1 || credential.AccountID != u.AccountID || credential.SigningKeyID != u.CandidateSigningKeyID ||
		credential.SigningPublicKey != u.CandidateSigningPublic || credential.WrapKeyID != u.CandidateWrapKeyID || credential.WrapPublicKey != u.CandidateWrapPublic ||
		credential.JoinNonce != u.JoinNonce || credential.NotBeforeUnix != u.NotBeforeUnix || credential.NotAfterUnix != u.NotAfterUnix ||
		credential.EnrollmentContextHash != contextHash || credential.IssuanceMode != "recovery" || credential.IssuedUnderAuthorityEpoch != candidate.AuthorityEpoch ||
		credential.IssuingAuthorityStateHash != candidate.StateHash || credential.ApproverDeviceID != "" || credential.ApproverSigningKeyID != ([32]byte{}) ||
		credential.ApproverProof != ([64]byte{}) || verifySig(credential.SigningPublicKey[:], "aplexica/device-possession/v1", devicePossessionPreimage(credential), credential.CandidateProof[:]) != nil {
		return ErrInvalidDeviceTransition
	}
	return nil
}

func recoveryAuthorityHasLineage(previous VerifiedRoster, transition AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, authority RosterAuthorityV1) bool {
	if activeRoster, err := activeRosterAuthority(previous.Authority, previous.Manifest.Manifest.Devices, authority.SigningKeyID, transition.Transition.IssuedAtUnix); err == nil &&
		activeRoster.DeviceID == authority.DeviceID && activeRoster.SigningPublicKey == authority.SigningPublicKey {
		return true
	}
	enrollment, err := verifiedRecoveryEnrollment(previous, transition, enrollments, authority.DeviceID)
	return err == nil && enrollment.Enrollment.CandidateSigningKeyID == authority.SigningKeyID && enrollment.Enrollment.CandidateSigningPublic == authority.SigningPublicKey
}
