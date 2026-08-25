package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeDaemonAccessor is a minimal stand-in for the live daemon state
// the DaemonHandler reads at request time.
type fakeDaemonAccessor struct {
	version        string
	pid            int
	watchedDir     string
	paused         bool
	startedAt      time.Time
	state          string
	pendingImports int
	pauseErr       error
	resumeErr      error
	pausedCalls    int
	resumedCalls   int
}

func (f *fakeDaemonAccessor) Version() string      { return f.version }
func (f *fakeDaemonAccessor) PID() int             { return f.pid }
func (f *fakeDaemonAccessor) WatchedDir() string   { return f.watchedDir }
func (f *fakeDaemonAccessor) Paused() bool         { return f.paused }
func (f *fakeDaemonAccessor) StartedAt() time.Time { return f.startedAt }
func (f *fakeDaemonAccessor) State() string        { return f.state }
func (f *fakeDaemonAccessor) PendingImports() int  { return f.pendingImports }
func (f *fakeDaemonAccessor) Pause() error {
	f.pausedCalls++
	if f.pauseErr != nil {
		return f.pauseErr
	}
	f.paused = true
	return nil
}
func (f *fakeDaemonAccessor) Resume() error {
	f.resumedCalls++
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.paused = false
	return nil
}

func TestDaemonGET_HappyPath(t *testing.T) {
	startedAt := time.Now().Add(-2 * time.Hour)
	acc := &fakeDaemonAccessor{
		version:        "v0.107.0-test",
		pid:            12345,
		watchedDir:     "/tmp/watch",
		paused:         false,
		startedAt:      startedAt,
		state:          "active",
		pendingImports: 2,
	}
	h := NewDaemonHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/daemon", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["version"] != "v0.107.0-test" {
		t.Errorf("version = %v, want v0.107.0-test", got["version"])
	}
	if got["pid"] != float64(12345) {
		t.Errorf("pid = %v, want 12345", got["pid"])
	}
	if got["watchedDir"] != "/tmp/watch" {
		t.Errorf("watchedDir = %v, want /tmp/watch", got["watchedDir"])
	}
	if got["paused"] != false {
		t.Errorf("paused = %v, want false", got["paused"])
	}
	if got["pendingImports"] != float64(2) {
		t.Errorf("pendingImports = %v, want 2", got["pendingImports"])
	}
	// uptime must be a non-negative number of seconds.
	if u, ok := got["uptime"].(float64); !ok || u < 0 {
		t.Errorf("uptime = %v (type %T), want non-negative number", got["uptime"], got["uptime"])
	}
}

func TestDaemonPause_HappyPath(t *testing.T) {
	acc := &fakeDaemonAccessor{paused: false}
	h := NewDaemonHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/pause", nil)
	rr := httptest.NewRecorder()
	h.Pause(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if acc.pausedCalls != 1 {
		t.Errorf("Pause called %d times, want 1", acc.pausedCalls)
	}
	if !acc.paused {
		t.Errorf("accessor.paused = false after Pause, want true")
	}
}

func TestDaemonResume_HappyPath(t *testing.T) {
	acc := &fakeDaemonAccessor{paused: true}
	h := NewDaemonHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/resume", nil)
	rr := httptest.NewRecorder()
	h.Resume(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if acc.resumedCalls != 1 {
		t.Errorf("Resume called %d times, want 1", acc.resumedCalls)
	}
	if acc.paused {
		t.Errorf("accessor.paused = true after Resume, want false")
	}
}

func TestDaemonPause_ErrorReturns500(t *testing.T) {
	acc := &fakeDaemonAccessor{pauseErr: errFake("boom")}
	h := NewDaemonHandler(acc)

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/pause", nil)
	rr := httptest.NewRecorder()
	h.Pause(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code == "" {
		t.Errorf("error envelope missing code: %+v", body)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
