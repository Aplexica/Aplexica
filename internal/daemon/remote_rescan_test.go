package daemon

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/stretchr/testify/require"
)

func TestRemoteRescanMarkerAlternatesAndRetainsDirtyObligation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "markers")
	c := &RemoteMutationCoordinator{Root: root}
	epoch := sha256.Sum256([]byte("access"))
	barrier := sha256.Sum256([]byte("barrier"))
	entry := outboxEntry{Event: proto.RemoteEvent{
		AccessGeneration: 4, AccessSetHash: epoch,
		SecurityGeneration: 2, SecurityBarrierID: barrier,
		KeyMode: "namespace-key-v1", KeyVersion: 7,
	}}

	tx, err := c.Begin("account", entry)
	if err != nil {
		t.Fatal(err)
	}
	path := tx.path
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	} // crash before intent persistence
	m, _, err := loadMarker(path, "account")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "dirty" || m.MutationGeneration != 1 || m.CompletedGeneration != 0 || m.TargetSecurityBarrierID != barrier {
		t.Fatalf("lost rescan obligation: %+v", m)
	}

	tx, err = c.Begin("account", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Complete(); err != nil {
		t.Fatal(err)
	}
	m, _, err = loadMarker(path, "account")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "dirty" || m.CompletedGeneration != 0 || m.MutationGeneration != 2 || m.ReasonFlags != rescanReasonCapacity {
		t.Fatalf("later success erased prior dirty generation: %+v", m)
	}
	completed, err := c.CompleteRecovery("account", m.MutationGeneration)
	if err != nil || !completed {
		t.Fatalf("CompleteRecovery = %v, %v", completed, err)
	}
	m, _, err = loadMarker(path, "account")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "clean" || m.CompletedGeneration != m.MutationGeneration {
		t.Fatalf("exact recovery CAS did not clean marker: %+v", m)
	}
}

func TestRemoteRescanMarkerNeverAcceptsPartialOrUnknownGeneration(t *testing.T) {
	c := &RemoteMutationCoordinator{Root: filepath.Join(t.TempDir(), "markers")}
	hash := sha256.Sum256([]byte("access"))
	barrier := sha256.Sum256([]byte("barrier"))
	for _, event := range []proto.RemoteEvent{
		{EventID: "partial", ArtifactID: "artifact", AccessGeneration: 1, AccessSetHash: hash},
		{EventID: "unknown", ArtifactID: "artifact", AccessGeneration: 1, AccessSetHash: hash, SecurityGeneration: 1, SecurityBarrierID: barrier, KeyMode: "unknown-v9"},
		{EventID: "bad-wrap-version", ArtifactID: "artifact", AccessGeneration: 1, AccessSetHash: hash, SecurityGeneration: 1, SecurityBarrierID: barrier, KeyMode: "recipient-wrap-v2", KeyVersion: 1},
	} {
		tx, err := c.Begin("account", outboxEntry{Event: event})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Close(); err != nil {
			t.Fatal(err)
		}
		marker, _, err := loadMarker(tx.path, "account")
		if err != nil {
			t.Fatal(err)
		}
		if markerHasAnyGeneration(marker) {
			t.Fatalf("invalid tuple became recovery authority: %+v", marker)
		}
		completed, err := c.CompleteRecovery("account", marker.MutationGeneration)
		if err != nil || !completed {
			t.Fatalf("test cleanup failed: completed=%v err=%v", completed, err)
		}
	}
}

func TestRemoteRescanCheckpointRequirementCannotRaceRecoveryClear(t *testing.T) {
	c := &RemoteMutationCoordinator{Root: filepath.Join(t.TempDir(), "markers")}
	tx, err := c.Begin("account", outboxEntry{Event: proto.RemoteEvent{ArtifactID: "artifact", EventID: "event", BranchID: "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
	before, _, err := loadMarker(tx.path, "account")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequireCheckpoint("account"); err != nil {
		t.Fatal(err)
	}
	completed, err := c.CompleteRecovery("account", before.MutationGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if completed {
		t.Fatal("stale recovery CAS cleared a concurrent checkpoint obligation")
	}
	after, _, err := loadMarker(tx.path, "account")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "dirty" || after.ReasonFlags&rescanReasonCheckpoint == 0 || after.MutationGeneration <= before.MutationGeneration {
		t.Fatalf("checkpoint obligation not retained: before=%+v after=%+v", before, after)
	}
	completed, err = c.CompleteRecovery("account", after.MutationGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if completed {
		t.Fatal("checkpoint-marked scope cleared without checkpoint fulfillment")
	}
}

func TestRemoteRescanSecurityCutoverIsExactIdempotentAndFullScope(t *testing.T) {
	coordinator := &RemoteMutationCoordinator{Root: filepath.Join(t.TempDir(), "markers")}
	scope := "0197f30a-3c58-7000-8000-000000000001"
	seed := outboxEntry{Event: proto.RemoteEvent{
		NamespaceID: scope, ArtifactID: "one-artifact", BranchID: "review", EventID: "event-a",
		AccessGeneration: 1, AccessSetHash: sha256.Sum256([]byte("old-access")),
		SecurityGeneration: 1, SecurityBarrierID: sha256.Sum256([]byte("old-barrier")),
		KeyMode: "namespace-key-v1", KeyVersion: 1,
	}}
	mutation, err := coordinator.Begin(scope, seed)
	require.NoError(t, err)
	require.NoError(t, mutation.Close())
	next := securityepoch.SecurityEpoch{
		CoordinatorGeneration: 2, AccessGeneration: 2,
		AccessSetHash: sha256.Sum256([]byte("next-access")), BarrierID: sha256.Sum256([]byte("next-barrier")),
		KeyMode: "namespace-key-v1", KeyVersion: 2,
	}
	require.NoError(t, coordinator.RequireSecurityCutover(scope, next))
	first, exists, err := coordinator.Snapshot(scope)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, uint32(rescanReasonAccessCutover), first.ReasonFlags&rescanReasonAccessCutover)
	require.Empty(t, first.TargetArtifactID)
	require.Empty(t, first.TargetBranchID)
	require.Empty(t, first.TargetEventID)
	require.Equal(t, next.CoordinatorGeneration, first.TargetSecurityGeneration)
	require.Equal(t, next.AccessGeneration, first.TargetAccessGeneration)

	require.NoError(t, coordinator.RequireSecurityCutover(scope, next))
	second, exists, err := coordinator.Snapshot(scope)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, first.MutationGeneration, second.MutationGeneration)
	require.Equal(t, first.RecordSequence, second.RecordSequence)
}

func TestRemoteRescanMarkerRejectsTwoCorruptSlots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "markers")
	c := &RemoteMutationCoordinator{Root: root}
	tx, err := c.Begin("account", outboxEntry{})
	if err != nil {
		t.Fatal(err)
	}
	path := tx.path
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, remoteRescanFileBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadMarker(path, "account"); err == nil {
		t.Fatal("corrupt marker unexpectedly accepted")
	}
}

func TestRemoteRescanMarkerReadsLegacyPartialTupleAsNonAuthority(t *testing.T) {
	raw, err := encodeMarker(RemoteRescanMarkerV1{Version: 1, ScopeID: "account", State: "dirty", ReasonFlags: rescanReasonCapacity, MutationGeneration: 1, RecordSequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	access := sha256.Sum256([]byte("legacy-access"))
	barrier := sha256.Sum256([]byte("legacy-barrier"))
	binary.BigEndian.PutUint64(raw[290:298], 3)
	copy(raw[298:330], access[:])
	copy(raw[330:362], barrier[:])
	binary.BigEndian.PutUint64(raw[362:370], 4)
	sum := sha256.Sum256(append([]byte("aplexica/remote-rescan-marker/v1\x00"), raw[:remoteRescanSlotBytes-32]...))
	copy(raw[remoteRescanSlotBytes-32:], sum[:])

	marker, err := decodeMarker(raw)
	if err != nil {
		t.Fatal(err)
	}
	if markerHasAnyGeneration(marker) {
		t.Fatalf("legacy partial tuple became recovery authority: %+v", marker)
	}
}
