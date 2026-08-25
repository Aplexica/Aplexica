package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aplexica/aplexica/internal/pending"
)

type fakePendingAccessor struct {
	list    []pending.Project
	links   []linkCall
	linkErr error
}

type linkCall struct {
	id, path string
}

func (f *fakePendingAccessor) List() ([]pending.Project, error) {
	return f.list, nil
}

func (f *fakePendingAccessor) Link(id, localPath string) error {
	f.links = append(f.links, linkCall{id, localPath})
	return f.linkErr
}

func TestPendingList_HappyPath(t *testing.T) {
	acc := &fakePendingAccessor{
		list: []pending.Project{
			{ID: "p1", ArtifactCount: 3, SamplePath: "/a/b"},
		},
	}
	h := NewPendingHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/pending", nil)
	rr := httptest.NewRecorder()
	h.List(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got []pending.Project
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Errorf("pending = %+v", got)
	}
}

func TestPendingLink_HappyPath(t *testing.T) {
	acc := &fakePendingAccessor{}
	h := NewPendingHandler(acc)

	body, _ := json.Marshal(map[string]any{"localPath": "/tmp/proj"})
	req := httptest.NewRequest(http.MethodPost, "/api/pending/p1/link", bytes.NewReader(body))
	req.SetPathValue("id", "p1")
	rr := httptest.NewRecorder()
	h.Link(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if len(acc.links) != 1 || acc.links[0].id != "p1" || acc.links[0].path != "/tmp/proj" {
		t.Errorf("link calls = %+v", acc.links)
	}
}

func TestPendingLink_MissingLocalPath(t *testing.T) {
	acc := &fakePendingAccessor{}
	h := NewPendingHandler(acc)

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/api/pending/p1/link", bytes.NewReader(body))
	req.SetPathValue("id", "p1")
	rr := httptest.NewRecorder()
	h.Link(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
