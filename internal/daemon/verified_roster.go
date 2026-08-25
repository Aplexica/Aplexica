package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityerr"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

type securityEpochState struct {
	Version               uint16   `json:"version"`
	ScopeType             string   `json:"scopeType"`
	ScopeID               string   `json:"scopeId"`
	RosterHash            [32]byte `json:"rosterHash"`
	AccessGeneration      uint64   `json:"accessGeneration"`
	AccessSetHash         [32]byte `json:"accessSetHash"`
	BarrierID             [32]byte `json:"barrierId"`
	TreeHeadDigest        [32]byte `json:"treeHeadDigest"`
	KeyMode               string   `json:"keyMode"`
	KeyVersion            uint64   `json:"keyVersion"`
	CoordinatorGeneration uint64   `json:"coordinatorGeneration"`
}

type VerifiedRosterProvider struct {
	Chain     *identity.ChainStore
	EpochPath string
	BaseDir   string
}

func NewVerifiedRosterProvider(identityDir string) *VerifiedRosterProvider {
	return &VerifiedRosterProvider{BaseDir: identityDir, Chain: &identity.ChainStore{Path: filepath.Join(identityDir, "account", "chain.cbor")}, EpochPath: filepath.Join(identityDir, "account", "security-epoch.json")}
}

func (p *VerifiedRosterProvider) Current(_ context.Context, scopeType, scopeID string) (syncd.RosterSnapshot, error) {
	if p == nil || p.Chain == nil {
		return syncd.RosterSnapshot{}, securityerr.ErrUntrustedRoster
	}
	chain := p.Chain
	epochPath := p.EpochPath
	if scopeType == "namespace" {
		if err := acf.ValidateWireUUIDv7(scopeID); err != nil || p.BaseDir == "" {
			return syncd.RosterSnapshot{}, securityerr.ErrUntrustedRoster
		}
		chain = &identity.ChainStore{Path: filepath.Join(p.BaseDir, "namespaces", scopeID, "chain.cbor")}
		epochPath = filepath.Join(p.BaseDir, "namespaces", scopeID, "security-epoch.json")
	} else if scopeType != "account" {
		return syncd.RosterSnapshot{}, securityerr.ErrUntrustedRoster
	}
	roster, err := chain.Current(time.Now())
	if err != nil {
		return syncd.RosterSnapshot{}, err
	}
	abs, err := filepath.Abs(epochPath)
	if err != nil {
		return syncd.RosterSnapshot{}, err
	}
	root, err := privatefs.OpenRoot(filepath.Dir(abs), privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	if err != nil {
		return syncd.RosterSnapshot{}, err
	}
	defer root.Close()
	f, err := root.OpenReadRegular(filepath.Base(abs))
	if err != nil {
		return syncd.RosterSnapshot{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil || len(b) >= 64<<10 {
		return syncd.RosterSnapshot{}, securityerr.ErrLimitExceeded
	}
	var state securityEpochState
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&state); err != nil {
		return syncd.RosterSnapshot{}, err
	}
	if err = dec.Decode(&struct{}{}); err != io.EOF {
		return syncd.RosterSnapshot{}, securityerr.ErrMetadataMismatch
	}
	m := roster.Manifest.Manifest
	modeOK := state.KeyMode == "recipient-wrap-v2" && state.KeyVersion == 0 || state.KeyMode == "namespace-key-v1" && state.KeyVersion > 0 && scopeType == "namespace"
	if state.Version != 1 || state.CoordinatorGeneration == 0 || state.ScopeType != scopeType || (scopeID != "" && state.ScopeID != scopeID) || state.RosterHash != [32]byte(roster.Hash) || state.AccessGeneration != m.AccessGeneration || state.AccessSetHash != m.AccessSetHash || state.BarrierID == ([32]byte{}) || state.TreeHeadDigest == ([32]byte{}) || !modeOK {
		return syncd.RosterSnapshot{}, securityerr.ErrMetadataMismatch
	}
	return syncd.RosterSnapshot{Roster: roster, BarrierID: state.BarrierID, TreeHeadDigest: state.TreeHeadDigest, KeyMode: state.KeyMode, KeyVersion: state.KeyVersion, CoordinatorGeneration: state.CoordinatorGeneration}, nil
}

var _ syncd.VerifiedRosterProvider = (*VerifiedRosterProvider)(nil)
