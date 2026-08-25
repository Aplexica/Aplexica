package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	syncd "github.com/aplexica/aplexica/internal/sync"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
	"github.com/stretchr/testify/require"
)

func TestConflictsWebResolve_AcceptRemoteHeadWritesResolutionEvent(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	conflictsDir := filepath.Join(tmp, "conflicts")

	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# local edit\n")
	store := &acf.Store{Root: storeRoot}
	localEvents, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, localEvents, 1)
	localHead := localEvents[0]

	remoteContent := strings.Repeat("remote-divergent-content ", 40)
	remotePayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: remoteContent})
	require.NoError(t, err)
	remoteEventID := acf.NewID()

	cs := &conflicts.Store{Root: conflictsDir}
	require.NoError(t, cs.Init())
	require.NoError(t, cs.Record(conflicts.Conflict{
		ArtifactID: id,
		Kind:       acf.KindMemory,
		Heads: []conflicts.Head{
			{
				SourceAgent:   localHead.Provenance.SourceAgent,
				EventID:       localHead.EventID,
				ContentSHA256: localHead.Hash,
			},
			{
				SourceAgent:    "codex",
				EventID:        remoteEventID,
				ContentSHA256:  "remote-hash",
				PayloadPreview: string(remotePayload[:200]),
				FullPayload:    json.RawMessage(remotePayload),
			},
		},
	}))

	orch, err := syncd.NewOrchestrator(syncd.Config{
		Dir:           t.TempDir(),
		Store:         store,
		LocalDeviceID: "local-device",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, orch.Close()) })
	acc := &conflictsWebAccessor{deps: &webAPIDeps{store: store, conf: cs, orch: orch}}
	require.NoError(t, acc.Resolve(id, apiweb.ResolveAcceptB, ""))

	final, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	var resolution *acf.Event
	for i := range final {
		if final[i].Type == acf.EventTypeResolution {
			resolution = &final[i]
		}
	}
	require.NotNil(t, resolution, "expected web resolve to append a resolution event")
	require.Equal(t, "aplexica:web-resolve", resolution.Provenance.SourceAgent)
	require.Equal(t, "local-device", resolution.Provenance.DeviceID)
	require.JSONEq(t, string(remotePayload), string(resolution.Payload))
	require.NoError(t, acf.VerifyChain(final))

	_, err = cs.Get(id)
	require.ErrorIs(t, err, conflicts.ErrNotRecorded)
}

func TestConflictsWebResolve_ManualMemoryWritesResolutionEvent(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	conflictsDir := filepath.Join(tmp, "conflicts")

	id, _, _ := seedTwoHeadConflict(t, storeRoot, conflictsDir)
	store := &acf.Store{Root: storeRoot}
	cs := &conflicts.Store{Root: conflictsDir}

	acc := &conflictsWebAccessor{deps: &webAPIDeps{store: store, conf: cs}}
	require.NoError(t, acc.Resolve(id, apiweb.ResolveManual, "# merged manually\n"))

	final, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	var resolution *acf.Event
	for i := range final {
		if final[i].Type == acf.EventTypeResolution {
			resolution = &final[i]
		}
	}
	require.NotNil(t, resolution)
	payload, err := acf.DecodeMemoryPayload(*resolution)
	require.NoError(t, err)
	require.Equal(t, "markdown", payload.Format)
	require.Equal(t, "# merged manually\n", payload.Content)
}

func TestConflictsWebResolve_MissingArtifactRecreatesShellFromSidecarPayload(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	conflictsDir := filepath.Join(tmp, "conflicts")

	store := &acf.Store{Root: storeRoot}
	id := acf.NewID()
	winnerPayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "# recovered\n"})
	require.NoError(t, err)

	cs := &conflicts.Store{Root: conflictsDir}
	require.NoError(t, cs.Init())
	require.NoError(t, cs.Record(conflicts.Conflict{
		ArtifactID: id,
		Kind:       acf.KindMemory,
		Heads: []conflicts.Head{
			{SourceAgent: "kilo", EventID: "missing-a", ContentSHA256: "hash-a"},
			{
				SourceAgent:   "codex",
				EventID:       "missing-b",
				ContentSHA256: "hash-b",
				FullPayload:   json.RawMessage(winnerPayload),
			},
		},
	}))

	acc := &conflictsWebAccessor{deps: &webAPIDeps{store: store, conf: cs}}
	require.NoError(t, acc.Resolve(id, apiweb.ResolveAcceptB, ""))

	art, err := store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, art.Kind)
	require.Equal(t, acf.ScopeGlobal, art.Scope)
	require.NotEmpty(t, art.HeadEventHash)

	final, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, final, 1)
	require.Equal(t, acf.EventType(acf.EventTypeResolution), final[0].Type)
	require.Empty(t, final[0].ParentHash)
	require.JSONEq(t, string(winnerPayload), string(final[0].Payload))

	_, err = cs.Get(id)
	require.ErrorIs(t, err, conflicts.ErrNotRecorded)
}

func TestConflictsWebGet_AutoResolvableClearsButReturnsSnapshot(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	cs := &conflicts.Store{Root: filepath.Join(tmp, "conflicts")}
	require.NoError(t, cs.Init())

	payload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "# same text\n"})
	require.NoError(t, err)
	id := acf.NewID()
	require.NoError(t, cs.Record(conflicts.Conflict{
		ArtifactID: id,
		Kind:       acf.KindMemory,
		Heads: []conflicts.Head{
			{SourceAgent: "claude-code", EventID: "a", ContentSHA256: "hash-a", FullPayload: json.RawMessage(payload)},
			{SourceAgent: "codex", EventID: "b", ContentSHA256: "hash-b", FullPayload: json.RawMessage(payload)},
		},
	}))

	acc := &conflictsWebAccessor{deps: &webAPIDeps{store: store, conf: cs}}
	got, ok, err := acc.Get(id)
	require.NoError(t, err)
	require.True(t, ok, "detail request should still get a reader-facing snapshot after auto-clear")
	require.Equal(t, id, got.ArtifactID)

	_, err = cs.Get(id)
	require.ErrorIs(t, err, conflicts.ErrNotRecorded)
}
