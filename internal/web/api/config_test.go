package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
)

type fakeConfigAccessor struct {
	cfg    *daemon.Config
	path   string
	loadOK bool
	patch  map[string]any
}

func (f *fakeConfigAccessor) Load() (*daemon.Config, error) {
	if !f.loadOK {
		return &daemon.Config{}, nil
	}
	return f.cfg, nil
}

func (f *fakeConfigAccessor) Patch(updates map[string]any) error {
	f.patch = updates
	return nil
}

func (f *fakeConfigAccessor) RawPath() string {
	return f.path
}

func TestConfigGet_HappyPath(t *testing.T) {
	acc := &fakeConfigAccessor{
		loadOK: true,
		cfg:    &daemon.Config{LogLevel: "debug", Dir: "/tmp/dir"},
	}
	h := NewConfigHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["logLevel"] != "debug" {
		t.Errorf("logLevel = %v, want debug", got["logLevel"])
	}
}

func TestConfigPatch_AcceptsWhitelistedKey(t *testing.T) {
	acc := &fakeConfigAccessor{loadOK: true, cfg: &daemon.Config{}}
	h := NewConfigHandler(acc)

	body, _ := json.Marshal(map[string]any{"logLevel": "warn"})
	req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Patch(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if acc.patch["logLevel"] != "warn" {
		t.Errorf("accessor.patch[logLevel] = %v, want warn", acc.patch["logLevel"])
	}
}

func TestConfigPatch_RejectsNonWhitelisted(t *testing.T) {
	acc := &fakeConfigAccessor{}
	h := NewConfigHandler(acc)

	body, _ := json.Marshal(map[string]any{"hermesDB": "/etc/shadow"})
	req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Patch(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var be ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &be); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if be.Code != "validation" {
		t.Errorf("code = %q, want validation", be.Code)
	}
}

func TestConfigPatch_TypeMismatch(t *testing.T) {
	// A whitelisted key whose value has the wrong type must 400, NOT 200
	// with a false {"updated": ...} echo — the accessor silently drops a
	// mismatched value, so the handler may not assert it was applied.
	cases := []struct {
		name string
		body map[string]any
	}{
		{"stringKeyGetsNumber", map[string]any{"logLevel": 123}},
		{"numberKeyGetsString", map[string]any{"snapshotCadenceConversation": "abc"}},
		{"watermarkGetsString", map[string]any{"storeHighWatermarkGB": "lots"}},
		{"durationGetsBadString", map[string]any{"hermesWatchInterval": "not-a-duration"}},
		{"durationGetsBool", map[string]any{"snapshotMaxAgeMemory": true}},
		{"objectKeyGetsScalar", map[string]any{"web": "abc"}},
		{"webPortGetsString", map[string]any{"web": map[string]any{"port": "abc"}}},
		{"trayEnabledGetsString", map[string]any{"tray": map[string]any{"enabled": "yes"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := &fakeConfigAccessor{}
			h := NewConfigHandler(acc)

			body, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewReader(body))
			rr := httptest.NewRecorder()
			h.Patch(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if acc.patch != nil {
				t.Errorf("accessor.Patch was called with %v; want not called on type mismatch", acc.patch)
			}
			var be ErrorBody
			if err := json.Unmarshal(rr.Body.Bytes(), &be); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if be.Code != "validation" {
				t.Errorf("code = %q, want validation", be.Code)
			}
		})
	}
}

func TestConfigPatch_AcceptsValidTypedValues(t *testing.T) {
	// The valid-type forms the accessor honors must still pass: numbers
	// for cadences/watermark, duration strings AND numbers for max-ages,
	// and the nested tray/web objects.
	cases := []struct {
		name string
		body map[string]any
	}{
		{"cadenceNumber", map[string]any{"snapshotCadenceConversation": 100}},
		{"watermarkNumber", map[string]any{"storeHighWatermarkGB": 12.5}},
		{"maxAgeDurationString", map[string]any{"snapshotMaxAgeMemory": "24h"}},
		{"maxAgeNumberSeconds", map[string]any{"snapshotMaxAgeTool": 3600}},
		{"hermesIntervalString", map[string]any{"hermesWatchInterval": "5s"}},
		{"webObject", map[string]any{"web": map[string]any{"enabled": true, "port": float64(7600)}}},
		{"trayObject", map[string]any{"tray": map[string]any{"enabled": false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := &fakeConfigAccessor{loadOK: true, cfg: &daemon.Config{}}
			h := NewConfigHandler(acc)

			body, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewReader(body))
			rr := httptest.NewRecorder()
			h.Patch(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
			if acc.patch == nil {
				t.Errorf("accessor.Patch was not called; want applied for valid value")
			}
		})
	}
}

func TestConfigPatch_EmptyBody(t *testing.T) {
	acc := &fakeConfigAccessor{}
	h := NewConfigHandler(acc)

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPatch, "/api/config", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Patch(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestConfigRawPath_HappyPath(t *testing.T) {
	acc := &fakeConfigAccessor{path: "/Users/me/.aplexica/state/config.json"}
	h := NewConfigHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/config/raw-path", nil)
	rr := httptest.NewRecorder()
	h.RawPath(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["path"] != "/Users/me/.aplexica/state/config.json" {
		t.Errorf("path = %v", got["path"])
	}
}
