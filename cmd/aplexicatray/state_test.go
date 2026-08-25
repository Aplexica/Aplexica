//go:build tray

package main

import (
	"testing"
	"time"
)

func TestDeriveState(t *testing.T) {
	base := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	di := func(started time.Time) *DaemonInfo {
		return &DaemonInfo{PID: 1, StartedAt: started, WatchedDir: "/x", Version: "test"}
	}
	cases := []struct {
		name       string
		snap       StatusSnapshot
		lastActive time.Time
		now        time.Time
		want       TrayState
	}{
		{"daemon-down→error",
			StatusSnapshot{DaemonAvailable: false},
			base, base, StateError},
		{"conflicts-trump-everything",
			StatusSnapshot{DaemonAvailable: true, ConflictCount: 1, DaemonInfo: di(base)},
			base, base, StateConflict},
		{"pending-projects-do-not-force-attention-icon",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: &DaemonInfo{
				PID: 1, StartedAt: base, WatchedDir: "/x",
				PendingProjects: []map[string]any{{"id": "repo", "artifactCount": float64(1)}},
			}},
			time.Time{}, base, StateIdle},
		{"adapter-errors-do-not-force-attention-icon",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: &DaemonInfo{
				PID: 1, StartedAt: base, WatchedDir: "/x",
				AdapterLastErrors: map[string]string{"codex": "permission denied"},
			}},
			time.Time{}, base, StateIdle},
		{"store-pressure-needs-attention",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: &DaemonInfo{
				PID: 1, StartedAt: base, WatchedDir: "/x",
				OverHighWatermark: true,
			}},
			base, base, StateConflict},
		{"sync-paused→paused",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: &DaemonInfo{
				PID: 1, StartedAt: base, WatchedDir: "/x", Paused: true,
			}},
			base, base, StatePaused},
		{"recent-activity→active",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: di(base)},
			base.Add(-5 * time.Second), base, StateActive},
		{"long-quiet-still-running→idle",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: di(base.Add(-10 * time.Minute))},
			base.Add(-10 * time.Minute), base, StateIdle},
		{"daemon-up-nothing→idle",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: di(base.Add(-1 * time.Minute))},
			time.Time{}, base, StateIdle},
		{"no-daemon-info-not-paused",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: nil},
			time.Time{}, base, StateIdle},
		{"daemon-LastActivity-recent→active (overrides absent tick)",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: &DaemonInfo{
				PID: 1, StartedAt: base.Add(-1 * time.Hour),
				WatchedDir: "/x", Version: "v0.39.0",
				LastActivity: base.Add(-5 * time.Second),
			}},
			time.Time{}, base, StateActive},
		{"daemon-LastActivity-stale-long→idle",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: &DaemonInfo{
				PID: 1, StartedAt: base.Add(-1 * time.Hour),
				WatchedDir: "/x", Version: "v0.39.0",
				LastActivity: base.Add(-10 * time.Minute),
			}},
			time.Time{}, base, StateIdle},
		{"daemon-LastActivity-overrides-stale-tick",
			StatusSnapshot{DaemonAvailable: true, DaemonInfo: &DaemonInfo{
				PID: 1, StartedAt: base.Add(-1 * time.Hour),
				WatchedDir: "/x", Version: "v0.39.0",
				LastActivity: base.Add(-5 * time.Second),
			}},
			base.Add(-10 * time.Minute), base, StateActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveState(tc.snap, tc.lastActive, tc.now,
				30*time.Second, 5*time.Minute)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}
