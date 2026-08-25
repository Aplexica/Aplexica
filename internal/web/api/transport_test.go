package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aplexica/aplexica/internal/transport"
)

type fakeTransportAccessor struct {
	info      transport.Info
	setCalled string
	setErr    error
	byoCalled transport.BYORelayOpts
	byoErr    error
}

func (f *fakeTransportAccessor) Get() transport.Info {
	return f.info
}

func (f *fakeTransportAccessor) Set(mode string) error {
	f.setCalled = mode
	return f.setErr
}

func (f *fakeTransportAccessor) SetBYO(opts transport.BYORelayOpts) error {
	f.byoCalled = opts
	return f.byoErr
}

func TestTransportGet_LocalOnly(t *testing.T) {
	acc := &fakeTransportAccessor{info: transport.LocalOnly}
	h := NewTransportHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/transport", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["mode"] != "local" {
		t.Errorf("mode = %v, want local", got["mode"])
	}
}

func TestTransportSet_LocalOK(t *testing.T) {
	acc := &fakeTransportAccessor{info: transport.LocalOnly}
	h := NewTransportHandler(acc)

	body, _ := json.Marshal(map[string]any{"mode": "local"})
	req := httptest.NewRequest(http.MethodPut, "/api/transport", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Set(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if acc.setCalled != "local" {
		t.Errorf("Set mode = %q, want local", acc.setCalled)
	}
}

func TestTransportSet_LocalOnlyAlias(t *testing.T) {
	// The spec's wire shape mentions "local-only" as the readable alias
	// for the locally-bound mode; the daemon-side enum uses "local".
	// The handler must accept the spec's spelling and translate.
	acc := &fakeTransportAccessor{info: transport.LocalOnly}
	h := NewTransportHandler(acc)

	body, _ := json.Marshal(map[string]any{"mode": "local-only"})
	req := httptest.NewRequest(http.MethodPut, "/api/transport", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Set(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestTransportSet_BYORelayDeferred(t *testing.T) {
	acc := &fakeTransportAccessor{info: transport.LocalOnly}
	h := NewTransportHandler(acc)

	body, _ := json.Marshal(map[string]any{"mode": "byo-relay"})
	req := httptest.NewRequest(http.MethodPut, "/api/transport", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Set(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	var be ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &be); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if be.Code != "not_yet_implemented" {
		t.Errorf("code = %q, want not_yet_implemented", be.Code)
	}
}

func TestTransportSet_InvalidMode(t *testing.T) {
	acc := &fakeTransportAccessor{}
	h := NewTransportHandler(acc)

	body, _ := json.Marshal(map[string]any{"mode": "yolo"})
	req := httptest.NewRequest(http.MethodPut, "/api/transport", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Set(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestTransportSetBYO_Deferred(t *testing.T) {
	acc := &fakeTransportAccessor{}
	h := NewTransportHandler(acc)

	body, _ := json.Marshal(map[string]any{
		"url":          "mqtts://example.com",
		"mtlsCertPath": "/c.pem",
		"mtlsKeyPath":  "/k.pem",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/transport/byo", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.SetBYO(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	var be ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &be); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if be.Code != "not_yet_implemented" {
		t.Errorf("code = %q, want not_yet_implemented", be.Code)
	}
}

func TestTransportSetBYO_MissingURL(t *testing.T) {
	acc := &fakeTransportAccessor{}
	h := NewTransportHandler(acc)

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/api/transport/byo", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.SetBYO(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
