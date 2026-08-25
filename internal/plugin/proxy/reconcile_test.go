package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func newTempStore(t *testing.T) *acf.Store {
	t.Helper()
	dir := t.TempDir()
	s := &acf.Store{Root: filepath.Join(dir, "store")}
	if err := s.Init(); err != nil {
		t.Fatalf("init store: %v", err)
	}
	return s
}

func memPayload(t *testing.T, content string) json.RawMessage {
	t.Helper()
	b, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestApplyImportedItemCreate(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "CLAUDE.md")
	os.WriteFile(sourcePath, []byte("hello"), 0o644)

	item := proto.ImportedItem{
		Kind:       acf.KindMemory,
		Scope:      acf.ScopeProject,
		Name:       "CLAUDE.md",
		SourcePath: sourcePath,
		Payload:    memPayload(t, "hello"),
	}
	rec := Reconciler{
		Store:          store,
		DeviceID:       "host-1",
		SourceAgent:    "claude-code",
		AdapterVersion: "0.2.0",
	}
	id, err := rec.Apply(context.Background(), item, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if id == "" {
		t.Error("id must be non-empty")
	}
	art, err := store.ReadArtifact(acf.KindMemory, id)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if art.SourcePath != sourcePath {
		t.Errorf("got source_path %q", art.SourcePath)
	}
	events, _ := store.ReadEvents(acf.KindMemory, id)
	if len(events) != 1 || events[0].Type != acf.EventTypeCreate {
		t.Errorf("events: %+v", events)
	}
}

func TestApplyImportedItemUpdateReusesID(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "CLAUDE.md")
	os.WriteFile(sourcePath, []byte("hello"), 0o644)

	rec := Reconciler{Store: store, DeviceID: "h", SourceAgent: "claude-code", AdapterVersion: "0.2.0"}

	first := proto.ImportedItem{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: "CLAUDE.md", SourcePath: sourcePath, Payload: memPayload(t, "v1")}
	id1, err := rec.Apply(context.Background(), first, "")
	if err != nil {
		t.Fatal(err)
	}

	second := proto.ImportedItem{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: "CLAUDE.md", SourcePath: sourcePath, Payload: memPayload(t, "v2")}
	id2, err := rec.Apply(context.Background(), second, "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("re-import must reuse ID: got %s then %s", id1, id2)
	}
	events, _ := store.ReadEvents(acf.KindMemory, id2)
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[1].Type != acf.EventTypeUpdate {
		t.Errorf("second event must be update, got %s", events[1].Type)
	}
	if events[1].ParentHash != events[0].Hash {
		t.Errorf("parent_hash chain broken")
	}
}

func TestApplyImportedItemCausedByStamped(t *testing.T) {
	store := newTempStore(t)
	tmp := t.TempDir()
	sourcePath := filepath.Join(tmp, "CLAUDE.md")
	os.WriteFile(sourcePath, []byte("hello"), 0o644)

	rec := Reconciler{Store: store, DeviceID: "h", SourceAgent: "claude-code", AdapterVersion: "0.2.0"}
	item := proto.ImportedItem{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: "CLAUDE.md", SourcePath: sourcePath, Payload: memPayload(t, "x")}
	id, err := rec.Apply(context.Background(), item, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	events, _ := store.ReadEvents(acf.KindMemory, id)
	if events[0].Provenance.CausedBy != "deadbeef" {
		t.Errorf("caused_by = %q, want deadbeef", events[0].Provenance.CausedBy)
	}
}
