package generationactivation

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const stateVersion uint16 = 1

type pendingActivation struct {
	BindingDigest   [32]byte  `json:"binding_digest"`
	AttestationBlob []byte    `json:"attestation_blob"`
	PreparedAt      time.Time `json:"prepared_at"`
}

type durableState struct {
	Version                uint16             `json:"version"`
	StreamEpoch            string             `json:"stream_epoch"`
	PublishedDigest        [32]byte           `json:"published_digest"`
	ActivatedBindingDigest [32]byte           `json:"activated_binding_digest"`
	AuthorityDigest        [32]byte           `json:"authority_digest"`
	AuthorityRevision      uint64             `json:"authority_revision"`
	Pending                *pendingActivation `json:"pending,omitempty"`
}

type StateStore interface {
	Load() (durableState, error)
	Save(durableState) error
}

type FileStateStore struct{ Path string }

// LoadSecurityEpoch reads an already committed security-epoch barrier. It is
// deliberately read-only and never falls back to security-coordinator.json or
// derives missing hashes/counters from mutable runtime state.
func LoadSecurityEpoch(path string) (SecurityEpochState, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return SecurityEpochState{}, err
	}
	root, err := privatefs.OpenRoot(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	if err != nil {
		return SecurityEpochState{}, err
	}
	defer root.Close()
	file, err := root.OpenReadRegular(filepath.Base(abs))
	if err != nil {
		return SecurityEpochState{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 64<<10+1))
	closeErr := file.Close()
	if readErr != nil {
		return SecurityEpochState{}, readErr
	}
	if closeErr != nil {
		return SecurityEpochState{}, closeErr
	}
	if len(raw) > 64<<10 {
		return SecurityEpochState{}, ErrInvalidState
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var state SecurityEpochState
	if err := dec.Decode(&state); err != nil {
		return SecurityEpochState{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return SecurityEpochState{}, ErrInvalidState
	}
	modeOK := state.KeyMode == "recipient-wrap-v2" && state.KeyVersion == 0 || state.KeyMode == "namespace-key-v1" && state.KeyVersion > 0 && state.ScopeType == "namespace"
	if state.Version != 1 || (state.ScopeType != "account" && state.ScopeType != "namespace") || !validOpaque(state.ScopeID, 256) ||
		state.RosterHash == ([32]byte{}) || state.AccessGeneration == 0 || state.AccessSetHash == ([32]byte{}) ||
		state.BarrierID == ([32]byte{}) || state.TreeHeadDigest == ([32]byte{}) || state.CoordinatorGeneration == 0 || !modeOK {
		return SecurityEpochState{}, ErrInvalidState
	}
	return state, nil
}

func (s FileStateStore) Load() (durableState, error) {
	abs, err := filepath.Abs(s.Path)
	if err != nil {
		return durableState{}, err
	}
	root, err := privatefs.OpenRoot(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return durableState{}, err
	}
	defer root.Close()
	file, err := root.OpenReadRegular(filepath.Base(abs))
	if err != nil {
		return durableState{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 2<<20+1))
	closeErr := file.Close()
	if readErr != nil {
		return durableState{}, readErr
	}
	if closeErr != nil {
		return durableState{}, closeErr
	}
	if len(raw) > 2<<20 {
		return durableState{}, ErrInvalidState
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var state durableState
	if err := dec.Decode(&state); err != nil {
		return durableState{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return durableState{}, ErrInvalidState
	}
	if err := validateDurableState(state); err != nil {
		return durableState{}, err
	}
	return state, nil
}

func (s FileStateStore) Save(state durableState) error {
	if err := validateDurableState(state); err != nil {
		return err
	}
	abs, err := filepath.Abs(s.Path)
	if err != nil {
		return err
	}
	if err := privatefs.EnsureDir(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return err
	}
	root, err := privatefs.OpenRoot(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return err
	}
	defer root.Close()
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > 2<<20 {
		return ErrInvalidState
	}
	return root.WriteFile(filepath.Base(abs), raw, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func validateDurableState(state durableState) error {
	if state.Version != stateVersion || !validOpaque(state.StreamEpoch, 256) {
		return ErrInvalidState
	}
	activated := state.ActivatedBindingDigest != ([32]byte{}) || state.AuthorityDigest != ([32]byte{}) || state.AuthorityRevision != 0
	if activated && (state.ActivatedBindingDigest == ([32]byte{}) || state.AuthorityDigest == ([32]byte{}) || state.AuthorityRevision == 0) {
		return ErrInvalidState
	}
	if state.Pending != nil {
		if state.PublishedDigest == ([32]byte{}) || state.Pending.BindingDigest == ([32]byte{}) || state.Pending.PreparedAt.IsZero() || len(state.Pending.AttestationBlob) == 0 || len(state.Pending.AttestationBlob) > 1<<20 {
			return ErrInvalidState
		}
		signed, err := DecodeCanonical(state.Pending.AttestationBlob)
		if err != nil || signed.Attestation.StreamEpoch != state.StreamEpoch {
			return ErrInvalidState
		}
		binding, err := BindingDigest(signed.Attestation)
		if err != nil || binding != state.Pending.BindingDigest {
			return ErrInvalidState
		}
	}
	return nil
}

func parseAuthorityDigest(value string) ([32]byte, error) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 32 || value != hex.EncodeToString(raw) {
		return [32]byte{}, fmt.Errorf("%w: invalid authority digest", ErrInvalidState)
	}
	var digest [32]byte
	copy(digest[:], raw)
	return digest, nil
}
