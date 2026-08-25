package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

// memoryEncoder is a synthetic encoder for testing — wraps MemoryPayload.
func memoryEncoder(content []byte) (json.RawMessage, error) {
	return acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: string(content)})
}

func memoryDecoder(e acf.Event) (string, error) {
	p, err := acf.DecodeMemoryPayload(e)
	return p.Content, err
}

func TestImportOpaque_WritesArtifactAndEvent(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "FILE.md")
	require.NoError(t, os.WriteFile(src, []byte("# Test\n"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}
	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, got.Kind)
	require.Equal(t, acf.ScopeProject, got.Scope)
	require.Equal(t, "FILE.md", got.Name)

	events, err := s.ReadEvents(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, "test-agent", events[0].Provenance.SourceAgent)
	require.Equal(t, "0.0.1", events[0].Provenance.AdapterVersion)
	require.NoError(t, acf.VerifyChain(events))
}

func TestImportOpaque_RespectsScopeInference(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "FILE.md")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeGlobal },
	}
	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)

	got, _ := s.ReadArtifact(acf.KindMemory, ids[0])
	require.Equal(t, acf.ScopeGlobal, got.Scope)
}

// TestImportOpaque_GlobalScope_NotRecapturedByParentProject reproduces the
// home-dir scope-capture bug. The daemon registers its --dir as an implicit
// LOCAL project; when --dir is $HOME, that registration's path is an
// ancestor of the agent's OWN global-state root (~/.claude/). A path the
// adapter's InferScope deliberately classifies as ScopeGlobal (e.g. a
// ~/.claude/projects/<cwd>/<session>.jsonl conversation) must NOT be recaptured
// into the parent project's local scope — otherwise every Claude Code
// conversation becomes project-scoped under the home project and is stranded in
// `pending` on any device that hasn't linked that home project.
func TestImportOpaque_GlobalScope_NotRecapturedByParentProject(t *testing.T) {
	home := t.TempDir()
	physicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	home = physicalHome

	// Register $HOME as an implicit local project, mirroring the daemon's
	// "register --dir as a local project" startup step.
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID: "local:home", Path: home, VCS: "none", Scope: "local",
	}))

	// A file under the agent's global-state root (~/.claude/projects/<enc>/).
	convDir := filepath.Join(home, ".claude", "projects", "-enc-cwd")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	src := filepath.Join(convDir, "session.jsonl")
	require.NoError(t, os.WriteFile(src, []byte("# global agent state\n"), 0o644))

	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, s.Init())

	// Mirror claudecode.inferScope: anything under <home>/.claude/ is global.
	claudeRoot := filepath.Join(home, ".claude") + string(filepath.Separator)
	inferScope := func(p string) acf.Scope {
		if strings.HasPrefix(p, claudeRoot) {
			return acf.ScopeGlobal
		}
		return acf.ScopeProject
	}

	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "claude-code",
		AdapterVersion: "test",
		InferScope:     inferScope,
		InferProject:   DefaultInferProject,
		Registry:       reg,
	}, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.ScopeGlobal, got.Scope,
		"a path InferScope marks global must stay global even under a registered parent project")
	require.Nil(t, got.Project,
		"global agent-state must not be attributed to the parent (home) project")
}

func TestImportOpaque_HonorsCancelledContext(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	params := OpaqueParams{InferScope: func(string) acf.Scope { return acf.ScopeProject }}
	_, err := ImportOpaque(ctx, s, acf.KindMemory, params, "/tmp/whatever", memoryEncoder)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cancelled")
}

func TestExportOpaque_RoundTripsBytes(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "IN.md")
	original := "# Round trip\n\nBody.\n"
	require.NoError(t, os.WriteFile(src, []byte(original), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}
	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "OUT.md")
	require.NoError(t, ExportOpaque(context.Background(), s, acf.KindMemory, ids[0], dest, memoryDecoder))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
}

func TestExportOpaque_NoEventsIsError(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	err := ExportOpaque(context.Background(), s, acf.KindMemory, "01956a39-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		filepath.Join(t.TempDir(), "out.md"), memoryDecoder)
	require.Error(t, err)
}

func TestExportOpaque_BadChainIsError(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	// Write an artifact + an event with a wrong Hash to break the chain.
	id := acf.NewID()
	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "X.md",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}))
	payload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "x"})
	e := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       "create",
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{SourceAgent: "test", AdapterVersion: "0"},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, s.AppendEvent(acf.KindMemory, e))

	// Now corrupt the events file to break VerifyChain.
	eventsPath := filepath.Join(s.Root, "events", "memories", id+".jsonl")
	require.NoError(t, os.WriteFile(eventsPath, []byte(`{"eventId":"x","artifactId":"`+id+`","type":"create","parentHash":"","hash":"DELIBERATELY-WRONG"}`+"\n"), 0o644))

	err := ExportOpaque(context.Background(), s, acf.KindMemory, id, filepath.Join(t.TempDir(), "out.md"), memoryDecoder)
	require.Error(t, err)
	require.Contains(t, err.Error(), "event log")
}

func TestImportOpaque_PopulatesSourcePath(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "FILE.md")
	require.NoError(t, os.WriteFile(src, []byte("v1"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}
	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	absSrc, err := filepath.Abs(src)
	require.NoError(t, err)
	require.Equal(t, absSrc, got.SourcePath,
		"new artifact must record the absolute source path it was imported from")
}

// TestImportOpaque_ReImportUnchangedContent_NoNewEvent guards the startup
// skip-if-equal behavior: the daemon's InitialScan re-reads every native file
// on each restart. When a file's bytes are unchanged since the last import,
// the re-import must NOT append a redundant "update" event (which would bloat
// the event log and flood the events feed with a same-second burst of "synced"
// rows on every restart). Identity reconciliation still reuses the artifact id
// so the fan-out contract is unchanged. A subsequent real change still records
// exactly one update event.
func TestImportOpaque_ReImportUnchangedContent_NoNewEvent(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "CLAUDE.md")
	require.NoError(t, os.WriteFile(src, []byte("# stable\n"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}

	ids1, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	// Re-import the SAME bytes — reuses the id, appends no event.
	ids2, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Equal(t, ids1, ids2,
		"re-importing unchanged content must reuse the artifact ID (identity reconciliation)")

	events, err := s.ReadEvents(acf.KindMemory, ids1[0])
	require.NoError(t, err)
	require.Len(t, events, 1,
		"re-importing byte-identical content must NOT append a redundant update event")

	// A genuine change after a no-op still records the update.
	require.NoError(t, os.WriteFile(src, []byte("# changed\n"), 0o644))
	ids3, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Equal(t, ids1, ids3, "a real change reuses the artifact ID")
	events, err = s.ReadEvents(acf.KindMemory, ids1[0])
	require.NoError(t, err)
	require.Len(t, events, 2, "a real change after a no-op appends exactly one update event")
}

func TestImportOpaque_ReImportSameFile_ReusesArtifactID(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "CLAUDE.md")
	require.NoError(t, os.WriteFile(src, []byte("# v1\n"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}

	ids1, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	require.NoError(t, os.WriteFile(src, []byte("# v2\n"), 0o644))
	ids2, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids2, 1)

	require.Equal(t, ids1[0], ids2[0],
		"re-importing the same file MUST return the same artifact ID (ADR-0027 stable IDs)")

	events, err := s.ReadEvents(acf.KindMemory, ids1[0])
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, acf.EventTypeUpdate, events[1].Type)

	require.NoError(t, acf.VerifyChain(events))

	dest := filepath.Join(tmp, "out", "CLAUDE.md")
	require.NoError(t, ExportOpaque(context.Background(), s, acf.KindMemory, ids1[0], dest, memoryDecoder))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "# v2\n", string(got))
}

func TestImportOpaque_ReImportRepairsStaleArtifactHead(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "CLAUDE.md")
	require.NoError(t, os.WriteFile(src, []byte("# v1\n"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}

	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(src, []byte("# v2\n"), 0o644))
	_, err = ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)

	events, err := s.ReadEvents(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 2)

	art, err := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	art.HeadEventHash = events[0].Hash
	art.BranchHeads[acf.MainBranch] = events[0].Hash
	require.NoError(t, s.WriteArtifact(art))

	require.NoError(t, os.WriteFile(src, []byte("# v3\n"), 0o644))
	_, err = ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)

	events, err = s.ReadEvents(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, events[1].Hash, events[2].ParentHash)
	require.NoError(t, acf.VerifyChain(events))

	art, err = s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, events[2].Hash, art.HeadEventHash)
	require.Equal(t, events[2].Hash, art.BranchHeads[acf.MainBranch])
}

func TestImportOpaque_DifferentPathSameName_MintsNewArtifact(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	dir1 := filepath.Join(tmp, "proj1")
	dir2 := filepath.Join(tmp, "proj2")
	require.NoError(t, os.Mkdir(dir1, 0o755))
	require.NoError(t, os.Mkdir(dir2, 0o755))
	src1 := filepath.Join(dir1, "CLAUDE.md")
	src2 := filepath.Join(dir2, "CLAUDE.md")
	require.NoError(t, os.WriteFile(src1, []byte("p1"), 0o644))
	require.NoError(t, os.WriteFile(src2, []byte("p2"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}
	ids1, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src1, memoryEncoder)
	require.NoError(t, err)
	ids2, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src2, memoryEncoder)
	require.NoError(t, err)

	require.NotEqual(t, ids1[0], ids2[0],
		"same basename in different directories must yield distinct artifacts")
}

func TestImportOpaque_RollsBackOrphanArtifactOnAppendFailure(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "FILE.md")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))

	// Force AppendEvent to fail by replacing the per-kind events directory
	// with a regular file. Now any AppendEvent for a memory artifact fails
	// at os.MkdirAll because the path is a file.
	eventsKindDir := filepath.Join(s.Root, "events", "memories")
	require.NoError(t, os.Remove(eventsKindDir))
	require.NoError(t, os.WriteFile(eventsKindDir, []byte("not a directory"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}
	_, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.Error(t, err, "AppendEvent should fail because the events kind dir is now a file")

	// The store should NOT contain an orphan memory artifact.
	artifacts, err := s.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	require.Empty(t, artifacts,
		"failed import must roll back the artifact write — no orphans allowed")
}

func TestImportOpaque_DoesNotRollBackExistingOnUpdateFailure(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "FILE.md")
	require.NoError(t, os.WriteFile(src, []byte("v1"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}
	// First import: creates artifact + event.
	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Corrupt the events kind dir. Move the existing events file out first.
	eventsKindDir := filepath.Join(s.Root, "events", "memories")
	existingEvents := filepath.Join(eventsKindDir, ids[0]+".jsonl")
	preservedEvents, err := os.ReadFile(existingEvents)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(eventsKindDir))
	require.NoError(t, os.WriteFile(eventsKindDir, []byte("not a directory"), 0o644))

	require.NoError(t, os.WriteFile(src, []byte("v2"), 0o644))
	_, err = ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.Error(t, err, "AppendEvent should fail on the update path too")

	// The pre-existing artifact must STILL be there.
	_, derr := s.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, derr, "existing artifact must survive a failed re-import")

	// Best-effort cleanup so test env exits clean.
	_ = os.Remove(eventsKindDir)
	_ = os.MkdirAll(eventsKindDir, 0o755)
	_ = os.WriteFile(existingEvents, preservedEvents, 0o644)
}

func TestExportOpaque_TombstonedReturnsSentinel(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "tomb",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, store.WriteArtifact(art))

	payload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "x"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Payload:    payload,
	}))
	got, err := store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeRedaction,
		Timestamp:  now,
		Payload:    nil,
		ParentHash: got.HeadEventHash,
	}))

	dst := filepath.Join(t.TempDir(), "out.txt")
	err = ExportOpaque(context.Background(), store, acf.KindMemory, id, dst, memoryDecoder)
	require.ErrorIs(t, err, ErrArtifactTombstoned)
	_, statErr := os.Stat(dst)
	require.True(t, os.IsNotExist(statErr), "no file should have been written for a tombstoned artifact")
}

func TestResolveRegisteredScope_LocalFolderStaysProject(t *testing.T) {
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	home := t.TempDir()
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID: "local:home", Path: home, VCS: "none", Scope: "local",
	}))
	// A non-git path UNDER the registered local folder.
	scope, projID, registered := ResolveRegisteredScope(reg, filepath.Join(home, "AGENTS.md"))
	require.True(t, registered)
	require.Equal(t, acf.ScopeProject, scope)
	require.Equal(t, "local:home", projID)

	// A path NOT under any registered folder → not registered.
	_, _, reg2 := ResolveRegisteredScope(reg, "/tmp/elsewhere/AGENTS.md")
	require.False(t, reg2)

	// nil registry → not registered, no panic.
	_, _, reg3 := ResolveRegisteredScope(nil, "/anything/AGENTS.md")
	require.False(t, reg3)
}

func TestResolveRegisteredScope_NestedProjects_LongestWins(t *testing.T) {
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	parent := t.TempDir()
	parent, err = filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	child := filepath.Join(parent, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	// Register the BROADER parent FIRST, and give it an ID that sorts
	// BEFORE the child's. reg.List() iterates in ID-sorted order, so the
	// broader parent would be visited first — under the old first-match
	// logic it would shadow the deeper child and key child files to the
	// parent. This ordering genuinely pins longest-match behavior: it
	// fails if ResolveRegisteredScope reverts to returning the first
	// prefix match.
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID: "local:aaa-parent", Path: parent, VCS: "none", Scope: "local",
	}))
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID: "local:zzz-child", Path: child, VCS: "none", Scope: "local",
	}))

	// A file under the deeper child must resolve to the child, not the parent.
	scope, projID, registered := ResolveRegisteredScope(reg, filepath.Join(child, "CLAUDE.md"))
	require.True(t, registered)
	require.Equal(t, acf.ScopeProject, scope)
	require.Equal(t, "local:zzz-child", projID,
		"a file under the deepest registered folder must key to that folder, not a broader ancestor")

	// A file under the parent but NOT under the child resolves to the parent.
	scope, projID, registered = ResolveRegisteredScope(reg, filepath.Join(parent, "other", "CLAUDE.md"))
	require.True(t, registered)
	require.Equal(t, acf.ScopeProject, scope)
	require.Equal(t, "local:aaa-parent", projID)
}

func TestResolveRegisteredScope_InactiveEntryCannotAuthorizeAdapterRouting(t *testing.T) {
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	missing := filepath.Join(t.TempDir(), "missing-project")
	require.NoError(t, reg.Add(project.Entry{ID: "inactive-project", Path: missing, VCS: "none", Scope: "local", Inactive: true}))

	_, _, registered := ResolveRegisteredScope(reg, filepath.Join(missing, "AGENTS.md"))
	require.False(t, registered, "an inactive recovery record must never authorize adapter project routing")
	require.Empty(t, reg.List())
	require.Len(t, reg.ListAll(), 1)
}

// TestReplayOpaqueContent_AfterSnapshotAndPrune covers the confirmed P1
// retention regression for the SHARED opaque export path that
// claudecode/codex/kilo/openclaw use for conversation/memory/skill/tool
// exports. Once retention.CreateSnapshot + PruneArtifact run on a main-only
// artifact, every pre-snapshot create/update event moves into the .compacted
// layer and the ACTIVE log holds only the snapshot event. ReplayOpaqueContent
// must still materialize the content. Before the fix the active-only
// VerifyChain fails across the prune boundary (the snapshot's ParentHash
// references the now-compacted pre-snapshot head) with "event log is invalid:
// ... ParentHash ... does not match head of branch main".
func TestReplayOpaqueContent_AfterSnapshotAndPrune(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "FILE.md")
	require.NoError(t, os.WriteFile(src, []byte("survives prune\n"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeGlobal },
	}
	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]

	// Snapshot, then prune. After this the active log is snapshot-only and the
	// create event lives in .compacted (the exact production sequence the
	// retention engine runs on a main-only artifact).
	_, err = retention.CreateSnapshot(context.Background(), s, acf.KindMemory, id)
	require.NoError(t, err)
	moved, _, err := retention.PruneArtifact(context.Background(), s, acf.KindMemory, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, moved, "the single pre-snapshot create event must move to .compacted")

	active, err := s.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, active, 1, "active log is snapshot-only after prune")
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)

	content, tombstoned, err := ReplayOpaqueContent(s, acf.KindMemory, id, memoryDecoder)
	require.NoError(t, err, "replay must still materialize after snapshot+prune (no 'event log is invalid')")
	require.False(t, tombstoned)
	require.Equal(t, "survives prune\n", content)

	// End-to-end through ExportOpaque (what the per-adapter exporters call).
	dest := filepath.Join(tmp, "out", "OUT.md")
	require.NoError(t, ExportOpaque(context.Background(), s, acf.KindMemory, id, dest, memoryDecoder))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "survives prune\n", string(got))
}

// TestReplayOpaqueContent_LegacyPayloadlessSnapshotAndPrune isolates the
// compacted-layer fallback: a snapshot written BEFORE FR-02.32 carries NO
// payload. After prune the active log is that payload-less snapshot alone — so
// the root fix (payload-bearing snapshot) does NOT apply and the ONLY thing
// that can re-materialize the content is the fallback to
// ReadEventsIncludingCompacted. Proves defense-in-depth holds for legacy
// snapshots already in the wild.
func TestReplayOpaqueContent_LegacyPayloadlessSnapshotAndPrune(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Scope: acf.ScopeGlobal, Name: "legacy", CreatedAt: now, UpdatedAt: now,
	}))
	createPayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "legacy survives\n"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: createPayload,
	}))

	// Append a PAYLOAD-LESS snapshot by hand (the pre-FR-02.32 shape).
	head, err := s.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(acf.KindMemory, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     now.Add(time.Second),
		ParentHash:    head.HeadEventHash,
		SnapshotState: "sha256:deadbeef",
		Payload:       nil,
	}))

	moved, _, err := retention.PruneArtifact(context.Background(), s, acf.KindMemory, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, moved)

	active, err := s.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)
	require.False(t, acf.HasPayload(active[0].Payload), "this snapshot is intentionally payload-less (legacy shape)")

	content, tombstoned, err := ReplayOpaqueContent(s, acf.KindMemory, id, memoryDecoder)
	require.NoError(t, err, "the compacted-layer fallback must re-materialize even a payload-less snapshot")
	require.False(t, tombstoned)
	require.Equal(t, "legacy survives\n", content)
}

// TestReplayOpaqueContent_CorruptChainStillErrors proves the fallback does NOT
// mask genuine corruption: an active log whose chain is broken (and has no
// .compacted layer to repair it) must still fail with a clear "event log is
// invalid" error, exactly as before the prune-resilience fallback was added.
func TestReplayOpaqueContent_CorruptChainStillErrors(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Scope: acf.ScopeGlobal, Name: "corrupt", CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "x"})
	require.NoError(t, err)
	// Write a create event with a BOGUS non-empty ParentHash DIRECTLY to the
	// events file (store.AppendEvent would reject it). A genesis event must have
	// ParentHash "", so VerifyChain fails. The event's own Hash is set correctly
	// so the failure is specifically the chain break, not a hash mismatch. No
	// .compacted file exists, so the fallback cannot repair it.
	corrupt := acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now,
		Payload: payload, ParentHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	corrupt.Hash, err = acf.ComputeHash(corrupt)
	require.NoError(t, err)
	line, err := json.Marshal(corrupt)
	require.NoError(t, err)
	eventsPath := filepath.Join(s.Root, "events", "memories", id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(eventsPath), 0o755))
	require.NoError(t, os.WriteFile(eventsPath, append(line, '\n'), 0o644))

	_, _, err = ReplayOpaqueContent(s, acf.KindMemory, id, memoryDecoder)
	require.Error(t, err, "a genuinely corrupt chain must still error")
	require.Contains(t, err.Error(), "event log is invalid",
		"corruption must surface as 'event log is invalid', not silently materialize")
}

// TestReplayOpaqueContent_RedactionHeadDoesNotResurrect proves a redaction is
// authoritative: when the latest mutating event is a redaction (so the artifact
// is tombstoned), replay must report tombstoned=true with empty content and
// must NOT fall back to the compacted layer to resurrect a pre-redaction
// payload — even after a snapshot+prune that compacted the original content.
func TestReplayOpaqueContent_RedactionHeadDoesNotResurrect(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Scope: acf.ScopeGlobal, Name: "redacted", CreatedAt: now, UpdatedAt: now,
	}))
	secret, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "secret\n"})
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: secret,
	}))
	h1, err := s.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	// Snapshot then redact, so the materialized "secret" payload sits in the
	// snapshot/compacted layers and the head is a redaction.
	_, err = retention.CreateSnapshot(context.Background(), s, acf.KindMemory, id)
	require.NoError(t, err)
	_ = h1
	h2, err := s.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.NoError(t, s.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeRedaction,
		Timestamp: now.Add(2 * time.Second), ParentHash: h2.HeadEventHash,
	}))

	got, err := s.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.True(t, got.Tombstoned, "a redaction head must set Tombstoned")

	content, tombstoned, err := ReplayOpaqueContent(s, acf.KindMemory, id, memoryDecoder)
	require.NoError(t, err)
	require.True(t, tombstoned, "a redacted artifact must export as tombstoned")
	require.Equal(t, "", content, "redaction must NOT resurrect the pre-redaction payload")

	// ExportOpaque must surface the tombstone sentinel and write no file.
	dst := filepath.Join(tmp, "out.md")
	err = ExportOpaque(context.Background(), s, acf.KindMemory, id, dst, memoryDecoder)
	require.ErrorIs(t, err, ErrArtifactTombstoned)
	_, statErr := os.Stat(dst)
	require.True(t, os.IsNotExist(statErr), "no file should be written for a tombstoned artifact")
}

func TestImportOpaque_ReImport_PreV020Artifact_DoesNotMatch(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	preID := acf.NewID()
	pre := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       preID,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "CLAUDE.md",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	require.NoError(t, s.WriteArtifact(pre))

	src := filepath.Join(tmp, "CLAUDE.md")
	require.NoError(t, os.WriteFile(src, []byte("new"), 0o644))

	params := OpaqueParams{
		DeviceID:       "dev",
		SourceAgent:    "test-agent",
		AdapterVersion: "0.0.1",
		InferScope:     func(string) acf.Scope { return acf.ScopeProject },
	}
	ids, err := ImportOpaque(context.Background(), s, acf.KindMemory, params, src, memoryEncoder)
	require.NoError(t, err)
	require.NotEqual(t, preID, ids[0],
		"re-import against a pre-v0.2.0 artifact must mint a new ID (CHANGELOG documents this caveat)")
}
