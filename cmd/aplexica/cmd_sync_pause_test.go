package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/pausestate"
)

func newPauseState() pausestate.State {
	return pausestate.State{Adapters: map[string]pausestate.AdapterState{}}
}

// An expired global pause (non-zero Until in the past) must render as
// "not paused" — the orchestrator already treats it as resumed, so the
// read-only display must agree even before the daemon's periodic
// CleanExpired sweep removes it from disk.
func TestRenderPauseStatus_ExpiredGlobalShowsNotPaused(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	st := newPauseState()
	st.Global.Paused = true
	st.Global.Until = now.Add(-1 * time.Hour) // expired

	var buf bytes.Buffer
	if err := renderPauseStatus(st, now, &buf); err != nil {
		t.Fatalf("renderPauseStatus: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "sync: not paused") {
		t.Errorf("expired global pause should show 'sync: not paused', got:\n%s", got)
	}
	if strings.Contains(got, "global") {
		t.Errorf("expired global pause should not appear in the table, got:\n%s", got)
	}
}

// An expired adapter pause must be skipped; a still-active one (or one
// with no expiry) must still render.
func TestRenderPauseStatus_ExpiredAdapterSkippedActiveShown(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	st := newPauseState()
	st.Adapters["codex"] = pausestate.AdapterState{Paused: true, Until: now.Add(-time.Minute)} // expired
	st.Adapters["hermes"] = pausestate.AdapterState{Paused: true}                              // no expiry → active

	var buf bytes.Buffer
	if err := renderPauseStatus(st, now, &buf); err != nil {
		t.Fatalf("renderPauseStatus: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "codex") {
		t.Errorf("expired adapter 'codex' must not render, got:\n%s", got)
	}
	if !strings.Contains(got, "adapter:hermes") {
		t.Errorf("active adapter 'hermes' must render, got:\n%s", got)
	}
}

// A future-dated global pause must still render as paused with its
// expiry timestamp.
func TestRenderPauseStatus_FutureGlobalShowsPaused(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	st := newPauseState()
	st.Global.Paused = true
	st.Global.Until = now.Add(time.Hour) // active

	var buf bytes.Buffer
	if err := renderPauseStatus(st, now, &buf); err != nil {
		t.Fatalf("renderPauseStatus: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "global") || !strings.Contains(got, "paused") {
		t.Errorf("future global pause should render as paused, got:\n%s", got)
	}
	if strings.Contains(got, "sync: not paused") {
		t.Errorf("future global pause must not report not-paused, got:\n%s", got)
	}
}

// A no-expiry global pause renders as paused with "(no expiry)".
func TestRenderPauseStatus_NoExpiryGlobalShowsPaused(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	st := newPauseState()
	st.Global.Paused = true // zero Until → no expiry

	var buf bytes.Buffer
	if err := renderPauseStatus(st, now, &buf); err != nil {
		t.Fatalf("renderPauseStatus: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "global") || !strings.Contains(got, "(no expiry)") {
		t.Errorf("no-expiry global pause should render with '(no expiry)', got:\n%s", got)
	}
}
