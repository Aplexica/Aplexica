package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"

	"github.com/aplexica/aplexica/internal/securityerr"
)

func VerifyTrustAnchor(anchor AccountTrustAnchorV1, expectedRecoveryRoot ed25519.PublicKey) (VerifiedAuthorityState, error) {
	a := anchor.Anchor
	if a.Version != 1 || a.AuthorityEpoch != 1 || a.RecoveryKDFProfileID != "argon2id-256m-t3-p1-v1" || !validText(a.ServiceOrigin, 512) || !validText(a.AccountID, 256) || !validText(a.PersonalScopeID, 128) || !nonzero32(a.RecoveryRootPublicKey) || !nonzero32(a.RecoveryWrapPublicKey) || sha256.Sum256(a.RecoveryWrapPublicKey[:]) != a.RecoveryWrapKeyID || !authoritiesValid(a.Authorities, a.AuthorityThreshold) {
		return VerifiedAuthorityState{}, fmt.Errorf("%w: invalid trust anchor", securityerr.ErrUntrustedRoster)
	}
	if len(expectedRecoveryRoot) != 32 || !ed25519.PublicKey(a.RecoveryRootPublicKey[:]).Equal(expectedRecoveryRoot) {
		return VerifiedAuthorityState{}, fmt.Errorf("%w: recovery root mismatch", securityerr.ErrUntrustedRoster)
	}
	if err := verifySig(a.RecoveryRootPublicKey[:], "aplexica/account-trust-anchor/v1", a, anchor.RecoverySignature[:]); err != nil {
		return VerifiedAuthorityState{}, err
	}
	ah, err := digest("aplexica/account-trust-anchor-hash/v1", anchor)
	if err != nil {
		return VerifiedAuthorityState{}, err
	}
	sh, err := digest("aplexica/authority-state-genesis/v1", ah, a.AuthorityEpoch, a.Authorities, a.AuthorityThreshold)
	if err != nil {
		return VerifiedAuthorityState{}, err
	}
	return VerifiedAuthorityState{Anchor: anchor, AnchorHash: TrustAnchorHash(ah), StateHash: sh, AuthorityEpoch: a.AuthorityEpoch, Authorities: mapAuthorities(a.Authorities), Threshold: a.AuthorityThreshold}, nil
}
func AuthorityTransitionHash(t AuthorityTransitionV1) ([32]byte, error) {
	return digest("aplexica/authority-transition-hash/v1", t)
}
func VerifyAuthorityTransition(previous VerifiedAuthorityState, t AuthorityTransitionV1) (VerifiedAuthorityState, error) {
	u := t.Transition
	if u.Version != 1 || u.AccountID != previous.Anchor.Anchor.AccountID || u.TrustAnchorHash != [32]byte(previous.AnchorHash) || u.PreviousStateHash != previous.StateHash || u.PreviousAuthorityEpoch != previous.AuthorityEpoch || u.NewAuthorityEpoch != previous.AuthorityEpoch+1 || !authoritiesValid(u.NewAuthorities, u.NewThreshold) || !futureSane(u.IssuedAtUnix) || !nonzero32(u.Nonce) {
		return VerifiedAuthorityState{}, fmt.Errorf("%w: invalid authority transition", securityerr.ErrUntrustedRoster)
	}
	switch u.AuthorizationMode {
	case "operational":
		if err := verifyThreshold(t.SignerKeyIDs, t.Signatures, previous.Threshold, previous.Authorities, "aplexica/authority-transition/v1", u); err != nil {
			return VerifiedAuthorityState{}, err
		}
	case "recovery":
		rid := sha256.Sum256(previous.Anchor.Anchor.RecoveryRootPublicKey[:])
		if len(t.SignerKeyIDs) != 1 || len(t.Signatures) != 1 || t.SignerKeyIDs[0] != rid {
			return VerifiedAuthorityState{}, securityerr.ErrInvalidSignature
		}
		if err := verifySig(previous.Anchor.Anchor.RecoveryRootPublicKey[:], "aplexica/authority-recovery/v1", u, t.Signatures[0][:]); err != nil {
			return VerifiedAuthorityState{}, err
		}
	default:
		return VerifiedAuthorityState{}, fmt.Errorf("%w: invalid authorization mode", securityerr.ErrUntrustedRoster)
	}
	th, err := AuthorityTransitionHash(t)
	if err != nil {
		return VerifiedAuthorityState{}, err
	}
	next, err := digest("aplexica/authority-state-next/v1", previous.StateHash, th, u.NewAuthorityEpoch, u.NewAuthorities, u.NewThreshold)
	if err != nil {
		return VerifiedAuthorityState{}, err
	}
	return VerifiedAuthorityState{Anchor: previous.Anchor, AnchorHash: previous.AnchorHash, StateHash: next, AuthorityEpoch: u.NewAuthorityEpoch, Authorities: mapAuthorities(u.NewAuthorities), Threshold: u.NewThreshold}, nil
}
