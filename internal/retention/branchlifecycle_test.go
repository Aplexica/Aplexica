package retention

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// seedBranch creates an artifact + initial main event + a fork onto
// `branch`. The fork event is written with timestamp = lastEventAt so
// RefreshBranchIndex deterministically reports that branch's
// LastEventAt as lastEventAt.
func seedBranch(t *testing.T, store *acf.Store, branch string, lastEventAt time.Time) string {
	t.Helper()
	id := uuid.NewString()
	created := lastEventAt.Add(-time.Hour)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Name:             "test",
		CreatedAt:        created,
		UpdatedAt:        created,
	}))
	e1 := acf.Event{
		EventID:    uuid.NewString(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  created,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, e1))
	mainHead, err := store.HeadHashByBranch(acf.KindMemory, id, acf.MainBranch)
	require.NoError(t, err)
	fork := acf.Event{
		EventID:          uuid.NewString(),
		ArtifactID:       id,
		Type:             acf.EventTypeForkOuter,
		Timestamp:        lastEventAt,
		ParentHash:       mainHead,
		Branch:           branch,
		ForkSourceBranch: acf.MainBranch,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, fork))
	// Materialize the branch index so direct LoadBranchIndex calls find
	// the seeded branch without needing a TickAutoArchive pass first.
	_, err = store.RefreshBranchIndex(acf.KindMemory, id)
	require.NoError(t, err)
	return id
}

func TestTickAutoArchive_ArchivesStaleBranch(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	staleTime := time.Now().Add(-200 * 24 * time.Hour) // 200 days ago
	id := seedBranch(t, store, "stale-branch", staleTime)

	res, err := TickAutoArchive(context.Background(), store, 90*24*time.Hour, time.Now())
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Inspected, 1)
	require.NotEmpty(t, res.Archived)

	bi, err := store.LoadBranchIndex(acf.KindMemory, id)
	require.NoError(t, err)
	require.True(t, bi.Branches["stale-branch"].Archived, "branch should be archived; got %+v", bi.Branches["stale-branch"])
	require.Equal(t, "auto:stale", bi.Branches["stale-branch"].ArchiveReason)
}

func TestTickAutoArchive_SkipsFreshBranch(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	id := seedBranch(t, store, "fresh-branch", time.Now().Add(-1*time.Hour))
	_, err := TickAutoArchive(context.Background(), store, 90*24*time.Hour, time.Now())
	require.NoError(t, err)

	bi, err := store.LoadBranchIndex(acf.KindMemory, id)
	require.NoError(t, err)
	require.False(t, bi.Branches["fresh-branch"].Archived)
}

func TestTickAutoArchive_ZeroThresholdDisablesPass(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	staleTime := time.Now().Add(-365 * 24 * time.Hour)
	id := seedBranch(t, store, "ancient", staleTime)

	res, err := TickAutoArchive(context.Background(), store, 0, time.Now())
	require.NoError(t, err)
	require.Empty(t, res.Archived)

	bi, _ := store.LoadBranchIndex(acf.KindMemory, id)
	require.False(t, bi.Branches["ancient"].Archived)
}

func TestTickAutoArchive_DoNotArchiveTagExempts(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	staleTime := time.Now().Add(-365 * 24 * time.Hour)
	id := seedBranch(t, store, "pinned", staleTime)

	bi, err := store.LoadBranchIndex(acf.KindMemory, id)
	require.NoError(t, err)
	bi.Branches["pinned"].Tags = []string{DoNotArchiveTag}
	require.NoError(t, store.WriteBranchIndex(bi))

	res, err := TickAutoArchive(context.Background(), store, 90*24*time.Hour, time.Now())
	require.NoError(t, err)
	require.NotContains(t, res.Archived, string(acf.KindMemory)+"/"+id+":pinned")

	final, _ := store.LoadBranchIndex(acf.KindMemory, id)
	require.False(t, final.Branches["pinned"].Archived)
}

func TestAutoArchiveRunner_HotReloadThreshold(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	runner := NewAutoArchiveRunner(store, 0, time.Hour)
	require.Equal(t, time.Duration(0), runner.Threshold())

	runner.SetThreshold(30 * 24 * time.Hour)
	require.Equal(t, 30*24*time.Hour, runner.Threshold())

	runner.SetInterval(2 * time.Hour)
	require.Equal(t, 2*time.Hour, runner.Interval())
}
