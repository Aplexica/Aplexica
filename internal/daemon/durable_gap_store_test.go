package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

func durableGapTestDelivery(position uint64, predecessorCursor, cursor string) proto.RemoteInboundDeliveryV2 {
	body := json.RawMessage(`{"version":2,"ciphertext":"c2VhbGVkLW9ubHk"}`)
	bodyDigest := sha256.Sum256(body)
	cursorDigest := sha256.Sum256([]byte(cursor))
	return proto.RemoteInboundDeliveryV2{
		DeliveryID:          "delivery-gap-1",
		Cursor:              cursor,
		ProtocolVersion:     1,
		StreamID:            "stream-1",
		StreamEpoch:         "epoch-1",
		PredecessorCursor:   predecessorCursor,
		PredecessorPosition: position - 1,
		Position:            position,
		CursorDigest:        hex.EncodeToString(cursorDigest[:]),
		Events: []proto.RemoteEvent{{
			NamespaceID: "namespace-1",
			ArtifactID:  "artifact-1",
			EventID:     "event-gap-1",
			EventHash:   strings.Repeat("b", 64),
			ParentHash:  strings.Repeat("a", 64),
			BodyDigest:  hex.EncodeToString(bodyDigest[:]),
			Bytes:       body,
		}},
	}
}

func durableGapTestKey(position uint64) DurableGapKey {
	return DurableGapKey{RemoteIdentity: "device-1", StreamID: "stream-1", StreamEpoch: "epoch-1", Position: position}
}

func durableGapTestBatch(t *testing.T) proto.RemoteInboundDeliveryV2 {
	t.Helper()
	delivery := durableGapTestDelivery(3, "cursor-1", "cursor-3")
	delivery.DeliveryID = "delivery-gap-batch-1"
	delivery.PredecessorPosition = 1
	second := delivery.Events[0]
	second.ArtifactID = "artifact-2"
	second.EventID = "event-gap-2"
	second.EventHash = strings.Repeat("c", 64)
	body := json.RawMessage(`{"version":2,"ciphertext":"c2Vjb25k"}`)
	digest := sha256.Sum256(body)
	second.Bytes = body
	second.BodyDigest = hex.EncodeToString(digest[:])
	delivery.Events = append(delivery.Events, second)
	delivery.BatchEventCount = uint16(len(delivery.Events))
	batchDigest, err := proto.RemoteReplayBatchDigest(delivery)
	require.NoError(t, err)
	delivery.BatchDigest = batchDigest
	return delivery
}

func TestDurableGapStorePersistsEncryptedDeliveryAcrossRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-gaps")
	key := durableGapTestKey(2)
	delivery := durableGapTestDelivery(2, "cursor-1", "cursor-2")
	store := &DurableGapStore{Root: root}

	stored, err := store.Put(key, delivery, delivery.Events[0].ParentHash)
	require.NoError(t, err)
	require.Equal(t, delivery, stored.Delivery)
	require.False(t, stored.CreatedAt.IsZero())

	// Exact redelivery is idempotent and retains the original creation time.
	repeated, err := (&DurableGapStore{Root: root}).Put(key, delivery, delivery.Events[0].ParentHash)
	require.NoError(t, err)
	require.Equal(t, stored.CreatedAt, repeated.CreatedAt)

	loaded, err := (&DurableGapStore{Root: root}).Load(key)
	require.NoError(t, err)
	require.Equal(t, delivery.Events[0].Bytes, loaded.Delivery.Events[0].Bytes)
	listed, err := (&DurableGapStore{Root: root}).List()
	require.NoError(t, err)
	require.Len(t, listed, 1)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	var recordPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), durableGapRecordPrefix) {
			recordPath = filepath.Join(root, entry.Name())
			require.NotContains(t, entry.Name(), key.RemoteIdentity)
			require.NotContains(t, entry.Name(), key.StreamID)
		}
	}
	require.NotEmpty(t, recordPath)
	raw, err := os.ReadFile(recordPath)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "plain conversation text")
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(recordPath)
		require.NoError(t, statErr)
		require.Zero(t, info.Mode().Perm()&0o077)
	}

	require.ErrorIs(t, store.Resolve(key, "different-delivery"), ErrDurableGapConflict)
	require.NoError(t, store.Resolve(key, delivery.DeliveryID))
	require.NoError(t, (&DurableGapStore{Root: root}).Resolve(key, delivery.DeliveryID), "resolve must be restart-idempotent")
	_, err = store.Load(key)
	require.ErrorIs(t, err, ErrDurableGapNotFound)
}

func TestDurableGapStoreRejectsEquivocationAndPlainOrInvalidBodies(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-gaps")
	key := durableGapTestKey(2)
	delivery := durableGapTestDelivery(2, "cursor-1", "cursor-2")
	store := &DurableGapStore{Root: root}
	_, err := store.Put(key, delivery, delivery.Events[0].ParentHash)
	require.NoError(t, err)

	changed := delivery
	changed.DeliveryID = "delivery-gap-equivocation"
	changed.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	changed.Events[0].Bytes = json.RawMessage(`{"ciphertext":"different"}`)
	digest := sha256.Sum256(changed.Events[0].Bytes)
	changed.Events[0].BodyDigest = hex.EncodeToString(digest[:])
	_, err = store.Put(key, changed, changed.Events[0].ParentHash)
	require.ErrorIs(t, err, ErrDurableGapConflict)

	invalid := durableGapTestDelivery(3, "cursor-2", "cursor-3")
	invalid.Events[0].BodyDigest = strings.Repeat("0", 64)
	_, err = store.Put(durableGapTestKey(3), invalid, invalid.Events[0].ParentHash)
	require.ErrorIs(t, err, ErrDurableGapInvalid)

	invalid = durableGapTestDelivery(3, "cursor-2", "cursor-3")
	invalid.Events[0].Bytes = nil
	invalid.Events[0].BodyDigest = hex.EncodeToString(make([]byte, sha256.Size))
	_, err = store.Put(durableGapTestKey(3), invalid, invalid.Events[0].ParentHash)
	require.ErrorIs(t, err, ErrDurableGapInvalid)
}

func TestDurableGapStorePersistsExactBatchSelectorAcrossRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-gaps")
	delivery := durableGapTestBatch(t)
	key := durableGapTestKey(delivery.Position)
	store := &DurableGapStore{Root: root}

	stored, err := store.Put(key, delivery, delivery.Events[1].ParentHash, 1)
	require.NoError(t, err)
	require.Equal(t, uint16(1), stored.MissingEventIndex)
	require.Equal(t, delivery.BatchDigest, stored.Delivery.BatchDigest)
	require.Equal(t, delivery.Events[1].Bytes, stored.Delivery.Events[1].Bytes)

	loaded, err := (&DurableGapStore{Root: root}).Load(key)
	require.NoError(t, err)
	require.Equal(t, stored, loaded)
	positions := make([]uint64, len(loaded.Delivery.Events))
	for index, event := range loaded.Delivery.Events {
		positions[index] = loaded.Delivery.PredecessorPosition + uint64(index) + 1
		require.Equal(t, delivery.Events[index].EventHash, event.EventHash)
		require.Equal(t, delivery.Events[index].BodyDigest, event.BodyDigest)
	}
	require.Equal(t, []uint64{2, 3}, positions, "ordered event indexes derive exact contiguous durable positions")
	require.Equal(t, loaded.Delivery.Position, loaded.Delivery.PredecessorPosition+uint64(len(loaded.Delivery.Events)))
	repeated, err := (&DurableGapStore{Root: root}).Put(key, delivery, delivery.Events[1].ParentHash, 1)
	require.NoError(t, err)
	require.Equal(t, stored.CreatedAt, repeated.CreatedAt)

	_, err = store.Put(key, delivery, delivery.Events[0].ParentHash, 0)
	require.ErrorIs(t, err, ErrDurableGapConflict, "the missing-event selector is part of idempotency")

	tampered := delivery
	tampered.BatchDigest = strings.Repeat("0", 64)
	_, err = (&DurableGapStore{Root: filepath.Join(t.TempDir(), "tampered")}).Put(durableGapTestKey(tampered.Position), tampered, tampered.Events[1].ParentHash, 1)
	require.ErrorIs(t, err, ErrDurableGapInvalid)

	tamperedEventHash := delivery
	tamperedEventHash.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	tamperedEventHash.Events[0].EventHash = strings.Repeat("d", 64)
	_, err = (&DurableGapStore{Root: filepath.Join(t.TempDir(), "tampered-event-hash")}).Put(durableGapTestKey(tamperedEventHash.Position), tamperedEventHash, tamperedEventHash.Events[1].ParentHash, 1)
	require.ErrorIs(t, err, ErrDurableGapInvalid, "the ordered batch digest binds every event hash")

	reordered := delivery
	reordered.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	reordered.Events[0], reordered.Events[1] = reordered.Events[1], reordered.Events[0]
	_, err = (&DurableGapStore{Root: filepath.Join(t.TempDir(), "reordered")}).Put(durableGapTestKey(reordered.Position), reordered, reordered.Events[0].ParentHash, 0)
	require.ErrorIs(t, err, ErrDurableGapInvalid, "the ordered batch digest binds event-to-position assignment")

	mixedSecurity := delivery
	mixedSecurity.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	mixedSecurity.Events[1].SecurityGeneration++
	mixedSecurity.BatchDigest, err = proto.RemoteReplayBatchDigest(mixedSecurity)
	require.NoError(t, err)
	_, err = (&DurableGapStore{Root: filepath.Join(t.TempDir(), "mixed-security")}).Put(durableGapTestKey(mixedSecurity.Position), mixedSecurity, mixedSecurity.Events[1].ParentHash, 1)
	require.ErrorIs(t, err, ErrDurableGapInvalid, "a persisted batch cannot cross a security tuple")

	invalidParent := delivery
	invalidParent.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	invalidParent.Events[0].ParentHash = "not-a-digest"
	invalidParent.BatchDigest, err = proto.RemoteReplayBatchDigest(invalidParent)
	require.NoError(t, err)
	_, err = (&DurableGapStore{Root: filepath.Join(t.TempDir(), "invalid-parent")}).Put(durableGapTestKey(invalidParent.Position), invalidParent, invalidParent.Events[1].ParentHash, 1)
	require.ErrorIs(t, err, ErrDurableGapInvalid)
}

func TestDurableGapStoreAtomicallyAdvancesExactBatchSelector(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-gaps")
	delivery := durableGapTestBatch(t)
	key := durableGapTestKey(delivery.Position)
	store := &DurableGapStore{Root: root}
	first, err := store.Put(key, delivery, delivery.Events[0].ParentHash, 0)
	require.NoError(t, err)

	advanced, err := (&DurableGapStore{Root: root}).AdvanceSelector(
		key, delivery,
		delivery.Events[0].ParentHash, 0,
		delivery.Events[1].ParentHash, 1,
	)
	require.NoError(t, err)
	require.Equal(t, first.CreatedAt, advanced.CreatedAt)
	require.Equal(t, uint16(1), advanced.MissingEventIndex)

	retried, err := (&DurableGapStore{Root: root}).AdvanceSelector(
		key, delivery,
		delivery.Events[0].ParentHash, 0,
		delivery.Events[1].ParentHash, 1,
	)
	require.NoError(t, err, "a crash after atomic replacement must be retry-idempotent")
	require.Equal(t, advanced, retried)
	loaded, err := (&DurableGapStore{Root: root}).Load(key)
	require.NoError(t, err)
	require.Equal(t, advanced, loaded)

	_, err = store.AdvanceSelector(
		key, delivery,
		delivery.Events[1].ParentHash, 1,
		delivery.Events[0].ParentHash, 0,
	)
	require.ErrorIs(t, err, ErrDurableGapInvalid, "selector advancement is strictly monotonic")
}

func TestDurableGapStoreCorruptionFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-gaps")
	key := durableGapTestKey(2)
	delivery := durableGapTestDelivery(2, "cursor-1", "cursor-2")
	store := &DurableGapStore{Root: root}
	_, err := store.Put(key, delivery, delivery.Events[0].ParentHash)
	require.NoError(t, err)

	digest, err := durableGapKeyDigest(key)
	require.NoError(t, err)
	path := filepath.Join(root, durableGapName(digest))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	corrupt := bytes.Replace(raw, []byte(delivery.Events[0].ParentHash), []byte(strings.Repeat("f", 64)), 1)
	require.NotEqual(t, raw, corrupt)
	require.NoError(t, os.WriteFile(path, corrupt, 0o600))

	_, err = (&DurableGapStore{Root: root}).Load(key)
	require.ErrorIs(t, err, ErrDurableGapCorrupt)
	_, err = (&DurableGapStore{Root: root}).Put(key, delivery, delivery.Events[0].ParentHash)
	require.ErrorIs(t, err, ErrDurableGapCorrupt)
}

func TestDurableGapCapacityIsHardBounded(t *testing.T) {
	require.True(t, durableGapCapacityAvailable(0, 0, 1))
	require.True(t, durableGapCapacityAvailable(durableGapMaxRecords-1, durableGapMaxTotalBytes-1, 1))
	require.False(t, durableGapCapacityAvailable(durableGapMaxRecords, 0, 1))
	require.False(t, durableGapCapacityAvailable(0, durableGapMaxTotalBytes, 1))
	require.False(t, durableGapCapacityAvailable(0, 0, durableGapMaxRecord+1))
	require.False(t, durableGapCapacityAvailable(0, 0, 0))
}
