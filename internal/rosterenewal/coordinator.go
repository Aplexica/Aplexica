// Package rosterenewal maintains the short-lived signed roster head. Renewal
// is authority-threshold work; the service is never a freshness authority.
package rosterenewal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/aplexica/aplexica/internal/securityerr"
)

var (
	ErrInvalidPolicy                = errors.New("roster renewal: invalid scheduler policy")
	ErrRosterExpired                = errors.New("roster renewal: roster expired; cloud sync remains paused")
	ErrCredentialRenewalUnavailable = errors.New("roster renewal: credential renewal authority unavailable")
	ErrPendingRenewal               = errors.New("roster renewal: unresolved durable renewal")
)

type Policy struct {
	RenewAfter            time.Duration
	RetryInterval         time.Duration
	CredentialRenewBefore time.Duration
}

type FreshnessEndorsementCollector interface {
	CollectFreshnessEndorsements(context.Context, identity.VerifiedRoster, identity.RosterManifestUnsignedV1) (identity.RosterManifestUnsignedV1, []identity.RosterFreshnessEndorsementV1, error)
}

// CredentialRenewer returns one complete authority-threshold-signed successor
// with unchanged access projection and renewed credential validity. It may
// collect candidate possession and issuer signatures on other devices; no
// private key crosses this interface.
type CredentialRenewer interface {
	RenewCredentials(context.Context, identity.VerifiedRoster, time.Time) (identity.RosterManifestV1, error)
}

type Result struct {
	Renewed    bool
	RosterHash identity.RosterHash
	NextTry    time.Time
}

type epochRecord struct {
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

type renewalJournal struct {
	Version            uint16                    `json:"version"`
	PreviousRosterHash [32]byte                  `json:"previousRosterHash"`
	NextRoster         identity.RosterManifestV1 `json:"nextRoster"`
	NextEpoch          epochRecord               `json:"nextEpoch"`
	Checksum           [32]byte                  `json:"checksum"`
}

type crashHooks struct {
	afterJournal func() error
	afterChain   func() error
	afterEpoch   func() error
}

type Coordinator struct {
	IdentityRoot string
	Chain        *identity.ChainStore
	Security     *securityepoch.Coordinator
	Collector    FreshnessEndorsementCollector
	Credentials  CredentialRenewer
	Policy       Policy
	Now          func() time.Time
	hooks        crashHooks
}

func (c *Coordinator) RunOnce(ctx context.Context) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{}, err
	}
	head, err := c.Chain.Head()
	if err != nil {
		return Result{}, err
	}
	root, scope, err := c.openScope(head)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	lock, err := filelock.Acquire(filepath.Join(c.scopePath(head), ".roster-renewal.lock"), 30*time.Second)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if journal, readErr := readRenewalJournal(root); readErr == nil {
		verified, applyErr := c.apply(ctx, root, scope, journal)
		return Result{Renewed: applyErr == nil, RosterHash: verified.Hash}, applyErr
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return Result{}, ErrPendingRenewal
	}
	now := c.now()
	manifest := head.Manifest.Manifest
	if now.Unix() >= manifest.NotAfterUnix {
		return Result{RosterHash: head.Hash}, ErrRosterExpired
	}
	credentialDue := false
	credentialAt := time.Unix(manifest.NotAfterUnix, 0)
	for _, signed := range manifest.Devices {
		deadline := time.Unix(signed.Certificate.NotAfterUnix, 0).Add(-c.Policy.CredentialRenewBefore)
		if deadline.Before(credentialAt) {
			credentialAt = deadline
		}
		if !now.Before(deadline) {
			credentialDue = true
		}
	}
	freshnessAt := time.Unix(manifest.IssuedAtUnix, 0).Add(c.Policy.RenewAfter)
	if !credentialDue && now.Before(freshnessAt) {
		if credentialAt.Before(freshnessAt) {
			freshnessAt = credentialAt
		}
		return Result{RosterHash: head.Hash, NextTry: freshnessAt}, nil
	}
	var next identity.RosterManifestV1
	if credentialDue {
		if c.Credentials == nil {
			return Result{RosterHash: head.Hash, NextTry: now.Add(c.Policy.RetryInterval)}, ErrCredentialRenewalUnavailable
		}
		next, err = c.Credentials.RenewCredentials(ctx, head, now)
		if err != nil {
			return Result{RosterHash: head.Hash, NextTry: now.Add(c.Policy.RetryInterval)}, err
		}
		verified, verifyErr := identity.VerifyTransition(head, head.Authority, next)
		if verifyErr != nil || verified.Manifest.Manifest.AccessGeneration != manifest.AccessGeneration || verified.Manifest.Manifest.AccessSetHash != manifest.AccessSetHash ||
			verified.Manifest.Manifest.NotAfterUnix <= manifest.NotAfterUnix || !dueCredentialsExtended(manifest, verified.Manifest.Manifest, now, c.Policy.CredentialRenewBefore) {
			return Result{}, ErrCredentialRenewalUnavailable
		}
	} else {
		if c.Collector == nil {
			return Result{RosterHash: head.Hash, NextTry: now.Add(c.Policy.RetryInterval)}, identity.ErrFreshnessAuthorityUnavailable
		}
		proposal, prepareErr := identity.PrepareFreshnessRenewal(head, now)
		if prepareErr != nil {
			return Result{}, prepareErr
		}
		selected, endorsements, collectErr := c.Collector.CollectFreshnessEndorsements(ctx, head, proposal)
		if collectErr != nil {
			return Result{RosterHash: head.Hash, NextTry: now.Add(c.Policy.RetryInterval)}, collectErr
		}
		next, _, err = identity.FinalizeFreshnessRenewal(head, selected, endorsements)
		if err != nil {
			return Result{}, err
		}
	}
	currentTuple, err := c.security().CurrentForRecovery(scope)
	if err != nil {
		return Result{}, err
	}
	currentFile, err := readEpochRecord(root)
	if err != nil || currentFile.RosterHash != [32]byte(head.Hash) || currentFile.AccessGeneration != manifest.AccessGeneration ||
		currentFile.AccessSetHash != manifest.AccessSetHash || currentFile.CoordinatorGeneration != currentTuple.CoordinatorGeneration ||
		currentFile.BarrierID != currentTuple.BarrierID || currentFile.KeyMode != currentTuple.KeyMode || currentFile.KeyVersion != currentTuple.KeyVersion {
		return Result{}, securityerr.ErrMetadataMismatch
	}
	nextHash, err := identity.HashRoster(next)
	if err != nil || currentFile.CoordinatorGeneration == ^uint64(0) {
		return Result{}, ErrPendingRenewal
	}
	nextFile := currentFile
	nextFile.RosterHash = [32]byte(nextHash)
	nextFile.CoordinatorGeneration++
	journal := renewalJournal{Version: 1, PreviousRosterHash: [32]byte(head.Hash), NextRoster: next, NextEpoch: nextFile}
	encoded, err := encodeRenewalJournal(journal)
	if err != nil {
		return Result{}, err
	}
	if err := createRenewalJournal(root, encoded); err != nil {
		return Result{}, err
	}
	if err := renewalBoundary(ctx, c.hooks.afterJournal); err != nil {
		return Result{}, err
	}
	persisted, err := readRenewalJournal(root)
	if err != nil {
		return Result{}, err
	}
	verified, err := c.apply(ctx, root, scope, persisted)
	return Result{Renewed: err == nil, RosterHash: verified.Hash, NextTry: time.Unix(verified.Manifest.Manifest.IssuedAtUnix, 0).Add(c.Policy.RenewAfter)}, err
}

func (c *Coordinator) Run(ctx context.Context) error {
	for {
		result, err := c.RunOnce(ctx)
		if errors.Is(err, ErrRosterExpired) {
			return err
		}
		wait := c.Policy.RetryInterval
		if err == nil && !result.NextTry.IsZero() {
			wait = time.Until(result.NextTry)
			if wait < 0 {
				wait = 0
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Coordinator) apply(ctx context.Context, root *privatefs.Root, scope string, journal renewalJournal) (identity.VerifiedRoster, error) {
	previous, err := c.Chain.VerifiedByHash(identity.RosterHash(journal.PreviousRosterHash))
	if err != nil {
		return identity.VerifiedRoster{}, err
	}
	next, err := identity.VerifyTransition(previous, previous.Authority, journal.NextRoster)
	if err != nil || next.Manifest.Manifest.AccessGeneration != previous.Manifest.Manifest.AccessGeneration || next.Manifest.Manifest.AccessSetHash != previous.Manifest.Manifest.AccessSetHash ||
		journal.NextEpoch.RosterHash != [32]byte(next.Hash) || journal.NextEpoch.AccessGeneration != next.Manifest.Manifest.AccessGeneration ||
		journal.NextEpoch.AccessSetHash != next.Manifest.Manifest.AccessSetHash {
		return identity.VerifiedRoster{}, ErrPendingRenewal
	}
	nextTuple := securityepoch.SecurityEpoch{CoordinatorGeneration: journal.NextEpoch.CoordinatorGeneration, AccessGeneration: journal.NextEpoch.AccessGeneration,
		AccessSetHash: journal.NextEpoch.AccessSetHash, BarrierID: journal.NextEpoch.BarrierID, KeyMode: journal.NextEpoch.KeyMode, KeyVersion: journal.NextEpoch.KeyVersion}
	err = c.security().Transition(ctx, scope, nextTuple, func() error {
		if _, err := c.Chain.AppendRosterExact(journal.NextRoster); err != nil {
			return err
		}
		if err := renewalBoundary(ctx, c.hooks.afterChain); err != nil {
			return err
		}
		raw, err := json.Marshal(journal.NextEpoch)
		if err != nil {
			return err
		}
		return root.WriteFile("security-epoch.json", raw, privatefs.FilePolicy{RejectWritableByOthers: true})
	})
	if err != nil {
		return identity.VerifiedRoster{}, err
	}
	if err := renewalBoundary(ctx, c.hooks.afterEpoch); err != nil {
		return identity.VerifiedRoster{}, err
	}
	head, err := c.Chain.Head()
	if err != nil || head.Hash != next.Hash || c.security().VerifyCurrent(scope, nextTuple) != nil {
		return identity.VerifiedRoster{}, ErrPendingRenewal
	}
	if err := root.RemoveRegular(securityepoch.TransitionJournalFilename); err != nil {
		return identity.VerifiedRoster{}, err
	}
	return head, nil
}

func (c *Coordinator) validate() error {
	if c == nil || !filepath.IsAbs(c.IdentityRoot) || filepath.Clean(c.IdentityRoot) != c.IdentityRoot || c.Chain == nil ||
		c.Policy.RenewAfter <= 0 || c.Policy.RetryInterval <= 0 || c.Policy.CredentialRenewBefore <= 0 {
		return ErrInvalidPolicy
	}
	return nil
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

func (c *Coordinator) security() *securityepoch.Coordinator {
	if c.Security != nil {
		return c.Security
	}
	return &securityepoch.Coordinator{Root: c.IdentityRoot}
}

func (c *Coordinator) scopePath(roster identity.VerifiedRoster) string {
	if roster.Manifest.Manifest.ScopeType == "account" {
		return filepath.Join(c.IdentityRoot, "account")
	}
	return filepath.Join(c.IdentityRoot, "namespaces", roster.Manifest.Manifest.ScopeID)
}

func (c *Coordinator) openScope(roster identity.VerifiedRoster) (*privatefs.Root, string, error) {
	scope := roster.Manifest.Manifest.ScopeID
	if roster.Manifest.Manifest.ScopeType == "account" {
		scope = "account"
	} else if roster.Manifest.Manifest.ScopeType != "namespace" {
		return nil, "", ErrPendingRenewal
	}
	root, err := privatefs.OpenRoot(c.scopePath(roster), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	return root, scope, err
}

func readEpochRecord(root *privatefs.Root) (epochRecord, error) {
	raw, err := readRenewalFile(root, "security-epoch.json", 64<<10)
	if err != nil {
		return epochRecord{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record epochRecord
	if decoder.Decode(&record) != nil || decoder.Decode(&struct{}{}) != io.EOF || record.Version != 1 || record.RosterHash == ([32]byte{}) ||
		record.AccessGeneration == 0 || record.AccessSetHash == ([32]byte{}) || record.BarrierID == ([32]byte{}) || record.TreeHeadDigest == ([32]byte{}) || record.CoordinatorGeneration == 0 {
		return epochRecord{}, securityerr.ErrMetadataMismatch
	}
	return record, nil
}

func renewalChecksum(journal renewalJournal) ([32]byte, error) {
	journal.Checksum = [32]byte{}
	raw, err := json.Marshal(journal)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("aplexica/roster-renewal-journal/v1\x00"), raw...)), nil
}

func encodeRenewalJournal(journal renewalJournal) ([]byte, error) {
	if journal.Version != 1 || journal.PreviousRosterHash == ([32]byte{}) {
		return nil, ErrPendingRenewal
	}
	var err error
	journal.Checksum, err = renewalChecksum(journal)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(journal)
	if err != nil || len(raw) > 8<<20 {
		return nil, securityerr.ErrLimitExceeded
	}
	return raw, nil
}

func readRenewalJournal(root *privatefs.Root) (renewalJournal, error) {
	raw, err := readRenewalFile(root, securityepoch.TransitionJournalFilename, 8<<20)
	if err != nil {
		return renewalJournal{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var journal renewalJournal
	if decoder.Decode(&journal) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return renewalJournal{}, ErrPendingRenewal
	}
	want, err := renewalChecksum(journal)
	canonical, marshalErr := json.Marshal(journal)
	if err != nil || marshalErr != nil || journal.Version != 1 || want != journal.Checksum || !bytes.Equal(canonical, raw) {
		return renewalJournal{}, ErrPendingRenewal
	}
	return journal, nil
}

func readRenewalFile(root *privatefs.Root, name string, maximum int64) ([]byte, error) {
	file, err := root.OpenReadRegular(name)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(raw)) > maximum {
		return nil, securityerr.ErrLimitExceeded
	}
	return raw, nil
}

func createRenewalJournal(root *privatefs.Root, raw []byte) error {
	file, err := root.CreateExclusive(securityepoch.TransitionJournalFilename, privatefs.FilePolicy{RejectWritableByOthers: true})
	if err != nil {
		return ErrPendingRenewal
	}
	written, writeErr := file.Write(raw)
	if writeErr == nil && written != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	return root.SyncDir(".")
}

func dueCredentialsExtended(previous, next identity.RosterManifestUnsignedV1, now time.Time, lead time.Duration) bool {
	nextByDevice := make(map[string]identity.DeviceCertificateUnsignedV1, len(next.Devices))
	for _, signed := range next.Devices {
		nextByDevice[signed.Certificate.DeviceID] = signed.Certificate
	}
	for _, signed := range previous.Devices {
		credential := signed.Certificate
		if now.Before(time.Unix(credential.NotAfterUnix, 0).Add(-lead)) {
			continue
		}
		renewed, ok := nextByDevice[credential.DeviceID]
		if !ok || renewed.NotAfterUnix <= credential.NotAfterUnix {
			return false
		}
	}
	return true
}

func renewalBoundary(ctx context.Context, hook func() error) error {
	if ctx == nil {
		return ErrPendingRenewal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		return hook()
	}
	return nil
}

func (j renewalJournal) String() string { return fmt.Sprintf("renewal %x", j.PreviousRosterHash) }
