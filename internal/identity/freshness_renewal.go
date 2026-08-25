package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/securityerr"
)

var (
	ErrInvalidFreshnessRenewal       = errors.New("identity: invalid roster freshness renewal")
	ErrCredentialRenewalRequired     = errors.New("identity: credential renewal required before roster renewal")
	ErrFreshnessAuthorityUnavailable = errors.New("identity: active roster authority unavailable")
)

// RosterFreshnessEndorsementV1 is one operational-authority signature over an
// unchanged-access freshness proposal. Signatures are sorted by SignerKeyID
// during finalization and are never accepted from a non-active roster device.
type RosterFreshnessEndorsementV1 struct {
	SignerKeyID [32]byte
	Signature   [64]byte
}

// PrepareFreshnessRenewal builds the unsigned next roster without changing
// any credential, access projection, authority binding, or wire minimum. It is
// intentionally persistence-free so callers can collect a configured
// authority threshold before starting a crash-safe state transition.
func PrepareFreshnessRenewal(previous VerifiedRoster, now time.Time) (RosterManifestUnsignedV1, error) {
	now = now.UTC().Truncate(time.Second)
	prior := previous.Manifest.Manifest
	if now.IsZero() || now.Unix() <= prior.IssuedAtUnix || now.Unix() >= prior.NotAfterUnix || prior.Epoch == math.MaxUint64 {
		return RosterManifestUnsignedV1{}, ErrInvalidFreshnessRenewal
	}
	previousHash, err := HashRoster(previous.Manifest)
	if err != nil || previousHash != previous.Hash {
		return RosterManifestUnsignedV1{}, ErrInvalidFreshnessRenewal
	}
	next := cloneRosterManifestUnsigned(prior)
	next.Epoch = prior.Epoch + 1
	next.PreviousHash = [32]byte(previous.Hash)
	next.IssuedAtUnix = now.Unix()
	next.NotAfterUnix = now.Add(24 * time.Hour).Unix()
	for _, signedCredential := range next.Devices {
		credential := signedCredential.Certificate
		if credential.NotBeforeUnix > next.IssuedAtUnix || credential.NotAfterUnix <= next.IssuedAtUnix {
			return RosterManifestUnsignedV1{}, ErrCredentialRenewalRequired
		}
		if credential.NotAfterUnix < next.NotAfterUnix {
			next.NotAfterUnix = credential.NotAfterUnix
		}
	}
	if next.NotAfterUnix <= prior.NotAfterUnix {
		return RosterManifestUnsignedV1{}, ErrCredentialRenewalRequired
	}
	recomputed, err := AccessSetHash(next)
	if err != nil || recomputed != prior.AccessSetHash {
		return RosterManifestUnsignedV1{}, ErrInvalidFreshnessRenewal
	}
	next.AccessSetHash = recomputed
	if err := validateFreshnessProposal(previous, next); err != nil {
		return RosterManifestUnsignedV1{}, err
	}
	return next, nil
}

// FreshnessEndorsementRoundDigest identifies the one immediate successor of a
// verified roster head. Different authority devices can prepare different
// short-lived timestamps, but the exchange elects one proposal per exact head.
func FreshnessEndorsementRoundDigest(previous VerifiedRoster) ([32]byte, error) {
	if previous.Hash == (RosterHash{}) {
		return [32]byte{}, ErrInvalidFreshnessRenewal
	}
	verified, err := HashRoster(previous.Manifest)
	if err != nil || verified != previous.Hash {
		return [32]byte{}, ErrInvalidFreshnessRenewal
	}
	return digest("aplexica/roster-freshness-endorsement-round/v1", [32]byte(previous.Hash))
}

// SignFreshnessRenewal returns one endorsement after proving that the private
// key belongs to both the current authority state and an active credential in
// the previous roster. The private key is neither retained nor returned.
func SignFreshnessRenewal(previous VerifiedRoster, proposal RosterManifestUnsignedV1, signerKeyID [32]byte, private ed25519.PrivateKey) (RosterFreshnessEndorsementV1, error) {
	if err := validateFreshnessProposal(previous, proposal); err != nil {
		return RosterFreshnessEndorsementV1{}, err
	}
	authority, err := activeFreshnessAuthority(previous, signerKeyID, proposal.IssuedAtUnix)
	if err != nil || len(private) != ed25519.PrivateKeySize {
		return RosterFreshnessEndorsementV1{}, ErrFreshnessAuthorityUnavailable
	}
	reconstructed := ed25519.NewKeyFromSeed(private.Seed())
	consistent := private.Equal(reconstructed)
	public := reconstructed.Public().(ed25519.PublicKey)
	publicID := sha256.Sum256(public)
	publicMatches := authority.SigningPublicKey == [32]byte(public) && authority.SigningKeyID == publicID
	clearBytes(reconstructed)
	if !consistent || !publicMatches {
		return RosterFreshnessEndorsementV1{}, ErrFreshnessAuthorityUnavailable
	}
	signature, err := signExistingAccountValue(private, "aplexica/roster-manifest/v1", proposal)
	if err != nil {
		return RosterFreshnessEndorsementV1{}, err
	}
	return RosterFreshnessEndorsementV1{SignerKeyID: signerKeyID, Signature: signature}, nil
}

// FinalizeFreshnessRenewal verifies a sorted, distinct authority threshold and
// the ordinary roster transition verifier before returning a usable next
// roster. It rejects membership/key/minimum changes even if enough authorities
// signed them; those require the normal access-cutover path instead.
func FinalizeFreshnessRenewal(previous VerifiedRoster, proposal RosterManifestUnsignedV1, endorsements []RosterFreshnessEndorsementV1) (RosterManifestV1, VerifiedRoster, error) {
	if err := validateFreshnessProposal(previous, proposal); err != nil {
		return RosterManifestV1{}, VerifiedRoster{}, err
	}
	if len(endorsements) < int(previous.Authority.Threshold) || len(endorsements) > len(previous.Authority.Authorities) {
		return RosterManifestV1{}, VerifiedRoster{}, securityerr.ErrInvalidSignature
	}
	sorted := append([]RosterFreshnessEndorsementV1(nil), endorsements...)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i].SignerKeyID[:], sorted[j].SignerKeyID[:]) < 0 })
	signerIDs := make([][32]byte, len(sorted))
	signatures := make([][64]byte, len(sorted))
	for index, endorsement := range sorted {
		if index > 0 && endorsement.SignerKeyID == sorted[index-1].SignerKeyID {
			return RosterManifestV1{}, VerifiedRoster{}, securityerr.ErrInvalidSignature
		}
		authority, err := activeFreshnessAuthority(previous, endorsement.SignerKeyID, proposal.IssuedAtUnix)
		if err != nil || verifySig(authority.SigningPublicKey[:], "aplexica/roster-manifest/v1", proposal, endorsement.Signature[:]) != nil {
			return RosterManifestV1{}, VerifiedRoster{}, securityerr.ErrInvalidSignature
		}
		signerIDs[index] = endorsement.SignerKeyID
		signatures[index] = endorsement.Signature
	}
	signed := RosterManifestV1{Manifest: proposal, SignerKeyIDs: signerIDs, Signatures: signatures}
	verified, err := VerifyTransition(previous, previous.Authority, signed)
	if err != nil {
		return RosterManifestV1{}, VerifiedRoster{}, err
	}
	if err := validateFreshnessProposal(previous, verified.Manifest.Manifest); err != nil {
		return RosterManifestV1{}, VerifiedRoster{}, err
	}
	return signed, verified, nil
}

func validateFreshnessProposal(previous VerifiedRoster, proposal RosterManifestUnsignedV1) error {
	prior := previous.Manifest.Manifest
	previousHash, err := HashRoster(previous.Manifest)
	if err != nil || previousHash != previous.Hash {
		return ErrInvalidFreshnessRenewal
	}
	if prior.Epoch == math.MaxUint64 || proposal.Version != prior.Version || proposal.ScopeType != prior.ScopeType || proposal.ScopeID != prior.ScopeID ||
		proposal.Epoch != prior.Epoch+1 || proposal.PreviousHash != [32]byte(previous.Hash) ||
		proposal.TrustAnchorHash != prior.TrustAnchorHash || proposal.AuthorityStateHash != prior.AuthorityStateHash ||
		proposal.AuthorityEpoch != prior.AuthorityEpoch || proposal.AccessGeneration != prior.AccessGeneration ||
		proposal.AccessSetHash != prior.AccessSetHash || proposal.MinEnvelopeVersion != prior.MinEnvelopeVersion ||
		proposal.IssuedAtUnix <= prior.IssuedAtUnix || proposal.IssuedAtUnix >= prior.NotAfterUnix ||
		proposal.NotAfterUnix <= prior.NotAfterUnix || proposal.NotAfterUnix <= proposal.IssuedAtUnix ||
		proposal.NotAfterUnix > proposal.IssuedAtUnix+int64(24*time.Hour/time.Second) {
		return ErrInvalidFreshnessRenewal
	}
	priorDevices, err := enc.Marshal(prior.Devices)
	if err != nil {
		return err
	}
	proposalDevices, err := enc.Marshal(proposal.Devices)
	if err != nil || !bytes.Equal(priorDevices, proposalDevices) {
		return ErrInvalidFreshnessRenewal
	}
	recomputed, err := AccessSetHash(proposal)
	if err != nil || recomputed != prior.AccessSetHash || recomputed != proposal.AccessSetHash {
		return ErrInvalidFreshnessRenewal
	}
	for _, signedCredential := range proposal.Devices {
		credential := signedCredential.Certificate
		if credential.NotBeforeUnix > proposal.IssuedAtUnix || credential.NotAfterUnix < proposal.NotAfterUnix {
			return ErrCredentialRenewalRequired
		}
	}
	return nil
}

func activeFreshnessAuthority(previous VerifiedRoster, signerKeyID [32]byte, issuedAt int64) (RosterAuthorityV1, error) {
	authority, ok := previous.Authority.Authorities[DeviceKeyID(signerKeyID)]
	if !ok || authority.SigningKeyID != signerKeyID {
		return RosterAuthorityV1{}, ErrFreshnessAuthorityUnavailable
	}
	for _, signedCredential := range previous.Manifest.Manifest.Devices {
		credential := signedCredential.Certificate
		if credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID &&
			credential.SigningPublicKey == authority.SigningPublicKey && credential.NotBeforeUnix <= issuedAt && issuedAt < credential.NotAfterUnix {
			return authority, nil
		}
	}
	return RosterAuthorityV1{}, ErrFreshnessAuthorityUnavailable
}

func cloneRosterManifestUnsigned(source RosterManifestUnsignedV1) RosterManifestUnsignedV1 {
	clone := source
	clone.Devices = make([]DeviceCertificateV1, len(source.Devices))
	for index, signed := range source.Devices {
		clone.Devices[index] = signed
		clone.Devices[index].Certificate.EnvelopeVersions = append([]uint16(nil), signed.Certificate.EnvelopeVersions...)
		clone.Devices[index].IssuerKeyIDs = append([][32]byte(nil), signed.IssuerKeyIDs...)
		clone.Devices[index].IssuanceSignatures = append([][64]byte(nil), signed.IssuanceSignatures...)
	}
	return clone
}
