package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
)

// seedBackfillArtifact writes one memory artifact with a single create event
// stamped at ts, attributed to agent. The event's Timestamp becomes the
// backfill Seq (Unix-ms), which is what the newest-first ordering keys on.
func seedBackfillArtifact(t *testing.T, s *acf.Store, name, agent string, ts time.Time) string {
	t.Helper()
	id := acf.NewID()
	if err := s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Name:             name,
		CreatedAt:        ts,
		UpdatedAt:        ts,
	}); err != nil {
		t.Fatalf("write artifact %q: %v", name, err)
	}
	payload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "x"})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	if err := s.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  ts,
		Payload:    payload,
		Provenance: acf.Provenance{SourceAgent: agent},
	}); err != nil {
		t.Fatalf("append event %q: %v", name, err)
	}
	return id
}

// appendMemoryUpdate appends one update event to an existing memory artifact,
// bumping its head hash without changing the artifact count — the case the
// feed cache must invalidate on via the metadata signature (not just count).
func appendMemoryUpdate(t *testing.T, s *acf.Store, id, agent string, ts time.Time) {
	t.Helper()
	a, err := s.ReadArtifact(acf.KindMemory, id)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	payload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "y"})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	// The store enforces a hash chain: ParentHash must match the current head.
	if err := s.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  ts,
		ParentHash: a.HeadEventHash,
		Payload:    payload,
		Provenance: acf.Provenance{SourceAgent: agent},
	}); err != nil {
		t.Fatalf("append update: %v", err)
	}
}

// TestEventsWebAccessor_CacheInvalidates guards the materialised-feed cache: the
// backfill memoises the sorted feed and rebuilds only when the store's metadata
// signature (per-artifact head + branch-head hashes) changes. New events — a new
// artifact (count changes) AND a continuation of an existing one (count stays,
// head hash changes) — MUST appear without restarting the daemon.
func TestEventsWebAccessor_CacheInvalidates(t *testing.T) {
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	base := time.Date(2026, 6, 17, 15, 44, 46, 0, time.UTC)
	seedBackfillArtifact(t, s, "old", "claude-code", base)
	existing := seedBackfillArtifact(t, s, "mid", "codex", base.Add(24*time.Hour))

	acc := &eventsWebAccessor{deps: &webAPIDeps{store: s}}

	// Prime the cache, then a repeat call (no change) must serve the cache and
	// stay correct.
	if p, err := acc.Backfill(apiweb.EventQuery{Limit: 10}); err != nil {
		t.Fatalf("prime: %v", err)
	} else if len(p.Events) != 2 {
		t.Fatalf("prime: got %d events, want 2", len(p.Events))
	}
	if p, err := acc.Backfill(apiweb.EventQuery{Limit: 10}); err != nil {
		t.Fatalf("cache-hit: %v", err)
	} else if len(p.Events) != 2 || p.Events[0].Name != "mid" {
		t.Fatalf("cache-hit: got %d events top %q, want 2 top \"mid\"", len(p.Events), firstName(p))
	}

	// New artifact: count changes -> cache invalidates, newest leads.
	seedBackfillArtifact(t, s, "new", "kilo", base.Add(96*time.Hour))
	if p, err := acc.Backfill(apiweb.EventQuery{Limit: 10}); err != nil {
		t.Fatalf("after new artifact: %v", err)
	} else if len(p.Events) != 3 {
		t.Fatalf("after new artifact: got %d events, want 3 (stale cache?)", len(p.Events))
	} else if p.Events[0].Name != "new" {
		t.Errorf("after new artifact: top = %q, want \"new\"", p.Events[0].Name)
	}

	// Continuation of an existing artifact: count is unchanged but the head hash
	// moves — the signature must still invalidate and surface the newer event.
	appendMemoryUpdate(t, s, existing, "codex", base.Add(120*time.Hour))
	if p, err := acc.Backfill(apiweb.EventQuery{Limit: 10}); err != nil {
		t.Fatalf("after continuation: %v", err)
	} else if len(p.Events) != 4 {
		t.Fatalf("after continuation: got %d events, want 4 (signature missed a same-count append?)", len(p.Events))
	} else if p.Events[0].Name != "mid" {
		t.Errorf("after continuation: top = %q, want \"mid\" (its update is now newest)", p.Events[0].Name)
	}
}

func firstName(p apiweb.EventPage) string {
	if len(p.Events) == 0 {
		return ""
	}
	return p.Events[0].Name
}

func TestEventsWebAccessor_UserFriendlyMetadata(t *testing.T) {
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	base := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	id := acf.NewID()
	sourcePath := filepath.Join(t.TempDir(), "project", "CLAUDE.md")
	projectPath := filepath.Dir(sourcePath)
	if err := s.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeProject,
		Name:             "CLAUDE.md",
		SourcePath:       sourcePath,
		CreatedAt:        base,
		UpdatedAt:        base,
		SyncedAgents:     []string{"kilo", "codex"},
		Project:          &project.ProjectInfo{ID: "proj-1", Path: projectPath, VCS: "git"},
	}); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	payload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "x"})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	if err := s.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  base,
		Payload:    payload,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	acc := &eventsWebAccessor{deps: &webAPIDeps{store: s}}
	page, err := acc.Backfill(apiweb.EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(page.Events))
	}
	got := page.Events[0]
	if got.Action != "synced" {
		t.Errorf("Action = %q, want synced when an imported artifact has target agents", got.Action)
	}
	if !slices.Equal(got.TargetAgents, []string{"codex", "kilo"}) {
		t.Errorf("TargetAgents = %+v, want [codex kilo]", got.TargetAgents)
	}
	displaySourcePath := redactedDisplayPath(sourcePath)
	displayProjectPath := redactedDisplayPath(projectPath)
	if got.SourcePath != displaySourcePath {
		t.Errorf("SourcePath = %q, want %q", got.SourcePath, displaySourcePath)
	}
	if got.Scope != string(acf.ScopeProject) || got.ProjectID != "proj-1" || got.ProjectPath != displayProjectPath {
		t.Errorf("project metadata = scope %q id %q path %q", got.Scope, got.ProjectID, got.ProjectPath)
	}
}

func TestEventsWebAccessor_ChartsExposeOnlyAgentIdentities(t *testing.T) {
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	base := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	seedBackfillArtifact(t, s, "native", "codex", base)
	seedBackfillArtifact(t, s, "repair", "internal-conversation-repair", base.Add(time.Second))

	page, err := (&eventsWebAccessor{deps: &webAPIDeps{store: s}}).Backfill(apiweb.EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Events))
	}
	if page.Events[0].Agent != "" {
		t.Errorf("repair Agent = %q, want empty so it cannot become a chart series", page.Events[0].Agent)
	}
	if page.Events[1].Agent != "codex" {
		t.Errorf("native Agent = %q, want codex", page.Events[1].Agent)
	}
}

// TestEventsWebAccessor_NewestFirst pins the fix for the /events staleness
// bug: the backfill must return the MOST RECENT events first (and page
// backward through history), not the oldest. The old forward cursor surfaced
// the store's genesis events at the top of the "Event stream", making it look
// like nothing had synced for days.
func TestEventsWebAccessor_NewestFirst(t *testing.T) {
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	base := time.Date(2026, 6, 17, 15, 44, 46, 0, time.UTC)
	// Oldest -> newest. "new" is the genuinely-recent event a user expects
	// to see at the top of the feed.
	seedBackfillArtifact(t, s, "old", "claude-code", base)
	seedBackfillArtifact(t, s, "mid", "codex", base.Add(24*time.Hour))
	seedBackfillArtifact(t, s, "new", "kilo", base.Add(96*time.Hour))

	acc := &eventsWebAccessor{deps: &webAPIDeps{store: s}}

	// First page (no Before cursor) -> the newest event leads.
	page, err := acc.Backfill(apiweb.EventQuery{Before: 0, Limit: 2})
	if err != nil {
		t.Fatalf("backfill page 1: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("page 1: got %d events, want 2", len(page.Events))
	}
	if page.Events[0].Name != "new" {
		t.Errorf("page 1 [0].Name = %q, want \"new\" (newest first)", page.Events[0].Name)
	}
	if page.Events[1].Name != "mid" {
		t.Errorf("page 1 [1].Name = %q, want \"mid\"", page.Events[1].Name)
	}
	if !(page.Events[0].Seq > page.Events[1].Seq) {
		t.Errorf("page 1 not descending by Seq: %d then %d", page.Events[0].Seq, page.Events[1].Seq)
	}

	// Second page via the NextBefore cursor -> the older remainder.
	page2, err := acc.Backfill(apiweb.EventQuery{Before: page.NextBefore, Limit: 2})
	if err != nil {
		t.Fatalf("backfill page 2: %v", err)
	}
	if len(page2.Events) != 1 {
		t.Fatalf("page 2: got %d events, want 1 (tail)", len(page2.Events))
	}
	if page2.Events[0].Name != "old" {
		t.Errorf("page 2 [0].Name = %q, want \"old\"", page2.Events[0].Name)
	}
}

func TestEventsWebAccessor_FirstPageDoesNotReadEntireStore(t *testing.T) {
	s := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	base := time.Date(2026, 6, 17, 15, 44, 46, 0, time.UTC)
	for i := 0; i < 250; i++ {
		seedBackfillArtifact(t, s, fmt.Sprintf("old-%03d", i), "claude-code", base.Add(time.Duration(i)*time.Minute))
	}
	recentBase := base.Add(30 * 24 * time.Hour)
	for i := 0; i < 120; i++ {
		seedBackfillArtifact(t, s, fmt.Sprintf("recent-%03d", i), "codex", recentBase.Add(time.Duration(i)*time.Minute))
	}

	reads := 0
	acc := &eventsWebAccessor{
		deps: &webAPIDeps{store: s},
		readEventHeaders: func(k acf.Kind, id string, beforeMillis int64, limit int) ([]acf.Event, error) {
			reads++
			return s.ReadRecentEventHeaders(k, id, beforeMillis, limit)
		},
	}
	page, err := acc.Backfill(apiweb.EventQuery{Limit: 100})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(page.Events) != 100 {
		t.Fatalf("events = %d, want 100", len(page.Events))
	}
	if reads > 105 {
		t.Fatalf("read %d event logs, want roughly the requested page size, not the whole store", reads)
	}
	for _, ev := range page.Events {
		if !strings.HasPrefix(ev.Name, "recent-") {
			t.Fatalf("first page included old event %q", ev.Name)
		}
	}

	reads = 0
	if _, err := acc.Backfill(apiweb.EventQuery{Limit: 100}); err != nil {
		t.Fatalf("cache-hit backfill: %v", err)
	}
	if reads != 0 {
		t.Fatalf("cache hit read %d event logs, want 0", reads)
	}
}
