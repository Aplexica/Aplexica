package acf

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeBranchName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"main", "main", false},
		{"feature-foo", "feature-foo", false},
		{"Feature_Foo Bar", "feature-foo-bar", false},
		{"work/topic.x", "work-topic-x", false},
		{"", "", true},
		{"---", "", true},
		{strings.Repeat("a", 80), strings.Repeat("a", 64), false},
		{"a😀b", "ab", false},
	}
	for _, c := range cases {
		got, err := NormalizeBranchName(c.in)
		if c.wantErr {
			require.Errorf(t, err, "expected error for %q", c.in)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, c.want, got, "for input %q", c.in)
	}
}

func TestAppendEvent_ForkAndMerge_BranchAware(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Root: dir}
	require.NoError(t, s.Init())

	now := time.Now().UTC().Truncate(time.Second)

	// Artifact + two main-branch events.
	a := Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       "art-1",
		Kind:             KindMemory,
		Name:             "test",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, s.WriteArtifact(a))

	e1 := Event{
		EventID:    "evt-1",
		ArtifactID: a.ArtifactID,
		Type:       EventTypeCreate,
		Timestamp:  now,
		ParentHash: "",
	}
	require.NoError(t, s.AppendEvent(a.Kind, e1))
	e1Hash, err := s.HeadHashByBranch(a.Kind, a.ArtifactID, MainBranch)
	require.NoError(t, err)
	require.NotEmpty(t, e1Hash)

	e2 := Event{
		EventID:    "evt-2",
		ArtifactID: a.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  now.Add(time.Second),
		ParentHash: e1Hash,
	}
	require.NoError(t, s.AppendEvent(a.Kind, e2))
	e2Hash, err := s.HeadHashByBranch(a.Kind, a.ArtifactID, MainBranch)
	require.NoError(t, err)

	// Fork from e1 onto branch "alt".
	fork := Event{
		EventID:          "evt-fork",
		ArtifactID:       a.ArtifactID,
		Type:             EventTypeForkOuter,
		Timestamp:        now.Add(time.Second * 2),
		ParentHash:       e1Hash,
		Branch:           "alt",
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  e1.EventID,
		ForkOriginAgent:  "codex",
	}
	require.NoError(t, s.AppendEvent(a.Kind, fork))
	forkHash, err := s.HeadHashByBranch(a.Kind, a.ArtifactID, "alt")
	require.NoError(t, err)

	// Continue on alt — parent must be the fork's hash.
	e3 := Event{
		EventID:    "evt-3",
		ArtifactID: a.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  now.Add(time.Second * 3),
		Branch:     "alt",
		ParentHash: forkHash,
	}
	require.NoError(t, s.AppendEvent(a.Kind, e3))
	e3Hash, err := s.HeadHashByBranch(a.Kind, a.ArtifactID, "alt")
	require.NoError(t, err)

	// Main head should still be e2.
	mainHead, err := s.HeadHashByBranch(a.Kind, a.ArtifactID, MainBranch)
	require.NoError(t, err)
	require.Equal(t, e2Hash, mainHead)

	require.NotEmpty(t, forkHash)
	require.NotEmpty(t, e3Hash)

	// Chain verifies.
	events, err := s.ReadEvents(a.Kind, a.ArtifactID)
	require.NoError(t, err)
	require.NoError(t, VerifyChain(events))

	// Merge alt into main.
	merge := Event{
		EventID:         "evt-merge",
		ArtifactID:      a.ArtifactID,
		Type:            EventTypeMergeOuter,
		Timestamp:       now.Add(time.Second * 4),
		Branch:          MainBranch,
		ParentHash:      e2Hash,
		MergeFromBranch: "alt",
		MergeFromHash:   e3Hash,
		MergeStrategy:   "manual",
	}
	require.NoError(t, s.AppendEvent(a.Kind, merge))

	events, err = s.ReadEvents(a.Kind, a.ArtifactID)
	require.NoError(t, err)
	require.NoError(t, VerifyChain(events))

	// Branch index now lists main and alt.
	branches, err := s.ListBranches(a.Kind, a.ArtifactID, true)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(branches), 2)
	names := map[string]bool{}
	for _, b := range branches {
		names[b.Name] = true
	}
	require.True(t, names[MainBranch])
	require.True(t, names["alt"])

	// alt should be marked as merged into main.
	for _, b := range branches {
		if b.Name == "alt" {
			require.Equal(t, MainBranch, b.MergedInto)
		}
	}
}

func TestAppendEvent_ForkRejectsExistingBranch(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Root: dir}
	require.NoError(t, s.Init())
	now := time.Now().UTC().Truncate(time.Second)

	a := Artifact{ArtifactID: "x", Kind: KindMemory, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.WriteArtifact(a))

	e1 := Event{EventID: "e1", ArtifactID: "x", Type: EventTypeCreate, Timestamp: now}
	require.NoError(t, s.AppendEvent(KindMemory, e1))
	mainHead, err := s.HeadHashByBranch(KindMemory, "x", MainBranch)
	require.NoError(t, err)

	fork := Event{
		EventID:          "ef",
		ArtifactID:       "x",
		Type:             EventTypeForkOuter,
		Branch:           "topic",
		Timestamp:        now.Add(time.Second),
		ParentHash:       mainHead,
		ForkSourceBranch: MainBranch,
	}
	require.NoError(t, s.AppendEvent(KindMemory, fork))

	// Second fork onto the same branch must be rejected.
	fork2 := Event{
		EventID:          "ef2",
		ArtifactID:       "x",
		Type:             EventTypeForkOuter,
		Branch:           "topic",
		Timestamp:        now.Add(time.Second * 2),
		ParentHash:       mainHead,
		ForkSourceBranch: MainBranch,
	}
	require.Error(t, s.AppendEvent(KindMemory, fork2))
}

func TestAppendEvent_ForkRejectsUnknownParent(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Root: dir}
	require.NoError(t, s.Init())
	now := time.Now().UTC().Truncate(time.Second)

	a := Artifact{ArtifactID: "x", Kind: KindMemory, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.WriteArtifact(a))

	e1 := Event{EventID: "e1", ArtifactID: "x", Type: EventTypeCreate, Timestamp: now}
	require.NoError(t, s.AppendEvent(KindMemory, e1))

	bogus := Event{
		EventID:    "bogus",
		ArtifactID: "x",
		Type:       EventTypeForkOuter,
		Branch:     "topic",
		Timestamp:  now.Add(time.Second),
		ParentHash: "deadbeef",
	}
	require.Error(t, s.AppendEvent(KindMemory, bogus))
}

func TestAppendEvent_NonFork_RejectsWrongBranchParent(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Root: dir}
	require.NoError(t, s.Init())
	now := time.Now().UTC().Truncate(time.Second)

	a := Artifact{ArtifactID: "x", Kind: KindMemory, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.WriteArtifact(a))

	e1 := Event{EventID: "e1", ArtifactID: "x", Type: EventTypeCreate, Timestamp: now}
	require.NoError(t, s.AppendEvent(KindMemory, e1))
	mainHead, err := s.HeadHashByBranch(KindMemory, "x", MainBranch)
	require.NoError(t, err)

	fork := Event{
		EventID:          "ef",
		ArtifactID:       "x",
		Type:             EventTypeForkOuter,
		Branch:           "topic",
		Timestamp:        now.Add(time.Second),
		ParentHash:       mainHead,
		ForkSourceBranch: MainBranch,
	}
	require.NoError(t, s.AppendEvent(KindMemory, fork))

	// Trying to append onto "topic" with main's head as parent must fail.
	wrong := Event{
		EventID:    "wrong",
		ArtifactID: "x",
		Type:       EventTypeUpdate,
		Branch:     "topic",
		Timestamp:  now.Add(time.Second * 2),
		ParentHash: mainHead,
	}
	require.Error(t, s.AppendEvent(KindMemory, wrong))
}

// fileMtime captures a file's modification time so a test can detect whether
// the file was rewritten: atomicfile.WriteFile replaces the file via a fresh
// temp + rename, which resets the mtime. The caller first pushes the mtime
// into the past (os.Chtimes), so a no-op refresh leaves it in the past while a
// rewrite resets it to ~now. Mtime-based rather than inode-based (syscall.Stat_t
// is Unix-only) so the check is cross-platform.
func fileMtime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.ModTime()
}

// TestRefreshBranchIndex_NoRewriteWhenUnchanged asserts that a second
// RefreshBranchIndex with no intervening change does NOT rewrite the
// on-disk branch index — avoiding needless write+fsync+rename churn on the
// read paths (branch list/checkout/merge/diff and the retention
// auto-archive tick).
func TestRefreshBranchIndex_NoRewriteWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Root: dir}
	require.NoError(t, s.Init())

	now := time.Now().UTC().Truncate(time.Second)

	a := Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       "art-norewrite",
		Kind:             KindMemory,
		Name:             "test",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, s.WriteArtifact(a))

	e1 := Event{
		EventID:    "evt-1",
		ArtifactID: a.ArtifactID,
		Type:       EventTypeCreate,
		Timestamp:  now,
		ParentHash: "",
	}
	require.NoError(t, s.AppendEvent(a.Kind, e1))
	e1Hash, err := s.HeadHashByBranch(a.Kind, a.ArtifactID, MainBranch)
	require.NoError(t, err)

	// Fork onto "alt" so the index has more than just main, and carries
	// fork-derived metadata that must compare equal across refreshes.
	fork := Event{
		EventID:          "evt-fork",
		ArtifactID:       a.ArtifactID,
		Type:             EventTypeForkOuter,
		Timestamp:        now.Add(time.Second),
		ParentHash:       e1Hash,
		Branch:           "alt",
		ForkSourceBranch: MainBranch,
		ForkFromEventID:  e1.EventID,
		ForkOriginAgent:  "codex",
	}
	require.NoError(t, s.AppendEvent(a.Kind, fork))

	// First refresh materialises the index file on disk.
	_, err = s.RefreshBranchIndex(a.Kind, a.ArtifactID)
	require.NoError(t, err)

	path := s.branchIndexPath(a.Kind, a.ArtifactID)
	require.FileExists(t, path)

	// Push the mtime into the past so a coarse filesystem clock can't mask a
	// rewrite, then snapshot the mtime.
	past := now.Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))
	mtimeBefore := fileMtime(t, path)

	// Second refresh with NO intervening change must be a no-op on disk.
	_, err = s.RefreshBranchIndex(a.Kind, a.ArtifactID)
	require.NoError(t, err)

	mtimeAfter := fileMtime(t, path)
	require.True(t, mtimeBefore.Equal(mtimeAfter),
		"branch index mtime changed (%v -> %v): file was rewritten on an unchanged refresh", mtimeBefore, mtimeAfter)
}
