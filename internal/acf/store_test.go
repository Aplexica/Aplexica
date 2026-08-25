package acf

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

func newTestArtifact(id string) Artifact {
	return Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindMemory,
		Scope:            ScopeProject,
		Name:             "CLAUDE.md",
		CreatedAt:        time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC),
	}
}

func TestStore_RoundTripArtifact(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	got, err := s.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, a, got)
}

func TestStore_HasEventIDBuildsBoundedIndexAndTracksAppends(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	art := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(art))

	first := Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Payload:    []byte(`{"format":"markdown","content":"seed"}`),
	}
	require.NoError(t, s.AppendEvent(KindMemory, first))
	found, err := s.HasEventID(KindMemory, art.ArtifactID, first.EventID)
	require.NoError(t, err)
	require.True(t, found)
	found, err = s.HasEventID(KindMemory, art.ArtifactID, "missing")
	require.NoError(t, err)
	require.False(t, found)

	stored, ok, err := s.LastEvent(KindMemory, art.ArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	second := Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		ParentHash: stored.Hash,
		Payload:    []byte(`{"format":"markdown","content":"next"}`),
	}
	require.NoError(t, s.AppendEvent(KindMemory, second))
	found, err = s.HasEventID(KindMemory, art.ArtifactID, second.EventID)
	require.NoError(t, err)
	require.True(t, found, "an index loaded before AppendEvent must be updated in O(1)")
}

func TestStore_FindRecentEventIdentityIsBoundedAndSkipsPayload(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	art := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(art))
	stored := make([]Event, 0, 3)
	parent := ""
	for index := 0; index < 3; index++ {
		payload := []byte(`{"format":"markdown","content":"` + strings.Repeat("x", 96<<10) + `,\"hash\":\"` + strings.Repeat("f", 64) + `"}`)
		event := Event{
			EventID: NewID(), ArtifactID: art.ArtifactID, Type: EventTypeUpdate,
			Timestamp: time.Now().UTC().Add(time.Duration(index) * time.Millisecond), ParentHash: parent, Payload: payload,
		}
		if index == 0 {
			event.Type = EventTypeCreate
		}
		require.NoError(t, s.AppendEvent(KindMemory, event))
		head, ok, err := s.LastEvent(KindMemory, art.ArtifactID)
		require.NoError(t, err)
		require.True(t, ok)
		stored = append(stored, head)
		parent = head.Hash
	}

	found, ok, err := s.FindRecentEventIdentity(KindMemory, art.ArtifactID, stored[1].EventID, 2, 1<<20)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, stored[1].EventID, found.EventID)
	require.Equal(t, stored[1].Hash, found.Hash, "payload lookalikes must not substitute the top-level canonical hash")

	_, ok, err = s.FindRecentEventIdentity(KindMemory, art.ArtifactID, stored[0].EventID, 2, 1<<20)
	require.NoError(t, err)
	require.False(t, ok, "the event-count budget must stop before older history")
}

func TestAppendEventFailsBeforeMetadataCommitWhenDurabilitySyncFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Store)
	}{
		{
			name: "event file",
			set: func(store *Store) {
				store.eventFileSync = func(*os.File) error { return errors.New("injected file sync failure") }
			},
		},
		{
			name: "event close",
			set: func(store *Store) {
				store.eventFileClose = func(*os.File) error { return errors.New("injected close failure") }
			},
		},
		{
			name: "event leaf directory",
			set: func(store *Store) {
				store.eventDirSync = func(_ *privatefs.Root, dir string) error {
					if dir == filepath.Join("events", "memories") {
						return errors.New("injected leaf directory sync failure")
					}
					return nil
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &Store{Root: filepath.Join(t.TempDir(), "store")}
			require.NoError(t, store.Init())
			artifact := newTestArtifact(NewID())
			require.NoError(t, store.WriteArtifact(artifact))
			tc.set(store)

			err := store.AppendEvent(KindMemory, Event{
				EventID: NewID(), ArtifactID: artifact.ArtifactID, Type: EventTypeCreate,
				Timestamp: time.Now().UTC(), Payload: []byte(`{"format":"markdown","content":"durable"}`),
			})
			require.Error(t, err)
			persisted, readErr := store.ReadArtifact(KindMemory, artifact.ArtifactID)
			require.NoError(t, readErr)
			require.Empty(t, persisted.HeadEventHash, "artifact metadata must not advertise an unsynced event")
			require.Zero(t, persisted.EventCount, "terminal metadata must remain behind the failed durability boundary")
		})
	}
}

func TestAppendEventCreatesAndDurablyCommitsFirstConversationPath(t *testing.T) {
	store := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	artifact := newTestArtifact(NewID())
	artifact.Kind = KindConversation
	require.NoError(t, store.WriteArtifact(artifact))
	event := Event{
		EventID: NewID(), ArtifactID: artifact.ArtifactID, Type: EventTypeCreate,
		Timestamp: time.Now().UTC(), Payload: []byte(`{"format":"markdown","content":"first conversation"}`),
	}
	require.NoError(t, store.AppendEvent(KindConversation, event))

	persisted, err := store.ReadArtifact(KindConversation, artifact.ArtifactID)
	require.NoError(t, err)
	require.NotEmpty(t, persisted.HeadEventHash)
	require.Equal(t, uint64(1), persisted.EventCount)
	events, err := store.ReadEvents(KindConversation, artifact.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, event.EventID, events[0].EventID)
}

func TestConfirmEventDurableAndRepairMetadataRerunsEveryFailedAppendBarrier(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Store, *int)
	}{
		{
			name: "file sync",
			set: func(store *Store, calls *int) {
				store.eventFileSync = func(file *os.File) error {
					(*calls)++
					if *calls == 1 {
						return errors.New("injected first file sync failure")
					}
					return file.Sync()
				}
			},
		},
		{
			name: "checked close",
			set: func(store *Store, calls *int) {
				store.eventFileClose = func(file *os.File) error {
					(*calls)++
					if *calls == 1 {
						return errors.New("injected first close failure")
					}
					return file.Close()
				}
			},
		},
		{
			name: "leaf directory sync",
			set: func(store *Store, calls *int) {
				store.eventDirSync = func(root *privatefs.Root, dir string) error {
					(*calls)++
					if *calls == 1 {
						return errors.New("injected first directory sync failure")
					}
					return root.SyncDir(dir)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &Store{Root: filepath.Join(t.TempDir(), "store")}
			require.NoError(t, store.Init())
			artifact := newTestArtifact(NewID())
			require.NoError(t, store.WriteArtifact(artifact))
			event := Event{
				EventID: NewID(), ArtifactID: artifact.ArtifactID, Type: EventTypeCreate,
				Timestamp: time.Now().UTC(), Payload: []byte(`{"format":"markdown","content":"visible before terminal"}`),
			}
			event.Hash, _ = ComputeHash(event)
			calls := 0
			tc.set(store, &calls)
			require.Error(t, store.AppendEvent(KindMemory, event))
			visible, ok, err := store.LastEvent(KindMemory, artifact.ArtifactID)
			require.NoError(t, err)
			require.True(t, ok, "failed barrier fixture must leave bytes visible for redelivery")
			require.Equal(t, event.EventID, visible.EventID)
			before, err := store.ReadArtifact(KindMemory, artifact.ArtifactID)
			require.NoError(t, err)
			require.Empty(t, before.HeadEventHash)

			confirmed, err := store.ConfirmEventDurableAndRepairMetadata(KindMemory, artifact.ArtifactID, event.EventID, event.Hash)
			require.NoError(t, err)
			require.Equal(t, event.EventID, confirmed.EventID)
			require.GreaterOrEqual(t, calls, 2, "redelivery must repeat the barrier that failed after bytes became visible")
			after, err := store.ReadArtifact(KindMemory, artifact.ArtifactID)
			require.NoError(t, err)
			require.Equal(t, event.Hash, after.HeadEventHash)
			require.Equal(t, event.Hash, after.BranchHeads[MainBranch])
			require.Equal(t, uint64(1), after.EventCount)
		})
	}
}

func TestConfirmEventDurableAndRepairMetadataSurvivesRestart(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "store")
	store := &Store{Root: rootPath}
	require.NoError(t, store.Init())
	artifact := newTestArtifact(NewID())
	require.NoError(t, store.WriteArtifact(artifact))
	event := Event{EventID: NewID(), ArtifactID: artifact.ArtifactID, Type: EventTypeCreate, Timestamp: time.Now().UTC(), Payload: []byte(`{"format":"markdown","content":"restart"}`)}
	event.Hash, _ = ComputeHash(event)
	store.eventDirSync = func(*privatefs.Root, string) error { return errors.New("injected pre-restart directory sync failure") }
	require.Error(t, store.AppendEvent(KindMemory, event))

	restarted := &Store{Root: rootPath}
	fileSyncs, closes, dirSyncs := 0, 0, 0
	restarted.eventFileSync = func(file *os.File) error { fileSyncs++; return file.Sync() }
	restarted.eventFileClose = func(file *os.File) error { closes++; return file.Close() }
	restarted.eventDirSync = func(root *privatefs.Root, dir string) error { dirSyncs++; return root.SyncDir(dir) }
	_, err := restarted.ConfirmEventDurableAndRepairMetadata(KindMemory, artifact.ArtifactID, event.EventID, event.Hash)
	require.NoError(t, err)
	require.Equal(t, 1, fileSyncs)
	require.Equal(t, 1, closes)
	require.Equal(t, 1, dirSyncs)
	repaired, err := restarted.ReadArtifact(KindMemory, artifact.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, event.Hash, repaired.HeadEventHash)
	require.Equal(t, uint64(1), repaired.EventCount)
}

func TestConfirmOlderVisibleEventRepairsToConcurrentCanonicalTail(t *testing.T) {
	store := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	artifact := newTestArtifact(NewID())
	require.NoError(t, store.WriteArtifact(artifact))
	first := Event{EventID: NewID(), ArtifactID: artifact.ArtifactID, Type: EventTypeCreate, Timestamp: time.Now().UTC(), Payload: []byte(`{"format":"markdown","content":"first"}`)}
	first.Hash, _ = ComputeHash(first)
	dirCalls := 0
	store.eventDirSync = func(root *privatefs.Root, dir string) error {
		dirCalls++
		if dirCalls == 1 {
			return errors.New("injected first directory sync failure")
		}
		return root.SyncDir(dir)
	}
	require.Error(t, store.AppendEvent(KindMemory, first))
	second := Event{
		EventID: NewID(), ArtifactID: artifact.ArtifactID, Type: EventTypeUpdate, Timestamp: first.Timestamp.Add(time.Second),
		ParentHash: first.Hash, Payload: []byte(`{"format":"markdown","content":"newer concurrent tail"}`),
	}
	second.Hash, _ = ComputeHash(second)
	require.NoError(t, store.AppendEvent(KindMemory, second), "a successor that wins before redelivery must remain canonical")
	stale, err := store.ReadArtifact(KindMemory, artifact.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, second.Hash, stale.HeadEventHash)
	require.Equal(t, uint64(1), stale.EventCount, "failed predecessor metadata is deliberately still missing")

	confirmed, err := store.ConfirmEventDurableAndRepairMetadata(KindMemory, artifact.ArtifactID, first.EventID, first.Hash)
	require.NoError(t, err)
	require.Equal(t, first.EventID, confirmed.EventID)
	repaired, err := store.ReadArtifact(KindMemory, artifact.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, second.Hash, repaired.HeadEventHash, "older redelivery must never rewind a newer tail")
	require.Equal(t, second.Hash, repaired.BranchHeads[MainBranch])
	require.Equal(t, uint64(2), repaired.EventCount)
}

func TestAppendEvent_ConcurrentSiblingWritersSerializeHeadValidation(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	art := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(art))
	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Payload:    []byte(`{"format":"markdown","content":"seed"}`),
	}))
	head, ok, err := s.LastEvent(KindMemory, art.ArtifactID)
	require.NoError(t, err)
	require.True(t, ok)

	const writers = 16
	start := make(chan struct{})
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- s.AppendEvent(KindMemory, Event{
				EventID:    NewID(),
				ArtifactID: art.ArtifactID,
				Type:       EventTypeUpdate,
				Timestamp:  time.Now().UTC(),
				ParentHash: head.Hash,
				Payload:    []byte(`{"format":"markdown","content":"racing update"}`),
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded := 0
	rejected := 0
	for appendErr := range results {
		switch {
		case appendErr == nil:
			succeeded++
		case errors.Is(appendErr, ErrHeadMismatch):
			rejected++
		default:
			require.NoError(t, appendErr)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, writers-1, rejected)
	events, err := s.ReadEvents(KindMemory, art.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 2, "only one sibling may pass the parent/head transaction")
}

func TestAppendEventWithMaterializedBranch_StaleParentCannotResetConcurrentHead(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	art := newTestArtifact(NewID())
	art.Kind = KindConversation
	art.Name = "rollout.jsonl"
	require.NoError(t, s.WriteArtifact(art))
	require.NoError(t, s.AppendEvent(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Payload:    []byte(`{"format":"acf.conversation.v1","events":[]}`),
	}))
	refreshed, ok, err := s.LastEvent(KindConversation, art.ArtifactID)
	require.NoError(t, err)
	require.True(t, ok)

	// This append lands after a continuation importer has refreshed its parent
	// but before it submits the event. The old importer wrote its stale Artifact
	// snapshot here, resetting bookkeeping to refreshed.Hash and allowing a
	// sibling event to pass. The locked API must reject that stale parent and
	// preserve the peer head.
	require.NoError(t, s.AppendEvent(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		ParentHash: refreshed.Hash,
		Payload:    []byte(`{"format":"acf.conversation.delta.v1","events":[]}`),
	}))
	peerHead, ok, err := s.LastEvent(KindConversation, art.ArtifactID)
	require.NoError(t, err)
	require.True(t, ok)

	err = s.AppendEventWithMaterializedBranch(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		ParentHash: refreshed.Hash,
		Payload:    []byte(`{"format":"acf.conversation.delta.v1","events":[]}`),
	}, "codex", MainBranch)
	require.ErrorIs(t, err, ErrHeadMismatch)

	afterReject, err := s.ReadArtifact(KindConversation, art.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, peerHead.Hash, afterReject.HeadEventHash)
	require.Empty(t, afterReject.MaterializedBranchByAgent)

	// Retrying from the fresh parent commits both the event and branch marker in
	// the same append-locked artifact write.
	require.NoError(t, s.AppendEventWithMaterializedBranch(KindConversation, Event{
		EventID:    NewID(),
		ArtifactID: art.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		ParentHash: peerHead.Hash,
		Payload:    []byte(`{"format":"acf.conversation.delta.v1","events":[]}`),
	}, "codex", MainBranch))
	finalHead, ok, err := s.LastEvent(KindConversation, art.ArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	finalArt, err := s.ReadArtifact(KindConversation, art.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, finalHead.Hash, finalArt.HeadEventHash)
	require.Equal(t, MainBranch, finalArt.MaterializedBranchByAgent["codex"])
	events, err := s.ReadEvents(KindConversation, art.ArtifactID)
	require.NoError(t, err)
	require.NoError(t, VerifyChain(events))
}

func TestStore_ReadArtifactRetriesConcurrentAtomicReplacement(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	const iterations = 1_000
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range iterations {
			next := a
			next.Name = fmt.Sprintf("memory-%d", i)
			if err := s.WriteArtifact(next); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			if _, err := s.ReadArtifact(KindMemory, a.ArtifactID); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err,
			"a trusted atomic replacement may invalidate an opened old inode, but a fresh retained-root open must succeed")
	}
}

// TestReadEvents_EventLineLargerThanLegacyCap is the regression test for the
// `aplexica list`/`show`/`log` crash "acf: scan events <ns>: bufio.Scanner:
// token too long". A single conversation event can approach the max-artifact-
// size (default 64 MiB), but the event-log scanner capped a line at 4 MiB, so
// reading any artifact with one large event aborted the whole command. The
// scanner cap must exceed the largest event the store could legitimately hold.
func TestReadEvents_EventLineLargerThanLegacyCap(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	path := s.eventsPath(KindConversation, "huge-event-artifact")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	// One JSON event line larger than the legacy 4 MiB cap. The field name is
	// deliberately not part of Event, so json.Unmarshal ignores it — the test
	// exercises the scanner, not event semantics.
	big := strings.Repeat("x", 5*1024*1024) // 5 MiB > legacy 4 MiB cap
	line := `{"x_oversized_padding":"` + big + `"}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))

	events, err := s.ReadEvents(KindConversation, "huge-event-artifact")
	require.NoError(t, err,
		"an event line larger than the legacy 4 MiB scanner cap must read without 'token too long'")
	require.Len(t, events, 1)
}

func TestReadRecentEvents_ReadsOnlyMatchingTailEvents(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))
	now := time.Now().UTC()
	appendEvent := func(eventType EventType, source, content string) {
		t.Helper()
		art, err := s.ReadArtifact(KindMemory, a.ArtifactID)
		require.NoError(t, err)
		payload, err := EncodePayload(MemoryPayload{Format: "markdown", Content: content})
		require.NoError(t, err)
		require.NoError(t, s.AppendEvent(KindMemory, Event{
			EventID:    NewID(),
			ArtifactID: a.ArtifactID,
			Type:       eventType,
			Timestamp:  now,
			Provenance: Provenance{SourceAgent: source},
			Payload:    payload,
			ParentHash: art.HeadEventHash,
		}))
		now = now.Add(time.Second)
	}
	appendEvent(EventTypeCreate, "legacy", "oldest")
	appendEvent(EventTypeUpdate, "claude-code", "first")
	appendEvent(EventTypeSnapshot, "aplexica", "bookkeeping")
	appendEvent(EventTypeUpdate, "codex", "second")
	appendEvent(EventTypeMerge, "aplexica", "bookkeeping")

	// Damage the oldest line after all appends. A whole-log replay now fails,
	// while a bounded request for the two latest content events must never read
	// far enough back to encounter it.
	path := s.eventsPath(KindMemory, a.ArtifactID)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	firstNewline := strings.IndexByte(string(data), '\n')
	require.Positive(t, firstNewline)
	data = append([]byte("{not-json}\n"), data[firstNewline+1:]...)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	events, err := s.ReadRecentEvents(
		KindMemory,
		a.ArtifactID,
		2,
		EventTypeCreate,
		EventTypeUpdate,
		EventTypeResolution,
	)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "claude-code", events[0].Provenance.SourceAgent)
	require.Equal(t, EventTypeUpdate, events[0].Type)
	require.Equal(t, "codex", events[1].Provenance.SourceAgent)
	require.Equal(t, EventTypeUpdate, events[1].Type)

	_, err = s.ReadEvents(KindMemory, a.ArtifactID)
	require.Error(t, err, "the test fixture must prove a whole-log replay reaches the damaged prefix")
}

type countingStringReader struct {
	reader *strings.Reader
	read   int
}

func (r *countingStringReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestDecodeEventHeader_DoesNotReadLargePayload(t *testing.T) {
	timestamp := "2026-07-16T16:00:00Z"
	line := `{"eventId":"event-1","artifactId":"artifact-1","type":"update","timestamp":"` +
		timestamp +
		`","provenance":{"deviceId":"device-1","sourceAgent":"codex","agentVersion":"unknown","adapterVersion":"1.0.0"},"payload":{"content":"` +
		strings.Repeat("x", 8*1024*1024) +
		`"},"parentHash":"parent","hash":"hash"}`
	reader := &countingStringReader{reader: strings.NewReader(line)}

	event, err := decodeEventHeader(reader)
	require.NoError(t, err)
	require.Equal(t, "event-1", event.EventID)
	require.Equal(t, EventTypeUpdate, event.Type)
	require.Equal(t, "codex", event.Provenance.SourceAgent)
	require.Nil(t, event.Payload)
	require.Less(t, reader.read, 64*1024,
		"metadata-only decode must stop before reading a multi-megabyte payload")
}

func TestReadRecentEventHeaders_BeforeAndBoundaryTies(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))
	base := time.Date(2026, 7, 16, 16, 0, 0, 0, time.UTC)
	appendAt := func(timestamp time.Time, source string) {
		t.Helper()
		artifact, err := s.ReadArtifact(KindMemory, a.ArtifactID)
		require.NoError(t, err)
		payload, err := EncodePayload(MemoryPayload{
			Format:  "markdown",
			Content: strings.Repeat("payload", 1024),
		})
		require.NoError(t, err)
		require.NoError(t, s.AppendEvent(KindMemory, Event{
			EventID:    NewID(),
			ArtifactID: a.ArtifactID,
			Type:       EventTypeUpdate,
			Timestamp:  timestamp,
			Provenance: Provenance{SourceAgent: source},
			Payload:    payload,
			ParentHash: artifact.HeadEventHash,
		}))
	}
	appendAt(base, "old")
	appendAt(base.Add(time.Second), "tie-a")
	appendAt(base.Add(time.Second), "tie-b")
	appendAt(base.Add(2*time.Second), "new")

	beforeNewest := base.Add(2*time.Second).UnixNano() / int64(time.Millisecond)
	events, err := s.ReadRecentEventHeaders(KindMemory, a.ArtifactID, beforeNewest, 1)
	require.NoError(t, err)
	require.Len(t, events, 2, "the complete same-millisecond boundary group must be returned")
	require.Equal(t, "tie-a", events[0].Provenance.SourceAgent)
	require.Equal(t, "tie-b", events[1].Provenance.SourceAgent)
	for _, event := range events {
		require.Nil(t, event.Payload)
		require.NotEmpty(t, event.EventID)
		require.Equal(t, base.Add(time.Second), event.Timestamp)
	}
}

func TestLastEvent_ReturnsLastNonEmptyLine(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	e1 := Event{
		EventID:    NewID(),
		ArtifactID: a.ArtifactID,
		Type:       EventTypeCreate,
		Timestamp:  time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC),
		Payload:    []byte(`{"format":"markdown","content":"one"}`),
	}
	require.NoError(t, s.AppendEvent(KindMemory, e1))
	parent, err := s.HeadHashByBranch(KindMemory, a.ArtifactID, MainBranch)
	require.NoError(t, err)

	e2 := Event{
		EventID:    NewID(),
		ArtifactID: a.ArtifactID,
		Type:       EventTypeUpdate,
		Timestamp:  time.Date(2026, 6, 29, 9, 1, 0, 0, time.UTC),
		ParentHash: parent,
		Payload:    []byte(`{"format":"markdown","content":"two"}`),
	}
	require.NoError(t, s.AppendEvent(KindMemory, e2))

	f, err := os.OpenFile(s.eventsPath(KindMemory, a.ArtifactID), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("\n\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, ok, err := s.LastEvent(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, e2.EventID, got.EventID)
	require.Equal(t, e2.Payload, got.Payload)
	require.NotEmpty(t, got.Hash)
	artifact, err := s.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), artifact.EventCount)

	count, err := s.EventCount(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), count)
}

func TestLastEvent_MissingLog(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	_, ok, err := s.LastEvent(KindMemory, "missing")
	require.NoError(t, err)
	require.False(t, ok)

	count, err := s.EventCount(KindMemory, "missing")
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestEventLogSize(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	require.NoError(t, s.Init())
	id := NewID()
	now := time.Now().UTC()
	require.NoError(t, s.WriteArtifact(Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindMemory,
		Scope:            ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "size me"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID:    NewID(),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Timestamp:  now,
		Payload:    payload,
	}))

	size, err := s.EventLogSize(KindMemory, id)
	require.NoError(t, err)
	require.Positive(t, size)
}

// TestFindByNativeID_ResolvesConversationSessionID is the regression test for
// the `aplexica show`/`log` trap: those commands resolved only by the daemon-
// assigned ArtifactID, so a native Claude Code session-id (the .jsonl basename,
// e.g. fbcdc154-...) returned "not found" even though the conversation was fully
// stored — the session-id lives in Name/SourcePath, not the id.
func TestFindByNativeID_ResolvesConversationSessionID(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	sessionID := "fbcdc154-c8be-4787-9158-781d269d176d"
	a := Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       NewID(), // daemon-assigned UUIDv7, deliberately != sessionID
		Kind:             KindConversation,
		Scope:            ScopeProject,
		Name:             sessionID + ".jsonl",
		SourcePath:       "/Users/exampleuser/.claude/projects/-Users-exampleuser-x/" + sessionID + ".jsonl",
		CreatedAt:        time.Date(2026, 6, 24, 20, 24, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 6, 24, 20, 24, 0, 0, time.UTC),
	}
	require.NoError(t, s.WriteArtifact(a))

	// Bare native session-id (the .jsonl basename without extension).
	k, got, found, err := s.FindByNativeID(sessionID)
	require.NoError(t, err)
	require.True(t, found, "a native session-id must resolve to its stored conversation")
	require.Equal(t, KindConversation, k)
	require.Equal(t, a.ArtifactID, got.ArtifactID)

	// Namespaced form ("conversation/<sessionID>") must also resolve.
	_, got2, found2, err := s.FindByNativeID("conversation/" + sessionID)
	require.NoError(t, err)
	require.True(t, found2)
	require.Equal(t, a.ArtifactID, got2.ArtifactID)

	// With the .jsonl extension included.
	_, _, found3, err := s.FindByNativeID(sessionID + ".jsonl")
	require.NoError(t, err)
	require.True(t, found3, "the basename with extension must also resolve")

	// The real ArtifactID still resolves (back-compat with direct lookups).
	_, _, found4, err := s.FindByNativeID(a.ArtifactID)
	require.NoError(t, err)
	require.True(t, found4)

	// A genuine miss returns found=false, no error.
	_, _, found5, err := s.FindByNativeID("00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	require.False(t, found5)
}

// TestStore_RoundTripArtifact_WithProject (v0.54.0; BRD-02 §4.13)
// asserts the new Project field survives a write-read cycle and that
// nil Project (the pre-v0.54.0 case) continues to round-trip cleanly.
func TestStore_RoundTripArtifact_WithProject(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	a.Project = &project.ProjectInfo{
		ID:   "github.com/example-user/sample-project",
		Path: "/home/example-user/code/sample-project",
		VCS:  "git",
	}
	require.NoError(t, s.WriteArtifact(a))

	got, err := s.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.NotNil(t, got.Project)
	require.Equal(t, "github.com/example-user/sample-project", got.Project.ID)
	require.Equal(t, "/home/example-user/code/sample-project", got.Project.Path)
	require.Equal(t, "git", got.Project.VCS)
	require.False(t, got.Project.Ephemeral)
	// Full struct equality — nothing else mutated.
	require.Equal(t, a, got)
}

// TestStore_RoundTripArtifact_NilProject_OmitsField asserts that
// an artifact with Project=nil round-trips as nil (not as an empty
// struct), preserving the omitempty wire-shape compactness.
func TestStore_RoundTripArtifact_NilProject_OmitsField(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.Nil(t, a.Project, "default Artifact has no Project")
	require.NoError(t, s.WriteArtifact(a))

	got, err := s.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Nil(t, got.Project, "read-back artifact should also have nil Project")
}

func TestStore_AppendEvent_BuildsChain(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	e1 := newTestEvent("")
	e1.ArtifactID = a.ArtifactID
	e1.EventID = NewID()
	require.NoError(t, s.AppendEvent(KindMemory, e1))

	head, err := s.HeadHash(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.NotEmpty(t, head)

	e2 := newTestEvent(head)
	e2.ArtifactID = a.ArtifactID
	e2.EventID = NewID()
	require.NoError(t, s.AppendEvent(KindMemory, e2))

	events, err := s.ReadEvents(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.NoError(t, VerifyChain(events))
}

func TestStore_AppendEvent_RejectsBadParent(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	e1 := newTestEvent("")
	e1.ArtifactID = a.ArtifactID
	e1.EventID = NewID()
	require.NoError(t, s.AppendEvent(KindMemory, e1))

	bad := newTestEvent("wrong-parent")
	bad.ArtifactID = a.ArtifactID
	bad.EventID = NewID()
	require.Error(t, s.AppendEvent(KindMemory, bad))
}

func TestStore_ReadArtifact_NotFound(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	_, err := s.ReadArtifact(KindMemory, "does-not-exist")
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist),
		"ReadArtifact must wrap the underlying os.ErrNotExist for caller introspection")
}

func TestStore_HeadHash_EmptyLog(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	head, err := s.HeadHash(KindMemory, "any-id")
	require.NoError(t, err)
	require.Equal(t, "", head, "HeadHash returns empty when no events file exists")
}

func TestStore_WritesSkillArtifactWithLazyDir(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init()) // only creates the memories dirs in current code

	// Write a skill artifact — the skills directory does not exist yet.
	a := Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       NewID(),
		Kind:             KindSkill,
		Scope:            ScopeProject,
		Name:             "SKILL.md",
		CreatedAt:        time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, s.WriteArtifact(a),
		"WriteArtifact must lazy-create the kind directory if Init didn't")

	got, err := s.ReadArtifact(KindSkill, a.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, KindSkill, got.Kind)
}

func TestStore_ListArtifacts_Empty(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	got, err := s.ListArtifacts(KindMemory)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestStore_ListArtifacts_ReturnsWrittenArtifacts(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	// Write 3 artifacts: 2 memory, 1 skill.
	m1 := newTestArtifact(NewID())
	m1.Name = "first.md"
	require.NoError(t, s.WriteArtifact(m1))

	m2 := newTestArtifact(NewID())
	m2.Name = "second.md"
	require.NoError(t, s.WriteArtifact(m2))

	sk := newTestArtifact(NewID())
	sk.Kind = KindSkill
	sk.Name = "a-skill.md"
	require.NoError(t, s.WriteArtifact(sk))

	memos, err := s.ListArtifacts(KindMemory)
	require.NoError(t, err)
	require.Len(t, memos, 2)
	names := []string{memos[0].Name, memos[1].Name}
	require.Contains(t, names, "first.md")
	require.Contains(t, names, "second.md")

	skills, err := s.ListArtifacts(KindSkill)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	require.Equal(t, "a-skill.md", skills[0].Name)
}

func TestStore_ListArtifacts_SortedByCreatedAt(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	earlier := newTestArtifact(NewID())
	earlier.Name = "early.md"
	earlier.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, s.WriteArtifact(earlier))

	later := newTestArtifact(NewID())
	later.Name = "late.md"
	later.CreatedAt = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, s.WriteArtifact(later))

	got, err := s.ListArtifacts(KindMemory)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "early.md", got[0].Name, "earlier CreatedAt should come first")
	require.Equal(t, "late.md", got[1].Name)
}

func TestStore_ListArtifacts_HandlesNonexistentKindDir(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	// KindTool's directory has never been created in this store.
	got, err := s.ListArtifacts(KindTool)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestStore_DeleteArtifact(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	payload, _ := EncodePayload(MemoryPayload{Format: "markdown", Content: "x"})
	e := Event{
		EventID:    NewID(),
		ArtifactID: a.ArtifactID,
		Type:       "create",
		Timestamp:  time.Now().UTC(),
		Provenance: Provenance{DeviceID: "d", SourceAgent: "t", AdapterVersion: "0"},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, s.AppendEvent(KindMemory, e))

	// Sanity: both files exist before delete.
	_, err := s.ReadArtifact(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	events, err := s.ReadEvents(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)

	// Delete.
	require.NoError(t, s.DeleteArtifact(KindMemory, a.ArtifactID))

	// After delete: artifact read returns os.ErrNotExist; ReadEvents returns nil, nil.
	_, err = s.ReadArtifact(KindMemory, a.ArtifactID)
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist))

	events, err = s.ReadEvents(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestStore_DeleteArtifact_NotFound(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	err := s.DeleteArtifact(KindMemory, "01956a39-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrNotExist),
		"delete of non-existent artifact must surface os.ErrNotExist for caller introspection")
}

func TestStore_DeleteArtifact_RemovesOnlyTargetedArtifact(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	keep := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(keep))

	target := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(target))

	require.NoError(t, s.DeleteArtifact(KindMemory, target.ArtifactID))

	// The other artifact should be untouched.
	_, err := s.ReadArtifact(KindMemory, keep.ArtifactID)
	require.NoError(t, err)
}

func TestStore_AppendsSkillEventWithLazyDir(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       NewID(),
		Kind:             KindSkill,
		Scope:            ScopeProject,
		Name:             "SKILL.md",
		CreatedAt:        time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, s.WriteArtifact(a))

	payload, _ := EncodePayload(SkillPayload{Format: "skill.md", Content: "x"})
	e := Event{
		EventID:    NewID(),
		ArtifactID: a.ArtifactID,
		Type:       "create",
		Timestamp:  time.Now().UTC(),
		Provenance: Provenance{DeviceID: "dev", SourceAgent: "claude-code", AdapterVersion: "0.2.0"},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, s.AppendEvent(KindSkill, e),
		"AppendEvent must lazy-create the events/skills dir")
}

func TestStore_FindBySourcePath_NoMatch(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	got, found, err := s.FindBySourcePath(KindMemory, "/some/path/CLAUDE.md")
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, Artifact{}, got)
}

func TestStore_FindBySourcePath_MatchesByExactPath(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	a.SourcePath = "/home/u/proj/CLAUDE.md"
	require.NoError(t, s.WriteArtifact(a))

	got, found, err := s.FindBySourcePath(KindMemory, "/home/u/proj/CLAUDE.md")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, a.ArtifactID, got.ArtifactID)
}

func TestStore_FindBySourcePath_PrefersNewestDuplicate(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	sourcePath := "/home/u/.claude/projects/demo/session.jsonl"
	stale := newTestArtifact("stale")
	stale.SourcePath = sourcePath
	stale.CreatedAt = time.Date(2026, 7, 6, 13, 13, 47, 0, time.UTC)
	stale.UpdatedAt = stale.CreatedAt
	require.NoError(t, s.WriteArtifact(stale))

	live := newTestArtifact("live")
	live.SourcePath = sourcePath
	live.CreatedAt = stale.CreatedAt.Add(time.Second)
	live.UpdatedAt = stale.CreatedAt.Add(time.Hour)
	require.NoError(t, s.WriteArtifact(live))

	got, found, err := s.FindBySourcePath(KindMemory, sourcePath)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, live.ArtifactID, got.ArtifactID)
}

func TestStore_FindBySourcePath_IgnoresEmptySourcePath(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	pre := newTestArtifact(NewID())
	require.Empty(t, pre.SourcePath)
	require.NoError(t, s.WriteArtifact(pre))

	got, found, err := s.FindBySourcePath(KindMemory, "")
	require.NoError(t, err)
	require.False(t, found, "FindBySourcePath('') must NOT match pre-v0.2.0 artifacts")
	require.Equal(t, Artifact{}, got)
}

func TestStore_FindBySourcePath_ScopedToKind(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	mem := newTestArtifact(NewID())
	mem.SourcePath = "/shared/path"
	require.NoError(t, s.WriteArtifact(mem))

	sk := newTestArtifact(NewID())
	sk.Kind = KindSkill
	sk.SourcePath = "/shared/path"
	require.NoError(t, s.WriteArtifact(sk))

	gotMem, foundMem, err := s.FindBySourcePath(KindMemory, "/shared/path")
	require.NoError(t, err)
	require.True(t, foundMem)
	require.Equal(t, mem.ArtifactID, gotMem.ArtifactID)

	gotSk, foundSk, err := s.FindBySourcePath(KindSkill, "/shared/path")
	require.NoError(t, err)
	require.True(t, foundSk)
	require.Equal(t, sk.ArtifactID, gotSk.ArtifactID)

	_, foundTool, err := s.FindBySourcePath(KindTool, "/shared/path")
	require.NoError(t, err)
	require.False(t, foundTool)
}

func TestAppendEvent_RedactionSetsTombstoned(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	id := NewID()
	now := time.Now().UTC()
	art := Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindMemory,
		Name:             "t",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, s.WriteArtifact(art))

	createPayload, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "v1"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID:    NewID(),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Timestamp:  now,
		Payload:    createPayload,
	}))

	got, err := s.ReadArtifact(KindMemory, id)
	require.NoError(t, err)
	require.False(t, got.Tombstoned)

	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID:    NewID(),
		ArtifactID: id,
		Type:       EventTypeRedaction,
		Timestamp:  now,
		Payload:    nil,
		ParentHash: got.HeadEventHash,
	}))

	got, err = s.ReadArtifact(KindMemory, id)
	require.NoError(t, err)
	require.True(t, got.Tombstoned, "redaction event must set Tombstoned")
}

func TestAppendEvent_UpdateClearsTombstoned(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	id := NewID()
	now := time.Now().UTC()

	// Pre-seed an artifact with Tombstoned=true and a first create event so
	// the chain has a parent hash for the update event to point at.
	art := Artifact{
		AcfSchemaVersion: SchemaVersion,
		ArtifactID:       id,
		Kind:             KindMemory,
		Name:             "t",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, s.WriteArtifact(art))

	createPayload, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "v1"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID:    NewID(),
		ArtifactID: id,
		Type:       EventTypeCreate,
		Timestamp:  now,
		Payload:    createPayload,
	}))

	// Manually flip Tombstoned to true to simulate a prior redaction having
	// landed. The update event below must clear it.
	got, err := s.ReadArtifact(KindMemory, id)
	require.NoError(t, err)
	got.Tombstoned = true
	require.NoError(t, s.WriteArtifact(got))

	updatePayload, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "revived"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(KindMemory, Event{
		EventID:    NewID(),
		ArtifactID: id,
		Type:       EventTypeUpdate,
		Timestamp:  now,
		Payload:    updatePayload,
		ParentHash: got.HeadEventHash,
	}))

	got, err = s.ReadArtifact(KindMemory, id)
	require.NoError(t, err)
	require.False(t, got.Tombstoned, "update event must clear Tombstoned")
}

func TestStore_FindBySourcePath_MultipleArtifactsDifferentPaths(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a1 := newTestArtifact(NewID())
	a1.SourcePath = "/a/CLAUDE.md"
	require.NoError(t, s.WriteArtifact(a1))

	a2 := newTestArtifact(NewID())
	a2.SourcePath = "/b/CLAUDE.md"
	require.NoError(t, s.WriteArtifact(a2))

	got, found, err := s.FindBySourcePath(KindMemory, "/b/CLAUDE.md")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, a2.ArtifactID, got.ArtifactID, "must return the artifact with the matching SourcePath, not the other one")
}

// errIngestGateBlocked is the sentinel a gate returns to refuse an append.
var errIngestGateBlocked = errors.New("ingest blocked by test gate")

func TestAppendEvent_IngestGateRefuses(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	// First append with a nil gate (the default) must succeed — the gate is a
	// no-op until the daemon wires one.
	e1 := newTestEvent("")
	e1.ArtifactID = a.ArtifactID
	e1.EventID = NewID()
	require.NoError(t, s.AppendEvent(KindMemory, e1))

	headBefore, err := s.HeadHash(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.NotEmpty(t, headBefore)
	eventsBefore, err := s.ReadEvents(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, eventsBefore, 1)

	// Now wire a gate that refuses. The next append must return the sentinel
	// error and NOT write the event (chain/head unchanged).
	s.IngestGate = func() error { return errIngestGateBlocked }

	e2 := newTestEvent(headBefore)
	e2.ArtifactID = a.ArtifactID
	e2.EventID = NewID()
	err = s.AppendEvent(KindMemory, e2)
	require.Error(t, err)
	require.ErrorIs(t, err, errIngestGateBlocked)

	headAfter, err := s.HeadHash(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Equal(t, headBefore, headAfter, "refused append must not advance the branch head")
	eventsAfter, err := s.ReadEvents(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, eventsAfter, 1, "refused append must not write the event")
}

func TestAppendEvent_NilIngestGateIsNoop(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())
	require.Nil(t, s.IngestGate, "IngestGate must default to nil (always allow)")

	a := newTestArtifact(NewID())
	require.NoError(t, s.WriteArtifact(a))

	e := newTestEvent("")
	e.ArtifactID = a.ArtifactID
	e.EventID = NewID()
	require.NoError(t, s.AppendEvent(KindMemory, e), "nil gate must allow the append")

	events, err := s.ReadEvents(KindMemory, a.ArtifactID)
	require.NoError(t, err)
	require.Len(t, events, 1)
}
