package acf

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStore_Init_CreatesVersionFile(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())
	data, err := os.ReadFile(filepath.Join(root, "VERSION"))
	require.NoError(t, err)
	require.Equal(t, "2\n", string(data))

	// v2 creates the content-addressed blob directory.
	info, err := os.Stat(s.BlobsDir())
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestStore_Init_IsIdempotentOnExistingV2Store(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VERSION"), []byte("2\n"), 0o644))
	s := &Store{Root: root}
	require.NoError(t, s.Init())
}

func TestStore_Init_PreV1StoreWithoutVersionFile(t *testing.T) {
	// Pre-v0.17.1 stores were initialized without a VERSION file. Re-Init
	// must write the file and not fail.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "acf", "memories"), 0o755))
	s := &Store{Root: root}
	require.NoError(t, s.Init())
	data, err := os.ReadFile(filepath.Join(root, "VERSION"))
	require.NoError(t, err)
	require.Equal(t, "2\n", string(data))
}

// TestStore_Init_UpgradesV1ToV2Transparently pins the attachment schema upgrade:
// an existing v1 store with real artifacts + events must upgrade to v2
// without touching any event, and the chain must still verify. The only
// delta is the additive blobs/ directory and the rewritten VERSION marker.
func TestStore_Init_UpgradesV1ToV2Transparently(t *testing.T) {
	root := t.TempDir()
	// Stand up a v1 store with a memory artifact + chained events.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "acf", "memories"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "events", "memories"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VERSION"), []byte("1\n"), 0o600))

	s := &Store{Root: root}
	id := NewID()
	now := time.Now().UTC()
	require.NoError(t, s.WriteArtifact(Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindMemory,
		Name:             "m",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	p, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "# hi\n"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID: NewID(), ArtifactID: id, Type: EventTypeCreate, Timestamp: now, Payload: p,
	}))
	head, err := s.HeadHash(KindMemory, id)
	require.NoError(t, err)
	p2, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "# hi v2\n"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID: NewID(), ArtifactID: id, Type: EventTypeUpdate, Timestamp: now.Add(time.Second),
		Payload: p2, ParentHash: head,
	}))

	preEvents, err := s.ReadEvents(KindMemory, id)
	require.NoError(t, err)
	require.NoError(t, VerifyChain(preEvents), "v1 chain verifies before upgrade")

	// The upgrade.
	require.NoError(t, s.Init())

	data, err := os.ReadFile(filepath.Join(root, "VERSION"))
	require.NoError(t, err)
	require.Equal(t, "2\n", string(data), "VERSION upgraded to 2")
	info, err := os.Stat(s.BlobsDir())
	require.NoError(t, err)
	require.True(t, info.IsDir(), "blobs/ created on upgrade")

	// Events are byte-for-byte unchanged and still verify.
	postEvents, err := s.ReadEvents(KindMemory, id)
	require.NoError(t, err)
	require.Equal(t, preEvents, postEvents, "no event mutated by the upgrade")
	require.NoError(t, VerifyChain(postEvents), "v2 chain verifies after upgrade")
}

func TestStore_Init_RejectsFutureVersion(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VERSION"), []byte("99\n"), 0o644))
	s := &Store{Root: root}
	err := s.Init()
	require.Error(t, err)
	require.Contains(t, err.Error(), "version")
}
