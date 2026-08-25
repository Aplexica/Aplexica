package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
)

type fakeConflictsAccessor struct {
	list          []conflicts.Conflict
	resolved      []resolveCall
	resolveOK     bool
	resolveOn     string
	analysis      *ConflictAnalysis
	listSummaries map[string]ConflictListSummary
}

type resolveCall struct {
	id, action string
	manualBody string
}

func (f *fakeConflictsAccessor) List() ([]conflicts.Conflict, error) {
	return f.list, nil
}

func (f *fakeConflictsAccessor) Get(id string) (conflicts.Conflict, bool, error) {
	for _, c := range f.list {
		if c.ArtifactID == id {
			return c, true, nil
		}
	}
	return conflicts.Conflict{}, false, nil
}

func (f *fakeConflictsAccessor) Resolve(id, action, manualBody string) error {
	f.resolved = append(f.resolved, resolveCall{id, action, manualBody})
	if !f.resolveOK {
		return ErrConflictNotFound
	}
	return nil
}

func (f *fakeConflictsAccessor) Analyze(conflicts.Conflict) (*ConflictAnalysis, error) {
	return f.analysis, nil
}

func (f *fakeConflictsAccessor) ConflictListSummary(c conflicts.Conflict) (ConflictListSummary, bool) {
	if f.listSummaries == nil {
		return ConflictListSummary{}, false
	}
	summary, ok := f.listSummaries[c.ArtifactID]
	return summary, ok
}

func TestConflictsList_HappyPath(t *testing.T) {
	acc := &fakeConflictsAccessor{
		list: []conflicts.Conflict{
			{ArtifactID: "a1", Kind: acf.KindMemory, Heads: []conflicts.Head{{SourceAgent: "claude-code"}}},
		},
	}
	h := NewConflictsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/conflicts", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []conflicts.Conflict
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].ArtifactID != "a1" {
		t.Errorf("conflicts = %+v", got)
	}
}

func TestConflictsList_IncludesReadableSummaryWhenAvailable(t *testing.T) {
	acc := &fakeConflictsAccessor{
		list: []conflicts.Conflict{
			{ArtifactID: "c1", Kind: acf.KindConversation, Heads: []conflicts.Head{{SourceAgent: "claude-code"}}},
		},
		listSummaries: map[string]ConflictListSummary{
			"c1": {
				Title:       "What is the luna size?",
				Description: "Closest galaxy depends on what you mean by closest.",
			},
		},
	}
	h := NewConflictsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/conflicts", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []struct {
		ArtifactID   string `json:"artifactId"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		PayloadBytes string `json:"payloadPreview"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("conflicts = %+v", got)
	}
	if got[0].Title != "What is the luna size?" || got[0].Description == "" {
		t.Fatalf("summary fields missing: %+v", got[0])
	}
}

func TestConflictsList_StripsPayloads(t *testing.T) {
	acc := &fakeConflictsAccessor{
		list: []conflicts.Conflict{
			{
				ArtifactID: "a1",
				Kind:       acf.KindConversation,
				Heads: []conflicts.Head{{
					SourceAgent:    "claude-code",
					EventID:        "e1",
					ContentSHA256:  "sha",
					PayloadPreview: `{"large":"preview"}`,
					FullPayload:    json.RawMessage(`{"large":"payload"}`),
				}},
			},
		},
	}
	h := NewConflictsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/conflicts", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []conflicts.Conflict
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || len(got[0].Heads) != 1 {
		t.Fatalf("conflicts = %+v", got)
	}
	head := got[0].Heads[0]
	if head.SourceAgent != "claude-code" || head.EventID != "e1" || head.ContentSHA256 != "sha" {
		t.Fatalf("head metadata was not preserved: %+v", head)
	}
	if head.PayloadPreview != "" || len(head.FullPayload) != 0 {
		t.Fatalf("list response leaked payload fields: %+v", head)
	}
}

func TestConflictsGet_HappyPath(t *testing.T) {
	acc := &fakeConflictsAccessor{
		list: []conflicts.Conflict{{ArtifactID: "a1"}},
	}
	h := NewConflictsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/conflicts/a1", nil)
	req.SetPathValue("id", "a1")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestConflictsGet_IncludesAnalysisWhenAvailable(t *testing.T) {
	acc := &fakeConflictsAccessor{
		list: []conflicts.Conflict{{ArtifactID: "a1"}},
		analysis: &ConflictAnalysis{
			Summary:        "Visible conversation differs at turn 1.",
			Recommendation: "Review the highlighted turn before choosing.",
			Differences: []ConflictDifference{{
				Label:  "Turn 1",
				HeadA:  "user: what is my name?",
				HeadB:  "user: what is my dog's name?",
				Status: "changed",
			}},
		},
	}
	h := NewConflictsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/conflicts/a1", nil)
	req.SetPathValue("id", "a1")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		ArtifactID string            `json:"artifactId"`
		Analysis   *ConflictAnalysis `json:"analysis"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Analysis == nil {
		t.Fatalf("analysis missing in response: %s", rr.Body.String())
	}
	if got.Analysis.Differences[0].HeadA != "user: what is my name?" {
		t.Fatalf("analysis = %+v", got.Analysis)
	}
}

func TestConflictsGet_StripsFullPayloadFromDetailConflict(t *testing.T) {
	acc := &fakeConflictsAccessor{
		list: []conflicts.Conflict{{
			ArtifactID: "a1",
			Kind:       acf.KindConversation,
			Heads: []conflicts.Head{{
				SourceAgent:    "claude-code",
				EventID:        "e1",
				ContentSHA256:  "sha",
				PayloadPreview: `{"large":"preview"}`,
				FullPayload:    json.RawMessage(`{"large":"payload"}`),
			}},
		}},
	}
	h := NewConflictsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/conflicts/a1", nil)
	req.SetPathValue("id", "a1")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	heads, ok := got["heads"].([]any)
	if !ok || len(heads) != 1 {
		t.Fatalf("heads = %#v", got["heads"])
	}
	head, ok := heads[0].(map[string]any)
	if !ok {
		t.Fatalf("head = %#v", heads[0])
	}
	if _, ok := head["fullPayload"]; ok {
		t.Fatalf("detail response leaked fullPayload: %s", rr.Body.String())
	}
	if head["payloadPreview"] == "" {
		t.Fatalf("detail response should keep the short preview: %s", rr.Body.String())
	}
}

func TestConflictsGet_NotFound(t *testing.T) {
	acc := &fakeConflictsAccessor{}
	h := NewConflictsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/conflicts/missing", nil)
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestConflictsResolve_AcceptA(t *testing.T) {
	acc := &fakeConflictsAccessor{resolveOK: true}
	h := NewConflictsHandler(acc)

	body, _ := json.Marshal(map[string]any{"action": "accept-a"})
	req := httptest.NewRequest(http.MethodPost, "/api/conflicts/a1/resolve", bytes.NewReader(body))
	req.SetPathValue("id", "a1")
	rr := httptest.NewRecorder()
	h.Resolve(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if len(acc.resolved) != 1 || acc.resolved[0].action != "accept-a" {
		t.Errorf("resolved = %+v", acc.resolved)
	}
}

func TestConflictsResolve_ManualRequiresBody(t *testing.T) {
	acc := &fakeConflictsAccessor{resolveOK: true}
	h := NewConflictsHandler(acc)

	body, _ := json.Marshal(map[string]any{"action": "manual"})
	req := httptest.NewRequest(http.MethodPost, "/api/conflicts/a1/resolve", bytes.NewReader(body))
	req.SetPathValue("id", "a1")
	rr := httptest.NewRecorder()
	h.Resolve(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestConflictsResolve_InvalidAction(t *testing.T) {
	acc := &fakeConflictsAccessor{}
	h := NewConflictsHandler(acc)

	body, _ := json.Marshal(map[string]any{"action": "yolo"})
	req := httptest.NewRequest(http.MethodPost, "/api/conflicts/a1/resolve", bytes.NewReader(body))
	req.SetPathValue("id", "a1")
	rr := httptest.NewRecorder()
	h.Resolve(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestConflictsResolve_NotFound(t *testing.T) {
	acc := &fakeConflictsAccessor{resolveOK: false}
	h := NewConflictsHandler(acc)

	body, _ := json.Marshal(map[string]any{"action": "accept-a"})
	req := httptest.NewRequest(http.MethodPost, "/api/conflicts/missing/resolve", bytes.NewReader(body))
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()
	h.Resolve(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}
