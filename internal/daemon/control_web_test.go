package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestControl_WebIssueToken_HappyPath(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{
		PID:        1234,
		StartedAt:  time.Now().UTC(),
		WatchedDir: "/tmp",
		Version:    "v0.0.0-test",
	}, nil)
	srv.SetWebTokenIssuer(func() (string, error) {
		return "http://127.0.0.1:51234/?bootstrap=ABC123", nil
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	resp, err := SendCommand(sockPath, Request{Command: "web-issue-token"})
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if !resp.OK {
		t.Fatalf("OK=false, error=%q", resp.Error)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data wrong type: %T", resp.Data)
	}
	if data["url"] != "http://127.0.0.1:51234/?bootstrap=ABC123" {
		t.Errorf("url = %v, want bootstrap URL", data["url"])
	}
}

func TestControl_WebIssueToken_WithoutIssuer_ReturnsError(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{}, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	resp, err := SendCommand(sockPath, Request{Command: "web-issue-token"})
	if err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if resp.OK {
		t.Error("OK=true; expected false (no issuer wired)")
	}
	if resp.Error == "" {
		t.Error("Error empty; expected explanation")
	}
}

func TestControl_WebIssueToken_IssuerErrorPropagates(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{}, nil)
	srv.SetWebTokenIssuer(func() (string, error) {
		return "", errors.New("listener not bound yet")
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	resp, _ := SendCommand(sockPath, Request{Command: "web-issue-token"})
	if resp.OK {
		t.Error("OK=true; expected false")
	}
	if resp.Error != "listener not bound yet" {
		t.Errorf("Error = %q, want %q", resp.Error, "listener not bound yet")
	}
}

func TestControl_WebRevokeSessions_HappyPath(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "test.sock")

	srv := NewControlServer(sockPath, &StatusInfo{}, nil)
	srv.SetWebSessionRevoker(func() int { return 3 })
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	resp, _ := SendCommand(sockPath, Request{Command: "web-revoke-sessions"})
	if !resp.OK {
		t.Fatalf("OK=false; error=%q", resp.Error)
	}
	data, _ := resp.Data.(map[string]any)
	if float64(3) != data["revoked"] {
		t.Errorf("revoked = %v, want 3", data["revoked"])
	}
}
