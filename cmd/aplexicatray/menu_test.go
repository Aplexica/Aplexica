//go:build tray

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
)

// TestApply_ConflictCountExceedsArray is the regression test for the
// latent index-out-of-range panic in apply(): the per-conflict submenu
// repaint must derive its visible-slot bound from len(s.Conflicts), NOT
// from s.ConflictCount. ConflictCount and Conflicts are independent JSON
// fields on the wire (snapshot.go); a daemon bug, a truncated JSON line,
// or a future daemon that reports a total count but a display-truncated
// array can deliver ConflictCount > len(Conflicts). When that happens
// the old code indexed s.Conflicts[i] past the end and panicked on the
// tray's only run goroutine, taking the whole tray (icon) down.
//
// apply() is safe to call here without a running systray event loop: the
// menu items built by onReady are registered but unbacked by a native
// status item, so the systray Set*/Show/Hide calls are effectively
// no-ops (verified against fyne.io/systray v1.12.1).
func TestApply_ConflictCountExceedsArray(t *testing.T) {
	cases := []struct {
		name          string
		conflictCount int
		conflicts     []Conflict
	}{
		{
			name:          "count-greater-than-array",
			conflictCount: 3,
			conflicts: []Conflict{
				{ArtifactID: "only-one", Kind: "memory"},
			},
		},
		{
			name:          "positive-count-nil-array",
			conflictCount: 2,
			conflicts:     nil,
		},
		{
			name:          "count-far-exceeds-slot-count",
			conflictCount: conflictSlotCount + 5,
			conflicts: []Conflict{
				{ArtifactID: "a", Kind: "tool"},
				{ArtifactID: "b", Kind: "skill"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTray("aplexica")
			tr.onReady(t.TempDir()) // build the pre-allocated submenu slots
			snap := StatusSnapshot{
				DaemonAvailable: true,
				ConflictCount:   tc.conflictCount,
				Conflicts:       tc.conflicts,
				DaemonInfo: &DaemonInfo{
					PID: 1, StartedAt: time.Now(), WatchedDir: "/x",
				},
			}
			// Must not panic. Pre-fix this index-out-of-ranges at
			// s.Conflicts[i] inside the conflict-slot repaint loop.
			tr.apply(snap, time.Now(), 60*time.Second, 10*time.Minute)
		})
	}
}

// TestApply_AllConflictsRendered guards the happy path: when the array
// and count agree, every conflict (up to the slot cap) is still applied
// without error after the bound is moved to len(s.Conflicts).
func TestApply_AllConflictsRendered(t *testing.T) {
	tr := newTray("aplexica")
	tr.onReady(t.TempDir())
	snap := StatusSnapshot{
		DaemonAvailable: true,
		ConflictCount:   2,
		Conflicts: []Conflict{
			{ArtifactID: "a", Kind: "memory"},
			{ArtifactID: "b", Kind: "tool"},
		},
		DaemonInfo: &DaemonInfo{PID: 1, StartedAt: time.Now(), WatchedDir: "/x"},
	}
	tr.apply(snap, time.Now(), 60*time.Second, 10*time.Minute)
}

func TestDaemonControlArgs_ForwardsTrayStateAndLogDirs(t *testing.T) {
	oldState, oldLog := *flagStateDir, *flagLogDir
	*flagStateDir = "/state"
	*flagLogDir = "/logs"
	t.Cleanup(func() {
		*flagStateDir = oldState
		*flagLogDir = oldLog
	})

	gotStart := daemonControlArgs("start", "/watch")
	wantStart := []string{"daemon", "--state-dir", "/state", "--log-dir", "/logs", "start", "--dir", "/watch"}
	if len(gotStart) != len(wantStart) {
		t.Fatalf("daemonControlArgs(start) length = %d, want %d: %#v", len(gotStart), len(wantStart), gotStart)
	}
	for i := range wantStart {
		if gotStart[i] != wantStart[i] {
			t.Fatalf("daemonControlArgs(start)[%d] = %q, want %q; full args %#v", i, gotStart[i], wantStart[i], gotStart)
		}
	}

	gotStop := daemonControlArgs("stop", "")
	wantStop := []string{"daemon", "--state-dir", "/state", "stop"}
	if len(gotStop) != len(wantStop) {
		t.Fatalf("daemonControlArgs(stop) length = %d, want %d: %#v", len(gotStop), len(wantStop), gotStop)
	}
	for i := range wantStop {
		if gotStop[i] != wantStop[i] {
			t.Fatalf("daemonControlArgs(stop)[%d] = %q, want %q; full args %#v", i, gotStop[i], wantStop[i], gotStop)
		}
	}
}

func TestDaemonStartDir_PrefersWatchedDir(t *testing.T) {
	oldState := *flagStateDir
	*flagStateDir = t.TempDir()
	t.Cleanup(func() { *flagStateDir = oldState })
	if err := daemon.WriteConfig(filepath.Join(*flagStateDir, "config.json"), &daemon.Config{Dir: "/from-config"}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	tr := newTray("aplexica")
	tr.watchedDir = "/from-snapshot"

	if got := tr.daemonStartDir(); got != "/from-snapshot" {
		t.Fatalf("daemonStartDir() = %q, want /from-snapshot", got)
	}
}

func TestDaemonStartDir_FallsBackToConfig(t *testing.T) {
	oldState := *flagStateDir
	*flagStateDir = t.TempDir()
	t.Cleanup(func() { *flagStateDir = oldState })
	if err := daemon.WriteConfig(filepath.Join(*flagStateDir, "config.json"), &daemon.Config{Dir: "/from-config"}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	tr := newTray("aplexica")

	if got := tr.daemonStartDir(); got != "/from-config" {
		t.Fatalf("daemonStartDir() = %q, want /from-config", got)
	}
}

func TestDaemonStartDir_FallsBackToHomeForLegacyConfig(t *testing.T) {
	oldState := *flagStateDir
	*flagStateDir = t.TempDir()
	t.Cleanup(func() { *flagStateDir = oldState })

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir: %v", err)
	}
	tr := newTray("aplexica")

	if got := tr.daemonStartDir(); got != home {
		t.Fatalf("daemonStartDir() = %q, want home %q", got, home)
	}
}
