package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeAgent struct {
	name           string
	version        string
	surfaces       []string
	activeSurfaces []string
	syncState      string
	lastActivity   time.Time
	namespaces     []string
	installed      bool
	globalRoots    []string
	artifactCount  int
}

type fakeAgentsAccessor struct {
	list    []fakeAgent
	getName string
	getOK   bool
	getOne  fakeAgent
}

func (f *fakeAgentsAccessor) List() []AgentSummary {
	out := make([]AgentSummary, 0, len(f.list))
	for _, a := range f.list {
		out = append(out, AgentSummary{
			Name:           a.name,
			Version:        a.version,
			Surfaces:       a.surfaces,
			ActiveSurfaces: a.activeSurfaces,
			SyncState:      a.syncState,
			LastActivity:   a.lastActivity,
			Installed:      a.installed,
			GlobalRoots:    a.globalRoots,
			ArtifactCount:  a.artifactCount,
		})
	}
	return out
}

func (f *fakeAgentsAccessor) Get(name string) (AgentDetail, bool) {
	if name != f.getName || !f.getOK {
		return AgentDetail{}, false
	}
	return AgentDetail{
		AgentSummary: AgentSummary{
			Name:           f.getOne.name,
			Version:        f.getOne.version,
			Surfaces:       f.getOne.surfaces,
			ActiveSurfaces: f.getOne.activeSurfaces,
			SyncState:      f.getOne.syncState,
			LastActivity:   f.getOne.lastActivity,
		},
		Namespaces:   f.getOne.namespaces,
		RecentEvents: []AgentEvent{},
	}, true
}

func TestAgentsList_HappyPath(t *testing.T) {
	now := time.Now()
	acc := &fakeAgentsAccessor{
		list: []fakeAgent{
			{name: "claude-code", version: "v1", syncState: "active", lastActivity: now},
			{name: "codex", version: "v2", syncState: "idle", lastActivity: time.Time{}},
		},
	}
	h := NewAgentsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2", len(got))
	}
	if got[0]["name"] != "claude-code" {
		t.Errorf("agents[0].name = %v, want claude-code", got[0]["name"])
	}
	if got[1]["syncState"] != "idle" {
		t.Errorf("agents[1].syncState = %v, want idle", got[1]["syncState"])
	}
}

func TestAgentSummary_PresenceFields(t *testing.T) {
	acc := &fakeAgentsAccessor{
		list: []fakeAgent{
			{name: "claude-code", version: "0.2.0", surfaces: []string{"cli", "desktop"}, activeSurfaces: []string{"desktop"}, syncState: "idle", installed: true, globalRoots: []string{"/home/u/.claude"}, artifactCount: 3},
			{name: "kilo", version: "0.1.0", syncState: "idle", installed: false},
		},
	}
	h := NewAgentsHandler(acc)
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	rec := httptest.NewRecorder()
	h.List(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `"installed":true`) || !strings.Contains(body, `"installed":false`) {
		t.Errorf("response missing installed flags: %s", body)
	}
	if !strings.Contains(body, `"artifactCount":3`) {
		t.Errorf("response missing artifactCount: %s", body)
	}
	if !strings.Contains(body, `"surfaces":["cli","desktop"]`) {
		t.Errorf("response missing surfaces: %s", body)
	}
	if !strings.Contains(body, `"activeSurfaces":["desktop"]`) {
		t.Errorf("response missing active surfaces: %s", body)
	}
	if !strings.Contains(body, `"globalRoots":["/home/u/.claude"]`) {
		t.Errorf("response missing globalRoots for installed agent: %s", body)
	}
}

func TestAgentsGet_HappyPath(t *testing.T) {
	acc := &fakeAgentsAccessor{
		getName: "claude-code",
		getOK:   true,
		getOne: fakeAgent{
			name:       "claude-code",
			version:    "v1",
			syncState:  "active",
			namespaces: []string{"global", "project:foo"},
		},
	}
	h := NewAgentsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/agents/claude-code", nil)
	req.SetPathValue("name", "claude-code")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["name"] != "claude-code" {
		t.Errorf("name = %v, want claude-code", got["name"])
	}
	if ns, ok := got["namespaces"].([]any); !ok || len(ns) != 2 {
		t.Errorf("namespaces = %v, want [global, project:foo]", got["namespaces"])
	}
}

func TestAgentsGet_NotFound(t *testing.T) {
	acc := &fakeAgentsAccessor{}
	h := NewAgentsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/agents/unknown", nil)
	req.SetPathValue("name", "unknown")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "not_found" {
		t.Errorf("code = %q, want not_found", body.Code)
	}
}
