package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeConversationBranchesAccessor struct {
	list     ConversationBranchesResponse
	listOK   bool
	listErr  error
	forkReq  ConversationForkRequest
	forkResp ConversationBranchMutationResponse
	forkErr  error
	coReq    ConversationCheckoutRequest
	coResp   ConversationBranchMutationResponse
	coErr    error
}

func (f *fakeConversationBranchesAccessor) ListConversationBranches(id string) (ConversationBranchesResponse, bool, error) {
	if f.list.ArtifactID == "" {
		f.list.ArtifactID = id
	}
	return f.list, f.listOK, f.listErr
}

func (f *fakeConversationBranchesAccessor) ForkConversation(_ string, req ConversationForkRequest) (ConversationBranchMutationResponse, error) {
	f.forkReq = req
	return f.forkResp, f.forkErr
}

func (f *fakeConversationBranchesAccessor) CheckoutConversation(_ string, req ConversationCheckoutRequest) (ConversationBranchMutationResponse, error) {
	f.coReq = req
	return f.coResp, f.coErr
}

func TestConversationBranchesList(t *testing.T) {
	acc := &fakeConversationBranchesAccessor{
		listOK: true,
		list: ConversationBranchesResponse{Branches: []ConversationBranch{{
			Name:               "main",
			LastEventAt:        time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
			EventCount:         3,
			MaterializedAgents: []string{"codex"},
		}}},
	}
	h := NewConversationBranchesHandler(acc)
	req := httptest.NewRequest(http.MethodGet, "/api/conversations/c1/branches", nil)
	req.SetPathValue("id", "c1")
	rr := httptest.NewRecorder()

	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got ConversationBranchesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ArtifactID != "c1" || len(got.Branches) != 1 || got.Branches[0].MaterializedAgents[0] != "codex" {
		t.Fatalf("response = %+v", got)
	}
}

func TestConversationBranchesList_DefaultsNilMaterializedAgents(t *testing.T) {
	acc := &fakeConversationBranchesAccessor{
		listOK: true,
		list: ConversationBranchesResponse{Branches: []ConversationBranch{{
			Name:       "main",
			EventCount: 3,
		}}},
	}
	h := NewConversationBranchesHandler(acc)
	req := httptest.NewRequest(http.MethodGet, "/api/conversations/c1/branches", nil)
	req.SetPathValue("id", "c1")
	rr := httptest.NewRecorder()

	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	branches := raw["branches"].([]any)
	agents, ok := branches[0].(map[string]any)["materializedAgents"].([]any)
	if !ok || len(agents) != 0 {
		t.Fatalf("materializedAgents should be []; body=%s", rr.Body.String())
	}
}

func TestConversationBranchesList_NotFound(t *testing.T) {
	h := NewConversationBranchesHandler(&fakeConversationBranchesAccessor{})
	req := httptest.NewRequest(http.MethodGet, "/api/conversations/missing/branches", nil)
	req.SetPathValue("id", "missing")
	rr := httptest.NewRecorder()

	h.List(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestConversationFork_Validation(t *testing.T) {
	h := NewConversationBranchesHandler(&fakeConversationBranchesAccessor{})
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/fork", bytes.NewReader([]byte(`{"targetAgent":"codex"}`)))
	req.SetPathValue("id", "c1")
	rr := httptest.NewRecorder()

	h.Fork(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestConversationFork_HappyPath(t *testing.T) {
	acc := &fakeConversationBranchesAccessor{
		forkResp: ConversationBranchMutationResponse{
			ArtifactID:    "c1",
			Branch:        "review",
			Agent:         "codex",
			Materialized:  true,
			Operation:     "fork",
			CreatedBranch: true,
		},
	}
	h := NewConversationBranchesHandler(acc)
	body := []byte(`{"fromEventId":"e1","targetAgent":"codex","branch":"review","rationale":"try it"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/fork", bytes.NewReader(body))
	req.SetPathValue("id", "c1")
	rr := httptest.NewRecorder()

	h.Fork(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if acc.forkReq.FromEventID != "e1" || acc.forkReq.TargetAgent != "codex" || acc.forkReq.Branch != "review" {
		t.Fatalf("request = %+v", acc.forkReq)
	}
}

func TestConversationCheckout_BranchNotFound(t *testing.T) {
	acc := &fakeConversationBranchesAccessor{
		coErr: ErrConversationBranchNotFound,
	}
	h := NewConversationBranchesHandler(acc)
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/checkout", bytes.NewReader([]byte(`{"agent":"codex","branch":"ghost"}`)))
	req.SetPathValue("id", "c1")
	rr := httptest.NewRecorder()

	h.Checkout(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestConversationCheckout_InternalError(t *testing.T) {
	acc := &fakeConversationBranchesAccessor{
		coErr: errors.New("boom"),
	}
	h := NewConversationBranchesHandler(acc)
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/c1/checkout", bytes.NewReader([]byte(`{"agent":"codex","branch":"main"}`)))
	req.SetPathValue("id", "c1")
	rr := httptest.NewRecorder()

	h.Checkout(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}
