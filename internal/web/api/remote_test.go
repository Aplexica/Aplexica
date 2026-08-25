package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeRemoteAccessor struct {
	pairDeviceID  string
	pairAccountID string
	pairErr       error
	pairCalls     []pairCall

	statusConfigured   bool
	statusEnabled      bool
	statusPaired       bool
	statusDeviceID     string
	statusAccountID    string
	statusConnState    string
	statusRestartCount uint64
	statusErr          error

	verifyConnected bool
	verifyMessage   string
	verifyErr       error

	unpairErr   error
	unpairCalls int
}

type pairCall struct {
	token, deviceName string
}

func (f *fakeRemoteAccessor) Pair(_ context.Context, token, deviceName string) (string, string, error) {
	f.pairCalls = append(f.pairCalls, pairCall{token, deviceName})
	return f.pairDeviceID, f.pairAccountID, f.pairErr
}

func (f *fakeRemoteAccessor) Status(_ context.Context) (bool, bool, bool, string, string, string, uint64, error) {
	return f.statusConfigured, f.statusEnabled, f.statusPaired,
		f.statusDeviceID, f.statusAccountID, f.statusConnState, f.statusRestartCount, f.statusErr
}

func (f *fakeRemoteAccessor) Verify(_ context.Context) (bool, string, error) {
	return f.verifyConnected, f.verifyMessage, f.verifyErr
}

func (f *fakeRemoteAccessor) Unpair(_ context.Context) error {
	f.unpairCalls++
	return f.unpairErr
}

func TestRemoteUnpair_OK(t *testing.T) {
	acc := &fakeRemoteAccessor{}
	h := NewRemoteHandler(acc)
	req := httptest.NewRequest(http.MethodPost, "/api/remote/unpair", nil)
	rr := httptest.NewRecorder()
	h.Unpair(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if acc.unpairCalls != 1 {
		t.Fatalf("unpairCalls = %d; want 1", acc.unpairCalls)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["unpaired"] != true {
		t.Errorf("response = %+v", got)
	}
}

func TestRemoteUnpair_NotConfigured(t *testing.T) {
	acc := &fakeRemoteAccessor{unpairErr: ErrRemoteNotConfigured}
	h := NewRemoteHandler(acc)
	req := httptest.NewRequest(http.MethodPost, "/api/remote/unpair", nil)
	rr := httptest.NewRecorder()
	h.Unpair(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["code"] != "remote_not_configured" {
		t.Errorf("response = %+v", got)
	}
}

func TestRemotePair_HappyPath(t *testing.T) {
	acc := &fakeRemoteAccessor{pairDeviceID: "dev-1", pairAccountID: "acct-1"}
	h := NewRemoteHandler(acc)

	body, _ := json.Marshal(map[string]any{"token": "tok-abc", "device_name": "example-mac"})
	req := httptest.NewRequest(http.MethodPost, "/api/remote/pair", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Pair(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["paired"] != true || got["device_id"] != "dev-1" || got["account_id"] != "acct-1" {
		t.Errorf("response = %+v", got)
	}
	if len(acc.pairCalls) != 1 || acc.pairCalls[0].token != "tok-abc" || acc.pairCalls[0].deviceName != "example-mac" {
		t.Errorf("pair calls = %+v", acc.pairCalls)
	}
}

func TestRemotePair_EmptyToken(t *testing.T) {
	acc := &fakeRemoteAccessor{}
	h := NewRemoteHandler(acc)

	body, _ := json.Marshal(map[string]any{"token": "   "})
	req := httptest.NewRequest(http.MethodPost, "/api/remote/pair", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Pair(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if code := errCode(t, rr); code != "validation" {
		t.Errorf("code = %q, want validation", code)
	}
	if len(acc.pairCalls) != 0 {
		t.Errorf("Pair should not have been called: %+v", acc.pairCalls)
	}
}

func TestRemotePair_NotConfigured(t *testing.T) {
	acc := &fakeRemoteAccessor{pairErr: ErrRemoteNotConfigured}
	h := NewRemoteHandler(acc)

	body, _ := json.Marshal(map[string]any{"token": "tok"})
	req := httptest.NewRequest(http.MethodPost, "/api/remote/pair", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Pair(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if code := errCode(t, rr); code != "remote_not_configured" {
		t.Errorf("code = %q, want remote_not_configured", code)
	}
}

func TestRemotePair_PairFailed(t *testing.T) {
	acc := &fakeRemoteAccessor{pairErr: ErrPairFailed}
	h := NewRemoteHandler(acc)

	body, _ := json.Marshal(map[string]any{"token": "tok"})
	req := httptest.NewRequest(http.MethodPost, "/api/remote/pair", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.Pair(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if code := errCode(t, rr); code != "pair_failed" {
		t.Errorf("code = %q, want pair_failed", code)
	}
}

func TestRemoteStatus_HappyPath(t *testing.T) {
	acc := &fakeRemoteAccessor{
		statusConfigured:   true,
		statusEnabled:      true,
		statusPaired:       true,
		statusDeviceID:     "dev-1",
		statusAccountID:    "acct-1",
		statusConnState:    "connected",
		statusRestartCount: 3,
	}
	h := NewRemoteHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/remote/status", nil)
	rr := httptest.NewRecorder()
	h.Status(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["configured"] != true || got["paired"] != true || got["conn_state"] != "connected" {
		t.Errorf("response = %+v", got)
	}
	if got["restart_count"] != float64(3) {
		t.Errorf("restart_count = %v, want 3", got["restart_count"])
	}
}

func TestRemoteStatus_Unconfigured(t *testing.T) {
	acc := &fakeRemoteAccessor{statusConnState: "unknown"}
	h := NewRemoteHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/remote/status", nil)
	rr := httptest.NewRecorder()
	h.Status(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["configured"] != false || got["paired"] != false {
		t.Errorf("response = %+v", got)
	}
}

func TestRemoteVerify_Connected(t *testing.T) {
	acc := &fakeRemoteAccessor{verifyConnected: true, verifyMessage: "connect-check: OK — mTLS connected"}
	h := NewRemoteHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/remote/verify", nil)
	rr := httptest.NewRecorder()
	h.Verify(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["connected"] != true {
		t.Errorf("response = %+v", got)
	}
}

func TestRemoteVerify_NotPaired(t *testing.T) {
	acc := &fakeRemoteAccessor{verifyErr: ErrNotPaired}
	h := NewRemoteHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/remote/verify", nil)
	rr := httptest.NewRecorder()
	h.Verify(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if code := errCode(t, rr); code != "not_paired" {
		t.Errorf("code = %q, want not_paired", code)
	}
}

func TestRemoteVerify_NotConfigured(t *testing.T) {
	acc := &fakeRemoteAccessor{verifyErr: ErrRemoteNotConfigured}
	h := NewRemoteHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/remote/verify", nil)
	rr := httptest.NewRecorder()
	h.Verify(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if code := errCode(t, rr); code != "remote_not_configured" {
		t.Errorf("code = %q, want remote_not_configured", code)
	}
}

// errCode decodes the standard ErrorBody envelope and returns its Code.
func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var eb ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &eb); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, rr.Body.String())
	}
	return eb.Code
}
