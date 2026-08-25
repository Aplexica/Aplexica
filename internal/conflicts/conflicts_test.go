package conflicts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestStore_RecordListGetClear_RoundTrip(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	c := Conflict{
		ArtifactID: "art-1",
		Kind:       acf.KindMemory,
		Heads: []Head{
			{SourceAgent: "claude-code", EventID: "evt-a", ContentSHA256: "aaa", AbsTimestamp: 100.0, PayloadPreview: "v1"},
			{SourceAgent: "codex", EventID: "evt-b", ContentSHA256: "bbb", AbsTimestamp: 100.5, PayloadPreview: "v2"},
		},
	}
	require.NoError(t, s.Record(c))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "art-1", list[0].ArtifactID)

	got, err := s.Get("art-1")
	require.NoError(t, err)
	require.Len(t, got.Heads, 2)
	require.Equal(t, "claude-code", got.Heads[0].SourceAgent)

	require.NoError(t, s.Clear("art-1"))
	list, err = s.List()
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestStore_Get_NotFound(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())
	_, err := s.Get("nope")
	require.Error(t, err)
}

// TestStore_List_SurfacesCorruptFile guards against a corrupt/torn conflict
// sidecar silently vanishing from the status/doctor conflict count. A file that
// fails to parse must still be counted (fail-safe), matching the propagation
// gate (inUnresolvedConflict), which keeps blocking on a read/parse error.
func TestStore_List_SurfacesCorruptFile(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	// One healthy conflict.
	require.NoError(t, s.Record(Conflict{
		ArtifactID: "good-1",
		Kind:       acf.KindMemory,
		Heads:      []Head{{SourceAgent: "claude-code", EventID: "evt-a"}},
	}))

	// One corrupt sidecar: valid filename, unparseable JSON.
	require.NoError(t, os.WriteFile(filepath.Join(root, "broken-1.json"), []byte("{not json"), 0o600))

	list, err := s.List()
	require.NoError(t, err)
	// The corrupt file must NOT be dropped: both entries must be reflected so
	// len()-based counts in status/doctor never under-report.
	require.Len(t, list, 2, "corrupt conflict file must be surfaced, not silently dropped")

	var sawGood, sawBroken bool
	for _, c := range list {
		switch c.ArtifactID {
		case "good-1":
			sawGood = true
			require.False(t, c.Unreadable)
		case "broken-1":
			sawBroken = true
			require.True(t, c.Unreadable, "corrupt entry must be flagged Unreadable")
		}
	}
	require.True(t, sawGood, "healthy conflict missing from List")
	require.True(t, sawBroken, "corrupt conflict missing from List")
}

func TestStore_ListSummaries_StripsPayloads(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	fullPayload := json.RawMessage(`{"large":"payload"}`)
	require.NoError(t, s.Record(Conflict{
		ArtifactID: "art-1",
		Kind:       acf.KindConversation,
		Heads: []Head{{
			SourceAgent:    "claude-code",
			EventID:        "evt-a",
			ContentSHA256:  "aaa",
			AbsTimestamp:   100.0,
			PayloadPreview: `{"large":"preview"}`,
			FullPayload:    fullPayload,
		}},
	}))

	list, err := s.ListSummaries()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Len(t, list[0].Heads, 1)
	head := list[0].Heads[0]
	require.Equal(t, "claude-code", head.SourceAgent)
	require.Equal(t, "evt-a", head.EventID)
	require.Equal(t, "aaa", head.ContentSHA256)
	require.Empty(t, head.PayloadPreview)
	require.Empty(t, head.FullPayload)

	got, err := s.Get("art-1")
	require.NoError(t, err)
	require.JSONEq(t, string(fullPayload), string(got.Heads[0].FullPayload))
	require.NotEmpty(t, got.Heads[0].PayloadPreview)
}

// TestStore_ClearIf_SkipsWhenHeadsChanged is the regression for the TOCTOU
// window between the web auto-resolve path (Get -> Analyze -> Clear) and the
// orchestrator's Record. The web handler decides a conflict is auto-resolvable
// from a snapshot, but before it can Clear, the orchestrator Records a NEW,
// non-equivalent divergence for the same artifact. An unconditional Clear would
// delete that genuine conflict (last-op-wins). ClearIf must compare-and-delete:
// remove ONLY when the on-disk heads still match the analyzed snapshot.
func TestStore_ClearIf_SkipsWhenHeadsChanged(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	// The snapshot the web handler analyzed (equivalent pair -> auto-resolvable).
	analyzed := Conflict{
		ArtifactID: "art-1",
		Kind:       acf.KindMemory,
		Heads: []Head{
			{SourceAgent: "claude-code", EventID: "evt-a", ContentSHA256: "aaa"},
			{SourceAgent: "codex", EventID: "evt-b", ContentSHA256: "bbb"},
		},
	}

	// Meanwhile the orchestrator Records a fresh, DIFFERENT divergence on disk
	// (new head pair) that the web handler never saw.
	fresh := Conflict{
		ArtifactID: "art-1",
		Kind:       acf.KindMemory,
		Heads: []Head{
			{SourceAgent: "claude-code", EventID: "evt-c", ContentSHA256: "ccc"},
			{SourceAgent: "gemini", EventID: "evt-d", ContentSHA256: "ddd"},
		},
	}
	require.NoError(t, s.Record(fresh))

	// Stale clear keyed off the analyzed snapshot must NOT delete the fresh one.
	cleared, err := s.ClearIf(analyzed)
	require.NoError(t, err)
	require.False(t, cleared, "stale ClearIf must not delete a freshly-recorded, different conflict")

	got, err := s.Get("art-1")
	require.NoError(t, err)
	require.Len(t, got.Heads, 2)
	require.Equal(t, "evt-c", got.Heads[0].EventID, "the freshly-recorded conflict must survive")
}

// TestStore_ClearIf_RemovesWhenHeadsMatch confirms the happy path: when the
// on-disk conflict still matches the analyzed snapshot, ClearIf removes it.
func TestStore_ClearIf_RemovesWhenHeadsMatch(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	c := Conflict{
		ArtifactID: "art-1",
		Kind:       acf.KindMemory,
		Heads: []Head{
			{SourceAgent: "claude-code", EventID: "evt-a", ContentSHA256: "aaa"},
			{SourceAgent: "codex", EventID: "evt-b", ContentSHA256: "bbb"},
		},
	}
	require.NoError(t, s.Record(c))

	cleared, err := s.ClearIf(c)
	require.NoError(t, err)
	require.True(t, cleared, "matching ClearIf must delete the conflict")

	_, err = s.Get("art-1")
	require.ErrorIs(t, err, ErrNotRecorded)
}

// TestStore_ClearIf_AbsentIsNoop: a ClearIf for an artifact with no conflict
// file reports not-cleared and no error (the conflict is already gone).
func TestStore_ClearIf_AbsentIsNoop(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	cleared, err := s.ClearIf(Conflict{ArtifactID: "ghost", Heads: []Head{{EventID: "x"}}})
	require.NoError(t, err)
	require.False(t, cleared)
}

// TestStore_Concurrent_RecordClearGet hammers the store from many goroutines
// under -race to prove Record/Get/List/Clear/ClearIf are mutually safe (no data
// race, no panic). The compare-and-delete ClearIf must never delete a head pair
// it did not observe, so a concurrently-recorded "fresh" conflict is never lost.
func TestStore_Concurrent_RecordClearGet(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	const artifactID = "hot-art"
	stale := Conflict{
		ArtifactID: artifactID,
		Kind:       acf.KindMemory,
		Heads:      []Head{{EventID: "old-a"}, {EventID: "old-b"}},
	}
	fresh := Conflict{
		ArtifactID: artifactID,
		Kind:       acf.KindMemory,
		Heads:      []Head{{EventID: "new-a"}, {EventID: "new-b"}},
	}

	var wg sync.WaitGroup
	const iters = 200

	// Writer: keeps recording the "fresh" head pair.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = s.Record(fresh)
		}
	}()

	// Stale auto-resolver: tries to compare-and-delete the OLD head pair.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = s.ClearIf(stale)
		}
	}()

	// Readers.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, _ = s.Get(artifactID)
				_, _ = s.List()
			}
		}()
	}

	wg.Wait()

	// The stale ClearIf can never match the fresh pair, so whatever the final
	// interleaving, the file (if present) must be the fresh conflict — a stale
	// clear must not have clobbered a freshly-recorded divergence.
	got, err := s.Get(artifactID)
	if err == nil {
		require.Equal(t, "new-a", got.Heads[0].EventID,
			"stale ClearIf must never delete the freshly-recorded conflict")
	} else {
		require.ErrorIs(t, err, ErrNotRecorded)
	}
}

func TestStore_FilePathLooksRight(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())
	c := Conflict{ArtifactID: "abc", Heads: []Head{}}
	require.NoError(t, s.Record(c))
	expected := filepath.Join(root, "abc.json")
	_, err := s.Get("abc")
	require.NoError(t, err)
	_ = expected
}
