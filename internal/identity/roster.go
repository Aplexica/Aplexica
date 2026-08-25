package identity

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math"
	"time"

	"github.com/aplexica/aplexica/internal/securityerr"
)

func accessProjection(m RosterManifestUnsignedV1) ([]AccessDeviceV1, error) {
	if len(m.Devices) > 255 || !sortedCerts(m.Devices) {
		return nil, fmt.Errorf("%w: invalid device order", securityerr.ErrUntrustedRoster)
	}
	out := make([]AccessDeviceV1, 0, len(m.Devices))
	seenD := map[string]bool{}
	seenS := map[[32]byte]bool{}
	seenW := map[[32]byte]bool{}
	for _, dc := range m.Devices {
		c := dc.Certificate
		if c.Version != 1 || !validText(c.DeviceID, 256) || c.KeyEpoch == 0 || sha256.Sum256(c.SigningPublicKey[:]) != c.SigningKeyID || sha256.Sum256(c.WrapPublicKey[:]) != c.WrapKeyID || seenD[c.DeviceID] || seenS[c.SigningKeyID] || seenW[c.WrapKeyID] {
			return nil, fmt.Errorf("%w: invalid device credential", securityerr.ErrUntrustedRoster)
		}
		seenD[c.DeviceID] = true
		seenS[c.SigningKeyID] = true
		seenW[c.WrapKeyID] = true
		out = append(out, AccessDeviceV1{c.DeviceID, c.KeyEpoch, c.SigningKeyID, c.SigningPublicKey, c.WrapKeyID, c.WrapPublicKey})
	}
	return out, nil
}
func AccessSetHash(m RosterManifestUnsignedV1) ([32]byte, error) {
	p, err := accessProjection(m)
	if err != nil {
		return [32]byte{}, err
	}
	return digest("aplexica/access-set/v1", m.ScopeType, m.ScopeID, m.MinEnvelopeVersion, p)
}
func HashRoster(r RosterManifestV1) (RosterHash, error) {
	h, err := digest("aplexica/roster-manifest-hash/v1", r)
	return RosterHash(h), err
}
func validateRosterCommon(a VerifiedAuthorityState, r RosterManifestV1) error {
	return validateRosterStructure(a, r, true)
}

func validateRosterStructure(a VerifiedAuthorityState, r RosterManifestV1, verifyCredentialIssuance bool) error {
	m := r.Manifest
	personalScopeMismatch := m.ScopeType == "account" && m.ScopeID != a.Anchor.Anchor.PersonalScopeID
	if m.Version != 1 || (m.ScopeType != "account" && m.ScopeType != "namespace") || !validText(m.ScopeID, 256) || personalScopeMismatch || m.TrustAnchorHash != [32]byte(a.AnchorHash) || m.AuthorityStateHash != a.StateHash || m.AuthorityEpoch != a.AuthorityEpoch || m.MinEnvelopeVersion < 2 || !futureSane(m.IssuedAtUnix) || m.NotAfterUnix <= m.IssuedAtUnix || m.NotAfterUnix > m.IssuedAtUnix+int64(24*time.Hour/time.Second) {
		return fmt.Errorf("%w: invalid roster metadata", securityerr.ErrUntrustedRoster)
	}
	h, err := AccessSetHash(m)
	if err != nil {
		return err
	}
	if h != m.AccessSetHash {
		return fmt.Errorf("%w: access set hash mismatch", securityerr.ErrMetadataMismatch)
	}
	if err := verifyThreshold(r.SignerKeyIDs, r.Signatures, a.Threshold, a.Authorities, "aplexica/roster-manifest/v1", m); err != nil {
		return err
	}
	for _, dc := range m.Devices {
		c := dc.Certificate
		if c.AccountID != a.Anchor.Anchor.AccountID || c.NotBeforeUnix > m.IssuedAtUnix || c.NotAfterUnix < m.NotAfterUnix || c.NotAfterUnix-c.NotBeforeUnix > int64(365*24*time.Hour/time.Second) || c.IssuedUnderAuthorityEpoch > m.AuthorityEpoch || !sortedUniqueVersions(c.EnvelopeVersions) {
			return fmt.Errorf("%w: credential validity mismatch", securityerr.ErrUntrustedRoster)
		}
		if verifyCredentialIssuance {
			if c.IssuingAuthorityStateHash != a.StateHash {
				return fmt.Errorf("%w: credential issuer state mismatch", securityerr.ErrUntrustedRoster)
			}
			if err := verifyThreshold(dc.IssuerKeyIDs, dc.IssuanceSignatures, a.Threshold, a.Authorities, "aplexica/device-credential/v1", c); err != nil {
				return err
			}
		}
	}
	return nil
}
func VerifyGenesis(a VerifiedAuthorityState, r RosterManifestV1) (VerifiedRoster, error) {
	m := r.Manifest
	if m.Epoch != 1 || m.PreviousHash != ([32]byte{}) || m.AccessGeneration != 1 {
		return VerifiedRoster{}, fmt.Errorf("%w: invalid genesis roster", securityerr.ErrUntrustedRoster)
	}
	if err := validateRosterCommon(a, r); err != nil {
		return VerifiedRoster{}, err
	}
	h, err := HashRoster(r)
	if err != nil {
		return VerifiedRoster{}, err
	}
	return VerifiedRoster{Manifest: r, Hash: h, Authority: a}, nil
}
func VerifyTransition(previous VerifiedRoster, a VerifiedAuthorityState, next RosterManifestV1) (VerifiedRoster, error) {
	m, p := next.Manifest, previous.Manifest.Manifest
	if m.Epoch != p.Epoch+1 || m.PreviousHash != [32]byte(previous.Hash) || m.ScopeType != p.ScopeType || m.ScopeID != p.ScopeID || m.TrustAnchorHash != p.TrustAnchorHash || m.MinEnvelopeVersion < p.MinEnvelopeVersion {
		return VerifiedRoster{}, fmt.Errorf("%w: invalid roster transition", securityerr.ErrUntrustedRoster)
	}
	if a.StateHash != previous.Authority.StateHash {
		return VerifiedRoster{}, fmt.Errorf("%w: authority change requires atomic transition", securityerr.ErrUntrustedRoster)
	}
	// Existing unchanged credentials were verified at the chain step that
	// introduced them. An authority-state change does not rewrite those
	// credentials, so ordinary successors validate structure here and verify
	// only changed/new issuance against the active previous roster below.
	if err := validateRosterStructure(a, next, false); err != nil {
		return VerifiedRoster{}, err
	}
	// For an ordinary transition, authority membership alone is insufficient:
	// every signer must also be an operational authority with an exact, live
	// credential in the locally pinned previous roster. This closes the path in
	// which a stale/removed authority key or a legacy cloud device row could
	// authorize a new roster.
	if err := verifyActivePreviousThreshold(previous, next.SignerKeyIDs, next.Signatures, "aplexica/roster-manifest/v1", m, m.IssuedAtUnix); err != nil {
		return VerifiedRoster{}, err
	}
	if err := requireAuthoritiesActiveInRoster(a, m.Devices, m.IssuedAtUnix); err != nil {
		return VerifiedRoster{}, err
	}
	changed := m.AccessSetHash != p.AccessSetHash
	if changed {
		if p.AccessGeneration == math.MaxUint64 || m.AccessGeneration != p.AccessGeneration+1 {
			return VerifiedRoster{}, fmt.Errorf("%w: access generation mismatch", securityerr.ErrMetadataMismatch)
		}
	} else if m.AccessGeneration != p.AccessGeneration {
		return VerifiedRoster{}, fmt.Errorf("%w: freshness renewal changed access generation", securityerr.ErrMetadataMismatch)
	}
	if err := verifyNewJoinProofs(previous, next, a); err != nil {
		return VerifiedRoster{}, err
	}
	h, err := HashRoster(next)
	if err != nil {
		return VerifiedRoster{}, err
	}
	return VerifiedRoster{Manifest: next, Hash: h, Authority: a}, nil
}
func VerifyAtomicAuthorityRosterTransition(previous VerifiedRoster, t AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, next RosterManifestV1) (VerifiedRoster, error) {
	if t.Transition.AuthorizationMode == "operational" {
		if err := verifyActivePreviousThreshold(previous, t.SignerKeyIDs, t.Signatures, "aplexica/authority-transition/v1", t.Transition, t.Transition.IssuedAtUnix); err != nil {
			return VerifiedRoster{}, err
		}
	}
	candidate, err := VerifyAuthorityTransition(previous.Authority, t)
	if err != nil {
		return VerifiedRoster{}, err
	}
	if !sortedEnrollments(enrollments) {
		return VerifiedRoster{}, fmt.Errorf("%w: recovery enrollments unsorted", securityerr.ErrUntrustedRoster)
	}
	th, err := AuthorityTransitionHash(t)
	if err != nil {
		return VerifiedRoster{}, err
	}
	for _, e := range enrollments {
		if e.Enrollment.AuthorityTransitionHash != th || e.Enrollment.TrustAnchorHash != [32]byte(previous.Authority.AnchorHash) || e.Enrollment.AccountID != previous.Authority.Anchor.Anchor.AccountID {
			return VerifiedRoster{}, securityerr.ErrMetadataMismatch
		}
		if err := verifySig(previous.Authority.Anchor.Anchor.RecoveryRootPublicKey[:], "aplexica/recovery-device-enrollment/v1", e.Enrollment, e.RecoverySignature[:]); err != nil {
			return VerifiedRoster{}, err
		}
	}
	m, p := next.Manifest, previous.Manifest.Manifest
	if m.Epoch != p.Epoch+1 || m.PreviousHash != [32]byte(previous.Hash) || m.AuthorityStateHash != candidate.StateHash || m.AuthorityEpoch != candidate.AuthorityEpoch {
		return VerifiedRoster{}, fmt.Errorf("%w: atomic roster binding mismatch", securityerr.ErrMetadataMismatch)
	}
	if err := validateRosterStructure(candidate, next, false); err != nil {
		return VerifiedRoster{}, err
	}
	if err := verifyAtomicCredentialLineage(previous, candidate, t, enrollments, next); err != nil {
		return VerifiedRoster{}, err
	}
	if err := verifyActiveRosterThreshold(candidate, next.Manifest.Devices, next.SignerKeyIDs, next.Signatures, "aplexica/roster-manifest/v1", next.Manifest, next.Manifest.IssuedAtUnix); err != nil {
		return VerifiedRoster{}, err
	}
	if err := requireAuthoritiesActiveInRoster(candidate, next.Manifest.Devices, next.Manifest.IssuedAtUnix); err != nil {
		return VerifiedRoster{}, err
	}
	h, err := HashRoster(next)
	if err != nil {
		return VerifiedRoster{}, err
	}
	return VerifiedRoster{Manifest: next, Hash: h, Authority: candidate}, nil
}

type devicePossession struct {
	IssuanceMode          string   `cbor:"issuanceMode"`
	DeviceID              string   `cbor:"deviceId"`
	KeyEpoch              uint64   `cbor:"keyEpoch"`
	SigningKeyID          [32]byte `cbor:"signingKeyId"`
	SigningPublicKey      [32]byte `cbor:"signingPublicKey"`
	WrapKeyID             [32]byte `cbor:"wrapKeyId"`
	WrapPublicKey         [32]byte `cbor:"wrapPublicKey"`
	JoinNonce             [32]byte `cbor:"joinNonce"`
	EnrollmentContextHash [32]byte `cbor:"enrollmentContextHash"`
}
type pairingApprovalPreimage struct {
	Possession         devicePossession `cbor:"possession"`
	PreviousRosterHash [32]byte         `cbor:"previousRosterHash"`
}

func devicePossessionPreimage(c DeviceCertificateUnsignedV1) devicePossession {
	return devicePossession{c.IssuanceMode, c.DeviceID, c.KeyEpoch, c.SigningKeyID, c.SigningPublicKey, c.WrapKeyID, c.WrapPublicKey, c.JoinNonce, c.EnrollmentContextHash}
}
func verifyCertificateProofs(cert DeviceCertificateV1, previous VerifiedRoster, a VerifiedAuthorityState) error {
	c := cert.Certificate
	pos := devicePossessionPreimage(c)
	if err := verifySig(c.SigningPublicKey[:], "aplexica/device-possession/v1", pos, c.CandidateProof[:]); err != nil {
		return err
	}
	var approver *DeviceCertificateUnsignedV1
	for i := range previous.Manifest.Manifest.Devices {
		x := &previous.Manifest.Manifest.Devices[i].Certificate
		if x.DeviceID == c.ApproverDeviceID && x.SigningKeyID == c.ApproverSigningKeyID {
			approver = x
			break
		}
	}
	if approver == nil {
		return fmt.Errorf("%w: approver is not active", securityerr.ErrUntrustedRoster)
	}
	approval := pairingApprovalPreimage{pos, [32]byte(previous.Hash)}
	if err := verifySig(approver.SigningPublicKey[:], "aplexica/pairing-approval/v1", approval, c.ApproverProof[:]); err != nil {
		return err
	}
	if a.StateHash != previous.Authority.StateHash || a.AuthorityEpoch != previous.Authority.AuthorityEpoch {
		return fmt.Errorf("%w: credential issuer state is not current", securityerr.ErrUntrustedRoster)
	}
	return verifyActivePreviousThreshold(previous, cert.IssuerKeyIDs, cert.IssuanceSignatures, "aplexica/device-credential/v1", c, c.NotBeforeUnix)
}
func verifyNewJoinProofs(previous VerifiedRoster, next RosterManifestV1, a VerifiedAuthorityState) error {
	old := map[string]DeviceCertificateV1{}
	for _, c := range previous.Manifest.Manifest.Devices {
		old[c.Certificate.DeviceID] = c
	}
	for _, c := range next.Manifest.Devices {
		prior, existed := old[c.Certificate.DeviceID]
		if existed && sameSignedCredential(prior, c) {
			continue
		}
		if existed && sameCredentialKeys(prior.Certificate, c.Certificate) {
			// Credential freshness is not an access-set change, but it is still a
			// fresh authority issuance. The candidate/approver proofs may remain
			// unchanged because their preimage deliberately excludes validity;
			// the complete renewed credential is covered by the issuer threshold.
			if err := verifyActivePreviousThreshold(previous, c.IssuerKeyIDs, c.IssuanceSignatures, "aplexica/device-credential/v1", c.Certificate, c.Certificate.NotBeforeUnix); err != nil {
				return err
			}
			continue
		}
		if existed {
			if prior.Certificate.KeyEpoch == math.MaxUint64 || c.Certificate.KeyEpoch != prior.Certificate.KeyEpoch+1 {
				return fmt.Errorf("%w: device key replacement requires the next key epoch", securityerr.ErrUntrustedRoster)
			}
		} else if c.Certificate.KeyEpoch != 1 {
			return fmt.Errorf("%w: new device key epoch must be one", securityerr.ErrUntrustedRoster)
		}
		if c.Certificate.IssuanceMode != "pairing" {
			return fmt.Errorf("%w: non-pairing join requires atomic recovery", securityerr.ErrUntrustedRoster)
		}
		if err := verifyCertificateProofs(c, previous, a); err != nil {
			return err
		}
	}
	return nil
}

func sameCredentialKeys(left, right DeviceCertificateUnsignedV1) bool {
	return left.DeviceID == right.DeviceID && left.KeyEpoch == right.KeyEpoch &&
		left.SigningKeyID == right.SigningKeyID && left.SigningPublicKey == right.SigningPublicKey &&
		left.WrapKeyID == right.WrapKeyID && left.WrapPublicKey == right.WrapPublicKey
}

func sameSignedCredential(left, right DeviceCertificateV1) bool {
	leftBytes, leftErr := enc.Marshal(left)
	rightBytes, rightErr := enc.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func activeRosterAuthority(a VerifiedAuthorityState, devices []DeviceCertificateV1, signerKeyID [32]byte, at int64) (RosterAuthorityV1, error) {
	authority, ok := a.Authorities[DeviceKeyID(signerKeyID)]
	if !ok || authority.SigningKeyID != signerKeyID {
		return RosterAuthorityV1{}, fmt.Errorf("%w: signer is not an authority", securityerr.ErrUntrustedRoster)
	}
	for _, signed := range devices {
		credential := signed.Certificate
		if credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID &&
			credential.SigningPublicKey == authority.SigningPublicKey && credential.NotBeforeUnix <= at && at < credential.NotAfterUnix {
			return authority, nil
		}
	}
	return RosterAuthorityV1{}, fmt.Errorf("%w: authority has no active roster credential", securityerr.ErrUntrustedRoster)
}

func verifyActiveRosterThreshold(a VerifiedAuthorityState, devices []DeviceCertificateV1, ids [][32]byte, signatures [][64]byte, domain string, value any, at int64) error {
	if len(ids) != len(signatures) || len(ids) < int(a.Threshold) || len(ids) > len(a.Authorities) {
		return securityerr.ErrInvalidSignature
	}
	for index, id := range ids {
		if index > 0 && bytes.Compare(ids[index-1][:], id[:]) >= 0 {
			return securityerr.ErrInvalidSignature
		}
		authority, err := activeRosterAuthority(a, devices, id, at)
		if err != nil || verifySig(authority.SigningPublicKey[:], domain, value, signatures[index][:]) != nil {
			return securityerr.ErrInvalidSignature
		}
	}
	return nil
}

func verifyActivePreviousThreshold(previous VerifiedRoster, ids [][32]byte, signatures [][64]byte, domain string, value any, at int64) error {
	return verifyActiveRosterThreshold(previous.Authority, previous.Manifest.Manifest.Devices, ids, signatures, domain, value, at)
}

func requireAuthoritiesActiveInRoster(a VerifiedAuthorityState, devices []DeviceCertificateV1, at int64) error {
	for keyID := range a.Authorities {
		if _, err := activeRosterAuthority(a, devices, [32]byte(keyID), at); err != nil {
			return err
		}
	}
	return nil
}

// RecoveryEnrollmentContextHash binds a recovery-mode credential to the exact
// complete root-signed enrollment, rather than to server-selected public
// fields. It is exported for the local recovery builder; verification never
// accepts a digest supplied by the service without the signed enrollment.
func RecoveryEnrollmentContextHash(enrollment RecoveryEnrollmentV1) ([32]byte, error) {
	return digest("aplexica/recovery-device-enrollment-context/v1", enrollment)
}

func verifyAtomicCredentialLineage(previous VerifiedRoster, candidate VerifiedAuthorityState, transition AuthorityTransitionV1, enrollments []RecoveryEnrollmentV1, next RosterManifestV1) error {
	priorByDevice := make(map[string]DeviceCertificateV1, len(previous.Manifest.Manifest.Devices))
	for _, signed := range previous.Manifest.Manifest.Devices {
		priorByDevice[signed.Certificate.DeviceID] = signed
	}
	enrollmentByDevice := make(map[string]RecoveryEnrollmentV1, len(enrollments))
	for _, enrollment := range enrollments {
		enrollmentByDevice[enrollment.Enrollment.CandidateDeviceID] = enrollment
	}
	changed := make(map[string]bool, len(next.Manifest.Devices))
	for _, signed := range next.Manifest.Devices {
		credential := signed.Certificate
		prior, existed := priorByDevice[credential.DeviceID]
		if existed && sameSignedCredential(prior, signed) {
			continue
		}
		changed[credential.DeviceID] = true
		if existed && sameCredentialKeys(prior.Certificate, credential) {
			// Credential-only renewal is still issued by the authorizing state
			// for this atomic package.
		} else if existed {
			if prior.Certificate.KeyEpoch == math.MaxUint64 || credential.KeyEpoch != prior.Certificate.KeyEpoch+1 {
				return fmt.Errorf("%w: recovery/rotation key epoch mismatch", securityerr.ErrUntrustedRoster)
			}
		} else if credential.KeyEpoch != 1 {
			return fmt.Errorf("%w: new recovery key epoch must be one", securityerr.ErrUntrustedRoster)
		}
		switch transition.Transition.AuthorizationMode {
		case "operational":
			if credential.IssuedUnderAuthorityEpoch != previous.Authority.AuthorityEpoch || credential.IssuingAuthorityStateHash != previous.Authority.StateHash {
				return fmt.Errorf("%w: paired credential was not issued by previous authority", securityerr.ErrUntrustedRoster)
			}
			if existed && sameCredentialKeys(prior.Certificate, credential) {
				if err := verifyActivePreviousThreshold(previous, signed.IssuerKeyIDs, signed.IssuanceSignatures, "aplexica/device-credential/v1", credential, credential.NotBeforeUnix); err != nil {
					return err
				}
			} else if credential.IssuanceMode != "pairing" {
				return fmt.Errorf("%w: operational authority addition requires pairing", securityerr.ErrUntrustedRoster)
			} else if err := verifyCertificateProofs(signed, previous, previous.Authority); err != nil {
				return err
			}
		case "recovery":
			enrollment, ok := enrollmentByDevice[credential.DeviceID]
			if !ok || credential.IssuanceMode != "recovery" || credential.ApproverDeviceID != "" ||
				credential.ApproverSigningKeyID != ([32]byte{}) || credential.ApproverProof != ([64]byte{}) ||
				credential.IssuedUnderAuthorityEpoch != candidate.AuthorityEpoch || credential.IssuingAuthorityStateHash != candidate.StateHash ||
				enrollment.Enrollment.CandidateSigningKeyID != credential.SigningKeyID || enrollment.Enrollment.CandidateSigningPublic != credential.SigningPublicKey ||
				enrollment.Enrollment.CandidateWrapKeyID != credential.WrapKeyID || enrollment.Enrollment.CandidateWrapPublic != credential.WrapPublicKey ||
				enrollment.Enrollment.JoinNonce != credential.JoinNonce || enrollment.Enrollment.NotBeforeUnix != credential.NotBeforeUnix ||
				enrollment.Enrollment.NotAfterUnix != credential.NotAfterUnix {
				return fmt.Errorf("%w: recovery credential/enrollment mismatch", securityerr.ErrUntrustedRoster)
			}
			contextHash, err := RecoveryEnrollmentContextHash(enrollment)
			if err != nil || credential.EnrollmentContextHash != contextHash ||
				verifySig(credential.SigningPublicKey[:], "aplexica/device-possession/v1", devicePossessionPreimage(credential), credential.CandidateProof[:]) != nil {
				return fmt.Errorf("%w: invalid recovery possession proof", securityerr.ErrUntrustedRoster)
			}
			if err := verifyActiveRosterThreshold(candidate, next.Manifest.Devices, signed.IssuerKeyIDs, signed.IssuanceSignatures, "aplexica/device-credential/v1", credential, credential.NotBeforeUnix); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: invalid authority transition mode", securityerr.ErrUntrustedRoster)
		}
	}
	for _, authority := range candidate.Authorities {
		lineageOK := false
		for _, prior := range previous.Manifest.Manifest.Devices {
			credential := prior.Certificate
			if credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID && credential.SigningPublicKey == authority.SigningPublicKey &&
				credential.NotBeforeUnix <= next.Manifest.IssuedAtUnix && next.Manifest.IssuedAtUnix < credential.NotAfterUnix {
				lineageOK = true
				break
			}
		}
		if lineageOK {
			continue
		}
		for _, signed := range next.Manifest.Devices {
			credential := signed.Certificate
			if changed[credential.DeviceID] && credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID && credential.SigningPublicKey == authority.SigningPublicKey {
				if transition.Transition.AuthorizationMode == "operational" && credential.IssuanceMode == "pairing" {
					lineageOK = true
				}
				if transition.Transition.AuthorizationMode == "recovery" {
					_, lineageOK = enrollmentByDevice[credential.DeviceID]
				}
				break
			}
		}
		if !lineageOK {
			return fmt.Errorf("%w: new authority lacks previous-roster or recovery lineage", securityerr.ErrUntrustedRoster)
		}
	}
	return nil
}
func VerifyJoinProof(cert DeviceCertificateV1, t PairingTranscriptV1, previous VerifiedRoster, a VerifiedAuthorityState) error {
	h, err := TranscriptHash(t)
	if err != nil {
		return err
	}
	if cert.Certificate.EnrollmentContextHash != h || cert.Certificate.DeviceID != t.CandidateDeviceID || cert.Certificate.ApproverDeviceID != t.ApproverDeviceID {
		return securityerr.ErrMetadataMismatch
	}
	return verifyCertificateProofs(cert, previous, a)
}
