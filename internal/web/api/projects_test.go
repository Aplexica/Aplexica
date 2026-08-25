package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/project"
)

// registerCall records one invocation of the injected onRegister stub.
type registerCall struct {
	id, path string
}

// newProjectsTestHandler wires a ProjectsHandler over a fresh on-disk
// registry plus a recording onRegister stub. The returned slice pointer
// accumulates every onRegister call so tests can assert (id, path).
func newProjectsTestHandler(t *testing.T) (*ProjectsHandler, *project.Registry, *[]registerCall) {
	t.Helper()
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	calls := &[]registerCall{}
	h := NewProjectsHandler(reg, func(id, path string) error {
		*calls = append(*calls, registerCall{id: id, path: path})
		return nil
	})
	return h, reg, calls
}

func TestProjectsList_ReturnsRegisteredEntries(t *testing.T) {
	h, reg, _ := newProjectsTestHandler(t)
	path := t.TempDir()
	path, _ = filepath.EvalSymlinks(path)
	if err := reg.AddOrUpdate(project.Entry{
		ID: "github.com/example-user/repo", Path: path, VCS: "git",
		Scope: "global", Agents: []string{"codex"}, DisplayName: "repo",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got []projectView
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	g := got[0]
	if g.ID != "github.com/example-user/repo" || g.Path != path ||
		g.Scope != "global" || g.VCS != "git" || g.DisplayName != "repo" {
		t.Errorf("entry = %+v", g)
	}
	if len(g.Agents) != 1 || g.Agents[0] != "codex" {
		t.Errorf("agents = %+v", g.Agents)
	}
}

func TestProjectsList_EmptyIsArray(t *testing.T) {
	h, _, _ := newProjectsTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Body.String(); got != "[]\n" {
		t.Errorf("body = %q, want empty JSON array", got)
	}
}

func TestProjectsCreate_HappyPath(t *testing.T) {
	h, reg, calls := newProjectsTestHandler(t)
	dir := t.TempDir()

	body, _ := json.Marshal(map[string]any{
		"path": dir, "scope": "local", "agents": []string{"codex"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Create(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var got projectView
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	abs, _ = filepath.EvalSymlinks(abs)
	if got.Path != abs {
		t.Errorf("path = %q, want %q", got.Path, abs)
	}
	if got.Scope != "local" {
		t.Errorf("scope = %q, want local", got.Scope)
	}
	if got.DisplayName != filepath.Base(abs) {
		t.Errorf("displayName = %q, want %q", got.DisplayName, filepath.Base(abs))
	}

	// Persisted in the registry with those fields.
	ent, ok := reg.Get(got.ID)
	if !ok {
		t.Fatalf("entry %q not persisted", got.ID)
	}
	if ent.Path != abs || ent.EffectiveScope() != "local" {
		t.Errorf("persisted = %+v", ent)
	}
	if len(ent.Agents) != 1 || ent.Agents[0] != "codex" {
		t.Errorf("persisted agents = %+v", ent.Agents)
	}

	// onRegister invoked with the created (id, abs path).
	if len(*calls) != 1 {
		t.Fatalf("onRegister calls = %d, want 1", len(*calls))
	}
	if (*calls)[0].id != got.ID || (*calls)[0].path != abs {
		t.Errorf("onRegister = %+v, want {id:%q path:%q}", (*calls)[0], got.ID, abs)
	}
}

func TestProjectsCreate_NonexistentPath(t *testing.T) {
	h, _, calls := newProjectsTestHandler(t)
	body, _ := json.Marshal(map[string]any{
		"path": filepath.Join(t.TempDir(), "does-not-exist"), "scope": "local",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Create(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if len(*calls) != 0 {
		t.Errorf("onRegister should not be called on failure: %+v", *calls)
	}
}

func TestProjectsCreate_MissingPath(t *testing.T) {
	h, _, _ := newProjectsTestHandler(t)
	body, _ := json.Marshal(map[string]any{"scope": "local"})
	req := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Create(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestProjectsCreate_InvalidScope(t *testing.T) {
	h, _, calls := newProjectsTestHandler(t)
	dir := t.TempDir()
	body, _ := json.Marshal(map[string]any{"path": dir, "scope": "bogus"})
	req := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Create(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if len(*calls) != 0 {
		t.Errorf("onRegister should not be called on invalid scope: %+v", *calls)
	}
}

func TestProjectsApprove_WithBodyPath(t *testing.T) {
	h, reg, calls := newProjectsTestHandler(t)
	dir := t.TempDir()
	abs, _ := filepath.Abs(dir)
	abs, _ = filepath.EvalSymlinks(abs)
	info, _ := project.Detect(abs)

	body, _ := json.Marshal(map[string]any{"scope": "global", "path": dir})
	req := httptest.NewRequest(http.MethodPost, "/api/pending/"+info.ID+"/approve", bytes.NewReader(body))
	req.SetPathValue("id", info.ID)
	rr := httptest.NewRecorder()
	h.Approve(rr, req)

	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got projectView
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Scope != "global" {
		t.Errorf("scope = %q, want global", got.Scope)
	}
	if got.Path != abs {
		t.Errorf("path = %q, want %q", got.Path, abs)
	}

	ent, ok := reg.Get(got.ID)
	if !ok {
		t.Fatalf("entry %q not persisted", got.ID)
	}
	if ent.EffectiveScope() != "global" {
		t.Errorf("persisted scope = %q, want global", ent.EffectiveScope())
	}

	if len(*calls) != 1 {
		t.Fatalf("onRegister calls = %d, want 1", len(*calls))
	}
	if (*calls)[0].path != abs {
		t.Errorf("onRegister path = %q, want %q", (*calls)[0].path, abs)
	}
}

func TestProjectsApprove_MissingPath(t *testing.T) {
	h, _, calls := newProjectsTestHandler(t)
	body, _ := json.Marshal(map[string]any{"scope": "local"})
	req := httptest.NewRequest(http.MethodPost, "/api/pending/some-id/approve", bytes.NewReader(body))
	req.SetPathValue("id", "some-id")
	rr := httptest.NewRecorder()
	h.Approve(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if len(*calls) != 0 {
		t.Errorf("onRegister should not be called: %+v", *calls)
	}
}
