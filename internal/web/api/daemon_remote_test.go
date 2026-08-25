package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// daemonRemoteStub satisfies both DaemonAccessor and
// RemoteStatusAccessor so the handler's optional cast lands.
type daemonRemoteStub struct {
	configured   bool
	enabled      bool
	connState    string
	restartCount uint64
}

func (s daemonRemoteStub) Version() string        { return "v0.108.0-test" }
func (s daemonRemoteStub) PID() int               { return 4321 }
func (s daemonRemoteStub) WatchedDir() string     { return "/tmp/dir" }
func (s daemonRemoteStub) Paused() bool           { return false }
func (s daemonRemoteStub) StartedAt() time.Time   { return time.Now().Add(-5 * time.Second) }
func (s daemonRemoteStub) State() string          { return "idle" }
func (s daemonRemoteStub) PendingImports() int    { return 0 }
func (s daemonRemoteStub) Pause() error           { return nil }
func (s daemonRemoteStub) Resume() error          { return nil }
func (s daemonRemoteStub) RemoteConfigured() bool { return s.configured }
func (s daemonRemoteStub) RemoteEnabled() bool    { return s.enabled }
func (s daemonRemoteStub) RemoteConnState() string {
	if s.connState == "" {
		return "unknown"
	}
	return s.connState
}
func (s daemonRemoteStub) RemoteRestartCount() uint64 { return s.restartCount }

// daemonOSSOnlyStub satisfies DaemonAccessor but NOT
// RemoteStatusAccessor — represents the OSS-only daemon
// configuration where no remote plugin is wired.
type daemonOSSOnlyStub struct{}

func (daemonOSSOnlyStub) Version() string      { return "v0.108.0-test" }
func (daemonOSSOnlyStub) PID() int             { return 4321 }
func (daemonOSSOnlyStub) WatchedDir() string   { return "/tmp/dir" }
func (daemonOSSOnlyStub) Paused() bool         { return false }
func (daemonOSSOnlyStub) StartedAt() time.Time { return time.Now().Add(-5 * time.Second) }
func (daemonOSSOnlyStub) State() string        { return "idle" }
func (daemonOSSOnlyStub) PendingImports() int  { return 0 }
func (daemonOSSOnlyStub) Pause() error         { return nil }
func (daemonOSSOnlyStub) Resume() error        { return nil }

func TestDaemonGET_OmitsRemoteWhenAccessorIsOSSOnly(t *testing.T) {
	h := NewDaemonHandler(daemonOSSOnlyStub{})
	req := httptest.NewRequest(http.MethodGet, "/api/daemon", nil)
	rec := httptest.NewRecorder()
	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// `remote` must be absent (omitempty trims the optional sub-obj).
	if strings.Contains(rec.Body.String(), "remote") {
		t.Errorf("body = %s; expected no 'remote' key for OSS-only accessor", rec.Body.String())
	}
}

func TestDaemonGET_IncludesRemoteWhenAccessorImplementsIt(t *testing.T) {
	stub := daemonRemoteStub{
		configured:   true,
		enabled:      true,
		connState:    "connected",
		restartCount: 2,
	}
	h := NewDaemonHandler(stub)
	req := httptest.NewRequest(http.MethodGet, "/api/daemon", nil)
	rec := httptest.NewRecorder()
	h.Get(rec, req)

	var body struct {
		Remote *remoteStatusBody `json:"remote"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Remote == nil {
		t.Fatal("body.Remote nil; expected populated sub-object")
	}
	if !body.Remote.Configured || !body.Remote.Enabled {
		t.Errorf("Remote = %+v; expected configured+enabled", body.Remote)
	}
	if body.Remote.ConnState != "connected" {
		t.Errorf("ConnState = %q", body.Remote.ConnState)
	}
	if body.Remote.RestartCount != 2 {
		t.Errorf("RestartCount = %d", body.Remote.RestartCount)
	}
}

func TestDaemonGET_RemoteConfiguredFalseStillEmitsSubObject(t *testing.T) {
	stub := daemonRemoteStub{
		configured: false,
		enabled:    false,
		connState:  "unknown",
	}
	h := NewDaemonHandler(stub)
	req := httptest.NewRequest(http.MethodGet, "/api/daemon", nil)
	rec := httptest.NewRecorder()
	h.Get(rec, req)

	if !strings.Contains(rec.Body.String(), `"remote":`) {
		t.Errorf("body = %s; expected remote sub-object even when not configured", rec.Body.String())
	}
}
