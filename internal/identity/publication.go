package identity

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// PublicationObject is one canonical, already client-signed identity object
// together with the immutable routing-chain metadata expected by the cloud
// roster store. Blob is canonical CBOR produced from the exact object pinned
// in ChainStore; no signature or trust state is manufactured here.
type PublicationObject struct {
	ScopeType    string
	ScopeID      string
	Kind         string
	Sequence     uint64
	PreviousHash [32]byte
	Hash         [32]byte
	Blob         []byte
}

// PublicationSnapshot is a verified, point-in-time export of the complete
// local trust chain needed by a zero-knowledge control plane to independently
// verify Current. Objects are ordered so publishing them in order is safe and
// idempotent: anchor, then each roster step's authority transition (when
// present), roster, and exact atomic authority/roster package. The separate
// atomic package keeps the already-published transition and roster object
// hashes stable while making recovery enrollment lineage independently hashed.
type PublicationSnapshot struct {
	AccountID string
	Objects   []PublicationObject
	Current   VerifiedRoster
}

// PublicationSnapshot returns only an already-initialized and currently valid
// chain. It never creates an anchor, roster, authority, or signing key.
func (s *ChainStore) PublicationSnapshot(now time.Time) (PublicationSnapshot, error) {
	if s == nil || now.IsZero() {
		return PublicationSnapshot{}, fmt.Errorf("identity: publication snapshot unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, current, err := s.loadLocked()
	if err != nil {
		return PublicationSnapshot{}, err
	}
	manifest := current.Manifest.Manifest
	if now.Before(time.Unix(manifest.IssuedAtUnix, 0)) || !now.Before(time.Unix(manifest.NotAfterUnix, 0)) {
		return PublicationSnapshot{}, fmt.Errorf("identity: publication roster is not current")
	}
	accountID := state.Anchor.Anchor.AccountID
	anchorBlob, err := enc.Marshal(state.Anchor)
	if err != nil {
		return PublicationSnapshot{}, fmt.Errorf("identity: encode trust anchor: %w", err)
	}
	anchorHash := sha256.Sum256(anchorBlob)
	objects := []PublicationObject{{
		ScopeType: "account", ScopeID: accountID, Kind: "trust-anchor",
		Sequence: 1, Hash: anchorHash, Blob: anchorBlob,
	}}

	previousAuthorityHash := anchorHash
	var previousRosterHash [32]byte
	for index, step := range state.Steps {
		if step.AuthorityTransition != nil {
			blob, marshalErr := enc.Marshal(*step.AuthorityTransition)
			if marshalErr != nil {
				return PublicationSnapshot{}, fmt.Errorf("identity: encode authority transition: %w", marshalErr)
			}
			hash := sha256.Sum256(blob)
			objects = append(objects, PublicationObject{
				ScopeType: "account", ScopeID: accountID, Kind: "authority-transition",
				Sequence:     step.AuthorityTransition.Transition.NewAuthorityEpoch,
				PreviousHash: previousAuthorityHash, Hash: hash, Blob: blob,
			})
			previousAuthorityHash = hash
		}
		previousRosterObjectHash := previousRosterHash
		blob, marshalErr := enc.Marshal(step.Roster)
		if marshalErr != nil {
			return PublicationSnapshot{}, fmt.Errorf("identity: encode roster: %w", marshalErr)
		}
		hash := sha256.Sum256(blob)
		epoch := step.Roster.Manifest.Epoch
		if epoch != uint64(index+1) {
			return PublicationSnapshot{}, fmt.Errorf("identity: non-contiguous publication roster")
		}
		objects = append(objects, PublicationObject{
			ScopeType: step.Roster.Manifest.ScopeType, ScopeID: step.Roster.Manifest.ScopeID,
			Kind: "roster", Sequence: epoch, PreviousHash: previousRosterHash,
			Hash: hash, Blob: blob,
		})
		previousRosterHash = hash
		if step.AuthorityTransition != nil {
			// The raw transition and roster remain separate, stable objects for
			// their existing chains. This third object is the exact locally pinned
			// atomic package and therefore commits recovery enrollments without
			// overloading the unhashed transparency ProofBlob field.
			atomic := AtomicAuthorityRosterTransitionV1{
				AuthorityTransition: *step.AuthorityTransition,
				RecoveryEnrollments: append([]RecoveryEnrollmentV1(nil), step.Enrollments...),
				NextRoster:          step.Roster,
			}
			atomicBlob, atomicErr := enc.Marshal(atomic)
			if atomicErr != nil {
				return PublicationSnapshot{}, fmt.Errorf("identity: encode atomic authority roster transition: %w", atomicErr)
			}
			atomicHash := sha256.Sum256(atomicBlob)
			objects = append(objects, PublicationObject{
				ScopeType: step.Roster.Manifest.ScopeType, ScopeID: step.Roster.Manifest.ScopeID,
				Kind: "atomic-authority-roster-transition", Sequence: epoch,
				PreviousHash: previousRosterObjectHash, Hash: atomicHash, Blob: atomicBlob,
			})
		}
	}
	return PublicationSnapshot{AccountID: accountID, Objects: objects, Current: current}, nil
}

// CanonicalDeviceCredentialBytes returns the exact canonical signed device
// credential embedded in a verified roster. It is used only to register the
// authenticated caller's existing credential with the control plane.
func CanonicalDeviceCredentialBytes(certificate DeviceCertificateV1) ([]byte, error) {
	return enc.Marshal(certificate)
}
