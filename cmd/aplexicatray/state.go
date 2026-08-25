//go:build tray

package main

import (
	"time"

	"github.com/aplexica/aplexica/internal/i18n"
)

type TrayState int

const (
	StateIdle TrayState = iota
	StateActive
	StatePaused
	StateConflict
	StateError
)

// String returns the localized state name (via the i18n catalog).
// Used in the Status header label rendered in the tray menu.
func (s TrayState) String() string {
	switch s {
	case StateIdle:
		return i18n.T("tray_state_idle")
	case StateActive:
		return i18n.T("tray_state_active")
	case StatePaused:
		return i18n.T("tray_state_paused")
	case StateConflict:
		return i18n.T("tray_state_conflict")
	case StateError:
		return i18n.T("tray_state_error")
	default:
		return i18n.T("tray_state_unknown")
	}
}

// deriveState maps the latest StatusSnapshot to a TrayState. Pure
// function — testable without any GUI dependency.
//
// Rules:
//
//	Error    : daemon unavailable (stopped)
//	Conflict : active conflicts or critical storage pressure
//	Paused   : global sync pause is active
//	Active   : last-known activity within activeWindow
//	Idle     : default — daemon up, nothing urgent to do
//
// "Last-known activity" prefers the daemon-supplied
// DaemonInfo.LastActivity (v0.39.0+) when populated; otherwise it falls
// back to the caller-supplied lastActive (the v0.36.0 snapshot-
// arrival tick-liveness proxy, kept for backward compatibility with
// older daemons that don't fill the field). lastActive == zero means
// the caller has observed no activity either.
func deriveState(s StatusSnapshot, lastActive time.Time, now time.Time,
	activeWindow, pausedThreshold time.Duration) TrayState {
	if !s.DaemonAvailable {
		return StateError
	}
	if hasActionNotifications(s) {
		return StateConflict
	}
	if s.DaemonInfo != nil && s.DaemonInfo.Paused {
		return StatePaused
	}
	// Prefer the daemon-supplied LastActivity (v0.39.0). Older daemons
	// omit it (json omitempty + zero time.Time); fall through to the
	// tick proxy.
	active := lastActive
	if s.DaemonInfo != nil && !s.DaemonInfo.LastActivity.IsZero() {
		active = s.DaemonInfo.LastActivity
	}
	_ = pausedThreshold // retained for flag/config compatibility; quiet is still running.
	if !active.IsZero() && now.Sub(active) < activeWindow {
		return StateActive
	}
	return StateIdle
}

func hasActionNotifications(s StatusSnapshot) bool {
	if s.ConflictCount > 0 {
		return true
	}
	if s.DaemonInfo == nil {
		return false
	}
	return s.DaemonInfo.OverHighWatermark ||
		s.DaemonInfo.OverEmergency
}
