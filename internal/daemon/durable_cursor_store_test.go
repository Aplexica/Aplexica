package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

func durableCursorTestKey() DurableCursorKey {
	return DurableCursorKey{
		RemoteIdentity: "cloud-account-fingerprint-1",
		StreamID:       "stream-opaque-1",
		StreamEpoch:    "epoch-opaque-1",
	}
}

func durableCursorTestState(cursor, _ string) DurableCursorState {
	computed := sha256.Sum256([]byte(cursor))
	return DurableCursorState{Cursor: cursor, CursorDigest: hex.EncodeToString(computed[:])}
}

func TestDurableCursorStoreCompareAndSwapSurvivesRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	key := durableCursorTestKey()
	store := &DurableCursorStore{Root: root}

	_, err := store.Load(key)
	require.ErrorIs(t, err, ErrDurableCursorNotFound)

	// A real signed cloud cursor is larger than the legacy inbound delivery
	// identifier limit. Keep this regression over 256 bytes.
	firstValue := durableCursorTestState(strings.Repeat("a", 320)+".signature", "index-digest-1")
	first, err := store.CompareAndSwap(key, nil, firstValue)
	require.NoError(t, err)
	require.Equal(t, uint64(1), first.Revision)

	restarted := &DurableCursorStore{Root: root}
	loaded, err := restarted.Load(key)
	require.NoError(t, err)
	require.Equal(t, first, loaded)

	// Redelivery after a crash is an idempotent repair even when the caller's
	// pre-admission expectation was lost with the old process.
	repaired, err := restarted.CompareAndSwap(key, nil, firstValue)
	require.NoError(t, err)
	require.Equal(t, first, repaired)

	stale := first
	stale.Revision++
	_, err = restarted.CompareAndSwap(key, &stale, durableCursorTestState("cursor-2", "index-digest-2"))
	require.ErrorIs(t, err, ErrDurableCursorConflict)

	second, err := restarted.CompareAndSwap(key, &first, durableCursorTestState("cursor-2", "index-digest-2"))
	require.NoError(t, err)
	require.Equal(t, uint64(2), second.Revision)

	// An identical token can never be rebound to different fetched-index
	// evidence, even with an otherwise-current CAS expectation. Position-zero
	// compatibility records retain their historical opaque digest shape, so
	// this is an equivocation conflict rather than an input-format failure.
	_, err = restarted.CompareAndSwap(key, &second, DurableCursorState{Cursor: second.Cursor, CursorDigest: strings.Repeat("0", sha256.Size*2)})
	require.ErrorIs(t, err, ErrDurableCursorConflict)

	final, err := store.Load(key)
	require.NoError(t, err)
	require.Equal(t, second, final)
}

func TestDurableCursorStoreEnforcesPositionedGenesisAndAdjacency(t *testing.T) {
	key := durableCursorTestKey()
	state := func(cursor, _ string, position uint64) DurableCursorState {
		value := durableCursorTestState(cursor, "")
		value.Position = position
		return value
	}

	t.Run("cannot create a skipped first position", func(t *testing.T) {
		store := &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
		_, err := store.CompareAndSwap(key, nil, state("cursor-2", "digest-2", 2))
		require.ErrorIs(t, err, ErrDurableCursorConflict)
	})

	t.Run("positioned chain advances exactly one", func(t *testing.T) {
		store := &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
		first, err := store.CompareAndSwap(key, nil, state("cursor-1", "digest-1", 1))
		require.NoError(t, err)
		require.Equal(t, uint64(1), first.Position)

		_, err = store.CompareAndSwap(key, &first, state("cursor-3", "digest-3", 3))
		require.ErrorIs(t, err, ErrDurableCursorConflict)

		second, err := store.CompareAndSwap(key, &first, state("cursor-2", "digest-2", 2))
		require.NoError(t, err)
		require.Equal(t, uint64(2), second.Position)

		replayed, err := (&DurableCursorStore{Root: store.Root}).CompareAndSwap(key, nil, state("cursor-2", "digest-2", 2))
		require.NoError(t, err)
		require.Equal(t, second, replayed)
	})

	t.Run("legacy unknown position cannot be promoted by guessing", func(t *testing.T) {
		store := &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
		legacyInput := DurableCursorState{Cursor: "legacy-cursor", CursorDigest: "legacy-index-digest"}
		legacy, err := store.CompareAndSwap(key, nil, legacyInput)
		require.NoError(t, err)
		reloaded, err := (&DurableCursorStore{Root: store.Root}).Load(key)
		require.NoError(t, err)
		require.Equal(t, legacy, reloaded)
		_, err = store.CompareAndSwap(key, &legacy, state("cursor-1", "digest-1", 1))
		require.ErrorIs(t, err, ErrDurableCursorConflict)
	})
}

func TestDurableCursorStoreSeedsOnlyAuthenticatedCheckpointCoverage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	key := durableCursorTestKey()
	store := &DurableCursorStore{Root: root}
	seedCursor := "signed-coverage-cursor-42"
	seedDigest := sha256.Sum256([]byte(seedCursor))
	seed := DurableCheckpointSeed{
		Cursor: seedCursor, CursorDigest: hex.EncodeToString(seedDigest[:]), Position: 42,
		CheckpointEventID: "checkpoint-event-50", CheckpointEventHash: strings.Repeat("a", 64),
		CheckpointAlignmentHash: strings.Repeat("c", 64),
		CheckpointGeneration:    strings.Repeat("b", 64), CheckpointPosition: 50, CheckpointCoverage: 42,
	}

	seeded, err := store.SeedFromCheckpoint(key, seed)
	require.NoError(t, err)
	require.Equal(t, uint64(42), seeded.Position)
	require.Equal(t, uint64(1), seeded.Revision)
	isSeed, err := (&DurableCursorStore{Root: root}).IsCurrentCheckpointSeed(key, seeded)
	require.NoError(t, err)
	require.True(t, isSeed, "the exact bootstrap coverage cursor must remain distinguishable from an ordinary delivery")

	replayed, err := (&DurableCursorStore{Root: root}).SeedFromCheckpoint(key, seed)
	require.NoError(t, err)
	require.Equal(t, seeded, replayed)

	next := durableCursorTestState("signed-cursor-43", "")
	next.Position = 43
	advanced, err := store.CompareAndSwap(key, &seeded, next)
	require.NoError(t, err)
	require.Equal(t, uint64(43), advanced.Position)
	isSeed, err = store.IsCurrentCheckpointSeed(key, advanced)
	require.NoError(t, err)
	require.False(t, isSeed, "a normal contiguous advance must stop being treated as the checkpoint bootstrap cursor")
	_, err = store.IsCurrentCheckpointSeed(key, seeded)
	require.ErrorIs(t, err, ErrDurableCursorConflict, "a stale bootstrap state must not authorize a replacement handoff")

	digest, err := durableCursorKeyDigest(key)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(root, durableCursorRecordName(digest)))
	require.NoError(t, err)
	record, err := decodeDurableCursorRecord(raw, digest)
	require.NoError(t, err)
	require.Equal(t, seed.CheckpointEventHash, record.BootstrapCheckpointEventHash)
	require.Equal(t, seed.CheckpointAlignmentHash, record.BootstrapCheckpointAlignmentHash)
	require.Equal(t, seed.CheckpointGeneration, record.BootstrapCheckpointGeneration)
	require.Equal(t, seed.Cursor, record.BootstrapCursor)
	require.Equal(t, seed.CheckpointCoverage, record.BootstrapCheckpointCoverage)

	// A record produced before checkpoint alignment was persisted must stop;
	// the daemon may never infer the covered head from ParentHash or any other
	// checkpoint field. Recompute the checksum to model an otherwise authentic
	// old-format record rather than mere byte corruption.
	legacyWithoutAlignment := record
	legacyWithoutAlignment.BootstrapCheckpointAlignmentHash = ""
	legacyWithoutAlignment.Checksum = ""
	legacyChecksum, checksumErr := durableCursorChecksum(legacyWithoutAlignment)
	require.NoError(t, checksumErr)
	legacyWithoutAlignment.Checksum = legacyChecksum
	legacyRaw, marshalErr := json.Marshal(legacyWithoutAlignment)
	require.NoError(t, marshalErr)
	_, decodeErr := decodeDurableCursorRecord(legacyRaw, digest)
	require.ErrorIs(t, decodeErr, ErrDurableCursorCorrupt)

	_, err = store.SeedFromCheckpoint(key, seed)
	require.ErrorIs(t, err, ErrDurableCursorConflict, "checkpoint seeding can never regress an advanced cursor")
}

func TestDurableCursorStoreRejectsUnboundCheckpointSeeds(t *testing.T) {
	validCursor := "coverage-cursor"
	digest := sha256.Sum256([]byte(validCursor))
	valid := DurableCheckpointSeed{
		Cursor: validCursor, CursorDigest: hex.EncodeToString(digest[:]), Position: 8,
		CheckpointEventID: "checkpoint-event", CheckpointEventHash: strings.Repeat("a", 64),
		CheckpointAlignmentHash: strings.Repeat("c", 64),
		CheckpointGeneration:    strings.Repeat("b", 64), CheckpointPosition: 9, CheckpointCoverage: 8,
	}
	for name, mutate := range map[string]func(*DurableCheckpointSeed){
		"coverage differs from seed position": func(seed *DurableCheckpointSeed) { seed.CheckpointCoverage-- },
		"checkpoint does not follow coverage": func(seed *DurableCheckpointSeed) { seed.CheckpointPosition = seed.CheckpointCoverage },
		"checkpoint hash is not a digest":     func(seed *DurableCheckpointSeed) { seed.CheckpointEventHash = "event-content" },
		"checkpoint alignment is absent":      func(seed *DurableCheckpointSeed) { seed.CheckpointAlignmentHash = "" },
		"checkpoint generation is absent":     func(seed *DurableCheckpointSeed) { seed.CheckpointGeneration = "" },
	} {
		t.Run(name, func(t *testing.T) {
			seed := valid
			mutate(&seed)
			_, err := (&DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}).SeedFromCheckpoint(durableCursorTestKey(), seed)
			require.ErrorIs(t, err, ErrDurableCursorInvalid)
		})
	}
}

func TestDurableCursorStoreIdentityAndEpochIsolation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	store := &DurableCursorStore{Root: root}
	keys := []DurableCursorKey{
		{RemoteIdentity: "remote-a", StreamID: "stream-a", StreamEpoch: "epoch-1"},
		{RemoteIdentity: "remote-a", StreamID: "stream-a", StreamEpoch: "epoch-2"},
		{RemoteIdentity: "remote-b", StreamID: "stream-a", StreamEpoch: "epoch-1"},
		{RemoteIdentity: "remote-a", StreamID: "stream-b", StreamEpoch: "epoch-1"},
	}
	for index, key := range keys {
		value := durableCursorTestState("cursor-"+string(rune('a'+index)), "digest-"+string(rune('a'+index)))
		state, err := store.CompareAndSwap(key, nil, value)
		require.NoError(t, err)
		require.Equal(t, uint64(1), state.Revision)
	}
	for index, key := range keys {
		state, err := (&DurableCursorStore{Root: root}).Load(key)
		require.NoError(t, err)
		require.Equal(t, "cursor-"+string(rune('a'+index)), state.Cursor)
	}

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	stateFiles := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), durableCursorRecordPrefix) {
			stateFiles++
			require.NotContains(t, entry.Name(), "remote")
			require.NotContains(t, entry.Name(), "stream")
			require.NotContains(t, entry.Name(), "epoch")
			raw, readErr := os.ReadFile(filepath.Join(root, entry.Name()))
			require.NoError(t, readErr)
			for _, key := range keys {
				require.NotContains(t, string(raw), key.RemoteIdentity)
				require.NotContains(t, string(raw), key.StreamID)
				require.NotContains(t, string(raw), key.StreamEpoch)
			}
		}
	}
	require.Equal(t, len(keys), stateFiles)

	// Length-prefixing prevents composite-key ambiguity.
	left, err := durableCursorKeyDigest(DurableCursorKey{RemoteIdentity: "a", StreamID: "bc", StreamEpoch: "d"})
	require.NoError(t, err)
	right, err := durableCursorKeyDigest(DurableCursorKey{RemoteIdentity: "ab", StreamID: "c", StreamEpoch: "d"})
	require.NoError(t, err)
	require.NotEqual(t, left, right)
}

func TestDurableCursorStoreCorruptionFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	key := durableCursorTestKey()
	store := &DurableCursorStore{Root: root}
	_, err := store.CompareAndSwap(key, nil, durableCursorTestState("cursor-old", "digest-old"))
	require.NoError(t, err)

	keyDigest, err := durableCursorKeyDigest(key)
	require.NoError(t, err)
	path := filepath.Join(root, durableCursorRecordName(keyDigest))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	corrupt := bytes.Replace(raw, []byte("cursor-old"), []byte("cursor-bad"), 1)
	require.NotEqual(t, raw, corrupt)
	require.NoError(t, os.WriteFile(path, corrupt, 0o600))

	_, err = store.Load(key)
	require.ErrorIs(t, err, ErrDurableCursorCorrupt)
	_, err = store.CompareAndSwap(key, nil, durableCursorTestState("cursor-new", "digest-new"))
	require.ErrorIs(t, err, ErrDurableCursorCorrupt)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, corrupt, after, "fail-closed CAS must not replace corrupt state")
}

func TestDurableCursorStoreAtomicPrivateReplace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	require.NoError(t, os.Mkdir(root, 0o755))
	require.NoError(t, os.Chmod(root, 0o755), "test setup must begin with an owned but non-private legacy directory")
	key := durableCursorTestKey()
	store := &DurableCursorStore{Root: root}
	current, err := store.CompareAndSwap(key, nil, durableCursorTestState("cursor-0", "digest-0"))
	require.NoError(t, err)
	for index := 1; index <= 32; index++ {
		next := durableCursorTestState("cursor-"+string(rune('A'+index)), "digest-"+string(rune('A'+index)))
		current, err = store.CompareAndSwap(key, &current, next)
		require.NoError(t, err)
	}

	keyDigest, err := durableCursorKeyDigest(key)
	require.NoError(t, err)
	name := durableCursorRecordName(keyDigest)
	raw, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	record, err := decodeDurableCursorRecord(raw, keyDigest)
	require.NoError(t, err)
	require.Equal(t, current, durableCursorState(record))

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".aplexica-write-"), "atomic replace left a temporary sibling")
	}

	privateRoot, err := privatefs.OpenRoot(root, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	require.NoError(t, err)
	f, err := privateRoot.OpenReadRegular(name)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, privateRoot.Close())
	if runtime.GOOS != "windows" {
		rootInfo, statErr := os.Stat(root)
		require.NoError(t, statErr)
		require.Zero(t, rootInfo.Mode().Perm()&0o077)
		fileInfo, statErr := os.Stat(filepath.Join(root, name))
		require.NoError(t, statErr)
		require.Zero(t, fileInfo.Mode().Perm()&0o077)
	}
}

func TestDurableCursorStoreCrossInstanceCAS(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	key := durableCursorTestKey()
	initial, err := (&DurableCursorStore{Root: root}).CompareAndSwap(key, nil, durableCursorTestState("cursor-0", "digest-0"))
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, suffix := range []string{"a", "b"} {
		suffix := suffix
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, casErr := (&DurableCursorStore{Root: root}).CompareAndSwap(key, &initial, durableCursorTestState("cursor-"+suffix, "digest-"+suffix))
			errs <- casErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrDurableCursorConflict):
			conflicts++
		default:
			t.Fatalf("unexpected CAS error: %v", err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	final, err := (&DurableCursorStore{Root: root}).Load(key)
	require.NoError(t, err)
	require.Equal(t, uint64(2), final.Revision)
	require.Contains(t, []string{"cursor-a", "cursor-b"}, final.Cursor)
}

func TestDurableCursorStoreRejectsUnsafeInput(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	store := &DurableCursorStore{Root: root}
	key := durableCursorTestKey()

	_, err := (*DurableCursorStore)(nil).Load(key)
	require.ErrorIs(t, err, ErrDurableCursorInvalid)
	_, err = (&DurableCursorStore{Root: "relative"}).Load(key)
	require.ErrorIs(t, err, ErrDurableCursorInvalid)

	unsafeKey := key
	unsafeKey.StreamID = "stream\ncontent"
	_, err = store.CompareAndSwap(unsafeKey, nil, durableCursorTestState("cursor", "digest"))
	require.ErrorIs(t, err, ErrDurableCursorInvalid)

	_, err = store.CompareAndSwap(key, nil, DurableCursorState{Cursor: strings.Repeat("x", durableCursorMaxToken+1), CursorDigest: "digest"})
	require.ErrorIs(t, err, ErrDurableCursorInvalid)
	_, err = store.CompareAndSwap(key, nil, DurableCursorState{Cursor: "cursor", CursorDigest: "digest", Revision: 1})
	require.ErrorIs(t, err, ErrDurableCursorInvalid)

	expected := DurableCursorState{Cursor: "cursor", CursorDigest: "digest"}
	_, err = store.CompareAndSwap(key, &expected, durableCursorTestState("next", "next-digest"))
	require.ErrorIs(t, err, ErrDurableCursorInvalid)
}
