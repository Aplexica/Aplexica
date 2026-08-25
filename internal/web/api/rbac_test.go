package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeRBACAccessor struct {
	role     string
	caps     []string
	err      error
	askedNS  string
	askCount int
}

func (f *fakeRBACAccessor) NamespaceRole(_ context.Context, namespaceID string) (string, []string, error) {
	f.askedNS = namespaceID
	f.askCount++
	return f.role, f.caps, f.err
}

func TestRBAC_GET_ReturnsRoleAndCapabilities(t *testing.T) {
	acc := &fakeRBACAccessor{role: "editor", caps: []string{"read", "create_artifact", "edit_artifact"}}
	h := NewRBACHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rbac/namespace/ns-1", nil)
	req.SetPathValue("id", "ns-1")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Role         string   `json:"role"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Role != "editor" {
		t.Errorf("role = %q, want editor", got.Role)
	}
	if len(got.Capabilities) != 3 || got.Capabilities[0] != "read" {
		t.Errorf("capabilities = %v", got.Capabilities)
	}
	if acc.askedNS != "ns-1" {
		t.Errorf("accessor asked namespace %q, want ns-1", acc.askedNS)
	}
}

// No membership / unpaired: the accessor returns an empty role + empty caps
// (no error). The endpoint is total like /api/remote/status — it returns 200
// with role:"" so the SPA renders "no access" uniformly.
func TestRBAC_GET_NoMembership_Returns200EmptyRole(t *testing.T) {
	acc := &fakeRBACAccessor{role: "", caps: []string{}}
	h := NewRBACHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rbac/namespace/ns-x", nil)
	req.SetPathValue("id", "ns-x")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["role"] != "" {
		t.Errorf("role = %v, want empty", got["role"])
	}
	// capabilities must serialize as an empty array, never null, so the SPA
	// can iterate it unconditionally.
	caps, ok := got["capabilities"].([]any)
	if !ok || len(caps) != 0 {
		t.Errorf("capabilities = %v (type %T), want []", got["capabilities"], got["capabilities"])
	}
}

// A missing path id is a client error (defensive — the router pattern always
// supplies it, but the handler must not ask the accessor with an empty id).
func TestRBAC_GET_MissingID(t *testing.T) {
	acc := &fakeRBACAccessor{}
	h := NewRBACHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rbac/namespace/", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if errCode(t, rr) != "validation" {
		t.Errorf("code = %q, want validation", errCode(t, rr))
	}
	if acc.askCount != 0 {
		t.Errorf("accessor must not be called with an empty id")
	}
}

// A transient transport failure (plugin reconnecting) is surfaced as a
// documented retryable code, distinct from a no-access answer, so the SPA can
// retry rather than render "no access".
func TestRBAC_GET_Unavailable(t *testing.T) {
	acc := &fakeRBACAccessor{err: ErrRBACUnavailable}
	h := NewRBACHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rbac/namespace/ns-1", nil)
	req.SetPathValue("id", "ns-1")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if errCode(t, rr) != "rbac_unavailable" {
		t.Errorf("code = %q, want rbac_unavailable", errCode(t, rr))
	}
}

// A generic accessor error maps to a 500 internal.
func TestRBAC_GET_InternalError(t *testing.T) {
	acc := &fakeRBACAccessor{err: errors.New("boom")}
	h := NewRBACHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/rbac/namespace/ns-1", nil)
	req.SetPathValue("id", "ns-1")
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if errCode(t, rr) != "internal" {
		t.Errorf("code = %q, want internal", errCode(t, rr))
	}
}
