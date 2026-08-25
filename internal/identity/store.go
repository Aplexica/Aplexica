package identity

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityerr"
)

const chainStateMaxBytes = 8 << 20

type rosterStep struct {
	Roster              RosterManifestV1       `cbor:"roster"`
	AuthorityTransition *AuthorityTransitionV1 `cbor:"authorityTransition,omitempty"`
	Enrollments         []RecoveryEnrollmentV1 `cbor:"enrollments,omitempty"`
}
type chainState struct {
	Version              uint16               `cbor:"version"`
	ExpectedRecoveryRoot [32]byte             `cbor:"expectedRecoveryRoot"`
	Anchor               AccountTrustAnchorV1 `cbor:"anchor"`
	Steps                []rosterStep         `cbor:"steps"`
}

// ChainStore pins the complete verified authority/roster hash chain. The
// service can replay, omit, or corrupt bytes, but cannot replace this state or
// advance it without the required client signatures.
type ChainStore struct {
	Path string
	mu   sync.Mutex
}

func (s *ChainStore) root() (*privatefs.Root, string, error) {
	abs, err := filepath.Abs(s.Path)
	if err != nil {
		return nil, "", err
	}
	parent, base := filepath.Dir(abs), filepath.Base(abs)
	if err := privatefs.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return nil, "", err
	}
	r, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	return r, base, err
}

func verifyChain(st chainState) (VerifiedRoster, error) {
	if st.Version != 1 || len(st.Steps) == 0 {
		return VerifiedRoster{}, fmt.Errorf("%w: empty identity chain", securityerr.ErrUntrustedRoster)
	}
	a, err := VerifyTrustAnchor(st.Anchor, ed25519.PublicKey(st.ExpectedRecoveryRoot[:]))
	if err != nil {
		return VerifiedRoster{}, err
	}
	current, err := VerifyGenesis(a, st.Steps[0].Roster)
	if err != nil {
		return VerifiedRoster{}, err
	}
	if st.Steps[0].AuthorityTransition != nil || len(st.Steps[0].Enrollments) != 0 {
		return VerifiedRoster{}, fmt.Errorf("%w: genesis has transition", securityerr.ErrUntrustedRoster)
	}
	for i := 1; i < len(st.Steps); i++ {
		step := st.Steps[i]
		if step.AuthorityTransition == nil {
			if len(step.Enrollments) != 0 {
				return VerifiedRoster{}, securityerr.ErrMetadataMismatch
			}
			current, err = VerifyTransition(current, current.Authority, step.Roster)
		} else {
			current, err = VerifyAtomicAuthorityRosterTransition(current, *step.AuthorityTransition, step.Enrollments, step.Roster)
		}
		if err != nil {
			return VerifiedRoster{}, fmt.Errorf("identity: verify step %d: %w", i, err)
		}
	}
	return current, nil
}

func (s *ChainStore) loadLocked() (chainState, VerifiedRoster, error) {
	r, base, err := s.root()
	if err != nil {
		return chainState{}, VerifiedRoster{}, err
	}
	defer r.Close()
	f, err := r.OpenReadRegular(base)
	if err != nil {
		return chainState{}, VerifiedRoster{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, chainStateMaxBytes+1))
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil || len(b) > chainStateMaxBytes {
		return chainState{}, VerifiedRoster{}, fmt.Errorf("identity: read chain: %w", err)
	}
	var st chainState
	if err = dec.Unmarshal(b, &st); err != nil {
		return chainState{}, VerifiedRoster{}, fmt.Errorf("identity: decode chain: %w", err)
	}
	v, err := verifyChain(st)
	return st, v, err
}

func (s *ChainStore) saveLocked(st chainState) error {
	if _, err := verifyChain(st); err != nil {
		return err
	}
	b, err := enc.Marshal(st)
	if err != nil {
		return err
	}
	if len(b) > chainStateMaxBytes {
		return securityerr.ErrLimitExceeded
	}
	r, base, err := s.root()
	if err != nil {
		return err
	}
	defer r.Close()
	return r.WriteFile(base, b, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func (s *ChainStore) Initialize(anchor AccountTrustAnchorV1, expected ed25519.PublicKey, genesis RosterManifestV1) (VerifiedRoster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(expected) != ed25519.PublicKeySize {
		return VerifiedRoster{}, securityerr.ErrUntrustedRoster
	}
	if _, _, err := s.loadLocked(); err == nil {
		return VerifiedRoster{}, fmt.Errorf("identity: chain already initialized")
	} else if !errors.Is(err, os.ErrNotExist) {
		return VerifiedRoster{}, err
	}
	var root [32]byte
	copy(root[:], expected)
	st := chainState{Version: 1, ExpectedRecoveryRoot: root, Anchor: anchor, Steps: []rosterStep{{Roster: genesis}}}
	v, err := verifyChain(st)
	if err != nil {
		return VerifiedRoster{}, err
	}
	if err = s.saveLocked(st); err != nil {
		return VerifiedRoster{}, err
	}
	return v, nil
}

func (s *ChainStore) Current(now time.Time) (VerifiedRoster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, v, err := s.loadLocked()
	if err != nil {
		return VerifiedRoster{}, err
	}
	m := v.Manifest.Manifest
	if now.Before(time.Unix(m.IssuedAtUnix, 0)) || !now.Before(time.Unix(m.NotAfterUnix, 0)) {
		return VerifiedRoster{}, securityerr.ErrStaleRoster
	}
	return v, nil
}

// Head returns the highest fully verified local chain head without applying a
// wall-clock freshness policy. It exists for crash recovery and renewal only;
// publication/admission callers must continue to use Current.
func (s *ChainStore) Head() (VerifiedRoster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, verified, err := s.loadLocked()
	return verified, err
}

// VerifiedByHash reconstructs a particular locally pinned roster from the
// complete verified chain. It is intended for journal recovery after the head
// has already advanced; it never accepts a roster supplied outside that chain.
func (s *ChainStore) VerifiedByHash(want RosterHash) (VerifiedRoster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, _, err := s.loadLocked()
	if err != nil {
		return VerifiedRoster{}, err
	}
	authority, err := VerifyTrustAnchor(state.Anchor, ed25519.PublicKey(state.ExpectedRecoveryRoot[:]))
	if err != nil {
		return VerifiedRoster{}, err
	}
	current, err := VerifyGenesis(authority, state.Steps[0].Roster)
	if err != nil {
		return VerifiedRoster{}, err
	}
	if current.Hash == want {
		return current, nil
	}
	for index := 1; index < len(state.Steps); index++ {
		step := state.Steps[index]
		if step.AuthorityTransition == nil {
			current, err = VerifyTransition(current, current.Authority, step.Roster)
		} else {
			current, err = VerifyAtomicAuthorityRosterTransition(current, *step.AuthorityTransition, step.Enrollments, step.Roster)
		}
		if err != nil {
			return VerifiedRoster{}, err
		}
		if current.Hash == want {
			return current, nil
		}
	}
	return VerifiedRoster{}, fmt.Errorf("identity: roster hash is not locally pinned")
}

// AppendRosterExact is crash-retry safe. It appends the immediate successor,
// or succeeds only when the exact same signed successor is already the durable
// head. It never skips an epoch or accepts same-epoch equivocation.
func (s *ChainStore) AppendRosterExact(next RosterManifestV1) (VerifiedRoster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, current, err := s.loadLocked()
	if err != nil {
		return VerifiedRoster{}, err
	}
	nextHash, err := HashRoster(next)
	if err != nil {
		return VerifiedRoster{}, err
	}
	if nextHash == current.Hash {
		currentBytes, leftErr := enc.Marshal(current.Manifest)
		nextBytes, rightErr := enc.Marshal(next)
		if leftErr != nil || rightErr != nil || !bytes.Equal(currentBytes, nextBytes) {
			return VerifiedRoster{}, fmt.Errorf("identity: same-hash roster mismatch")
		}
		return current, nil
	}
	verified, err := VerifyTransition(current, current.Authority, next)
	if err != nil {
		return VerifiedRoster{}, err
	}
	state.Steps = append(state.Steps, rosterStep{Roster: next})
	if err := s.saveLocked(state); err != nil {
		return VerifiedRoster{}, err
	}
	return verified, nil
}

func (s *ChainStore) AppendRoster(next RosterManifestV1) (VerifiedRoster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, current, err := s.loadLocked()
	if err != nil {
		return VerifiedRoster{}, err
	}
	v, err := VerifyTransition(current, current.Authority, next)
	if err != nil {
		return VerifiedRoster{}, err
	}
	st.Steps = append(st.Steps, rosterStep{Roster: next})
	if err = s.saveLocked(st); err != nil {
		return VerifiedRoster{}, err
	}
	return v, nil
}

func (s *ChainStore) AppendAtomic(pkg AtomicAuthorityRosterTransitionV1) (VerifiedRoster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, current, err := s.loadLocked()
	if err != nil {
		return VerifiedRoster{}, err
	}
	v, err := VerifyAtomicAuthorityRosterTransition(current, pkg.AuthorityTransition, pkg.RecoveryEnrollments, pkg.NextRoster)
	if err != nil {
		return VerifiedRoster{}, err
	}
	transition := pkg.AuthorityTransition
	st.Steps = append(st.Steps, rosterStep{Roster: pkg.NextRoster, AuthorityTransition: &transition, Enrollments: append([]RecoveryEnrollmentV1(nil), pkg.RecoveryEnrollments...)})
	if err = s.saveLocked(st); err != nil {
		return VerifiedRoster{}, err
	}
	return v, nil
}
