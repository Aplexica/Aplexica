package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/securityepoch"
)

const (
	remoteRescanSlotBytes     = 4096
	remoteRescanFileBytes     = 2 * remoteRescanSlotBytes
	rescanReasonCapacity      = 1
	rescanReasonCheckpoint    = 2
	rescanReasonAccessCutover = 4
	remoteRescanKeyModeMax    = 64
	remoteRescanArtifactMax   = 256
	remoteRescanBranchMax     = 128
	remoteRescanEventIDMax    = 512
)

type RemoteRescanMarkerV1 struct {
	Version                  uint16
	ScopeID                  string
	State                    string
	ReasonFlags              uint32
	MutationGeneration       uint64
	CompletedGeneration      uint64
	TargetAccessGeneration   uint64
	TargetAccessSetHash      [32]byte
	TargetSecurityBarrierID  [32]byte
	TargetSecurityGeneration uint64
	TargetKeyMode            string
	TargetKeyVersion         uint64
	TargetArtifactID         string
	TargetBranchID           string
	TargetEventID            string
	TargetEventHash          [32]byte
	RecordSequence           uint64
	Checksum                 [32]byte
}

// RemoteRescanSnapshot is the content-free, generation-bound recovery work
// visible to the outbound recovery worker. Marker paths are deliberately not
// exposed: scope IDs are authenticated marker payload, never path material.
type RemoteRescanSnapshot struct {
	Marker RemoteRescanMarkerV1
}

type RemoteMutationCoordinator struct{ Root string }

type RemoteMutation struct {
	coordinator *RemoteMutationCoordinator
	lock        *filelock.Lock
	path        string
	marker      RemoteRescanMarkerV1
	slot        int
	wasDirty    bool
}

func markerScope(eventScope string) (string, error) {
	if eventScope == "" {
		return "account", nil
	}
	if len(eventScope) > 256 || strings.ContainsAny(eventScope, "\x00\r\n") {
		return "", fmt.Errorf("remote rescan: invalid scope")
	}
	return eventScope, nil
}

func markerBase(scope string) string {
	d := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("%x.marker", d[:])
}

func validMarkerOpaque(value string, max int) bool {
	if len(value) > max || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	return true
}

func markerHasAnyGeneration(m RemoteRescanMarkerV1) bool {
	return m.TargetAccessGeneration != 0 || m.TargetAccessSetHash != ([sha256.Size]byte{}) ||
		m.TargetSecurityGeneration != 0 || m.TargetSecurityBarrierID != ([sha256.Size]byte{}) ||
		m.TargetKeyMode != "" || m.TargetKeyVersion != 0
}

func validMarkerRecoveryGeneration(m RemoteRescanMarkerV1) bool {
	if !markerHasAnyGeneration(m) {
		return true
	}
	return m.TargetAccessGeneration > 0 && m.TargetAccessSetHash != ([sha256.Size]byte{}) &&
		m.TargetSecurityGeneration > 0 && m.TargetSecurityBarrierID != ([sha256.Size]byte{}) &&
		validRecoveryKeyModeVersion(m.TargetKeyMode, m.TargetKeyVersion)
}

func clearMarkerRecoveryGeneration(m *RemoteRescanMarkerV1) {
	m.TargetAccessGeneration = 0
	m.TargetAccessSetHash = [sha256.Size]byte{}
	m.TargetSecurityGeneration = 0
	m.TargetSecurityBarrierID = [sha256.Size]byte{}
	m.TargetKeyMode = ""
	m.TargetKeyVersion = 0
}

func encodeMarker(m RemoteRescanMarkerV1) ([]byte, error) {
	if m.Version != 1 || len(m.ScopeID) == 0 || len(m.ScopeID) > 256 || (m.State != "clean" && m.State != "dirty") || m.RecordSequence == 0 ||
		!validMarkerRecoveryGeneration(m) ||
		!validMarkerOpaque(m.TargetKeyMode, remoteRescanKeyModeMax) ||
		!validMarkerOpaque(m.TargetArtifactID, remoteRescanArtifactMax) ||
		!validMarkerOpaque(m.TargetBranchID, remoteRescanBranchMax) ||
		!validMarkerOpaque(m.TargetEventID, remoteRescanEventIDMax) {
		return nil, fmt.Errorf("remote rescan: invalid marker")
	}
	b := make([]byte, remoteRescanSlotBytes)
	copy(b[:8], []byte("APXRSCN1"))
	binary.BigEndian.PutUint16(b[8:10], m.Version)
	if m.State == "dirty" {
		b[10] = 1
	}
	binary.BigEndian.PutUint16(b[12:14], uint16(len(m.ScopeID)))
	copy(b[14:270], m.ScopeID)
	binary.BigEndian.PutUint32(b[270:274], m.ReasonFlags)
	binary.BigEndian.PutUint64(b[274:282], m.MutationGeneration)
	binary.BigEndian.PutUint64(b[282:290], m.CompletedGeneration)
	binary.BigEndian.PutUint64(b[290:298], m.TargetAccessGeneration)
	copy(b[298:330], m.TargetAccessSetHash[:])
	copy(b[330:362], m.TargetSecurityBarrierID[:])
	binary.BigEndian.PutUint64(b[362:370], m.TargetKeyVersion)
	binary.BigEndian.PutUint64(b[370:378], m.RecordSequence)
	binary.BigEndian.PutUint64(b[378:386], m.TargetSecurityGeneration)
	binary.BigEndian.PutUint16(b[386:388], uint16(len(m.TargetKeyMode)))
	copy(b[388:452], m.TargetKeyMode)
	binary.BigEndian.PutUint16(b[452:454], uint16(len(m.TargetArtifactID)))
	copy(b[454:710], m.TargetArtifactID)
	binary.BigEndian.PutUint16(b[710:712], uint16(len(m.TargetBranchID)))
	copy(b[712:840], m.TargetBranchID)
	binary.BigEndian.PutUint16(b[840:842], uint16(len(m.TargetEventID)))
	copy(b[842:1354], m.TargetEventID)
	copy(b[1354:1386], m.TargetEventHash[:])
	domain := []byte("aplexica/remote-rescan-marker/v1\x00")
	sum := sha256.Sum256(append(domain, b[:remoteRescanSlotBytes-32]...))
	copy(b[remoteRescanSlotBytes-32:], sum[:])
	return b, nil
}

func decodeMarker(b []byte) (RemoteRescanMarkerV1, error) {
	if len(b) != remoteRescanSlotBytes || !bytes.Equal(b[:8], []byte("APXRSCN1")) {
		return RemoteRescanMarkerV1{}, errors.New("remote rescan: invalid slot")
	}
	want := sha256.Sum256(append([]byte("aplexica/remote-rescan-marker/v1\x00"), b[:remoteRescanSlotBytes-32]...))
	if !bytes.Equal(want[:], b[remoteRescanSlotBytes-32:]) {
		return RemoteRescanMarkerV1{}, errors.New("remote rescan: checksum mismatch")
	}
	n := binary.BigEndian.Uint16(b[12:14])
	if n == 0 || n > 256 {
		return RemoteRescanMarkerV1{}, errors.New("remote rescan: invalid scope")
	}
	keyModeLen := binary.BigEndian.Uint16(b[386:388])
	artifactLen := binary.BigEndian.Uint16(b[452:454])
	branchLen := binary.BigEndian.Uint16(b[710:712])
	eventIDLen := binary.BigEndian.Uint16(b[840:842])
	if keyModeLen > remoteRescanKeyModeMax || artifactLen > remoteRescanArtifactMax || branchLen > remoteRescanBranchMax || eventIDLen > remoteRescanEventIDMax {
		return RemoteRescanMarkerV1{}, errors.New("remote rescan: invalid target identity")
	}
	m := RemoteRescanMarkerV1{
		Version: binary.BigEndian.Uint16(b[8:10]), ScopeID: string(b[14 : 14+n]), State: "clean",
		ReasonFlags: binary.BigEndian.Uint32(b[270:274]), MutationGeneration: binary.BigEndian.Uint64(b[274:282]),
		CompletedGeneration: binary.BigEndian.Uint64(b[282:290]), TargetAccessGeneration: binary.BigEndian.Uint64(b[290:298]),
		TargetKeyVersion: binary.BigEndian.Uint64(b[362:370]), RecordSequence: binary.BigEndian.Uint64(b[370:378]),
		TargetSecurityGeneration: binary.BigEndian.Uint64(b[378:386]), TargetKeyMode: string(b[388 : 388+keyModeLen]),
		TargetArtifactID: string(b[454 : 454+artifactLen]), TargetBranchID: string(b[712 : 712+branchLen]),
		TargetEventID: string(b[842 : 842+eventIDLen]),
	}
	if b[10] == 1 {
		m.State = "dirty"
	} else if b[10] != 0 {
		return RemoteRescanMarkerV1{}, errors.New("remote rescan: invalid state")
	}
	copy(m.TargetAccessSetHash[:], b[298:330])
	copy(m.TargetSecurityBarrierID[:], b[330:362])
	copy(m.TargetEventHash[:], b[1354:1386])
	copy(m.Checksum[:], b[remoteRescanSlotBytes-32:])
	if !validMarkerRecoveryGeneration(m) {
		// Pre-extension schema-v1 markers carried access/barrier/key-version
		// hints but had no security generation, key mode, or canonical target
		// fields. Keep those files readable while explicitly stripping their
		// incomplete tuple so they can never become recovery authority.
		legacyV1 := m.TargetSecurityGeneration == 0 && m.TargetKeyMode == "" &&
			m.TargetArtifactID == "" && m.TargetEventID == ""
		if !legacyV1 {
			return RemoteRescanMarkerV1{}, errors.New("remote rescan: invalid recovery generation")
		}
		clearMarkerRecoveryGeneration(&m)
	}
	if _, err := encodeMarker(m); err != nil {
		return RemoteRescanMarkerV1{}, err
	}
	return m, nil
}

func loadMarker(path, scope string) (RemoteRescanMarkerV1, int, error) {
	m, slot, err := loadMarkerAny(path)
	if err != nil {
		return RemoteRescanMarkerV1{}, 0, err
	}
	if m.ScopeID != scope {
		return RemoteRescanMarkerV1{}, 0, fmt.Errorf("remote rescan: scope mismatch")
	}
	return m, slot, nil
}

func loadMarkerAny(path string) (RemoteRescanMarkerV1, int, error) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) != remoteRescanFileBytes {
		return RemoteRescanMarkerV1{}, 0, fmt.Errorf("remote rescan: corrupt marker")
	}
	a, ea := decodeMarker(b[:remoteRescanSlotBytes])
	c, ec := decodeMarker(b[remoteRescanSlotBytes:])
	if ea != nil && ec != nil {
		return RemoteRescanMarkerV1{}, 0, fmt.Errorf("remote rescan: both slots invalid")
	}
	if ea == nil && (ec != nil || a.RecordSequence > c.RecordSequence) {
		return a, 0, nil
	}
	if ec == nil && (ea != nil || c.RecordSequence > a.RecordSequence) {
		return c, 1, nil
	}
	if a != c {
		return RemoteRescanMarkerV1{}, 0, fmt.Errorf("remote rescan: equivocal slots")
	}
	return a, 0, nil
}

func ensureMarker(path, scope string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Truncate(remoteRescanFileBytes); err != nil {
		return err
	}
	b, err := encodeMarker(RemoteRescanMarkerV1{Version: 1, ScopeID: scope, State: "clean", RecordSequence: 1})
	if err == nil {
		_, err = f.WriteAt(b, 0)
	}
	if err == nil {
		err = f.Sync()
	}
	return err
}

func (c *RemoteMutationCoordinator) Begin(scope string, event outboxEntry) (*RemoteMutation, error) {
	scope, err := markerScope(scope)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(c.Root, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(c.Root, markerBase(scope))
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return nil, err
	}
	if err = ensureMarker(path, scope); err != nil {
		_ = lock.Close()
		return nil, err
	}
	m, slot, err := loadMarker(path, scope)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	wasDirty := m.State == "dirty"
	m.State = "dirty"
	m.ReasonFlags |= rescanReasonCapacity
	m.MutationGeneration++
	// Once a scope is dirty, its first missing/in-flight identity is the lower
	// bound of recovery. A later successful append must never overwrite that
	// authority or clean the older obligation.
	if !wasDirty {
		m.TargetAccessGeneration = event.Event.AccessGeneration
		m.TargetAccessSetHash = event.Event.AccessSetHash
		m.TargetSecurityBarrierID = event.Event.SecurityBarrierID
		m.TargetSecurityGeneration = event.Event.SecurityGeneration
		m.TargetKeyMode = event.Event.KeyMode
		m.TargetKeyVersion = event.Event.KeyVersion
		if !validMarkerRecoveryGeneration(m) {
			// Keep the canonical target durable even when a producer supplies a
			// partial or unknown security tuple. A zero tuple is explicitly not
			// recovery authority and will become a checkpoint obligation.
			clearMarkerRecoveryGeneration(&m)
		}
		m.TargetArtifactID = event.Event.ArtifactID
		m.TargetBranchID = event.Event.BranchID
		if m.TargetBranchID == "" {
			m.TargetBranchID = "main"
		}
		m.TargetEventID = event.Event.EventID
		m.TargetEventHash = [32]byte{}
		if decoded, decodeErr := hex.DecodeString(event.Event.EventHash); decodeErr == nil && len(decoded) == sha256.Size {
			copy(m.TargetEventHash[:], decoded)
		}
	}
	m.RecordSequence++
	next := 1 - slot
	if err = writeMarkerSlot(path, next, m); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &RemoteMutation{coordinator: c, lock: lock, path: path, marker: m, slot: next, wasDirty: wasDirty}, nil
}

func writeMarkerSlot(path string, slot int, marker RemoteRescanMarkerV1) error {
	b, err := encodeMarker(marker)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteAt(b, int64(slot*remoteRescanSlotBytes)); err != nil {
		return err
	}
	return f.Sync()
}

func (m *RemoteMutation) Complete() error {
	if m == nil || m.lock == nil {
		return errors.New("remote rescan: transaction closed")
	}
	if !m.wasDirty {
		m.marker.State = "clean"
		m.marker.ReasonFlags = 0
		m.marker.CompletedGeneration = m.marker.MutationGeneration
	}
	m.marker.RecordSequence++
	err := writeMarkerSlot(m.path, 1-m.slot, m.marker)
	closeErr := m.lock.Close()
	m.lock = nil
	if err == nil {
		err = closeErr
	}
	return err
}

// ListDirty returns every authenticated dirty marker in stable scope order.
// Corruption is fail-closed: callers receive an error and must not clear any
// recovery work based on a partial directory view.
func (c *RemoteMutationCoordinator) ListDirty() ([]RemoteRescanSnapshot, error) {
	if c == nil || c.Root == "" {
		return nil, errors.New("remote rescan: coordinator unavailable")
	}
	entries, err := os.ReadDir(c.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("remote rescan: list markers: %w", err)
	}
	result := make([]RemoteRescanSnapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".marker" {
			continue
		}
		path := filepath.Join(c.Root, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != remoteRescanFileBytes {
			if statErr != nil {
				return nil, fmt.Errorf("remote rescan: inspect marker: %w", statErr)
			}
			return nil, errors.New("remote rescan: unsafe marker entry")
		}
		marker, _, loadErr := loadMarkerAny(path)
		if loadErr != nil {
			return nil, loadErr
		}
		if markerBase(marker.ScopeID) != entry.Name() {
			return nil, errors.New("remote rescan: marker filename binding mismatch")
		}
		if marker.State == "dirty" {
			result = append(result, RemoteRescanSnapshot{Marker: marker})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Marker.ScopeID < result[j].Marker.ScopeID })
	return result, nil
}

// IsDirty reports the current authenticated scope state. It is used to keep
// newer work off the live queue while canonical recovery owns ordering.
func (c *RemoteMutationCoordinator) IsDirty(scope string) (bool, error) {
	scope, err := markerScope(scope)
	if err != nil {
		return false, err
	}
	path := filepath.Join(c.Root, markerBase(scope))
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	marker, _, err := loadMarker(path, scope)
	return marker.State == "dirty", err
}

// Snapshot returns the authenticated marker for one scope. It is used after a
// crash between checkpoint-marker completion and obligation-file removal: a
// clean marker whose CompletedGeneration covers the committed obligation is
// sufficient to finish deleting that content-free obligation.
func (c *RemoteMutationCoordinator) Snapshot(scope string) (RemoteRescanMarkerV1, bool, error) {
	scope, err := markerScope(scope)
	if err != nil {
		return RemoteRescanMarkerV1{}, false, err
	}
	path := filepath.Join(c.Root, markerBase(scope))
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return RemoteRescanMarkerV1{}, false, nil
	} else if err != nil {
		return RemoteRescanMarkerV1{}, false, err
	}
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return RemoteRescanMarkerV1{}, false, err
	}
	defer lock.Close()
	marker, _, err := loadMarker(path, scope)
	return marker, err == nil, err
}

// RequireCheckpoint records that delta replay cannot safely fill this scope.
// It advances MutationGeneration so a recovery pass holding the prior value
// can never race the obligation write and clear it.
func (c *RemoteMutationCoordinator) RequireCheckpoint(scope string) error {
	scope, err := markerScope(scope)
	if err != nil {
		return err
	}
	path := filepath.Join(c.Root, markerBase(scope))
	if err = os.MkdirAll(c.Root, 0o700); err != nil {
		return err
	}
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = ensureMarker(path, scope); err != nil {
		return err
	}
	marker, slot, err := loadMarker(path, scope)
	if err != nil {
		return err
	}
	if marker.State == "dirty" && marker.ReasonFlags&rescanReasonCheckpoint != 0 {
		return nil
	}
	marker.State = "dirty"
	marker.ReasonFlags |= rescanReasonCheckpoint
	marker.MutationGeneration++
	marker.RecordSequence++
	return writeMarkerSlot(path, 1-slot, marker)
}

// RequireSecurityCutover establishes the generation-bound full canonical
// sweep before old cached ciphertext is removed. Exact retries are idempotent;
// a different tuple always advances the marker generation, so an in-flight
// recovery pass can never clear work for the new access epoch.
func (c *RemoteMutationCoordinator) RequireSecurityCutover(scope string, next securityepoch.SecurityEpoch) error {
	scope, err := markerScope(scope)
	if err != nil || next.CoordinatorGeneration == 0 || next.AccessGeneration == 0 || next.AccessSetHash == ([32]byte{}) ||
		next.BarrierID == ([32]byte{}) || !validRecoveryKeyModeVersion(next.KeyMode, next.KeyVersion) {
		return fmt.Errorf("remote rescan: invalid security cutover")
	}
	if err = os.MkdirAll(c.Root, 0o700); err != nil {
		return err
	}
	path := filepath.Join(c.Root, markerBase(scope))
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = ensureMarker(path, scope); err != nil {
		return err
	}
	marker, slot, err := loadMarker(path, scope)
	if err != nil {
		return err
	}
	if marker.State == "dirty" && marker.ReasonFlags&rescanReasonAccessCutover != 0 &&
		marker.TargetAccessGeneration == next.AccessGeneration && marker.TargetAccessSetHash == next.AccessSetHash &&
		marker.TargetSecurityGeneration == next.CoordinatorGeneration && marker.TargetSecurityBarrierID == next.BarrierID &&
		marker.TargetKeyMode == next.KeyMode && marker.TargetKeyVersion == next.KeyVersion {
		return nil
	}
	marker.State = "dirty"
	marker.ReasonFlags |= rescanReasonAccessCutover
	marker.MutationGeneration++
	marker.TargetAccessGeneration = next.AccessGeneration
	marker.TargetAccessSetHash = next.AccessSetHash
	marker.TargetSecurityGeneration = next.CoordinatorGeneration
	marker.TargetSecurityBarrierID = next.BarrierID
	marker.TargetKeyMode = next.KeyMode
	marker.TargetKeyVersion = next.KeyVersion
	// A cutover always scans the complete canonical scope; a prior capacity
	// marker's single-event lower bound must not narrow it.
	marker.TargetArtifactID = ""
	marker.TargetBranchID = ""
	marker.TargetEventID = ""
	marker.TargetEventHash = [32]byte{}
	marker.RecordSequence++
	return writeMarkerSlot(path, 1-slot, marker)
}

// CompleteRecovery clears only the exact dirty generation the caller scanned.
// A concurrent canonical commit or append attempt increments the generation,
// leaves the marker dirty, and forces a fresh proof pass.
func (c *RemoteMutationCoordinator) CompleteRecovery(scope string, observedGeneration uint64) (bool, error) {
	scope, err := markerScope(scope)
	if err != nil || observedGeneration == 0 {
		return false, errors.New("remote rescan: invalid recovery completion")
	}
	path := filepath.Join(c.Root, markerBase(scope))
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	marker, slot, err := loadMarker(path, scope)
	if err != nil {
		return false, err
	}
	if marker.State == "clean" {
		return marker.CompletedGeneration >= observedGeneration, nil
	}
	if marker.ReasonFlags&rescanReasonCheckpoint != 0 {
		return false, nil
	}
	if marker.MutationGeneration != observedGeneration {
		return false, nil
	}
	marker.State = "clean"
	marker.ReasonFlags = 0
	marker.CompletedGeneration = marker.MutationGeneration
	marker.RecordSequence++
	if err := writeMarkerSlot(path, 1-slot, marker); err != nil {
		return false, err
	}
	return true, nil
}

// SatisfyCheckpoint removes only the checkpoint barrier for the exact marker
// generation. The underlying capacity/rescan reason remains dirty so ordinary
// canonical recovery can prove the newly installed checkpoint watermark and
// clear the marker through CompleteRecovery. This prevents one artifact's
// checkpoint from accidentally hiding unrelated missing work in the scope.
func (c *RemoteMutationCoordinator) SatisfyCheckpoint(scope string, observedGeneration uint64) (bool, error) {
	scope, err := markerScope(scope)
	if err != nil || observedGeneration == 0 {
		return false, errors.New("remote rescan: invalid checkpoint satisfaction")
	}
	path := filepath.Join(c.Root, markerBase(scope))
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	marker, slot, err := loadMarker(path, scope)
	if err != nil {
		return false, err
	}
	if marker.State != "dirty" || marker.MutationGeneration != observedGeneration || marker.ReasonFlags&rescanReasonCheckpoint == 0 {
		return false, nil
	}
	marker.ReasonFlags &^= rescanReasonCheckpoint
	marker.RecordSequence++
	if err := writeMarkerSlot(path, 1-slot, marker); err != nil {
		return false, err
	}
	return true, nil
}

// BindCheckpointTarget replaces the marker's original transport failure
// identity with the exact canonical head proven by a verified checkpoint. A
// retained wire EventID is intentionally opaque and may not itself exist in
// the canonical log; ordinary range recovery needs this canonical binding
// after the checkpoint barrier is removed.
func (c *RemoteMutationCoordinator) BindCheckpointTarget(value RemoteCheckpointObligationV1) (bool, error) {
	if value.ScopeID == "" || value.MarkerGeneration == 0 || value.CheckpointState != "verified" ||
		value.ArtifactID == "" || value.BranchID == "" || value.CheckpointHeadEventID == "" ||
		!validateWatermarkDigest(value.CheckpointHeadHash) || value.AccessGeneration == 0 ||
		!validateWatermarkDigest(value.AccessSetHash) || value.SecurityGeneration == 0 ||
		!validateWatermarkDigest(value.SecurityBarrier) || !validRecoveryKeyModeVersion(value.KeyMode, value.KeyVersion) {
		return false, errors.New("remote rescan: invalid checkpoint target")
	}
	scope, err := markerScope(value.ScopeID)
	if err != nil {
		return false, err
	}
	path := filepath.Join(c.Root, markerBase(scope))
	lock, err := filelock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	marker, slot, err := loadMarker(path, scope)
	if err != nil {
		return false, err
	}
	if marker.State != "dirty" || marker.MutationGeneration != value.MarkerGeneration || marker.ReasonFlags&rescanReasonCheckpoint == 0 {
		return false, nil
	}
	accessHash, accessOK := decodeRecoveryDigest(value.AccessSetHash)
	barrier, barrierOK := decodeRecoveryDigest(value.SecurityBarrier)
	headHash, headOK := decodeRecoveryDigest(value.CheckpointHeadHash)
	if !accessOK || !barrierOK || !headOK {
		return false, errors.New("remote rescan: invalid checkpoint target digest")
	}
	marker.TargetAccessGeneration = value.AccessGeneration
	marker.TargetAccessSetHash = accessHash
	marker.TargetSecurityGeneration = value.SecurityGeneration
	marker.TargetSecurityBarrierID = barrier
	marker.TargetKeyMode = value.KeyMode
	marker.TargetKeyVersion = value.KeyVersion
	marker.TargetArtifactID = value.ArtifactID
	marker.TargetBranchID = value.BranchID
	marker.TargetEventID = value.CheckpointHeadEventID
	marker.TargetEventHash = headHash
	marker.RecordSequence++
	if err := writeMarkerSlot(path, 1-slot, marker); err != nil {
		return false, err
	}
	return true, nil
}

func (m *RemoteMutation) Close() error {
	if m == nil || m.lock == nil {
		return nil
	}
	err := m.lock.Close()
	m.lock = nil
	return err
}
