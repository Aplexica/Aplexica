package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

type rediscoveringAdapter struct {
	name      string
	version   string
	discover  func() adapter.Discovery
	discovers int
}

func (a *rediscoveringAdapter) Name() string    { return a.name }
func (a *rediscoveringAdapter) Version() string { return a.version }
func (a *rediscoveringAdapter) Import(context.Context, *acf.Store, string) ([]string, error) {
	return nil, nil
}
func (a *rediscoveringAdapter) Export(context.Context, *acf.Store, string, string) error {
	return nil
}
func (a *rediscoveringAdapter) NativePath(acf.Artifact, string) (string, bool, error) {
	return "", false, nil
}
func (a *rediscoveringAdapter) HandlesFormat(acf.Kind, string) bool { return false }
func (a *rediscoveringAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Name: a.name}
}
func (a *rediscoveringAdapter) Discover() (adapter.Discovery, error) {
	a.discovers++
	if a.discover == nil {
		return adapter.Discovery{}, nil
	}
	return a.discover(), nil
}

func physicalTestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	physical, err := filepath.EvalSymlinks(directory)
	require.NoError(t, err)
	return physical
}

// TestAgentWatchedLocations exercises the pure agentWatchedLocations
// helper: global + recursive roots appear first, then local-scope
// project folders the agent participates in, sorted by path.
// Projects with non-local scope or that don't list the agent are
// excluded.
func TestAgentWatchedLocations(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "projects.json")

	reg, err := project.NewRegistry(regPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	projX := physicalTestDirectory(t)
	other := physicalTestDirectory(t)
	global := physicalTestDirectory(t)
	// proj-x — local scope, claude-code in agents => INCLUDED
	if err := reg.Add(project.Entry{
		ID:     "proj-x",
		Path:   projX,
		Scope:  "local",
		Agents: []string{"claude-code"},
	}); err != nil {
		t.Fatalf("add proj-x: %v", err)
	}
	// other — local scope, only codex => NOT included for claude-code
	if err := reg.Add(project.Entry{
		ID:     "other",
		Path:   other,
		Scope:  "local",
		Agents: []string{"codex"},
	}); err != nil {
		t.Fatalf("add other: %v", err)
	}
	// glob — global scope => NOT included (scope != "local")
	if err := reg.Add(project.Entry{
		ID:    "glob",
		Path:  global,
		Scope: "global",
	}); err != nil {
		t.Fatalf("add glob: %v", err)
	}

	d := adapter.Discovery{
		Installed:      true,
		GlobalRoots:    []string{"/h/.claude"},
		RecursiveRoots: []string{"/h/.claude/projects"},
	}

	got := agentWatchedLocations(d, reg, "claude-code")
	want := []string{"/h/.claude", "/h/.claude/projects", projX}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("agentWatchedLocations = %v, want %v", got, want)
	}
}

func TestAgentsAccessorListRediscoverOnDemand(t *testing.T) {
	ad := &rediscoveringAdapter{
		name:    "kilo",
		version: "0.1.0",
		discover: func() adapter.Discovery {
			return adapter.Discovery{
				Installed:   true,
				GlobalRoots: []string{"/h/.config/kilo"},
				Detail:      "/h/.config/kilo present",
			}
		},
	}
	acc := &agentsWebAccessor{deps: &webAPIDeps{
		adapters: []adapter.Adapter{ad},
		discoveries: map[string]adapter.Discovery{
			"kilo": {Installed: false, Detail: "missing at startup"},
		},
	}}

	got := acc.List()

	require.Len(t, got, 1)
	require.Equal(t, 1, ad.discovers)
	require.True(t, got[0].Installed)
	require.Equal(t, []string{"/h/.config/kilo"}, got[0].GlobalRoots)
}

func TestSurfaceStringsPreservesDeclaredOrder(t *testing.T) {
	got := surfaceStrings([]adapter.Surface{adapter.SurfaceCLI, adapter.SurfaceDesktop})
	require.Equal(t, []string{"cli", "desktop"}, got)
	require.Nil(t, surfaceStrings(nil))
}

// TestAgentWatchedLocations_AllAgents verifies that a project with an
// empty Agents slice is treated as "all installed agents" and therefore
// included for any agent name.
func TestAgentWatchedLocations_AllAgents(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "projects.json")

	reg, err := project.NewRegistry(regPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// empty Agents => all agents participate
	if err := reg.Add(project.Entry{
		ID:    "shared",
		Path:  physicalTestDirectory(t),
		Scope: "local",
	}); err != nil {
		t.Fatalf("add shared: %v", err)
	}

	d := adapter.Discovery{
		Installed:   true,
		GlobalRoots: []string{"/h/.claude"},
	}

	got := agentWatchedLocations(d, reg, "claude-code")
	want := []string{"/h/.claude", reg.List()[0].Path}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("agentWatchedLocations = %v, want %v", got, want)
	}
}

// TestAgentWatchedLocations_NilRegistry verifies that a nil registry
// doesn't panic and returns only the discovery roots.
func TestAgentWatchedLocations_NilRegistry(t *testing.T) {
	d := adapter.Discovery{
		Installed:      true,
		GlobalRoots:    []string{"/h/.claude"},
		RecursiveRoots: []string{"/h/.claude/projects"},
	}

	got := agentWatchedLocations(d, nil, "claude-code")
	want := []string{"/h/.claude", "/h/.claude/projects"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("agentWatchedLocations = %v, want %v", got, want)
	}
}

// TestAgentWatchedLocations_Dedup verifies that a project path that
// duplicates a GlobalRoot is not listed twice.
func TestAgentWatchedLocations_Dedup(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "projects.json")

	reg, err := project.NewRegistry(regPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	duplicatePath := physicalTestDirectory(t)
	// project path matches a GlobalRoot — should NOT appear twice
	if err := reg.Add(project.Entry{
		ID:     "dup",
		Path:   duplicatePath,
		Scope:  "local",
		Agents: []string{"claude-code"},
	}); err != nil {
		t.Fatalf("add dup: %v", err)
	}

	d := adapter.Discovery{
		Installed:   true,
		GlobalRoots: []string{duplicatePath},
	}

	got := agentWatchedLocations(d, reg, "claude-code")
	want := []string{duplicatePath}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("agentWatchedLocations = %v, want %v (should dedup)", got, want)
	}
}

func TestRecentAgentEvents_DisplaysSnapshotsAsInternalCheckpoints(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	artifactID := acf.NewID()
	createdAt := time.Date(2026, 6, 4, 14, 45, 0, 0, time.UTC)
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "session.jsonl",
		SourcePath:       "/h/.claude/projects/-h-test/session.jsonl",
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	}
	require.NoError(t, store.WriteArtifact(art))

	payload, err := json.Marshal(acf.ConversationPayload{Format: "claude-code.session.jsonl", Content: "{}\n"})
	require.NoError(t, err)

	update := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  createdAt,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		Payload:    payload,
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, update))

	head, err := store.HeadHash(acf.KindConversation, artifactID)
	require.NoError(t, err)
	snapshot := acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    artifactID,
		Type:          acf.EventType(acf.EventTypeSnapshot),
		Timestamp:     time.Date(2026, 6, 4, 15, 1, 25, 0, time.UTC),
		ParentHash:    head,
		SnapshotState: "sha256:checkpoint",
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, snapshot))

	acc := &agentsWebAccessor{deps: &webAPIDeps{store: store}}
	got := acc.recentAgentEvents("claude-code", []string{"/h/.claude"}, 2)

	require.Len(t, got, 2)
	require.Equal(t, "artifact.checkpoint", got[0].Type)
	require.Equal(t, "internal checkpoint · session.jsonl", got[0].Detail)
	require.Equal(t, "artifact.imported", got[1].Type)
	require.Equal(t, "imported conversation · session.jsonl", got[1].Detail)
}

func TestRecentAgentEvents_HidesReceivedEventsWhenSyncDisabled(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	artifactID := acf.NewID()
	createdAt := time.Date(2026, 6, 5, 17, 59, 16, 0, time.UTC)
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeGlobal,
		Name:             "AGENTS.md",
		SourcePath:       "/h/.codex/AGENTS.md",
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
		SyncedAgents:     []string{"kilo"},
	}
	require.NoError(t, store.WriteArtifact(art))

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "# from codex\n"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  createdAt,
		Provenance: acf.Provenance{SourceAgent: "codex"},
		Payload:    payload,
	}))

	acc := &agentsWebAccessor{deps: &webAPIDeps{store: store}}
	got := acc.recentAgentEvents("kilo", []string{"/h/.config/kilo"}, 25)

	require.Empty(t, got, "sync-off agents must not show received fan-out history")
}

func TestRecentAgentEvents_DisplaysOwnedEventsAsImports(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	artifactID := acf.NewID()
	createdAt := time.Date(2026, 6, 5, 17, 59, 16, 0, time.UTC)
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeGlobal,
		Name:             "AGENTS.md",
		SourcePath:       "/h/.config/kilo/AGENTS.md",
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	}
	require.NoError(t, store.WriteArtifact(art))

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "# from kilo\n"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  createdAt,
		Provenance: acf.Provenance{SourceAgent: "kilo"},
		Payload:    payload,
	}))

	acc := &agentsWebAccessor{deps: &webAPIDeps{store: store}}
	got := acc.recentAgentEvents("kilo", []string{"/h/.config/kilo"}, 25)

	require.Len(t, got, 1)
	require.Equal(t, "artifact.imported", got[0].Type)
	require.Equal(t, "imported memory · AGENTS.md", got[0].Detail)
}

// appendOwnedConversation writes a claude-code-owned conversation artifact
// (SourcePath under /h/.claude) and appends one main-branch update event per
// timestamp in tss, chaining ParentHash so the log is valid. The artifact's
// persisted UpdatedAt ends equal to the last (newest) timestamp.
func appendOwnedConversation(t *testing.T, store *acf.Store, name string, tss ...time.Time) {
	t.Helper()
	id := acf.NewID()
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             name,
		SourcePath:       "/h/.claude/projects/p/" + name,
		CreatedAt:        tss[0],
		UpdatedAt:        tss[0],
	}
	require.NoError(t, store.WriteArtifact(art))
	parent := ""
	for _, ts := range tss {
		payload, err := json.Marshal(acf.ConversationPayload{Format: "claude-code.session.jsonl", Content: "{}\n"})
		require.NoError(t, err)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID:    acf.NewID(),
			ArtifactID: id,
			Type:       acf.EventTypeUpdate,
			Timestamp:  ts,
			ParentHash: parent,
			Provenance: acf.Provenance{SourceAgent: "claude-code"},
			Payload:    payload,
		}))
		h, err := store.HeadHash(acf.KindConversation, id)
		require.NoError(t, err)
		parent = h
	}
}

// TestRecentAgentEvents_ReadsBoundedEventLogs is the regression guard for the
// "Agents screen took 17s" fix: with many attributed artifacts, the feed must
// parse only a bounded number of event logs (near `limit`), not every one.
func TestRecentAgentEvents_ReadsBoundedEventLogs(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	const n = 50
	for i := 0; i < n; i++ {
		appendOwnedConversation(t, store, fmt.Sprintf("s%02d.jsonl", i), base.Add(time.Duration(i)*time.Minute))
	}

	var reads int
	acc := &agentsWebAccessor{deps: &webAPIDeps{store: store}}
	acc.readEvents = func(k acf.Kind, id string) ([]acf.Event, error) {
		reads++
		return store.ReadEvents(k, id)
	}

	const limit = 10
	got := acc.recentAgentEvents("claude-code", []string{"/h/.claude"}, limit)

	require.Len(t, got, limit)
	require.Equal(t, base.Add(49*time.Minute), got[0].Timestamp, "newest first")
	require.Equal(t, base.Add(40*time.Minute), got[limit-1].Timestamp, "oldest kept")
	require.LessOrEqualf(t, reads, limit+2,
		"read %d event logs for %d artifacts; must stay bounded near limit=%d", reads, n, limit)
}

// TestRecentAgentEvents_RanksNewestAcrossManyArtifacts verifies the optimized
// feed still returns exactly the newest `limit` events in descending order
// across a large attributed set.
func TestRecentAgentEvents_RanksNewestAcrossManyArtifacts(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	const n = 60
	for i := 0; i < n; i++ {
		appendOwnedConversation(t, store, fmt.Sprintf("s%02d.jsonl", i), base.Add(time.Duration(i)*time.Minute))
	}

	acc := &agentsWebAccessor{deps: &webAPIDeps{store: store}}
	const limit = 25
	got := acc.recentAgentEvents("claude-code", []string{"/h/.claude"}, limit)

	require.Len(t, got, limit)
	for i := 0; i < limit; i++ {
		want := base.Add(time.Duration(n-1-i) * time.Minute)
		require.Equalf(t, want, got[i].Timestamp, "row %d timestamp", i)
		require.Equal(t, "artifact.imported", got[i].Type)
	}
}

// TestRecentAgentEvents_HotArtifactFillsFeed proves a single very active
// artifact can supply every row of the feed: early termination must not stop
// before draining enough of its log, and older artifacts must be excluded.
func TestRecentAgentEvents_HotArtifactFillsFeed(t *testing.T) {
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	base := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	// 40 older, single-event artifacts.
	for i := 0; i < 40; i++ {
		appendOwnedConversation(t, store, fmt.Sprintf("cold%02d.jsonl", i), base.Add(time.Duration(i)*time.Minute))
	}
	// One hot artifact with 30 newer events (base+100m .. base+129m).
	hot := make([]time.Time, 0, 30)
	for j := 0; j < 30; j++ {
		hot = append(hot, base.Add(time.Duration(100+j)*time.Minute))
	}
	appendOwnedConversation(t, store, "hot.jsonl", hot...)

	acc := &agentsWebAccessor{deps: &webAPIDeps{store: store}}
	const limit = 25
	got := acc.recentAgentEvents("claude-code", []string{"/h/.claude"}, limit)

	require.Len(t, got, limit)
	for i := 0; i < limit; i++ {
		want := base.Add(time.Duration(129-i) * time.Minute)
		require.Equalf(t, want, got[i].Timestamp, "row %d timestamp", i)
		require.Containsf(t, got[i].Detail, "hot.jsonl", "row %d should come from the hot artifact", i)
	}
}
