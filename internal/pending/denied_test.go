// SPDX-License-Identifier: AGPL-3.0-or-later
package pending

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeniedStore_AddHasRemoveListPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denied.json")
	s, err := LoadDenied(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Has("a") {
		t.Fatal("empty store should not contain 'a'")
	}

	if err := s.Add("a", "/x/a"); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if err := s.Add("b", "/x/b"); err != nil {
		t.Fatalf("add b: %v", err)
	}
	if err := s.Add("a", "/x/a"); err != nil { // idempotent
		t.Fatalf("add dup: %v", err)
	}
	if !s.Has("a") || !s.Has("b") {
		t.Fatal("expected a and b denied")
	}
	if got := len(s.List()); got != 2 {
		t.Fatalf("List len=%d want 2", got)
	}

	// Reload from disk — the denied set must persist.
	s2, err := LoadDenied(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !s2.Has("a") || !s2.Has("b") {
		t.Fatal("denied set was not persisted")
	}

	if err := s2.Remove("a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if s2.Has("a") {
		t.Fatal("'a' should be un-denied")
	}
	if err := s2.Remove("a"); err != nil { // idempotent
		t.Fatalf("remove idempotent: %v", err)
	}

	s3, _ := LoadDenied(path)
	if s3.Has("a") || !s3.Has("b") {
		t.Fatal("remove was not persisted correctly")
	}
}

func TestLoadDenied_MissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()

	s, err := LoadDenied(filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("missing file should yield an empty store")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadDenied(bad)
	if err != nil {
		t.Fatalf("corrupt file should not error: %v", err)
	}
	if len(s2.List()) != 0 {
		t.Fatal("corrupt file should be treated as empty")
	}
}

func TestLoadDenied_IgnoresFilesystemRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denied.json")
	err := os.WriteFile(path, []byte(`{
  "version": "1",
  "denied": [
    {"id": "local:root", "path": "/"},
    {"id": "local:repo", "path": "/Users/testuser/repo"}
  ]
}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	s, err := LoadDenied(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.Has("local:root") {
		t.Fatal("filesystem root should not be restored as a denied project")
	}
	if !s.Has("local:repo") {
		t.Fatal("valid denied project should still load")
	}

	if err := s.Add("local:root2", "/"); err != nil {
		t.Fatalf("add root: %v", err)
	}
	if s.Has("local:root2") {
		t.Fatal("filesystem root should not be persisted as a denied project")
	}
}

func TestApplyDenied(t *testing.T) {
	list := []Project{
		{ID: "p1", SamplePath: "/x/p1", Source: "discovered"},
		{ID: "p2", SamplePath: "/x/p2", Source: "discovered"},
	}
	denied := []DeniedEntry{
		{ID: "p2", Path: "/x/p2"}, // present in list → marked in place
		{ID: "p3", Path: "/x/p3"}, // absent → appended as synthetic row
	}

	out := ApplyDenied(list, denied)

	byID := map[string]Project{}
	for _, p := range out {
		byID[p.ID] = p
	}
	if byID["p1"].Denied {
		t.Error("p1 should not be denied")
	}
	if !byID["p2"].Denied {
		t.Error("p2 should be denied")
	}
	p3, ok := byID["p3"]
	if !ok {
		t.Fatal("p3 should be appended as a synthetic denied row")
	}
	if !p3.Denied || p3.SamplePath != "/x/p3" || p3.Source != "discovered" {
		t.Errorf("p3 synthetic row wrong: %+v", p3)
	}
	if len(out) != 3 {
		t.Fatalf("ApplyDenied len=%d want 3", len(out))
	}
	// Input slice must not be mutated.
	if list[1].Denied {
		t.Error("ApplyDenied mutated the input slice")
	}
}
